package discord

import (
	"log"
	"os"

	"smash-backend/internal/startgg"

	"github.com/bwmarrin/discordgo"
)

// FetchTournamentsFromDiscord se conecta a Discord, lee los mensajes del canal de calendario
// y extrae los slugs de Start.gg junto con sus imágenes adjuntas.
func FetchTournamentsFromDiscord() []startgg.TournamentInfo {
	botToken := os.Getenv("DISCORD_BOT_TOKEN")
	channelID := os.Getenv("DISCORD_CALENDAR_CHANNEL_ID")

	if botToken == "" || channelID == "" {
		log.Println("⚠️ DISCORD_BOT_TOKEN o DISCORD_CALENDAR_CHANNEL_ID no están configurados en el .env")
		return nil
	}

	session, err := discordgo.New("Bot " + botToken)
	if err != nil {
		log.Printf("❌ Error al crear la sesión de Discord: %v\n", err)
		return nil
	}

	// Opcional: configurar intents si el bot lo requiere (Guilds y GuildMessages)
	session.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsGuilds

	// Obtener los últimos mensajes del canal (puedes ajustar el límite, ej. 100 o 50)
	messages, err := session.ChannelMessages(channelID, 100, "", "", "")
	if err != nil {
		log.Printf("❌ Error al obtener los mensajes del canal de Discord: %v\n", err)
		return nil
	}

	log.Printf("📥 Se obtuvieron %d mensajes del canal de Discord.\n", len(messages))

	// Convertir []*discordgo.Message al formato interno que ya usa tu función ExtractTournamentsAndImages
	var internalMessages []startgg.DiscordMessage

	for _, msg := range messages {
		var attachments []startgg.Attachment
		for _, att := range msg.Attachments {
			attachments = append(attachments, startgg.Attachment{
				URL:      att.URL,
				FileName: att.Filename,
			})
		}

		var forwarded *startgg.ForwardedMessage
		if msg.ReferencedMessage != nil {
			var refAttachments []startgg.Attachment
			for _, att := range msg.ReferencedMessage.Attachments {
				refAttachments = append(refAttachments, startgg.Attachment{
					URL:      att.URL,
					FileName: att.Filename,
				})
			}
			forwarded = &startgg.ForwardedMessage{
				Content:     msg.ReferencedMessage.Content,
				Attachments: refAttachments,
			}
		}

		internalMessages = append(internalMessages, startgg.DiscordMessage{
			Content:          msg.Content,
			Attachments:      attachments,
			ForwardedMessage: forwarded,
		})
	}

	// Reutilizamos tu función existente para extraer los torneos únicos y sus imágenes
	return startgg.ExtractTournamentsAndImages(internalMessages)
}
