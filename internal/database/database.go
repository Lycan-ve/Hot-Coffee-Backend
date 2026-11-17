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
		&models.TournamentPlacement{},
		&models.TournamentRecord{},     // torneos con su imagen de Top 8 (para la web)
		&models.Matchmaking{},
		&models.MatchmakingPlayer{},
		&models.User{},                // login con Discord
		&models.SyncRequest{},         // solicitudes de sync manual desde la web
		&models.MatchmakingQueue{},    // 🆕 cola web de matchmaking
		&models.MatchmakingProposal{}, // 🆕 emparejamientos de la cola web
		&models.MatchmakingMessage{},  // 🆕 chat del set web
	); err != nil {
		log.Fatal("❌ Error en la migración automática:", err)
	}
	// Normaliza filas existentes creadas antes de incorporar Glicko-2. Las
	// columnas nuevas ya tienen defaults, pero este UPDATE cubre filas donde
	// el motor haya materializado cero en vez del default.
	if err := db.Model(&models.MatchmakingPlayer{}).
		Where("glicko_rating = 0 OR glicko_rd = 0 OR glicko_volatility = 0").
		Updates(map[string]any{
			"glicko_rating":     1500.0,
			"glicko_rd":         350.0,
			"glicko_volatility": 0.06,
		}).Error; err != nil {
		log.Fatal("❌ Error inicializando ratings Glicko-2 existentes:", err)
	}

	log.Println("✅ Base de datos conectada y migrada correctamente.")
	return db
}
