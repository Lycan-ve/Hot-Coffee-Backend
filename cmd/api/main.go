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

	db.AutoMigrate(&models.Player{})
	fmt.Println("✅ Base de datos conectada y migrada correctamente.")
}

func HandleGetRankings(w http.ResponseWriter, r *http.Request, league string) {
	var players []models.Player
	db.Where("league = ?", league).Order("points DESC").Find(&players)

	for i := range players {
		players[i].Ranking = i + 1
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(players)
}

func main() {
	godotenv.Load()
	InitDatabase()

	startGGToken := os.Getenv("STARTGG_TOKEN")

	http.HandleFunc("/api/rankings/2xko", func(w http.ResponseWriter, r *http.Request) {
		HandleGetRankings(w, r, "2xko_online")
	})

	http.HandleFunc("/api/sync", func(w http.ResponseWriter, r *http.Request) {
		// Ruta al archivo JSON exportado de Discord (puedes colocarlo en la raíz del proyecto)
		jsonFile := "Hot Coffee _ 2XKO - TORNEOS HOT COFFEE [1430651477612826624].json"
		if startGGToken != "" {
			go startgg.SyncTournamentsFromDiscordJSON(jsonFile, startGGToken, db)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write([]byte(`{"status": "success", "message": "Sincronización iniciada leyendo el canal de Discord."}`))
	})

	port := ":8080"
	fmt.Printf("🚀 Servidor corriendo en http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe(port, nil))
}
