package database

import (
	"log"
	"os"
	"time"

	"smash-backend/internal/models"
	"smash-backend/internal/startgg"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Connect conecta a PostgreSQL (Supabase) usando DATABASE_URL, acota el pool
// de conexiones (importante: ahora son DOS procesos sobre el mismo Supabase
// gratuito) y ejecuta la migración automática de todos los modelos.
func Connect() *gorm.DB {
	dsn := os.Getenv("DATABASE_URL")
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		log.Fatal("❌ Error al conectar a la base de datos:", err)
	}

	// Pool acotado para no agotar las conexiones de Supabase Free
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(10)
		sqlDB.SetMaxIdleConns(5)
		sqlDB.SetConnMaxLifetime(5 * time.Minute)
	}

	if err := db.AutoMigrate(
		&models.Player{},
		&startgg.ProcessedEvent{},
		&models.PlayerResult{},
		&models.Matchmaking{},
		&models.MatchmakingPlayer{},
		&models.User{},        // login con Discord
		&models.SyncRequest{}, // solicitudes de sync manual desde la web
	); err != nil {
		log.Fatal("❌ Error en la migración automática:", err)
	}
	log.Println("✅ Base de datos conectada y migrada correctamente.")
	return db
}
