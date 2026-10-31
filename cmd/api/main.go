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
	"smash-backend/internal/discord"
	"smash-backend/internal/models"
	"smash-backend/internal/startgg"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Variable global de la base de datos (instancia GORM)
var db *gorm.DB

// InitDatabase conecta a PostgreSQL utilizando la variable DATABASE_URL
// y ejecuta la migración automática de los modelos del sistema.
func InitDatabase() {
	dsn := os.Getenv("DATABASE_URL")
	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), // Silencia logs innecesarios de SQL
	})
	if err != nil {
		log.Fatal("❌ Error al conectar a la base de datos:", err)
	}

	// AutoMigrate crea o actualiza las tablas necesarias en PostgreSQL:
	// - Players: Perfiles de jugadores y puntos globales
	// - ProcessedEvents: Historial de eventos de Start.gg procesados
	// - PlayerResults: Resultados puntuales de cada jugador por evento
	// - Matchmaking: Registro de las retas/desafíos FT (estados, marcadores y código de sala)
	// - MatchmakingPlayers: Ranking de MATCHMAKING (separado del ranking de torneos)
	if err := db.AutoMigrate(
		&models.Player{},
		&startgg.ProcessedEvent{},
		&models.PlayerResult{},
		&models.Matchmaking{},
		&models.MatchmakingPlayer{},
		&models.User{},
	); err != nil {
		log.Fatal("❌ Error en la migración automática:", err)
	}
	fmt.Println("✅ Base de datos conectada y migrada correctamente.")
}

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

	// Si se proporcionan los parámetros 'from' y 'to', calcula los puntos dinámicamente sumando solo
	// los resultados obtenidos dentro de ese rango de torneos procesados.
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
		// De lo contrario, consulta la tabla directamente ordenada por puntos acumulados totales.
		err = db.Where("league = ?", league).Order("points DESC").Find(&players).Error
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Asigna la posición de ranking (1, 2, 3...) a cada jugador según el orden de la consulta
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

// runFullSync ejecuta el proceso completo de sincronización de torneos de Start.gg
func runFullSync(startGGToken string, db *gorm.DB, dgSession *discordgo.Session) {
	log.Printf("🔄 Iniciando sincronización completa...\n")

	// 1. Limpia perfiles o etiquetas duplicadas en la base de datos
	startgg.CleanupDuplicatePlayers(db)

	// Obtener los miembros activos del servidor de Discord para la validación estricta
	if dgSession == nil {
		log.Println("🛑 Sin sesión de Discord: se cancela la sync.")
		return
	}
	activeMembers := discord.GetGuildMembersMap(dgSession, os.Getenv("DISCORD_GUILD_ID"))
	if len(activeMembers) == 0 {
		log.Println("🛑 No se pudo cargar la lista de miembros de Discord: se cancela la sync para no marcar torneos como procesados sin jugadores.")
		return
	}

	// 2. Sincronizar utilizando el archivo JSON de Discord
	// (Asegúrate de cambiar "discord_export.json" por el nombre real de tu archivo si es distinto)
	startgg.SyncTournamentsFromDiscordJSON("Hot Coffee _ 2XKO - TORNEOS HOT COFFEE - 📊│hot-coffee-torneos [1430651477612826624].json", startGGToken, db, activeMembers)

	// [COMENTADO] Sincronización en vivo desde Discord (deshabilitada temporalmente)
	// discordTournaments, _ := discord.FetchTournamentsFromDiscord()
	// if len(discordTournaments) > 0 && dgSession != nil {
	// 	guildID := os.Getenv("DISCORD_GUILD_ID")
	// 	activeMembersLive := discord.GetGuildMembersMap(dgSession, guildID)
	// 	startgg.SyncTournamentsFromDiscordLive(discordTournaments, startGGToken, db, activeMembersLive)
	// }

	// 3. Ejecuta torneos configurados de forma manual (id > 900000)
	startgg.SyncManualTournaments(db, activeMembers)
	log.Printf("✅ Sincronización de torneos finalizada.\n")
}

