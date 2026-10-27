package models

import "time"

type Matchmaking struct {
	ID            uint   `gorm:"primaryKey" json:"id"`
	League        string `json:"league"`
	Player1ID     int    `json:"player1_id"`
	Player1Tag    string `json:"player1_tag"`
	Player2ID     int    `json:"player2_id"`
	Player2Tag    string `json:"player2_tag"`
	TargetFT      int    `json:"target_ft"`
	Score1        int    `json:"score_1"`
	Score2        int    `json:"score_2"`
	PointsAtStake int    `json:"points_at_stake"`
	Status        string `json:"status"` // PENDING, IN_PROGRESS, PENDING_AGREE, FINISHED, REJECTED
	WinnerID      int    `json:"winner_id"`
	LoserID       int    `json:"loser_id"`
	ThreadID      string `json:"thread_id"`

	// 🆕 Campos para confirmación doble (Agree)
	P1Confirmed bool `json:"p1_confirmed"`
	P2Confirmed bool `json:"p2_confirmed"`

	UpdatedAt time.Time `json:"updated_at"`
}
