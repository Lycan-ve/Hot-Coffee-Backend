package models

import "time"

// SyncRequest es la bandera que la API web enciende cuando alguien pulsa el
// botón de sincronizar: el BOT la detecta (poll cada 60 s) y ejecuta la sync.
type SyncRequest struct {
	ID          uint       `gorm:"primaryKey" json:"id"`
	Pending     bool       `json:"pending"`
	RequestedAt time.Time  `json:"requested_at"`
	CompletedAt *time.Time `json:"completed_at"`
}
