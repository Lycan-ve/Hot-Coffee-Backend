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

	db.AutoMigrate(
		&models.Player{},
		&startgg.ProcessedEvent{},
		&models.PlayerResult{},
		&models.Matchmaking{},
	)
	fmt.Println("✅ Base de datos conectada y migrada correctamente.")
}

func HandleGetRankings(w http.ResponseWriter, r *http.Request, league string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

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

func runFullSync(startGGToken string, db *gorm.DB, dgSession *discordgo.Session) {
	log.Printf("🔄 Iniciando sincronización completa...\n")
	startgg.CleanupDuplicatePlayers(db)

	discordTournaments, _ := discord.FetchTournamentsFromDiscord()

	if len(discordTournaments) > 0 && dgSession != nil {
		guildID := os.Getenv("DISCORD_GUILD_ID")
		activeMembers := discord.GetGuildMembersMap(dgSession, guildID)

		startgg.SyncTournamentsFromDiscordLive(discordTournaments, startGGToken, db, activeMembers)
	}

	startgg.SyncManualTournaments(db)
	log.Printf("✅ Sincronización de torneos finalizada.\n")
}

func main() {
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

	// Configurar intents necesarios
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsGuildMembers

	// Registrar manejadores de Matchmaking (botones y comandos)
	discord.RegisterMatchmakingHandlers(dg, db)

	// Abrir la conexión WebSocket (pone al bot en línea)
	err = dg.Open()
	if err != nil {
		log.Fatalf("❌ Error al conectar el bot con Discord: %v", err)
	}
	defer dg.Close()

	log.Printf("🤖 ¡Bot de Discord conectado y en línea exitosamente!")

	// Registrar el comando de barra /ft de forma instantánea en tu servidor (Guild Command)
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

	if guildID != "" {
		dg.ApplicationCommandCreate(dg.State.User.ID, guildID, cmd)
		log.Printf("📌 Comando /ft registrado instantáneamente para el servidor: %s", guildID)
	} else {
		dg.ApplicationCommandCreate(dg.State.User.ID, "", cmd)
		log.Println("⚠️ DISCORD_GUILD_ID no configurado; el comando /ft se registró globalmente (puede tardar en aparecer).")
	}

	// Ejecutar la sincronización inicial de Start.gg pasando la sesión activa del bot
	if startGGToken != "" {
		go runFullSync(startGGToken, db, dg)
	} else {
		log.Println("⚠️ Advertencia: STARTGG_TOKEN no está configurado en el .env")
	}

	// ==========================================
	// SERVIDOR HTTP
	// ==========================================
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
			go runFullSync(startGGToken, db, dg)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Write([]byte(`{"status": "success", "message": "Sincronización de torneos desde Discord iniciada."}`))
	})

	port := ":8080"
	fmt.Printf("🚀 Servidor corriendo en http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe(port, nil))
}
