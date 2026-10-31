package models

import "time"

// MatchmakingQueue es un jugador esperando rival en la cola de la WEB.
// Solo puede estar aquí quien NO tiene una propuesta activa ni un set en
// curso (la API lo garantiza al unirse). LastSeen es el heartbeat del
// navegador: si deja de latir 45 s, el sweeper lo saca de la cola.
type MatchmakingQueue struct {
	DiscordID string    `gorm:"primaryKey" json:"discordId"`
	Username  string    `json:"username"`
	AvatarURL string    `json:"avatarUrl"`
	Points    int       `json:"points"`
	TargetFT  int       `json:"targetFT"` // 3 o 5
	JoinedAt  time.Time `json:"joinedAt"`
	LastSeen  time.Time `json:"lastSeen"`
}

// MatchmakingProposal es un emparejamiento hecho por la cola: espera la
// doble aceptación (como Smash the Peak) y luego el BOT crea el hilo.
type MatchmakingProposal struct {
	ID               uint   `gorm:"primaryKey" json:"id"`
	Player1DiscordID string `gorm:"index" json:"player1DiscordId"`
	Player1Tag       string `json:"player1Tag"`
	Player1Avatar    string `json:"player1Avatar"`
	Player1Points    int    `json:"player1Points"`
	Player2DiscordID string `gorm:"index" json:"player2DiscordId"`
	Player2Tag       string `json:"player2Tag"`
	Player2Avatar    string `json:"player2Avatar"`
	Player2Points    int    `json:"player2Points"`
	TargetFT         int    `json:"targetFT"`
	PointsAtStake    int    `json:"pointsAtStake"`
	Accept1          bool   `json:"accept1"`
	Accept2          bool   `json:"accept2"`
	// WAITING (espera doble aceptación), CONFIRMED (ambos aceptaron; el bot
	// va a crear el hilo), LAUNCHING (el bot la tomó), LAUNCHED (hilo creado),
	// EXPIRED, CANCELLED, FAILED
	Status    string    `gorm:"index" json:"status"`
	MatchID   uint      `json:"matchId"`  // reta creada al confirmarse
	ThreadID  string    `json:"threadId"` // hilo de Discord (al lanzarse)
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// IsPlayer dice si el usuario participa en la propuesta.
func (p *MatchmakingProposal) IsPlayer(discordID string) bool {
	return discordID == p.Player1DiscordID || discordID == p.Player2DiscordID
}

// Side devuelve "p1" o "p2" según el usuario.
func (p *MatchmakingProposal) Side(discordID string) string {
	if discordID == p.Player1DiscordID {
		return "p1"
	}
	return "p2"
}
