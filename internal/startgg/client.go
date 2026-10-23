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
	"gorm.io/gorm/clause" // 🆕
)

const (
	apiURL           = "https://api.start.gg/gql/alpha"
	leagueType       = "2xko_online"
	standingsPerPage = 50
	requestDelay     = 800 * time.Millisecond
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
					Slug   string `json:"slug"` // 🆕 Slug del usuario en start.gg
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
              user {
                slug # 🆕 Solicitamos el slug del usuario
                images { type url }
              }
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

// 🆕 recordResult guarda los puntos de un jugador en UN evento.
// Si el mismo jugador aparece dos veces en el mismo evento, suma.
func recordResult(tx *gorm.DB, playerID, eventID, points int) error {
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "player_id"}, {Name: "league"}, {Name: "event_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"points": gorm.Expr("player_results.points + ?", points),
		}),
	}).Create(&models.PlayerResult{
		PlayerID: playerID,
		League:   leagueType,
		EventID:  eventID,
		Points:   points,
	}).Error
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

			err = db.Transaction(func(tx *gorm.DB) error {
				if err := applyStandings(tx, ev.ID, nodes); err != nil { // 🆕 ahora recibe ev.ID
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

// 🆕 firma nueva: recibe eventID
func applyStandings(tx *gorm.DB, eventID int, nodes []standingNode) error {

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

		if strings.EqualFold(gamerTag, "delete-user") || strings.EqualFold(rawName, "delete-user") ||
			strings.EqualFold(gamerTag, "deleted user") || strings.EqualFold(rawName, "deleted user") {
			log.Printf("   ⏭️  Omitiendo usuario bloqueado/eliminado: %s\n", rawName)
			continue
		}

		for _, participant := range node.Entrant.Participants {
			playerID := participant.Player.ID
			if playerID == 0 || blockedPlayerIDs[playerID] { // 🆕
				continue
			}

			// 🆕 cuenta duplicada: los puntos van a la cuenta real
			merged := false
			if realID, ok := playerIDAliases[playerID]; ok {
				playerID = realID
				merged = true
			}

			avatarURL := ""

			profileURL := ""
			if participant.Player.User.Slug != "" {
				slug := participant.Player.User.Slug
				if strings.HasPrefix(slug, "user/") {
					profileURL = "https://www.start.gg/" + slug
				} else {
					profileURL = "https://www.start.gg/user/" + slug
				}
			}

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
					ID:         playerID,
					League:     leagueType,
					GamerTag:   gamerTag,
					Sponsor:    sponsor,
					Character:  "2XKO_FIGHTER",
					AvatarURL:  avatarURL,
					ProfileURL: profileURL, // 🆕
					Points:     earnedPoints,
				}
				if err := tx.Create(&player).Error; err != nil {
					return err
				}
			case err != nil:
				return err
			default:
				player.Points += earnedPoints
				if !merged {
					player.GamerTag = gamerTag
					if sponsor != "" {
						player.Sponsor = sponsor
					}
					if avatarURL != "" {
						player.AvatarURL = avatarURL
					}
					if profileURL != "" { // 🆕
						player.ProfileURL = profileURL
					}
				}
				if err := tx.Save(&player).Error; err != nil {
					return err
				}
			}

			if err := recordResult(tx, playerID, eventID, earnedPoints); err != nil {
				return err
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
	default:
		return 0
	}
}
