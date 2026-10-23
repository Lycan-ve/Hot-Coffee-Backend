package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

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

	db.AutoMigrate(&models.Player{}, &startgg.ProcessedEvent{}, &models.PlayerResult{})
	fmt.Println("✅ Base de datos conectada y migrada correctamente.")
}

func HandleGetRankings(w http.ResponseWriter, r *http.Request, league string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	from, errFrom := strconv.Atoi(r.URL.Query().Get("from"))
	to, errTo := strconv.Atoi(r.URL.Query().Get("to"))

	// ¿Ya hay resultados por torneo guardados?
	var resultsCount int64
	db.Model(&models.PlayerResult{}).Where("league = ?", league).Count(&resultsCount)

	useRange := errFrom == nil && errTo == nil && resultsCount > 0

	players := []models.Player{} // así el JSON sale [] y no null
	var err error

	if useRange {
		err = db.Raw(`
		SELECT p.id, p.league, p.gamer_tag, p.sponsor, p.character, p.avatar_url,
		       SUM(pr.points) AS points
		FROM players p
		JOIN player_results pr
		  ON pr.player_id = p.id AND pr.league = p.league
		JOIN (
		  SELECT event_id, ROW_NUMBER() OVER (ORDER BY event_id) AS tournament_number
		  FROM processed_events
		) pe ON pe.event_id = pr.event_id
		WHERE p.league = ? AND pe.tournament_number BETWEEN ? AND ?
		GROUP BY p.id, p.league
		ORDER BY points DESC`,
			league, from, to).Scan(&players).Error
	} else {
		err = db.Where("league = ?", league).Order("points DESC").Find(&players).Error
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

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

	http.HandleFunc("/api/tournaments/count", func(w http.ResponseWriter, r *http.Request) {
		var n int64
		db.Table("processed_events").Count(&n)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]int64{"count": n})
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
