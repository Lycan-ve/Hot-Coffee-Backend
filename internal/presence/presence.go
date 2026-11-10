// Package presence guarda el estado de Discord de los miembros del servidor
// (en línea, inactivo, no molestar, jugando 2XKO...). El bot lo escribe desde
// el Gateway en tiempo real y la API (otro proceso) lo lee para la web.
package presence

import (
	"log"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Presence: una fila por miembro del servidor.
type Presence struct {
	DiscordID string    `gorm:"primaryKey;column:discord_id" json:"discordId"`
	Status    string    `gorm:"column:status" json:"status"`     // online | idle | dnd | offline
	Activity  string    `gorm:"column:activity" json:"activity"` // juego en curso, "" si ninguno
	UpdatedAt time.Time `gorm:"column:updated_at" json:"updatedAt"`
}

func (Presence) TableName() string { return "discord_presences" }

var (
	mu sync.Mutex
	db *gorm.DB
)

// Init conecta el paquete a la base de datos y crea la tabla si falta.
func Init(database *gorm.DB) {
	db = database
	if err := db.AutoMigrate(&Presence{}); err != nil {
		log.Println("❌ Presencias: no se pudo preparar la tabla discord_presences:", err)
	}
}

// Set guarda (upsert) el estado de un usuario. Lo llama el bot en cada
// PresenceUpdate y en el backfill de arranque. "invisible" se normaliza a
// "offline": Discord nunca revela la diferencia.
func Set(discordID, status, activity string) {
	if db == nil || discordID == "" || status == "" {
		return
	}
	if status == "invisible" || status == "offline" {
		status = "offline"
		activity = ""
	}
	mu.Lock()
	defer mu.Unlock()
	p := Presence{DiscordID: discordID, Status: status, Activity: activity, UpdatedAt: time.Now()}
	if err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "discord_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"status", "activity", "updated_at"}),
	}).Create(&p).Error; err != nil {
		log.Println("❌ Presencias: no se pudo guardar el estado:", err)
	}
}

// Get devuelve el estado de un usuario. Sin fila, o con datos viejos
// (más de 5 minutos: el bot pudo perder eventos al reconectarse), es offline.
func Get(discordID string) Presence {
	off := Presence{DiscordID: discordID, Status: "offline"}
	if db == nil || discordID == "" {
		return off
	}
	var row Presence
	if err := db.Where("discord_id = ?", discordID).First(&row).Error; err != nil {
		return off
	}
	if time.Since(row.UpdatedAt) > 5*time.Minute {
		row.Status = "offline"
		row.Activity = ""
	}
	return row
}
