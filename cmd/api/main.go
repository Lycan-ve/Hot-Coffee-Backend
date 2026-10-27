package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

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
	// - Matchmaking: Registro de las retas/desafíos FT (puntos, estados, marcadores)
	db.AutoMigrate(
		&models.Player{},
		&startgg.ProcessedEvent{},
		&models.PlayerResult{},
		&models.Matchmaking{},
	)
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

// runFullSync ejecuta el proceso completo de sincronización de torneos de Start.gg
func runFullSync(startGGToken string, db *gorm.DB, dgSession *discordgo.Session) {
	log.Printf("🔄 Iniciando sincronización completa...\n")

	// 1. Limpia perfiles o etiquetas duplicadas en la base de datos
	startgg.CleanupDuplicatePlayers(db)

	// Obtener los miembros activos del servidor de Discord para la validación estricta
	var activeMembers map[string]string
	if dgSession != nil {
		guildID := os.Getenv("DISCORD_GUILD_ID")
		activeMembers = discord.GetGuildMembersMap(dgSession, guildID)
	} else {
		activeMembers = make(map[string]string)
	}

	// 2. Sincronizar utilizando el archivo JSON de Discord
	// (Asegúrate de cambiar "discord_export.json" por el nombre real de tu archivo si es distinto)
	startgg.SyncTournamentsFromDiscordJSON("discord_export.json", startGGToken, db, activeMembers)

	// // [COMENTADO] Sincronización en vivo desde Discord (deshabilitada temporalmente)
	// discordTournaments, _ := discord.FetchTournamentsFromDiscord()
	// if len(discordTournaments) > 0 && dgSession != nil {
	// 	guildID := os.Getenv("DISCORD_GUILD_ID")
	// 	activeMembersLive := discord.GetGuildMembersMap(dgSession, guildID)
	// 	startgg.SyncTournamentsFromDiscordLive(discordTournaments, startGGToken, db, activeMembersLive)
	// }

	// 3. Ejecuta torneos configurados de forma manual (id > 900000)
	startgg.SyncManualTournaments(db)
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

	// Abre el WebSocket en vivo con los servidores de Discord (Pone al bot EN LÍNEA)
	err = dg.Open()
	if err != nil {
		log.Fatalf("❌ Error al conectar el bot con Discord: %v", err)
	}
	defer dg.Close() // Garantiza que la conexión se cierre al apagar la aplicación

	log.Printf("🤖 ¡Bot de Discord conectado y en línea exitosamente!")

	// Construcción del comando de barra /ft con sus opciones (objetivo y rival)
	cmd := &discordgo.ApplicationCommand{
		Name:        "ft",
		Description: "Inicia un matchmaking FT (3, 5, 7, 10) contra un rival",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "objetivo",
				Description: "Formato de la reta (3, 5, 7, 10)",
				Required:    true,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "FT 3", Value: 3},
					{Name: "FT 5", Value: 5},
					{Name: "FT 7", Value: 7},
					{Name: "FT 10", Value: 10},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionUser,
				Name:        "rival",
				Description: "Usuario contra el que jugarás",
				Required:    true,
			},
		},
	}

	// Registra el comando directamente en tu servidor (Guild Command) para disponibilidad instantánea.
	if guildID != "" {
		dg.ApplicationCommandCreate(dg.State.User.ID, guildID, cmd)
		log.Printf("📌 Comando /ft registrado instantáneamente para el servidor: %s", guildID)
	} else {
		dg.ApplicationCommandCreate(dg.State.User.ID, "", cmd)
		log.Println("⚠️ DISCORD_GUILD_ID no configurado; el comando /ft se registró globalmente (puede tardar en aparecer).")
	}

	// Ejecuta la sincronización de Start.gg al iniciar en una rutina asíncrona (goroutine)
	if startGGToken != "" {
		go runFullSync(startGGToken, db, dg)
	} else {
		log.Println("⚠️ Advertencia: STARTGG_TOKEN no está configurado en el .env")
	}

	// ==========================================
	// RUTAS HTTP (API REST)
	// ==========================================

	// GET /api/rankings/2xko -> Retorna la tabla de posiciones en formato JSON
	http.HandleFunc("/api/rankings/2xko", func(w http.ResponseWriter, r *http.Request) {
		HandleGetRankings(w, r, "2xko_online")
	})

	// GET /api/tournaments/count -> Retorna el total de eventos procesados en la base de datos
	http.HandleFunc("/api/tournaments/count", func(w http.ResponseWriter, r *http.Request) {
		var n int64
		db.Table("processed_events").Count(&n)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]int64{"count": n})
	})

	// GET /api/sync -> Permite disparar la sincronización manualmente llamando al endpoint HTTP
	http.HandleFunc("/api/sync", func(w http.ResponseWriter, r *http.Request) {
		if startGGToken != "" {
			go runFullSync(startGGToken, db, dg)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write([]byte(`{"status": "success", "message": "Sincronización de torneos desde Discord iniciada."}`))
	})

	// Inicia el servidor HTTP web en el puerto 8080
	port := ":8080"
	fmt.Printf("🚀 Servidor corriendo en http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe(port, nil))
}
