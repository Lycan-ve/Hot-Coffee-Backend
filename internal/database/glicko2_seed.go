package database

import (
	"log"
	"time"

	"smash-backend/internal/models"
	"smash-backend/internal/ranks"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const glicko2HistoryMigrationKey = "glicko2_history_seed_v1"

// seedGlicko2History initializes the new skill rating from the already
// completed matchmaking sets. The marker row is locked in a transaction so
// the API and Discord bot can start at the same time without replaying twice.
// LP/rank fields are never modified by this migration.
func seedGlicko2History(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		marker := models.Glicko2Migration{Key: glicko2HistoryMigrationKey}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&marker).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("key = ?", glicko2HistoryMigrationKey).First(&marker).Error; err != nil {
			return err
		}
		if marker.CompletedAt != nil {
			return nil
		}

		var matches []models.Matchmaking
		if err := tx.Where(
			"status = ? AND winner_discord_id <> '' AND loser_discord_id <> ''",
			"FINISHED",
		).Order("created_at ASC, id ASC").Find(&matches).Error; err != nil {
			return err
		}

		// Make sure every historical combatant has a matchmaking_players row.
		idsSet := make(map[string]bool)
		for _, match := range matches {
			if match.WinnerDiscordID != "" {
				idsSet[match.WinnerDiscordID] = true
			}
			if match.LoserDiscordID != "" {
				idsSet[match.LoserDiscordID] = true
			}
		}
		for id := range idsSet {
			player := models.MatchmakingPlayer{DiscordID: id}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&player).Error; err != nil {
				return err
			}
		}

		// Start from the standard prior for every current player, then replay
		// all confirmed/finished sets in chronological order. Existing LP and
		// win/loss records are left exactly as they are.
		var players []models.MatchmakingPlayer
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Find(&players).Error; err != nil {
			return err
		}
		ratings := make(map[string]ranks.Glicko2Rating, len(players))
		for _, player := range players {
			ratings[player.DiscordID] = ranks.DefaultGlicko2Rating()
		}

		processed := 0
		for _, match := range matches {
			winner, winnerOK := ratings[match.WinnerDiscordID]
			loser, loserOK := ratings[match.LoserDiscordID]
			if !winnerOK || !loserOK || match.WinnerDiscordID == match.LoserDiscordID {
				continue
			}
			playedAt := match.CreatedAt
			if playedAt.IsZero() {
				playedAt = match.UpdatedAt
			}
			if playedAt.IsZero() {
				log.Printf("⚠️ Glicko-2 seed: se omite el set %d porque no tiene fecha.", match.ID)
				continue
			}
			nextWinner, nextLoser := ranks.UpdateGlicko2Pair(winner, loser, 1, playedAt)
			ratings[match.WinnerDiscordID] = nextWinner
			ratings[match.LoserDiscordID] = nextLoser
			processed++
		}

		for discordID, rating := range ratings {
			if err := tx.Model(&models.MatchmakingPlayer{}).
				Where("discord_id = ?", discordID).
				Updates(map[string]any{
					"glicko_rating":      rating.Rating,
					"glicko_rd":          rating.RD,
					"glicko_volatility":  rating.Volatility,
					"glicko_last_rated_at": rating.LastRatedAt,
				}).Error; err != nil {
				return err
			}
		}

		completedAt := time.Now().UTC()
		marker.CompletedAt = &completedAt
		if err := tx.Save(&marker).Error; err != nil {
			return err
		}
		log.Printf("✅ Glicko-2: ratings inicializados a partir de %d sets históricos.", processed)
		return nil
	})
}
