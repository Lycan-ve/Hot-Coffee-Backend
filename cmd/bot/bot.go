package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"smash-backend/internal/database"
	"smash-backend/internal/discord"
	"smash-backend/internal/models"
	"smash-backend/internal/startgg"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"gorm.io/gorm"
)

const syncInterval = 6 * time.Hour

// runFullSync ejecuta el proceso completo de sincronización de torneos de Start.gg
// (es la misma función que vivía en cmd/api/main.go: necesita la sesión del bot)
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
	startgg.SyncTournamentsFromDiscordJSON("Hot Coffee _ 2XKO - TORNEOS HOT COFFEE - 📊│hot-coffee-torneos [1430651477612826624].json", startGGToken, db, activeMembers)

	// 3. Ejecuta torneos configurados de forma manual (id > 900000)
	startgg.SyncManualTournaments(db, activeMembers)
	log.Printf("✅ Sincronización de torneos finalizada.\n")
}

// startHealthServer: endpoint mínimo para UptimeRobot.
// Los hosts gratuitos duermen sin tráfico: el ping cada 5 min mantiene al bot despierto.
func startHealthServer() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
	log.Printf("❤️ Health server activo en el puerto %s (monitorea esta URL con UptimeRobot)", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func main() {
	// Carga las variables del archivo .env si se ejecuta localmente
	godotenv.Load()
	db := database.Connect()

	startGGToken := os.Getenv("STARTGG_TOKEN")
	botToken := os.Getenv("DISCORD_BOT_TOKEN")
	guildID := os.Getenv("DISCORD_GUILD_ID")

	if botToken == "" {
		log.Fatal("❌ DISCORD_BOT_TOKEN no está configurado en el .env")
	}

	// Health server en segundo plano (para UptimeRobot)
	go startHealthServer()

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

	// Janitor de matchmaking: auto-confirmaciones, cierres y cancelaciones automáticas
	discord.StartMatchmakingJanitor(dg, db)

	// 🌐 Worker de la cola web: lanza en Discord los sets confirmados desde la página
	discord.StartWebQueueWorker(dg, db)

	go func() {
		// 1) Conectar con reintentos (backoff 30 s -> 10 min).
		//    Si Cloudflare bloquea la IP del host (429 "error code: 1015"),
		//    el proceso NO muere: reintenta hasta que se levante el bloqueo.
		for attempt := 1; ; attempt++ {
			if err := dg.Open(); err != nil {
				wait := time.Duration(attempt) * 30 * time.Second
				if wait > 10*time.Minute {
					wait = 10 * time.Minute
				}
				log.Printf("❌ Error al conectar el bot con Discord: %v", err)
				log.Printf("⏳ Reintento #%d en %v...", attempt, wait)
				time.Sleep(wait)
				continue
			}
			log.Printf("🤖 ¡Bot de Discord conectado y en línea exitosamente!")
			break
		}

		// 2) Limpieza: /cerrarset fue eliminado (los mods cierran con el botón 🏳️ Cerrar set).
		if guildID != "" {
			if existing, err := dg.ApplicationCommands(dg.State.User.ID, guildID); err == nil {
				for _, c := range existing {
					if c.Name == "cerrarset" {
						_ = dg.ApplicationCommandDelete(dg.State.User.ID, guildID, c.ID)
					}
				}
			}
		}

		// 3) Construcción y registro del comando de barra /ft
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

		// 4) Sincronización de Start.gg: al arrancar y luego cada 6 horas
		if startGGToken != "" {
			runFullSync(startGGToken, db, dg)
			ticker := time.NewTicker(syncInterval)
			go func() {
				for range ticker.C {
					runFullSync(startGGToken, db, dg)
				}
			}()
		} else {
			log.Println("⚠️ Advertencia: STARTGG_TOKEN no está configurado en el .env")
		}

		// 5) Poll de solicitudes manuales de sync desde la web (botón "Sincronizar"):
		//    la API marca pending=true en sync_requests; aquí se ejecuta en < 60 s.
		go func() {
			ticker := time.NewTicker(time.Minute)
			for range ticker.C {
				var req models.SyncRequest
				if err := db.First(&req, 1).Error; err != nil {
					continue // la fila la crea la API la primera vez que se usa
				}
				if !req.Pending {
					continue
				}
				now := time.Now()
				db.Model(&models.SyncRequest{}).Where("id = 1").Updates(map[string]any{
					"pending":      false,
					"completed_at": now,
				})
				if startGGToken == "" {
					continue
				}
				log.Println("🔄 Sincronización manual solicitada desde la web...")
				runFullSync(startGGToken, db, dg)
			}
		}()
	}()

	defer dg.Close()

	// El proceso del bot vive para siempre (el health server corre en su goroutine)
	select {}
}
