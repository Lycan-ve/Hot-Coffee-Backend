package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"smash-backend/internal/database"
	"smash-backend/internal/discord"
	"smash-backend/internal/models"
	"smash-backend/internal/presence"
	"smash-backend/internal/startgg"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
	"gorm.io/gorm"
)

const syncInterval = 6 * time.Hour

// syncMu evita que dos sincronizaciones se pisen entre sí: la sync corre al
// arrancar, cada 6 h y desde el botón de la web (poll cada 60 s). Sin este
// candado, dos pasadas simultáneas podían sobrescribir los puntos de un
// mismo jugador (lost-update) aunque cada evento se procesara una sola vez.
var syncMu sync.Mutex

// runFullSync ejecuta el proceso completo de sincronización de torneos de Start.gg
// (es la misma función que vivía en cmd/api/main.go: necesita la sesión del bot).
// Devuelve false si ya había una sync en curso (el llamador decide si reintenta)
// y true si esta pasada se ejecutó.
func runFullSync(startGGToken string, db *gorm.DB, dgSession *discordgo.Session) bool {
	if !syncMu.TryLock() {
		log.Println("⏭️ Ya hay una sincronización en curso: esta pasada se omite.")
		return false
	}
	defer syncMu.Unlock()

	log.Printf("🔄 Iniciando sincronización completa...")

	// 1. Limpia perfiles o etiquetas duplicadas en la base de datos
	startgg.CleanupDuplicatePlayers(db)

	// Obtener los miembros activos del servidor de Discord para la validación estricta
	if dgSession == nil {
		log.Println("🛑 Sin sesión de Discord: se cancela la sync.")
		return false
	}
	activeMembers := discord.GetGuildMembersMap(dgSession, os.Getenv("DISCORD_GUILD_ID"))
	if len(activeMembers) == 0 {
		log.Println("🛑 No se pudo cargar la lista de miembros de Discord: se cancela la sync para no marcar torneos como procesados sin jugadores.")
		return false
	}

		// 2. Sincroniza leyendo los links de Start.gg publicados en los canales de
	//    Discord: el del calendario (DISCORD_CALENDAR_CHANNEL_ID) y el de
	//    torneos (DISCORD_TOURNAMENT_CHANNEL_ID). Basta con que uno de los dos
	//    esté configurado; los mensajes de ambos canales se combinan y se
	//    preprocesan juntos (la sync es idempotente por event_id, así que un
	//    mismo torneo publicado en los dos canales se procesa una sola vez).
	seen := make(map[string]bool)
	var channelIDs []string
	for _, id := range []string{os.Getenv("DISCORD_CALENDAR_CHANNEL_ID"), os.Getenv("DISCORD_TOURNAMENT_CHANNEL_ID")} {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		channelIDs = append(channelIDs, id)
	}
	if len(channelIDs) == 0 {
		log.Println("🛑 Ni DISCORD_CALENDAR_CHANNEL_ID ni DISCORD_TOURNAMENT_CHANNEL_ID están configurados en el .env: se cancela la sync de torneos.")
		return false
	}

	var messages []startgg.DiscordMessage
	for _, id := range channelIDs {
		messages = append(messages, discord.FetchTournamentMessagesFromChannel(dgSession, id)...)
	}
	tournaments := startgg.ExtractTournamentsAndImages(messages)
	startgg.SyncTournamentsFromDiscordLive(tournaments, startGGToken, db, activeMembers)

	// 3. Ejecuta torneos configurados de forma manual (id > 900000)
	startgg.SyncManualTournaments(db, activeMembers)

	// 4. Reconstruye el ranking desde el historial de torneos: garantiza que
	//    los puntos del ranking coincidan exactamente con la suma de los
	//    torneos que muestra el perfil web y repara dobles sumas históricas.
	if _, err := startgg.RepairRankingFromPlacements(db); err != nil {
		log.Printf("⚠️  No se pudo reconstruir el ranking desde el historial: %v", err)
	}
	log.Printf("✅ Sincronización de torneos finalizada.")
	return true
}

// onPresenceUpdate guarda cada cambio de estado de un miembro del servidor.
// Discord lo entrega en tiempo real por el Gateway; la API lo lee de la tabla.
func onPresenceUpdate(_ *discordgo.Session, p *discordgo.PresenceUpdate) {
	if p == nil || p.User == nil {
		return
	}
	presence.Set(p.User.ID, string(p.Status), playingName(p.Activities))
}

