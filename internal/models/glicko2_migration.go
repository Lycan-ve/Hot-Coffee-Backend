package models

import "time"

// Glicko2Migration registra las migraciones de datos que deben ejecutarse
// exactamente una vez aunque la API y el bot arranquen en paralelo.
type Glicko2Migration struct {
	Key         string     `gorm:"primaryKey" json:"key"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}
