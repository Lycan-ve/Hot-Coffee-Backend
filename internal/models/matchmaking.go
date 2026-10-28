package models

import "time"

// Matchmaking es una reta /ft entre dos usuarios de Discord.
// Se identifica a los jugadores SOLO por su ID de Discord (ya no depende de la tabla players, que es de torneos).
type Matchmaking struct {
	ID               uint   `gorm:"primaryKey" json:"id"`
	League           string `json:"league"`
	Player1DiscordID string `gorm:"index" json:"player1_discord_id"`
	Player1Tag       string `json:"player1_tag"`
	Player1Avatar    string `json:"player1_avatar"`
	Player2DiscordID string `gorm:"index" json:"player2_discord_id"`
	Player2Tag       string `json:"player2_tag"`
	Player2Avatar    string `json:"player2_avatar"`
	TargetFT         int    `json:"target_ft"`
	// RoomCode es el código de la sala que escribe el retador en /ft sala:...
	// El rival lo copia del panel para entrar a la sala. Vacío = reta sin sala definida.
	RoomCode string `json:"room_code"`
	// CloseRequestedBy guarda quién propuso el cierre anticipado del set (botón "Cerrar set")
	// mientras la reta está en estado CLOSING. Vacío = nadie ha propuesto cerrar.
	CloseRequestedBy string `json:"close_requested_by"`
	Score1           int    `json:"score_1"`
	Score2           int    `json:"score_2"`
	PointsAtStake    int    `json:"points_at_stake"`
	// PENDING, IN_PROGRESS, PENDING_AGREE, CLOSING, FINISHED, REJECTED, CANCELLED
	Status          string    `gorm:"index" json:"status"`
	WinnerDiscordID string    `json:"winner_discord_id"`
	LoserDiscordID  string    `json:"loser_discord_id"`
	ThreadID        string    `json:"thread_id"`
	P1Confirmed     bool      `json:"p1_confirmed"`
	P2Confirmed     bool      `json:"p2_confirmed"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// MatchmakingPlayer es el jugador del ranking de MATCHMAKING (tabla matchmaking_players).
// Es independiente de models.Player (tabla players), que pertenece al ranking de TORNEOS.
type MatchmakingPlayer struct {
	DiscordID string    `gorm:"primaryKey" json:"discordId"`
	Username  string    `json:"username"`
	AvatarURL string    `json:"avatarUrl"`
	Points    int       `gorm:"index" json:"points"`
	Wins      int       `json:"wins"`
	Losses    int       `json:"losses"`
	UpdatedAt time.Time `json:"updatedAt"`
	Ranking   int       `json:"ranking" gorm:"-"`
}