// onGuildPresencesBackfill: al conectar (y en cada reconexión) GUILD_CREATE
// trae el estado actual de todos los miembros; sin este backfill la web
// mostraría "desconectado" hasta que cada jugador cambiara de estado.
func onGuildPresencesBackfill(_ *discordgo.Session, g *discordgo.GuildCreate) {
	n := 0
	for _, pr := range g.Presences {
		if pr == nil || pr.User == nil {
			continue
		}
		presence.Set(pr.User.ID, string(pr.Status), playingName(pr.Activities))
		n++
	}
	log.Printf("👀 Presencias iniciales cargadas (%d miembros con estado conocido).", n)
}

// playingName devuelve el nombre del juego en curso (2XKO, etc.) o "".
func playingName(activities []*discordgo.Activity) string {
	for _, a := range activities {
		if a != nil && a.Type == discordgo.ActivityTypePlaying {
			return a.Name
		}
	}
	return ""
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
	srv := &http.Server{
		Addr:              ":" + port,
		ReadHeaderTimeout:  5 * time.Second,
		ReadTimeout:        10 * time.Second,
		WriteTimeout:       10 * time.Second,
		IdleTimeout:        60 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func main() {
	// Carga las variables del archivo .env si se ejecuta localmente
	godotenv.Load()

	// Health server PRIMERO: Back4App hace el health check a los ~6 s,
	// hay que abrir el puerto antes de conectar a la base de datos
	go startHealthServer()

	db := database.Connect()

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
	// IntentsGuildPresences: estado en vivo de los miembros. Requiere activar
	// "SERVER PRESENCES INTENT" en el Discord Developer Portal, o dg.Open()
	// fallará con "used disallowed intents".
	dg.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsGuildMembers | discordgo.IntentsGuildPresences

	// Registrar manejadores de eventos (escuchadores de clics de botones y slash commands)
	discord.RegisterMatchmakingHandlers(dg, db)

	// 👀 Presencias en vivo: el bot guarda el estado de cada miembro en la BD
	// y la API (cmd/api, otro proceso) lo lee para mostrarlo en la web.
	presence.Init(db)
	dg.AddHandler(onPresenceUpdate)
	dg.AddHandler(onGuildPresencesBackfill)

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
		//    (todos los sets ahora son FT 3: partidas equitativas por rango).
		cmd := &discordgo.ApplicationCommand{
			Name:        "ft",
			Description: "Reta a un rival a un set FT 3 (partidas equitativas por rango)",
			Options: []*discordgo.ApplicationCommandOption{
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
			log.Println("⚠️ DISCORD_GUILD_ID no configurado; los comandos se registraron globalmente (pueden tardar en aparecer.")
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

				// Reclama la solicitud ANTES de ejecutar: si la sync tarda varios
				// minutos, el siguiente tick no debe lanzarla otra vez.
				claim := db.Model(&models.SyncRequest{}).Where("id = ? AND pending = ?", 1, true).
					Updates(map[string]any{"pending": false, "requested_at": time.Now(), "completed_at": nil})
				if claim.Error != nil || claim.RowsAffected == 0 {
					continue
				}

				if startGGToken == "" {
					db.Model(&models.SyncRequest{}).Where("id = 1").Update("completed_at", time.Now())
					continue
				}

				log.Println("🔄 Sincronización manual solicitada desde la web...")
				if !runFullSync(startGGToken, db, dg) {
					// Ya había una sync en curso: devolver la solicitud a la cola
					// para que corra apenas termine la actual.
					db.Model(&models.SyncRequest{}).Where("id = 1").Update("pending", true)
					continue
				}
				// Recién al TERMINAR la sync queda "completada": el cooldown de
				// la web cuenta desde el final, no desde que se recogió la solicitud.
				db.Model(&models.SyncRequest{}).Where("id = 1").Update("completed_at", time.Now())
			}
		}()
	}()

	// Cierre limpio: con SIGINT/SIGTERM se desconecta Discord y el health server
	// se apaga sin dejar hilos ni paneles a medias.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	log.Println("🤖 Bot en línea. Ctrl+C para apagar.")
	<-sigCh
	log.Println("👋 Señal de apagado recibida: cerrando sesión de Discord…")
	dg.Close()
	log.Println("✅ Bot apagado correctamente.")
}
