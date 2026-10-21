package startgg

import (
	"errors"
	"log"
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

// SyncManualTournaments permite indexar torneos que no provienen de la API de Start.gg
func SyncManualTournaments(db *gorm.DB) {
	manualTournaments := []ManualTournament{
		{
			EventID: 900001, // ID ficticio único para Hot Coffee #1
			Slug:    "hot-coffee-manual-1",
			Name:    "Hot Coffee #1 (Manual)",
			Standings: []ManualStanding{
				{GamerTag: "delete-user", Placement: 1}, // O "Baneado"
				{GamerTag: "WS", Placement: 2},
				{GamerTag: "CRIS", Placement: 3},
				{GamerTag: "MIYAMURA", Placement: 4},
				// Agrega el resto del Top 8 si aplica...
			},
		},
		{
			EventID: 900002, // ID ficticio único para Hot Coffee #2
			Slug:    "hot-coffee-manual-2",
			Name:    "Hot Coffee #2 (Manual)",
			Standings: []ManualStanding{
				{GamerTag: "Shine Wizard", Placement: 1},
				{GamerTag: "Daigo Masafumi", Placement: 2},
				{GamerTag: "Vesani", Placement: 3},
				{GamerTag: "Sandor", Placement: 4},
				{GamerTag: "Pdpiu", Placement: 5},
				{GamerTag: "delete-user", Placement: 5}, // O "Baneado"
				{GamerTag: "Cris", Placement: 7},
				{GamerTag: "Gaskman", Placement: 7},
			},
		},
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

				// Omitir usuario "delete-user" o "baneado"
				if strings.EqualFold(item.GamerTag, "delete-user") || strings.EqualFold(item.GamerTag, "baneado") {
					log.Printf("   ⏭️  Omitiendo jugador bloqueado: %s\n", item.GamerTag)
					continue
				}

				// ID sintético a partir del nombre para la DB si no hay ID real de Start.gg
				syntheticID := hashGamerTag(item.GamerTag)

				var player models.Player
				err := tx.First(&player, "id = ? AND league = ?", syntheticID, leagueType).Error
				switch {
				case errors.Is(err, gorm.ErrRecordNotFound):
					player = models.Player{
						ID:        syntheticID,
						League:    leagueType,
						GamerTag:  item.GamerTag,
						Character: "2XKO_FIGHTER",
						Points:    earnedPoints,
					}
					if err := tx.Create(&player).Error; err != nil {
						return err
					}
				case err != nil:
					return err
				default:
					player.Points += earnedPoints
					player.GamerTag = item.GamerTag // Actualizar tag por si varía mayúsculas
					if err := tx.Save(&player).Error; err != nil {
						return err
					}
				}
			}

			// Registrar el evento como procesado para no duplicar puntos
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

// Genera un ID entero único constante basado en el GamerTag para jugadores manuales
func hashGamerTag(s string) int {
	h := 0
	for _, c := range strings.ToLower(s) {
		h = 31*h + int(c)
	}
	if h < 0 {
		h = -h
	}
	return h
}
