package models

import "time"

// Matchmaking es una reta /ft entre dos usuarios de Discord.
// Se identifica a los jugadores SOLO por su ID de Discord.
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
	RoomCode         string `json:"room_code"`
	Score1           int    `json:"score_1"`
	Score2           int    `json:"score_2"`
	PointsAtStake    int    `json:"points_at_stake"`
	// PENDING, IN_PROGRESS, PENDING_VERIFY, PENDING_CLOSE, FINISHED, REJECTED, CANCELLED
	Status          string `gorm:"index" json:"status"`
	WinnerDiscordID string `json:"winner_discord_id"`
	LoserDiscordID  string `json:"loser_discord_id"`
	ThreadID        string `json:"thread_id"`
	// Mensaje del panel de games dentro del hilo (el janitor lo edita cuando actúa solo).
	PanelMessageID string `json:"panel_message_id"`
	// Paneles de equipo dentro del hilo (uno por jugador).
	TeamPanelMessageID1 string `json:"team_panel_message_id_1"`
	TeamPanelMessageID2 string `json:"team_panel_message_id_2"`
	// EQUIPOS 2XKO (IDs del roster de internal/gamechars).
	P1Char1 string `json:"p1_char1"` // Personaje 1 de Player1 (point)
	P1Char2 string `json:"p1_char2"` // Personaje 2 de Player1 (assist)
	P1Fuse  string `json:"p1_fuse"`
	P2Char1 string `json:"p2_char1"`
	P2Char2 string `json:"p2_char2"`
	P2Fuse  string `json:"p2_fuse"`
	// ORDEN de las victorias, JSON de lados: "[1,2,1]" = P1 ganó el game 1, P2 el 2, P1 el 3.
	RoundHistory string `json:"round_history"`
	// Game reportado esperando verificación (1 = ganó Player1, 2 = Player2, 0 = ninguno).
	PendingGameWinner int `json:"pending_game_winner"`
	// Quién reportó el game pendiente: SOLO el otro jugador puede confirmarlo.
	PendingReportedBy string `json:"pending_reported_by"`
	// Relojes del janitor (en la BD: sobreviven reinicios del bot).
	CloseRequestedAt *time.Time `json:"close_requested_at"` // propuesta de cierre: se cierra a los 2 min
	CloseRequestedBy string     `json:"close_requested_by"` // quién pidió cerrar el set
	LastRoundAt      *time.Time `json:"last_round_at"`      // 2 h sin reportar = set cancelado
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// P1TeamComplete / P2TeamComplete: ¿el jugador ya eligió Personaje 1, Personaje 2 y Fusión?
func (m *Matchmaking) P1TeamComplete() bool {
	return m.P1Char1 != "" && m.P1Char2 != "" && m.P1Fuse != ""
}

func (m *Matchmaking) P2TeamComplete() bool {
	return m.P2Char1 != "" && m.P2Char2 != "" && m.P2Fuse != ""
}

// MatchmakingPlayer es el jugador del ranking de MATCHMAKING (tabla matchmaking_players).
// Es independiente de models.Player (tabla players), que es del ranking de TORNEOS.
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
