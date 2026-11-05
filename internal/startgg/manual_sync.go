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
}

func SyncManualTournaments(db *gorm.DB, activeMembers map[string]string) {
	manualTournaments := loadManualTournaments()
	if len(manualTournaments) == 0 {
		return
	}

	for _, t := range manualTournaments {
		var already int64
		db.Model(&ProcessedEvent{}).Where("event_id = ?", t.EventID).Count(&already)
		if already > 0 {
			log.Printf("   ⏭️  Torneo Manual ID %d (%s) ya procesado\n", t.EventID, t.Name)
			continue
		}

		err := db.Transaction(func(tx *gorm.DB) error {
			for _, item := range t.Standings {
				earnedPoints := calculatePoints(item.Placement)
				if earnedPoints == 0 {
					continue
				}

				if strings.EqualFold(item.GamerTag, "delete-user") || strings.EqualFold(item.GamerTag, "baneado") {
					log.Printf("   ⏭️  Omitiendo jugador bloqueado: %s\n", item.GamerTag)
					continue
				}

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
					log.Printf("   ⏭️  Omitiendo '%s' (top %d, torneo manual): no se pudo verificar su Discord en el servidor.\n", item.GamerTag, item.Placement)
					continue
				}

				if err := recordTournamentPlacement(tx, playerID, discordID, item.GamerTag, t.EventID, item.Placement, earnedPoints); err != nil {
					return err
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
					player.Points += earnedPoints
					player.DiscordID = discordID
					if !isAlias { // 🆕 no renombrar la cuenta real con el tag manual
						player.GamerTag = item.GamerTag
					}
					if err := tx.Save(&player).Error; err != nil {
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
			log.Printf("❌ Error al guardar torneo manual %s: %v\n", t.Name, err)
		} else {
			log.Printf("✅ Torneo Manual %s indexado correctamente.\n", t.Name)
		}
	}
}

// loadManualTournaments lee los torneos manuales desde torneos_manuales.json
// (junto al binario). Sin archivo o con JSON inválido no se procesa nada y se
// avisa por log: editar un torneo ya no exige recompilar el bot.
func loadManualTournaments() []ManualTournament {
	data, err := os.ReadFile("torneos_manuales.json")
	if err != nil {
		log.Printf("⚠️ No se pudo leer torneos_manuales.json: %v\n", err)
		return nil
	}
	var ts []ManualTournament
	if err := json.Unmarshal(data, &ts); err != nil {
		log.Printf("❌ torneos_manuales.json inválido: %v\n", err)
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
