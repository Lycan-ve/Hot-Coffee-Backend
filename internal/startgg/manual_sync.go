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

func SyncManualTournaments(db *gorm.DB) {
	manualTournaments := []ManualTournament{
		{
			EventID: 900001,
			Slug:    "hot-coffee-manual-1",
			Name:    "Hot Coffee #1 (Manual)",
			Standings: []ManualStanding{
				{GamerTag: "delete-user", Placement: 1},
				{GamerTag: "WS", Placement: 2},
				{GamerTag: "CRIS", Placement: 3},
				{GamerTag: "MIYAMURA", Placement: 4},
			},
		},
		{
			EventID: 900002,
			Slug:    "hot-coffee-manual-2",
			Name:    "Hot Coffee #2 (Manual)",
			Standings: []ManualStanding{
				{GamerTag: "Shine Wizard", Placement: 1},
				{GamerTag: "Daigo Masafumi", Placement: 2},
				{GamerTag: "Vesani", Placement: 3},
				{GamerTag: "Sandor", Placement: 4},
				{GamerTag: "Pdpiu", Placement: 5},
				{GamerTag: "delete-user", Placement: 5},
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

				if strings.EqualFold(item.GamerTag, "delete-user") || strings.EqualFold(item.GamerTag, "baneado") {
					log.Printf("   ⏭️  Omitiendo jugador bloqueado: %s\n", item.GamerTag)
					continue
				}

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
					player.GamerTag = item.GamerTag
					if err := tx.Save(&player).Error; err != nil {
						return err
					}
				}

				// 🆕 guardar los puntos de ESTE torneo
				if err := recordResult(tx, syntheticID, t.EventID, earnedPoints); err != nil {
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
