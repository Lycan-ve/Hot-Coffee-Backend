package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"smash-backend/internal/auth"
	"smash-backend/internal/database"
	"smash-backend/internal/models"
	"smash-backend/internal/webqueue"

	"github.com/joho/godotenv"
	"gorm.io/gorm"
)

// Variable global de la base de datos (instancia GORM)
var db *gorm.DB

// HandleGetRankings es el manejador HTTP para la API de Rankings (/api/rankings/2xko)
// Permite filtrar por rango de torneos (parámetros 'from' y 'to') o devolver la tabla general.
func HandleGetRankings(w http.ResponseWriter, r *http.Request, league string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*") // Habilita CORS para el Frontend

	from, errFrom := strconv.Atoi(r.URL.Query().Get("from"))
	to, errTo := strconv.Atoi(r.URL.Query().Get("to"))

	var resultsCount int64
	db.Model(&models.PlayerResult{}).Where("league = ?", league).Count(&resultsCount)
	useRange := errFrom == nil && errTo == nil && resultsCount > 0

	players := []models.Player{}
	var err error

	if useRange {
		err = db.Raw(`
			SELECT p.id, p.league, p.gamer_tag, p.sponsor, p.character, p.avatar_url, p.profile_url,
			       SUM(pr.points)::bigint AS points
			FROM players p
			JOIN player_results pr
			  ON pr.player_id = p.id AND pr.league = p.league
			JOIN (
			  SELECT event_id, ROW_NUMBER() OVER (ORDER BY event_id) AS tournament_number
			  FROM processed_events
			) pe ON pe.event_id = pr.event_id
			WHERE p.league = ? AND pe.tournament_number BETWEEN ? AND ?
			GROUP BY p.id, p.league, p.profile_url
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

	json.NewEncoder(w).Encode(players)
}

// HandleGetMatchmakingRankings es el manejador HTTP del ranking de MATCHMAKING (/api/rankings/matchmaking).
// Es independiente del ranking de torneos: lee la tabla matchmaking_players, no players.
func HandleGetMatchmakingRankings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	players := []models.MatchmakingPlayer{}
	if err := db.Order("points DESC, wins DESC, username ASC").Find(&players).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Posición con empates: quienes tienen los mismos puntos comparten posición (1, 1, 3...)
	for i := range players {
		if i > 0 && players[i].Points == players[i-1].Points {
			players[i].Ranking = players[i-1].Ranking
		} else {
			players[i].Ranking = i + 1
		}
	}

	json.NewEncoder(w).Encode(players)
}

func main() {
	// Carga las variables del archivo .env si se ejecuta localmente
	godotenv.Load()
	db = database.Connect()

	// ==========================================
	// RUTAS HTTP (API REST)
	// ==========================================

	// GET /api/rankings/2xko -> Retorna la tabla de posiciones de TORNEOS en formato JSON
	http.HandleFunc("/api/rankings/2xko", func(w http.ResponseWriter, r *http.Request) {
		HandleGetRankings(w, r, "2xko_online")
	})

	// GET /api/rankings/matchmaking -> Retorna la tabla de posiciones de MATCHMAKING (/ft)
	http.HandleFunc("/api/rankings/matchmaking", HandleGetMatchmakingRankings)

	// GET /api/tournaments/count -> Retorna el total de eventos procesados en la base de datos
	http.HandleFunc("/api/tournaments/count", func(w http.ResponseWriter, r *http.Request) {
		var n int64
		db.Table("processed_events").Count(&n)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]int64{"count": n})
	})

	// GET /api/sync -> Marca la solicitud en la BD: el BOT ejecuta la sync en < 60 s.
	// (La API ya no puede hacerla sola: la validación de miembros necesita al bot en línea)
	http.HandleFunc("/api/sync", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if err := db.Model(&models.SyncRequest{}).Where("id = 1").Updates(map[string]any{
			"pending":      true,
			"requested_at": time.Now(),
			"completed_at": nil,
		}).Error; err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"status": "success", "message": "Sincronización programada: el bot la ejecutará en menos de un minuto"}`))
	})

	// 🔐 Login con Discord + perfiles (módulo de login)
	auth.RegisterRoutes(db)

	// 🌐 Cola de matchmaking web: el sistema encuentra al rival (como Smash the Peak)
	webqueue.RegisterRoutes(db)

	// Sirve los íconos de rangos (Rangos/*.png) para el rank display del frontend
	http.Handle("/Rangos/", http.StripPrefix("/Rangos/", http.FileServer(http.Dir("Rangos"))))

	// Inicia el servidor HTTP web (Render inyecta PORT por env; en local usa 8080).
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("🚀 API web corriendo en http://localhost:%s\n", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