func main() {
	// Carga las variables del archivo .env si se ejecuta localmente
	godotenv.Load()
	InitDatabase()

	startGGToken := os.Getenv("STARTGG_TOKEN")
	botToken := os.Getenv("DISCORD_BOT_TOKEN")
	guildID := os.Getenv("DISCORD_GUILD_ID")

	if botToken == "" {
		log.Fatal("❌ DISCORD_BOT_TOKEN no está configurado en el .env")
	}

	// ==========================================
	// 🚀 CONEXIÓN PERSISTENTE DEL BOT DE DISCORD
	// ==========================================
	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		log.Fatalf("❌ Error al crear la sesión de Discord: %v", err)
	}

	// Configuración de Intents requeridos por Discord (Guilds, Mensajes y Miembros)
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsGuildMembers

	// Registrar manejadores de eventos (escuchadores de clics de botones y slash commands)
	discord.RegisterMatchmakingHandlers(dg, db)

	// Janitor de matchmaking: resuelve los casos en que nadie presiona los botones:
	// auto-confirmación tras 2 minutos, cierre automático de sets abandonados ("Cerrar set"),
	// cancelación de sets inactivos (2 h) y de retas vencidas sin respuesta (30 min).
	discord.StartMatchmakingJanitor(dg, db)

	// Abre el WebSocket en vivo con los servidores de Discord (Pone al bot EN LÍNEA)
	// ==========================================
	// 🚀 CONEXIÓN DEL BOT CON REINTENTOS
	// Si Cloudflare/Discord bloquea la IP de Render (429 "error code: 1015"),
	// el proceso NO muere: la API web sigue activa y el bot reintenta solo.
	// ==========================================
	dg.Debug = true  // Muestra logs de eventos de Discord (útil para depuración)
	defer dg.Close() // Se cierra cuando termine main (aquí ya nunca muere por Discord)

	go func() {
		// 1) Conectar con reintentos y backoff creciente
		for attempt := 1; ; attempt++ {
			if err := dg.Open(); err != nil {
				wait := time.Duration(attempt) * 30 * time.Second
				if wait > 10*time.Minute {
					wait = 10 * time.Minute
				}
				log.Printf("❌ Error al conectar el bot con Discord: %v", err)
				log.Printf("⏳ Reintento #%d en %v (la API web sigue activa)...", attempt, wait)
				time.Sleep(wait)
				continue
			}
			log.Printf("🤖 ¡Bot de Discord conectado y en línea exitosamente!")
			break
		}

		// 2) Todo lo que necesita al bot EN LÍNEA va aquí adentro
		//    (dg.State.User es nil hasta que Open() tiene éxito —
		//     por eso el registro de comandos NO puede ir antes).

		// Limpieza: /cerrarset fue eliminado (los mods cierran con el botón 🏳️ Cerrar set).
		if guildID != "" {
			if existing, err := dg.ApplicationCommands(dg.State.User.ID, guildID); err == nil {
				for _, c := range existing {
					if c.Name == "cerrarset" {
						_ = dg.ApplicationCommandDelete(dg.State.User.ID, guildID, c.ID)
					}
				}
			}
		}

		// Construcción del comando de barra /ft con sus opciones
		cmd := &discordgo.ApplicationCommand{
			Name:        "ft",
			Description: "Inicia un matchmaking FT (3 o 5) contra un rival",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "objetivo",
					Description: "Formato de la reta (3 o 5)",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "FT 3", Value: 3},
						{Name: "FT 5", Value: 5},
					},
				},
				{
					Type:        discordgo.ApplicationCommandOptionUser,
					Name:        "rival",
					Description: "Usuario contra el que jugarás",
					Required:    true,
				},
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "sala",
					Description: "Código de la sala para que tu rival copie y entre (opcional)",
					Required:    false,
				},
			},
		}

		// Registra los comandos directamente en tu servidor (Guild Command)
		cmds := []*discordgo.ApplicationCommand{cmd}
		for _, c := range cmds {
			if guildID != "" {
				dg.ApplicationCommandCreate(dg.State.User.ID, guildID, c)
			} else {
				dg.ApplicationCommandCreate(dg.State.User.ID, "", c)
			}
		}
		if guildID != "" {
			log.Printf("📌 Comandos /ft registrados instantáneamente para el servidor: %s", guildID)
		} else {
			log.Println("⚠️ DISCORD_GUILD_ID no configurado; los comandos se registraron globalmente (pueden tardar en aparecer).")
		}

		// 3) Sincronización de Start.gg (requiere la sesión del bot abierta)
		if startGGToken != "" {
			runFullSync(startGGToken, db, dg)
		} else {
			log.Println("⚠️ Advertencia: STARTGG_TOKEN no está configurado en el .env")
		}
	}()

	// ==========================================
	// 🔐 AUTENTICACIÓN CON DISCORD + PERFILES
	// ==========================================
	auth.RegisterRoutes(db)

	// Sirve los íconos de rangos (Rangos/*.png) para el rank display del frontend
	http.Handle("/Rangos/", http.StripPrefix("/Rangos/", http.FileServer(http.Dir("Rangos"))))

	// Inicia el servidor HTTP web (Render inyecta PORT por env; en local usa 8080).
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	fmt.Printf("🚀 Servidor corriendo en http://localhost%s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))

}
