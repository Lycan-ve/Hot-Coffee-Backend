package models

import "time"

type Matchmaking struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	League        string    `json:"league"`
	Player1ID     int       `json:"player1_id"`
	Player1Tag    string    `json:"player1_tag"`
	Player2ID     int       `json:"player2_id"`
	Player2Tag    string    `json:"player2_tag"`
	TargetFT      int       `json:"target_ft"`       // 3, 5, 7, 10
	Score1        int       `json:"score_1"`         // Rondas ganadas P1
	Score2        int       `json:"score_2"`         // Rondas ganadas P2
	PointsAtStake int       `json:"points_at_stake"` // Puntos apostados en la reta
	Status        string    `json:"status"`          // "IN_PROGRESS", "FINISHED"
	WinnerID      int       `json:"winner_id"`
	LoserID       int       `json:"loser_id"`
	UpdatedAt     time.Time `json:"updated_at"`
}
