package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	"smash-backend/internal/models"
	"smash-backend/internal/startgg"

	"github.com/joho/godotenv"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var db *gorm.DB

func InitDatabase() {
	dsn := os.Getenv("DATABASE_URL")
	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		log.Fatal("❌ Error al conectar a la base de datos:", err)
	}

	db.AutoMigrate(&models.Player{}, &startgg.ProcessedEvent{})
	fmt.Println("✅ Base de datos conectada y migrada correctamente.")
}

func HandleGetRankings(w http.ResponseWriter, r *http.Request, league string) {
	// 1. Capturar los parámetros 'from' y 'to' del Range Picker enviado por el frontend
	fromQuery := r.URL.Query().Get("from")
	toQuery := r.URL.Query().Get("to")

	var players []models.Player

	// 2. Dependiendo de cómo tengas estructurada tu base de datos:
	// Si tienes una tabla de resultados por torneo (ej. tournament_results o similar):
	if fromQuery != "" && toQuery != "" {
		// Ejemplo de consulta agrupando y sumando puntos en un rango de torneos/eventos
		// (Ajusta los nombres de las tablas y campos según tu modelo real en Go)
		db.Table("players").
			Select("players.*, SUM(results.points) as points").
			Joins("JOIN results ON results.player_id = players.id").
			Where("players.league = ? AND results.tournament_index BETWEEN ? AND ?", league, fromQuery, toQuery).
			Group("players.id").
			Order("points DESC").
			Find(&players)
	} else {
		// Si no se envían rangos, muestra el ranking global acumulado normal
		db.Where("league = ?", league).Order("points DESC").Find(&players)
	}

	// Asignar la posición numérica (Ranking 1, 2, 3...)
	for i := range players {
		players[i].Ranking = i + 1
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(players)
}

func runFullSync(jsonFile, startGGToken string, db *gorm.DB) {
	log.Printf("🔄 Iniciando sincronización completa...\n")

	// 1. Sincronizar torneos automáticos de Start.gg
	startgg.SyncTournamentsFromDiscordJSON(jsonFile, startGGToken, db)

	// 2. Indexar torneos manuales (Hot Coffee #1 y #2)
	startgg.SyncManualTournaments(db)

	log.Printf("✅ Sincronización de torneos finalizada.\n")
}

func main() {
	godotenv.Load()
	InitDatabase()

	startGGToken := os.Getenv("STARTGG_TOKEN")
	jsonFile := "Hot Coffee _ 2XKO - TORNEOS HOT COFFEE - 📊│hot-coffee-torneos [1430651477612826624].json"

	if startGGToken != "" {
		go func() {
			runFullSync(jsonFile, startGGToken, db)
		}()
	} else {
		log.Println("⚠️ Advertencia: STARTGG_TOKEN no está configurado en el .env")
	}

	http.HandleFunc("/api/rankings/2xko", func(w http.ResponseWriter, r *http.Request) {
		HandleGetRankings(w, r, "2xko_online")
	})

	http.HandleFunc("/api/sync", func(w http.ResponseWriter, r *http.Request) {
		if startGGToken != "" {
			go runFullSync(jsonFile, startGGToken, db)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write([]byte(`{"status": "success", "message": "Sincronización de torneos iniciada."}`))
	})

	port := ":8080"
	fmt.Printf("🚀 Servidor corriendo en http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe(port, nil))
}
