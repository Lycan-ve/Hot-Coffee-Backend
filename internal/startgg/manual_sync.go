package startgg

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"

	"smash-backend/internal/models"

	"gorm.io/gorm"
)

type ManualStanding struct {
	GamerTag  string
	Placement int
}

type ManualTournament struct {
	EventID   int
	Slug      string
	Name      string
	Standings []ManualStanding

	// StartggSlug: slug de un torneo externo de Start.gg (no publicado en los
	// canales de Discord de la liga). Con este campo el bot procesa el torneo
	// por el mismo camino en vivo que los publicados en Discord: baja sus
	// eventos de 2XKO terminados, sus standings completos, imágenes y
	// nacionalidades, y valida a los jugadores contra el servidor.
	// Con StartggSlug configurado se ignoran EventID/Standings.
	StartggSlug string
}

func SyncManualTournaments(db *gorm.DB, activeMembers map[string]string) {
	manualTournaments := loadManualTournaments()
	if len(manualTournaments) == 0 {
		return
	}

	for _, t := range manualTournaments {
		if t.StartggSlug != "" {
			// Los torneos externos por slug los procesa la sync en vivo
			// (los recoge ManualSlugTournaments); aquí no se tocan.
			continue
		}

		var already int64
		db.Model(&ProcessedEvent{}).Where("event_id = ?", t.EventID).Count(&already)
		if already > 0 {
			log.Printf("   ⏭️  Torneo Manual ID %d (%s) ya procesado", t.EventID, t.Name)
			continue
		}

		err := db.Transaction(func(tx *gorm.DB) error {
			for _, item := range t.Standings {
				if strings.EqualFold(item.GamerTag, "delete-user") || strings.EqualFold(item.GamerTag, "baneado") {
					log.Printf("   ⏭️  Omitiendo jugador bloqueado: %s", item.GamerTag)
					continue
				}

				top16 := isTop16Tournament(t.Name, t.Slug)

				earnedPoints := calculatePoints(item.Placement, top16)

				// 🆕 ¿este tag es en realidad una cuenta que ya existe?
				playerID := resolveManualPlayerID(tx, item.GamerTag)
				isAlias := playerID != hashGamerTag(item.GamerTag)

				var player models.Player
				err := tx.First(&player, "id = ? AND league = ?", playerID, leagueType).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}

				// Validación estricta: el jugador debe tener un Discord verificado dentro del servidor
				discordID := resolveManualDiscordID(item.GamerTag, player.DiscordID, activeMembers)
				if discordID == "" {
					log.Printf("   ⏭️  Omitiendo '%s' (top %d, torneo manual): no se pudo verificar su Discord en el servidor.", item.GamerTag, item.Placement)
					continue
				}

				// El puesto queda registrado en el historial aunque no sume
				// puntos (igual que la sync en vivo de Start.gg, que anota
				// TODOS los puestos verificados, no solo el top 8).
				if err := recordTournamentPlacement(tx, playerID, discordID, item.GamerTag, t.EventID, item.Placement, earnedPoints); err != nil {
					return err
				}

				if earnedPoints == 0 {
					continue
				}

				switch {
				case errors.Is(err, gorm.ErrRecordNotFound):
					player = models.Player{
						ID:        playerID,
						League:    leagueType,
						GamerTag:  item.GamerTag,
						Character: "2XKO_FIGHTER",
						DiscordID: discordID,
						Points:    earnedPoints,
					}
					if err := tx.Create(&player).Error; err != nil {
						return err
					}
				case err != nil:
					return err
				default:
					// Suma atómica (points + ?): nunca pisa los puntos de otra
					// sync en curso. Con alias no se renombra la cuenta real
					// con el tag escrito a mano.
					updates := map[string]any{
						"points":     gorm.Expr("points + ?", earnedPoints),
						"discord_id": discordID,
					}
					if !isAlias {
						updates["gamer_tag"] = item.GamerTag
					}
					if err := tx.Model(&models.Player{}).
						Where("id = ? AND league = ?", playerID, leagueType).
						Updates(updates).Error; err != nil {
						return err
					}
				}

				if err := recordResult(tx, playerID, t.EventID, earnedPoints); err != nil {
					return err
				}
			}

			return tx.Create(&ProcessedEvent{
				EventID: t.EventID,
				Slug:    t.Slug,
				Name:    t.Name,
			}).Error
		})

		if err != nil {
			log.Printf("❌ Error al guardar torneo manual %s: %v", t.Name, err)
		} else {
			log.Printf("✅ Torneo Manual %s indexado correctamente.", t.Name)
		}
	}
}

// ManualSlugTournaments devuelve los slugs de Start.gg configurados en
// torneos_manuales.json (campo "StartggSlug"): torneos externos que no se
// publican en los canales de Discord de la liga. El bot los añade a la cola
// de la sync en vivo (SyncTournamentsFromDiscordLive), que baja sus eventos,
// standings, imágenes y nacionalidades exactamente igual que con los torneos
// publicados en Discord.
func ManualSlugTournaments() []TournamentInfo {
	var out []TournamentInfo
	seen := make(map[string]bool)
	for _, t := range loadManualTournaments() {
		slug := strings.TrimSpace(t.StartggSlug)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, TournamentInfo{Slug: slug})
	}
	return out
}

// loadManualTournaments lee los torneos manuales desde torneos_manuales.json
// (junto al binario). Sin archivo o con JSON inválido no se procesa nada y se
// avisa por log: editar un torneo ya no exige recompilar el bot.
func loadManualTournaments() []ManualTournament {
	data, err := os.ReadFile("torneos_manuales.json")
	if err != nil {
		log.Printf("⚠️ No se pudo leer torneos_manuales.json: %v", err)
		return nil
	}
	var ts []ManualTournament
	if err := json.Unmarshal(data, &ts); err != nil {
		log.Printf("❌ torneos_manuales.json inválido: %v", err)
		return nil
	}
	return ts
}

// hashGamerTag genera el ID sintético de un tag nuevo con FNV-1a de 64 bits.
// El hash anterior (31*h con int) podía colisionar y fusionar jugadores distintos.
// Se acota a 31 bits positivos para mantener IDs compatibles con la tabla actual.
func hashGamerTag(s string) int {
	h := uint64(2166136261)
	for _, c := range strings.ToLower(s) {
		h ^= uint64(c)
		h *= 16777619
	}
	return int(h & 0x7FFFFFFF)
}
