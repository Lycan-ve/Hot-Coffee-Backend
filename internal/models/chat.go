package models

import "time"

// MatchmakingMessage es un mensaje del CHAT del set en la web: solo los dos
// combatientes pueden escribir, y la sala es visible mientras el set esté
// activo (o unos minutos después de terminar, para el "gg").
type MatchmakingMessage struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	MatchID   uint      `gorm:"index" json:"matchId"`
	SenderID  string    `json:"senderId"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
}
