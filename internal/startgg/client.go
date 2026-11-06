package startgg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"smash-backend/internal/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

type TournamentInfo struct {
	Slug       string
	Top8ImgURL string
}

// ProcessedEvent evita sumar dos veces los puntos del mismo evento.
type ProcessedEvent struct {
	EventID int `gorm:"primaryKey;autoIncrement:false"`
	Slug    string
	Name    string
	Image   string // 🆕 ícono del torneo en Start.gg (historial del perfil web)
}

// ---------- Start.gg (GraphQL) ----------

type gqlEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type eventSummary struct {
	ID        int         `json:"id"`
	Name      string      `json:"name"`
	State     interface{} `json:"state"` // 👈 Cambiado a interface{} para aceptar cualquier tipo que envíe Start.gg
	Videogame struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"videogame"`
}

type startggImage struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type tournamentEventsResp struct {
	Tournament *struct {
		Name   string         `json:"name"`
		Images []startggImage `json:"images"`
		Events []eventSummary `json:"events"`
	} `json:"tournament"`
}

type startggAuth struct {
	ExternalID       string `json:"externalId"`
	ExternalUsername string `json:"externalUsername"`
}

type standingNode struct {
	Placement int `json:"placement"`
	Entrant   struct {
		Name         string `json:"name"`
		Participants []struct {
			Player struct {
				ID   int `json:"id"`
				User struct {
					Slug   string `json:"slug"`
					Images []struct {
						Type string `json:"type"`
						URL  string `json:"url"`
					} `json:"images"`
					Authorizations []startggAuth `json:"authorizations"`
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
    images { type url }
    events {
      id
      name
      state # 🆕 Solicitamos el estado del evento a Start.gg
      videogame { id name }
    }
  }
}
`

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
                slug
                images { type url }
                authorizations(types: [DISCORD]) { externalId externalUsername }
              }
            }
          }
        }
      }
    }
  }
}
`

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
				// Los mensajes llegan del más nuevo al más viejo: conserva la
				// primera imagen no vacía (la del mensaje más reciente) y no
				// deja que un mensaje viejo sin adjunto la pise.
				if existing, ok := tournamentMap[slug]; !ok || existing == "" {
					tournamentMap[slug] = imgURL
				}
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
// Si el mismo jugador aparece dos veces en el mismo evento (p. ej. dos
// equipos en un 2v2), conserva su MEJOR puntaje: nunca suma doble.
func recordResult(tx *gorm.DB, playerID, eventID, points int) error {
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "player_id"}, {Name: "league"}, {Name: "event_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"points": gorm.Expr("GREATEST(player_results.points, ?)", points),
		}),
	}).Create(&models.PlayerResult{
		PlayerID: playerID,
		League:   leagueType,
		EventID:  eventID,
		Points:   points,
	}).Error
}

// recordTournamentPlacement guarda el puesto del miembro sin afectar el
// puntaje del ranking. Si una cuenta aparece dos veces en un evento, conserva
// su mejor puesto y el mayor puntaje de esas entradas.
func recordTournamentPlacement(tx *gorm.DB, playerID int, discordID, gamerTag string, eventID, placement, points int) error {
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "discord_id"},
			{Name: "league"},
			{Name: "event_id"},
		},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"player_id": playerID,
			"gamer_tag": gamerTag,
			"placement": gorm.Expr(
				"CASE WHEN tournament_placements.placement = 0 THEN ? ELSE LEAST(tournament_placements.placement, ?) END",
				placement, placement,
			),
			"points": gorm.Expr("GREATEST(tournament_placements.points, ?)", points),
		}),
	}).Create(&models.TournamentPlacement{
		DiscordID: discordID,
		League:    leagueType,
		EventID:   eventID,
		PlayerID:  playerID,
		GamerTag:  gamerTag,
		Placement: placement,
		Points:    points,
	}).Error
}

// resolveDiscordID devuelve el ID de Discord SOLO si ese ID pertenece al servidor.
// Todo se compara por ID numérico (snowflake), nunca por nombre de usuario.
// Orden de prioridad:
//  1. Excepción manual (por id original de Start.gg o por el id ya unificado por alias)
//  2. Discord vinculado en Start.gg (authorizations)
//  3. Solo si Start.gg no expone ningún Discord: el ID que ya se verificó antes y está en la DB
//
// Devuelve (id, "") si es válido, o ("", motivo) si no.
func resolveDiscordID(originalID, playerID int, auths []startggAuth, members map[string]string, storedID string) (string, string) {
	// 1) Excepción manual (Acepta el ID directamente sin validar members[id])
	for _, key := range []int{originalID, playerID} {
		if id, ok := manualDiscordIDs[key]; ok {
			return id, ""
		}
	}

	// 2) Discord vinculado en Start.gg
	linked := make([]string, 0, len(auths))
	for _, a := range auths {
		id := strings.TrimSpace(a.ExternalID)
		if id == "" {
			continue
		}
		if _, ok := members[id]; ok {
			return id, ""
		}
		linked = append(linked, a.ExternalUsername)
	}
	if len(linked) > 0 {
		return "", fmt.Sprintf("su Discord vinculado (%s) no está en el servidor", strings.Join(linked, ", "))
	}

	// 3) Start.gg no muestra Discord (oculto o sin vincular): usar el ID ya verificado antes
	if storedID != "" {
		if _, ok := members[storedID]; ok {
			return storedID, ""
		}
		return "", "el Discord verificado anteriormente ya no está en el servidor"
	}
	return "", "no tiene Discord vinculado en Start.gg (o lo tiene oculto)"
}

// tournamentImage devuelve la imagen de perfil del torneo en Start.gg y, si
// no existe, la primera imagen disponible (p. ej. el banner). Se guarda junto al
// evento y se muestra como ícono en el historial de torneos del perfil web.
func tournamentImage(images []startggImage) string {
	first := ""
	for _, img := range images {
		if img.URL == "" {
			continue
		}
		if img.Type == "profile" {
			return img.URL
		}
		if first == "" {
			first = img.URL
		}
	}
	return first
}

// ---------- Sincronización ----------

func SyncTournamentsFromDiscordLive(tournaments []TournamentInfo, apiToken string, db *gorm.DB, activeMembers map[string]string) {
	log.Printf("🔍 Procesando %d torneos únicos obtenidos desde Discord.", len(tournaments))

	client := &http.Client{Timeout: 30 * time.Second}

	for _, t := range tournaments {
		log.Printf("📥 Procesando torneo Start.gg [Slug: %s]", t.Slug)

		var tr tournamentEventsResp
		if err := gqlDo(client, apiToken, eventsQuery, map[string]interface{}{"slug": t.Slug}, &tr); err != nil {
			log.Printf("❌ [%s] no se pudo obtener la lista de eventos: %v", t.Slug, err)
			continue
		}
		if tr.Tournament == nil {
			log.Printf("⚠️  [%s] el torneo no existe o no es accesible", t.Slug)
			continue
		}
		log.Printf("   ↳ %d eventos encontrados en %s", len(tr.Tournament.Events), t.Slug)

		// 🆕 Ícono del torneo en Start.gg: se guarda junto a cada evento para
		// el historial de torneos del perfil web.
		tourneyImg := tournamentImage(tr.Tournament.Images)

		// 🆕 Excepción de puntaje: en las series TNS y Paragon también suman
		// los puestos 9-16 (5 pts del 9º al 12º, 3 pts del 13º al 16º).
		top16 := isTop16Tournament(tr.Tournament.Name, t.Slug)

		// Guarda/actualiza la imagen del Top 8 publicada en el canal del
		// calendario (tabla tournament_records, para la web).
		if t.Top8ImgURL != "" {
			if err := db.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "slug"}},
				DoUpdates: clause.Assignments(map[string]any{
					"name":       tr.Tournament.Name,
					"top8_image": t.Top8ImgURL,
				}),
			}).Create(&models.TournamentRecord{
				Slug:      t.Slug,
				Name:      tr.Tournament.Name,
				Top8Image: t.Top8ImgURL,
			}).Error; err != nil {
				log.Printf("⚠️  [%s] no se pudo guardar la imagen del Top 8: %v", t.Slug, err)
			}
		}

		for _, ev := range tr.Tournament.Events {
			// Filtro POSITIVO de videojuego: al ranking solo entran los eventos
			// de 2XKO. Antes solo se excluía Street Fighter, así que cualquier
			// otro juego de un torneo multi-juego sumaba puntos de 2XKO.
			vgName := strings.ToLower(ev.Videogame.Name)
			if !strings.Contains(vgName, "2xko") {
				log.Printf("   ⏭️  Evento %d (%s) omitido por videojuego: %s", ev.ID, ev.Name, ev.Videogame.Name)
				continue
			}

			isCompleted := false
			switch v := ev.State.(type) {
			case float64:
				if int(v) == 3 {
					isCompleted = true
				}
			case int:
				if v == 3 {
					isCompleted = true
				}
			case string:
				upperVal := strings.ToUpper(strings.TrimSpace(v))
				if upperVal == "3" || upperVal == "COMPLETED" || upperVal == "COMPLETE" {
					isCompleted = true
				}
			}

			// 🛑 REGLA ESTRICTA: El torneo/evento DEBE estar terminado
			if !isCompleted {
				log.Printf("   ⏭️  Evento %d (%s) omitido: El torneo aún no ha finalizado (Estado actual: %v)", ev.ID, ev.Name, ev.State)
				continue
			}

			var already int64
			db.Model(&ProcessedEvent{}).Where("event_id = ?", ev.ID).Count(&already)
			if already > 0 {
				// 🆕 Backfill: los eventos procesados antes de esta mejora no
				// guardaban el ícono del torneo; se completa sin reprocesar
				// standings ni volver a sumar puntos.
				if tourneyImg != "" {
					var pe ProcessedEvent
					if err := db.First(&pe, "event_id = ?", ev.ID).Error; err == nil && pe.Image == "" {
						db.Model(&ProcessedEvent{}).Where("event_id = ?", ev.ID).Update("image", tourneyImg)
					}
				}
				log.Printf("   ⏭️  Evento %d (%s) ya procesado anteriormente", ev.ID, ev.Name)
				continue
			}

			nodes, err := fetchAllStandings(client, apiToken, ev.ID)
			if err != nil {
				log.Printf("   ❌ Evento %d (%s): %v", ev.ID, ev.Name, err)
				continue
			}

			// Standings vacíos = Start.gg todavía no publicó resultados (o hubo
			// un fallo temporal): NO marcar el evento como procesado. Así la
			// próxima sync lo reintenta en vez de perder sus puntos para siempre.
			if len(nodes) == 0 {
				log.Printf("   ⏳ Evento %d (%s): Start.gg no devolvió standings; se reintenta en la próxima sync.", ev.ID, ev.Name)
				continue
			}

			err = db.Transaction(func(tx *gorm.DB) error {
				// 🔍 Aquí se ejecuta applyStandings, que analiza a los usuarios y valida si están en activeMembers (Discord)
				if err := applyStandings(tx, ev.ID, nodes, activeMembers, top16); err != nil {
					return err
				}
				return tx.Create(&ProcessedEvent{EventID: ev.ID, Slug: t.Slug, Name: ev.Name, Image: tourneyImg}).Error
			})
			if err != nil {
				log.Printf("   ❌ Evento %d (%s): error al guardar: %v", ev.ID, ev.Name, err)
				continue
			}
			log.Printf("   ✅ Evento %d (%s) [COMPLETADO]: %d participantes procesados y validados con Discord.", ev.ID, ev.Name, len(nodes))
		}
	}
	log.Printf("✅ Sincronización en vivo con Discord finalizada.")
}

func applyStandings(tx *gorm.DB, eventID int, nodes []standingNode, activeDiscordMembers map[string]string, top16 bool) error {
	// Sin lista de miembros es imposible validar: devolver error revierte la transacción
	// y el evento NO queda marcado como procesado (se reintenta en la próxima sync).
	if len(activeDiscordMembers) == 0 {
		return errors.New("lista de miembros de Discord vacía: no se puede validar a los jugadores")
	}

	for _, node := range nodes {
		earnedPoints := calculatePoints(node.Placement, top16)

		rawName := node.Entrant.Name
		sponsor := ""
		gamerTag := rawName
		if strings.Contains(rawName, " | ") {
			parts := strings.SplitN(rawName, " | ", 2)
			sponsor = parts[0]
			gamerTag = parts[1]
		}

		if strings.EqualFold(gamerTag, "delete-user") || strings.EqualFold(rawName, "delete-user") {
			continue
		}

		for _, participant := range node.Entrant.Participants {
			originalID := participant.Player.ID // id de Start.gg tal cual viene (antes de aplicar alias)
			playerID := originalID
			if playerID == 0 || blockedPlayerIDs[playerID] {
				continue
			}

			if realID, ok := playerIDAliases[playerID]; ok {
				playerID = realID
			}

			user := participant.Player.User
			// Construir profileURL si existe el slug
			avatarURL := ""
			profileURL := ""
			if user.Slug != "" {
				slug := user.Slug
				if strings.HasPrefix(slug, "user/") {
					profileURL = "https://www.start.gg/" + slug
				} else {
					profileURL = "https://www.start.gg/user/" + slug
				}
			}

			for _, img := range user.Images {
				if img.Type == "profile" {
					avatarURL = img.URL
					break
				}
			}

			// Se carga primero el jugador (si ya existe) para poder usar su Discord ya verificado como respaldo
			var player models.Player
			err := tx.First(&player, "id = ? AND league = ?", playerID, leagueType).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}

			// Validación estricta por ID de Discord (sin scraping ni comparar nombres)
			discordUserID, reason := resolveDiscordID(originalID, playerID, user.Authorizations, activeDiscordMembers, player.DiscordID)
			if discordUserID == "" {
				log.Printf("   ⏭️ Omitiendo %s (puesto %d, start.gg id %d): %s.", gamerTag, node.Placement, originalID, reason)
				continue
			}

			if err := recordTournamentPlacement(tx, playerID, discordUserID, gamerTag, eventID, node.Placement, earnedPoints); err != nil {
				return err
			}

			// Los puestos sin puntos quedan en el historial personal, pero no
			// crean jugadores de cero puntos ni alteran la tabla general.
			if earnedPoints == 0 {
				continue
			}

			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				// CASO A: Jugador nuevo en el ranking, lo indexamos con su Discord ID
				player = models.Player{
					ID:         playerID,
					League:     leagueType,
					GamerTag:   gamerTag,
					Sponsor:    sponsor,
					Character:  "2XKO_FIGHTER",
					AvatarURL:  avatarURL,
					ProfileURL: profileURL,
					DiscordID:  discordUserID, // 🆕 Indexamos su ID de Discord
					Points:     earnedPoints,
				}
				if err := tx.Create(&player).Error; err != nil {
					return err
				}
				log.Printf("✨ Nuevo jugador registrado en el ranking: %s (%d pts) [Discord ID: %s]", gamerTag, earnedPoints, discordUserID)

			case err != nil:
				return err

			default:
				// CASO B: El jugador ya existe. Los puntos se suman con un UPDATE
				// atómico (points + ?) en vez de leer-modificar-escribir: dos syncs
				// simultáneas ya no pueden pisarse los puntos entre sí.
				updates := map[string]any{
					"points":     gorm.Expr("points + ?", earnedPoints),
					"gamer_tag":   gamerTag,
					"discord_id":  discordUserID,
				}
				if sponsor != "" {
					updates["sponsor"] = sponsor
				}
				if avatarURL != "" {
					updates["avatar_url"] = avatarURL
				}
				if profileURL != "" {
					updates["profile_url"] = profileURL
				}
				if err := tx.Model(&models.Player{}).
					Where("id = ? AND league = ?", playerID, leagueType).
					Updates(updates).Error; err != nil {
					return err
				}
				// Relé dentro de la misma transacción para el log con el total.
				var fresh models.Player
				if err := tx.First(&fresh, "id = ? AND league = ?", playerID, leagueType).Error; err == nil {
					log.Printf("📈 Puntos actualizados para %s: Ahora tiene %d pts (+%d) [Discord ID: %s]", fresh.GamerTag, fresh.Points, earnedPoints, discordUserID)
				} else {
					log.Printf("📈 Puntos actualizados para %s (+%d) [Discord ID: %s]", gamerTag, earnedPoints, discordUserID)
				}
			}

			if err := recordResult(tx, playerID, eventID, earnedPoints); err != nil {
				return err
			}
		}
	}
	return nil
}

// isTop16Tournament detecta las series con excepción de puntaje (TNS y
// Paragon): en ellas también suman los puestos 9-16. Se compara por
// nombre y por slug, sin distinguir mayúsculas.
var top16Keywords = []string{"tns", "paragon"}

func isTop16Tournament(names ...string) bool {
	for _, n := range names {
		lower := strings.ToLower(n)
		for _, kw := range top16Keywords {
			if strings.Contains(lower, kw) {
				return true
			}
		}
	}
	return false
}

// calculatePoints es la tabla de puntaje del ranking. Corte general: top 8.
// En torneos TNS/Paragon (top16=true) se extiende al top 16: 9º-12º suman
// 5 pts y 13º-16º suman 3 pts.
func calculatePoints(placement int, top16 bool) int {
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
	case top16 && placement >= 9 && placement <= 12:
		return 5
	case top16 && placement >= 13 && placement <= 16:
		return 3
	default:
		return 0
	}
}
