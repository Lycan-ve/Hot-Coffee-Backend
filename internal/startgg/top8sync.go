package startgg

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"smash-backend/internal/models"

	"gorm.io/gorm"
)

// EventImage es una imagen de Top 8 ya asociada a su evento de Start.gg.
type EventImage struct {
	TournamentSlug string
	EventSlug      string
	ImageURL       string
}

// Key es único: "evento-2xko" se repite en hot-coffee-tournament-16 y hot-coffee-2xko-17,
// así que el slug del evento solo no sirve.
func (e EventImage) Key() string { return e.TournamentSlug + "__" + e.EventSlug }

func (e EventImage) StartGGSlug() string {
	return fmt.Sprintf("tournament/%s/event/%s", e.TournamentSlug, e.EventSlug)
}

var (
	eventURLRe = regexp.MustCompile(`start\.gg/tournament/([A-Za-z0-9_\-]+)(?:/event/([A-Za-z0-9_\-]+))?`)
	imageExtRe = regexp.MustCompile(`(?i)\.(png|jpe?g|webp|gif)(\?|$)`)
)

func firstImage(atts []Attachment) string {
	for _, a := range atts {
		if imageExtRe.MatchString(a.FileName) || imageExtRe.MatchString(a.URL) {
			return a.URL
		}
	}
	return ""
}

// ExtractEventImages asocia imágenes a eventos. En el canal hay dos patrones:
//  1. el mensaje trae el enlace del evento Y la imagen;
//  2. el enlace va en un mensaje y la imagen en el INMEDIATAMENTE siguiente.
//
// Una imagen sin evento claro se omite (mejor omitir que asignar el dúo al evento equivocado).
func ExtractEventImages(messages []DiscordMessage) []EventImage {
	var out []EventImage
	var pending *EventImage // evento visto en un mensaje sin imagen

	for i, msg := range messages {
		content := msg.Content
		atts := append([]Attachment{}, msg.Attachments...)
		if msg.ForwardedMessage != nil {
			content += " " + msg.ForwardedMessage.Content
			atts = append(atts, msg.ForwardedMessage.Attachments...)
		}
		img := firstImage(atts)
		m := eventURLRe.FindStringSubmatch(content)
		hasEvent := m != nil && m[2] != ""

		switch {
		case hasEvent && img != "":
			out = append(out, EventImage{TournamentSlug: m[1], EventSlug: m[2], ImageURL: img})
			pending = nil
		case hasEvent:
			pending = &EventImage{TournamentSlug: m[1], EventSlug: m[2]}
		case img != "" && pending != nil:
			out = append(out, EventImage{TournamentSlug: pending.TournamentSlug, EventSlug: pending.EventSlug, ImageURL: img})
			pending = nil
		case img != "":
			log.Printf("⏭️  Imagen del mensaje #%d sin evento asociado, se omite\n", i)
		default:
			pending = nil
		}
	}
	return out
}

const top8StandingsQuery = `
query Top8($slug: String) {
  event(slug: $slug) {
    standings(query: { page: 1, perPage: 8 }) {
      nodes {
        placement
        entrant {
          name
          participants { player { id } }
        }
      }
    }
  }
}`

// SyncTop8Characters lee los gráficos del Top 8 y guarda el dúo de cada jugador en
// players.character (formato "AKALI, YASUO") usando Gemini. Se procesa en orden cronológico.
//
// Sin apiKey solo descarga las imágenes a la caché (útil porque los enlaces caducan).
func SyncTop8Characters(startggClient *http.Client, startggToken, apiKey string, db *gorm.DB, messages []DiscordMessage) {
	images := ExtractEventImages(messages)
	log.Printf("🖼️  %d imágenes de Top 8 asociadas a un evento\n", len(images))

	cacheDir := os.Getenv("TOP8_CACHE_DIR")
	if cacheDir == "" {
		cacheDir = "top8_images"
	}
	model := os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = "gemini-1.5-flash"
	}
	ctx := context.Background()
	httpClient := &http.Client{Timeout: 2 * time.Minute}

	// Paso 1: bajar TODO a disco antes de nada (los enlaces de Discord caducan).
	for _, im := range images {
		if _, err := CacheImage(ctx, httpClient, im.ImageURL, cacheDir, im.Key()); err != nil {
			log.Printf("   ❌ [%s] imagen: %v\n", im.Key(), err)
		}
	}
	if apiKey == "" {
		log.Println("ℹ️  GEMINI_API_KEY no definida: imágenes guardadas, análisis de dúos omitido")
		return
	}

	// Paso 2: leer dúos y asignarlos.
	for _, im := range images {
		entries, err := LoadOrAnalyzeTop8(ctx, httpClient, im.ImageURL, cacheDir, im.Key(), apiKey, model)
		if err != nil {
			log.Printf("   ❌ [%s] análisis: %v\n", im.Key(), err)
			continue
		}
		if len(entries) == 0 {
			log.Printf("   ⚠️  [%s] la imagen no parece un Top 8 (0 filas)\n", im.Key())
			continue
		}

		var resp eventStandingsResp
		vars := map[string]interface{}{"slug": im.StartGGSlug()}
		if err := gqlDo(startggClient, startggToken, top8StandingsQuery, vars, &resp); err != nil {
			log.Printf("   ❌ [%s] Top 8 en Start.gg: %v\n", im.Key(), err)
			continue
		}
		if resp.Event == nil {
			log.Printf("   ⚠️  [%s] Start.gg no encontró el evento %s\n", im.Key(), im.StartGGSlug())
			continue
		}

		var cands []Candidate
		for _, n := range resp.Event.Standings.Nodes {
			for _, p := range n.Entrant.Participants {
				if p.Player.ID != 0 {
					cands = append(cands, Candidate{PlayerID: p.Player.ID, Tag: n.Entrant.Name, Placement: n.Placement})
				}
			}
		}

		matched, unmatched := MatchEntries(entries, cands)
		saved := 0
		for playerID, e := range matched {
			duo := FormatDuo(e.Characters)
			if duo == "" {
				continue
			}
			res := db.Model(&models.Player{}).
				Where("id = ? AND league = ?", playerID, leagueType).
				Update("character", duo)
			if res.Error != nil {
				log.Printf("   ❌ [%s] guardar dúo de %s: %v\n", im.Key(), e.Tag, res.Error)
				continue
			}
			if e.Confidence == "low" {
				log.Printf("   ⚠️  [%s] %s → %s (confianza baja, revisa %s.json)\n", im.Key(), e.Tag, duo, cacheName(im.Key()))
			}
			saved++
		}
		for _, e := range unmatched {
			log.Printf("   ⚠️  [%s] '%s' (puesto %d) no coincide con nadie del Top 8 de Start.gg\n", im.Key(), e.Tag, e.Placement)
		}
		log.Printf("   ✅ [%s] %d/%d dúos asignados\n", im.Key(), saved, len(entries))
	}
	log.Println("✅ Dúos del Top 8 actualizados.")
}

// compile-time: evita un import sin uso si se quita el logging de sponsors.
var _ = strings.TrimSpace
