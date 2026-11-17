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
		&models.Glicko2Migration{},    // migraciones de datos de Glicko-2
	); err != nil {
		log.Fatal("❌ Error en la migración automática:", err)
	}
	// Normaliza filas existentes creadas antes de incorporar Glicko-2. Las
	// columnas nuevas ya tienen defaults, pero este UPDATE cubre filas donde
	// el motor haya materializado cero en vez del default.
	for _, field := range []struct {
		column string
		value  float64
	}{
		{column: "glicko_rating", value: 1500.0},
		{column: "glicko_rd", value: 350.0},
		{column: "glicko_volatility", value: 0.06},
	} {
		if err := db.Model(&models.MatchmakingPlayer{}).
			Where(field.column+" = 0").
			Update(field.column, field.value).Error; err != nil {
			log.Fatal("❌ Error inicializando ", field.column, " de Glicko-2:", err)
		}
	}

	if err := seedGlicko2History(db); err != nil {
		log.Fatal("❌ Error inicializando el ranking Glicko-2:", err)
	}

	// Compatibilidad con los usuarios que ya habían elegido su país a mano
	// antes de añadir la confirmación explícita: esa acción ya expresaba
	// consentimiento y no deben volver a pasar por el aviso.
	if err := db.Model(&models.User{}).
		Where("country_source = ? AND country <> '' AND country_confirmed = ?", "manual", false).
		Update("country_confirmed", true).Error; err != nil {
		log.Fatal("❌ Error migrando las nacionalidades manuales confirmadas:", err)
	}

	log.Println("✅ Base de datos conectada y migrada correctamente.")
	return db
}
