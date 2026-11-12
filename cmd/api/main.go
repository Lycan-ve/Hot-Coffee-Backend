package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"smash-backend/internal/auth"
	"smash-backend/internal/database"
	"smash-backend/internal/gamechars"
	"smash-backend/internal/models"
	"smash-backend/internal/presence"
	"smash-backend/internal/ranks"
	"smash-backend/internal/webmatch"
	"smash-backend/internal/webqueue"

	"github.com/joho/godotenv"
	"gorm.io/gorm"
)

// Variable global de la base de datos (instancia GORM)
var db *gorm.DB

// Cooldown del botón "Sincronizar": como máximo una solicitud manual cada 10 min.
const syncCooldown = 10 * time.Minute

// HandleGetRankings es el manejador HTTP para la API de Rankings (/api/rankings/2xko)
// Permite filtrar por rango de torneos (parámetros 'from' y 'to') o devolver la tabla general.
// Con ?series=<serie> (p. ej. "tns" o "hot-coffee") suma SOLO los torneos de esa serie.
func HandleGetRankings(w http.ResponseWriter, r *http.Request, league string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*") // Habilita CORS para el Frontend

	from, errFrom := strconv.Atoi(r.URL.Query().Get("from"))
	to, errTo := strconv.Atoi(r.URL.Query().Get("to"))
	series := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("series")))

	// Serie pedida: se ignoran from/to y se suman solo sus torneos.
	var eventIDs []int
	if series != "" {
		eventIDs = seriesEvents(series)
		if len(eventIDs) == 0 {
			// Serie sin torneos procesados: tabla vacía (con [] y no con null).
			json.NewEncoder(w).Encode([]models.Player{})
			return
		}
	}

	var resultsCount int64
	db.Model(&models.PlayerResult{}).Where("league = ?", league).Count(&resultsCount)
	useRange := errFrom == nil && errTo == nil && resultsCount > 0

	players := []models.Player{}
	var err error

	switch {
	case len(eventIDs) > 0:
		err = db.Raw(`
			SELECT p.id, p.league, p.gamer_tag, p.sponsor, p.character, p.avatar_url, p.profile_url, p.discord_id,
			       SUM(pr.points)::bigint AS points
			FROM players p
			JOIN player_results pr
			  ON pr.player_id = p.id AND pr.league = p.league
			WHERE p.league = ? AND pr.event_id IN ?
			GROUP BY p.id, p.league, p.profile_url
			ORDER BY points DESC`,
			league, eventIDs).Scan(&players).Error
	case useRange:
		err = db.Raw(`
			SELECT p.id, p.league, p.gamer_tag, p.sponsor, p.character, p.avatar_url, p.profile_url, p.discord_id,
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
	default:
		err = db.Where("league = ?", league).Order("points DESC").Find(&players).Error
	}

	if err != nil {
		log.Println("❌ Rankings:", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "error_interno"})
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
		log.Println("❌ Rankings (matchmaking):", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "error_interno"})
		return
	}

	// Posición con empates: quienes tienen los mismos puntos comparten posición (1, 1, 3...)
	for i := range players {
		if i > 0 && players[i].Points == players[i-1].Points {
			players[i].Ranking = players[i-1].Ranking
		} else {
			players[i].Ranking = i + 1
		}
		// Clasificación: sets jugados y si ya obtuvo rango (5 sets).
		players[i].Games = players[i].Wins + players[i].Losses
		players[i].Placed = players[i].Games >= ranks.PlacementGames
	}

	json.NewEncoder(w).Encode(players)
}

// HandleGetRoster expone el roster de 2XKO (personajes y fusiones) para que
// la web arme los equipos con los mismos IDs que usa el bot.
func HandleGetRoster(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	chars := make([]map[string]string, 0, len(gamechars.Characters))
	for _, c := range gamechars.Characters {
		chars = append(chars, map[string]string{"id": c.ID, "name": c.Name})
	}
	fuses := make([]map[string]string, 0, len(gamechars.Fuses))
	for _, f := range gamechars.Fuses {
		fuses = append(fuses, map[string]string{"id": f.ID, "name": f.Name})
	}
	json.NewEncoder(w).Encode(map[string]any{"characters": chars, "fuses": fuses})
}

func main() {
	// Carga las variables del archivo .env si se ejecuta localmente
	godotenv.Load()
	db = database.Connect()

	// 👀 Presencias: inicializar el paquete presence TAMBIÉN en la API.
	// Sin esto su db interna queda nil y Get() devolvería siempre "offline"
	// aunque el bot esté guardando los estados en la tabla discord_presences.
	presence.Init(db)

	// ==========================================
	// RUTAS HTTP (API REST)
	// ==========================================

	// GET /api/rankings/2xko -> Retorna la tabla de posiciones de TORNEOS en formato JSON
	http.HandleFunc("/api/rankings/2xko", func(w http.ResponseWriter, r *http.Request) {
		HandleGetRankings(w, r, "2xko_online")
	})

	// GET /api/rankings/matchmaking -> Retorna la tabla de posiciones de MATCHMAKING (/ft)
	http.HandleFunc("/api/rankings/matchmaking", HandleGetMatchmakingRankings)

	// GET /api/game/roster -> Personajes y fusiones de 2XKO (armado de equipos en la web)
	http.HandleFunc("/api/game/roster", HandleGetRoster)

	// GET /api/tournaments/count -> Retorna el total de eventos procesados en la base de datos
	http.HandleFunc("/api/tournaments/count", func(w http.ResponseWriter, r *http.Request) {
		var n int64
		db.Table("processed_events").Count(&n)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]int64{"count": n})
	})

	// GET /api/tournaments/list -> Torneos procesados con numeración del ranking
	// y su SERIE (Hot Coffee, TNS, Tacos Tuesday...): banner y filtro de la web.
	http.HandleFunc("/api/tournaments/list", HandleGetTournaments)

	// GET /api/sync -> Marca la solicitud en la BD: el BOT ejecuta la sync en < 60 s.
	// 🔐 Requiere sesión iniciada (JWT) y respeta un cooldown entre solicitudes.
	http.HandleFunc("/api/sync", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Preflight CORS: el header Authorization exige responder OPTIONS.
		if auth.PreflightCors(w, r) {
			return
		}

		// Solo usuarios autenticados pueden pedir la sincronización.
		if _, ok := auth.UserIDFromRequest(r); !ok {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "no_autenticado",
				"message": "Inicia sesión con Discord para sincronizar",
			})
			return
		}

		// La fila de sync: se crea la primera vez que se usa.
		var req models.SyncRequest
		if err := db.First(&req, 1).Error; err != nil {
			if err := db.Create(&models.SyncRequest{ID: 1}).Error; err != nil {
				log.Println("❌ /api/sync: no se pudo crear la fila de sync:", err)
			}
		}

		now := time.Now()
		// Una solicitud "pending" que el bot no recogió (p. ej. estaba caído
		// cuando se pulsó) se considera abandonada tras el cooldown: se permite
		// pedir otra en vez de quedar atascada en "already_pending" para siempre.
		stalePending := req.Pending && !req.RequestedAt.IsZero() && now.Sub(req.RequestedAt) > syncCooldown
		if req.Pending && !stalePending {
			json.NewEncoder(w).Encode(map[string]string{
				"status":  "already_pending",
				"message": "Ya hay una sincronización en curso: el bot la ejecutará en menos de un minuto",
			})
			return
		}
		if req.CompletedAt != nil && now.Sub(*req.CompletedAt) < syncCooldown {
			wait := int((syncCooldown - now.Sub(*req.CompletedAt)).Minutes()) + 1
			json.NewEncoder(w).Encode(map[string]string{
				"status":  "cooldown",
				"message": fmt.Sprintf("Los torneos ya se sincronizaron hace poco; inténtalo de nuevo en ~%d min", wait),
			})
			return
		}

		if err := db.Model(&models.SyncRequest{}).Where("id = 1").Updates(map[string]any{
			"pending":      true,
			"requested_at": now,
			"completed_at": nil,
		}).Error; err != nil {
			log.Println("❌ /api/sync:", err)
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "error_interno"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "success",
			"message": "Sincronización programada: el bot la ejecutará en menos de un minuto",
		})
	})

	// 🔐 Login con Discord + perfiles (módulo de login)
	auth.RegisterRoutes(db)

	// 🌐 Cola de matchmaking web: el sistema encuentra al rival (como Smash the Peak)
	webqueue.RegisterRoutes(db)

	// 🕹️ Sala del set 100% web: equipo, chat y reporte dentro de la página
	//    (los sets de la cola ya NO dependen del bot de Discord)
	webmatch.RegisterRoutes(db)

	// Sirve los íconos de rangos (Rangos/*.png) para el rank display del frontend
	http.Handle("/Rangos/", http.StripPrefix("/Rangos/", http.FileServer(http.Dir("Rangos"))))
	// Sirve los íconos de fuses para el historial de equipos del perfil.
	http.Handle("/Fusion/", http.StripPrefix("/Fusion/", http.FileServer(http.Dir("Fusion"))))
	// Sirve los íconos de PERSONAJES para armar el equipo en la web (2XKO).
	http.Handle("/Personajes/", http.StripPrefix("/Personajes/", http.FileServer(http.Dir("Personajes"))))

	// Inicia el servidor HTTP web (Render inyecta PORT por env; en local usa 8080).
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Printf("🚀 API web corriendo en http://localhost:%s\n", port)
	srv := &http.Server{
		Addr:              ":" + port,
		ReadHeaderTimeout:  5 * time.Second,
		ReadTimeout:        15 * time.Second,
		WriteTimeout:       30 * time.Second,
		IdleTimeout:        60 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
