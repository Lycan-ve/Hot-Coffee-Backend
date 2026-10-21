package startgg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"smash-backend/internal/models"

	"gorm.io/gorm"
)

const (
	apiURL           = "https://api.start.gg/gql/alpha"
	leagueType       = "2xko_online"
	standingsPerPage = 50                     // pequeño a propósito: Start.gg limita ~1000 objetos por consulta
	requestDelay     = 800 * time.Millisecond // Start.gg permite ~80 peticiones por minuto
	maxRetries       = 3
)

// ---------- Discord ----------

type Attachment struct {
	URL      string `json:"url"`
	FileName string `json:"fileName"`
}

type ForwardedMessage struct {
	Content     string       `json:"content"`
	Attachments []Attachment `json:"attachments"`
}

type DiscordMessage struct {
	Content          string            `json:"content"`
	Attachments      []Attachment      `json:"attachments"`
	ForwardedMessage *ForwardedMessage `json:"forwardedMessage"`
}

type DiscordExport struct {
	Messages []DiscordMessage `json:"messages"`
}

type TournamentInfo struct {
	Slug       string
	Top8ImgURL string
}

// ProcessedEvent evita sumar dos veces los puntos del mismo evento.
// Añádelo a AutoMigrate en main.go:
//
//	db.AutoMigrate(&models.Player{}, &startgg.ProcessedEvent{})
type ProcessedEvent struct {
	EventID int `gorm:"primaryKey;autoIncrement:false"`
	Slug    string
	Name    string
}

// ---------- Start.gg (GraphQL) ----------

type gqlEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type eventSummary struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Videogame struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"videogame"`
}

type tournamentEventsResp struct {
	Tournament *struct {
		Name   string         `json:"name"`
		Events []eventSummary `json:"events"`
	} `json:"tournament"`
}

type standingNode struct {
	Placement int `json:"placement"`
	Entrant   struct {
		Name         string `json:"name"`
		Participants []struct {
			Player struct {
				ID   int `json:"id"`
				User struct {
					Images []struct {
						Type string `json:"type"`
						URL  string `json:"url"`
					} `json:"images"`
				} `json:"user"`
			} `json:"player"`
		} `json:"participants"`
	} `json:"entrant"`
}

type eventStandingsResp struct {
	Event *struct {
		Standings struct {
			PageInfo struct {
				TotalPages int `json:"totalPages"`
			} `json:"pageInfo"`
			Nodes []standingNode `json:"nodes"`
		} `json:"standings"`
	} `json:"event"`
}

// Consulta LIGERA: solo la lista de eventos del torneo, sin standings.
const eventsQuery = `
query TournamentEvents($slug: String!) {
  tournament(slug: $slug) {
    name
    events {
      id
      name
      videogame { id name }
    }
  }
}`

// Standings de UN evento, paginados.
const standingsQuery = `
query EventStandings($eventId: ID!, $page: Int!, $perPage: Int!) {
  event(id: $eventId) {
    standings(query: { page: $page, perPage: $perPage }) {
      pageInfo { totalPages }
      nodes {
        placement
        entrant {
          name
          participants {
            player {
              id
              user { images { type url } }
            }
          }
        }
      }
    }
  }
}`

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// gqlDo hace la petición, respeta el rate limit, reintenta en 429/5xx
// y DEVUELVE los errores de GraphQL en vez de ignorarlos.
func gqlDo(client *http.Client, token, query string, vars map[string]interface{}, out interface{}) error {
	body, err := json.Marshal(map[string]interface{}{"query": query, "variables": vars})
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		time.Sleep(requestDelay)

		req, err := http.NewRequest("POST", apiURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			time.Sleep(time.Duration(attempt) * 5 * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
		}

		var env gqlEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("respuesta no es JSON válido: %w", err)
		}
		if len(env.Errors) > 0 {
			return fmt.Errorf("start.gg: %s", env.Errors[0].Message)
		}
		return json.Unmarshal(env.Data, out)
	}
	return fmt.Errorf("reintentos agotados: %v", lastErr)
}

func fetchAllStandings(client *http.Client, token string, eventID int) ([]standingNode, error) {
	var all []standingNode
	for page := 1; ; page++ {
		var r eventStandingsResp
		vars := map[string]interface{}{"eventId": eventID, "page": page, "perPage": standingsPerPage}
		if err := gqlDo(client, token, standingsQuery, vars, &r); err != nil {
			return nil, fmt.Errorf("página %d: %w", page, err)
		}
		if r.Event == nil {
			return nil, fmt.Errorf("el evento %d no devolvió datos", eventID)
		}
		all = append(all, r.Event.Standings.Nodes...)
		if page >= r.Event.Standings.PageInfo.TotalPages {
			break
		}
	}
	return all, nil
}

// ---------- Discord -> slugs ----------

func ExtractTournamentsAndImages(messages []DiscordMessage) []TournamentInfo {
	tournamentMap := make(map[string]string)
	re := regexp.MustCompile(`start\.gg/tournament/([a-zA-Z0-9\-_]+)`)

	for _, msg := range messages {
		content := msg.Content
		var attachments []Attachment = msg.Attachments

		if msg.ForwardedMessage != nil {
			content += " " + msg.ForwardedMessage.Content
			attachments = append(attachments, msg.ForwardedMessage.Attachments...)
		}

		matches := re.FindAllStringSubmatch(content, -1)
		var imgURL string
		if len(attachments) > 0 {
			imgURL = attachments[0].URL
		}

		for _, match := range matches {
			if len(match) > 1 {
				slug := match[1]
				tournamentMap[slug] = imgURL
			}
		}
	}

	var results []TournamentInfo
	for slug, img := range tournamentMap {
		results = append(results, TournamentInfo{Slug: slug, Top8ImgURL: img})
	}
	return results
}

// ---------- Sincronización ----------

func SyncTournamentsFromDiscordJSON(filePath string, apiToken string, db *gorm.DB) {
	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("❌ Error al abrir el archivo de Discord: %v\n", err)
		return
	}
	defer file.Close()

	var exportData DiscordExport
	if err := json.NewDecoder(file).Decode(&exportData); err != nil {
		log.Printf("❌ Error al decodificar el JSON: %v\n", err)
		return
	}

	tournaments := ExtractTournamentsAndImages(exportData.Messages)
	log.Printf("🔍 Se encontraron %d torneos únicos en el canal de Discord.\n", len(tournaments))

	client := &http.Client{Timeout: 30 * time.Second}

	for _, t := range tournaments {
		log.Printf("📥 Procesando torneo Start.gg [Slug: %s]\n", t.Slug)

		var tr tournamentEventsResp
		if err := gqlDo(client, apiToken, eventsQuery, map[string]interface{}{"slug": t.Slug}, &tr); err != nil {
			log.Printf("❌ [%s] no se pudo obtener la lista de eventos: %v\n", t.Slug, err)
			continue
		}
		if tr.Tournament == nil {
			log.Printf("⚠️  [%s] el torneo no existe o no es accesible\n", t.Slug)
			continue
		}
		log.Printf("   ↳ %d eventos encontrados en %s\n", len(tr.Tournament.Events), t.Slug)

		for _, ev := range tr.Tournament.Events {
			vgName := strings.ToLower(ev.Videogame.Name)
			if strings.Contains(vgName, "street fighter") || strings.Contains(vgName, "sf6") {
				log.Printf("   ⏭️  Evento %d (%s) omitido: %s\n", ev.ID, ev.Name, ev.Videogame.Name)
				continue
			}

			var already int64
			db.Model(&ProcessedEvent{}).Where("event_id = ?", ev.ID).Count(&already)
			if already > 0 {
				log.Printf("   ⏭️  Evento %d (%s) ya procesado\n", ev.ID, ev.Name)
				continue
			}

			nodes, err := fetchAllStandings(client, apiToken, ev.ID)
			if err != nil {
				log.Printf("   ❌ Evento %d (%s): %v (se reintentará en la próxima sync)\n", ev.ID, ev.Name, err)
				continue
			}

			// Todo o nada: si algo falla a mitad, no quedan puntos a medias.
			err = db.Transaction(func(tx *gorm.DB) error {
				if err := applyStandings(tx, nodes); err != nil {
					return err
				}
				return tx.Create(&ProcessedEvent{EventID: ev.ID, Slug: t.Slug, Name: ev.Name}).Error
			})
			if err != nil {
				log.Printf("   ❌ Evento %d (%s): error al guardar: %v\n", ev.ID, ev.Name, err)
				continue
			}
			log.Printf("   ✅ Evento %d (%s): %d participantes\n", ev.ID, ev.Name, len(nodes))
		}
	}
	log.Printf("✅ Sincronización completa con los torneos de Discord.\n")
}

func applyStandings(tx *gorm.DB, nodes []standingNode) error {
	for _, node := range nodes {
		earnedPoints := calculatePoints(node.Placement)
		if earnedPoints == 0 {
			continue
		}

		rawName := node.Entrant.Name
		sponsor := ""
		gamerTag := rawName
		if strings.Contains(rawName, " | ") {
			parts := strings.SplitN(rawName, " | ", 2)
			sponsor = parts[0]
			gamerTag = parts[1]
		}

		for _, participant := range node.Entrant.Participants {
			playerID := participant.Player.ID
			if playerID == 0 {
				continue
			}

			avatarURL := ""
			for _, img := range participant.Player.User.Images {
				if img.Type == "profile" {
					avatarURL = img.URL
					break
				}
			}
			if avatarURL == "" && len(participant.Player.User.Images) > 0 {
				avatarURL = participant.Player.User.Images[0].URL
			}

			var player models.Player
			err := tx.First(&player, "id = ? AND league = ?", playerID, leagueType).Error
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				player = models.Player{
					ID:        playerID,
					League:    leagueType,
					GamerTag:  gamerTag,
					Sponsor:   sponsor,
					Character: "2XKO_FIGHTER",
					AvatarURL: avatarURL,
					Points:    earnedPoints,
				}
				if err := tx.Create(&player).Error; err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				player.Points += earnedPoints
				player.GamerTag = gamerTag
				if sponsor != "" {
					player.Sponsor = sponsor
				}
				if avatarURL != "" {
					player.AvatarURL = avatarURL
				}
				if err := tx.Save(&player).Error; err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func calculatePoints(placement int) int {
	switch {
	case placement == 1:
		return 100
	case placement == 2:
		return 70
	case placement == 3:
		return 50
	case placement == 4:
		return 35
	case placement >= 5 && placement <= 6:
		return 20
	case placement >= 7 && placement <= 8:
		return 10
	case placement >= 9 && placement <= 12:
		return 5
	case placement >= 13 && placement <= 16:
		return 3
	case placement > 16:
		return 1
	default:
		return 0
	}
}
