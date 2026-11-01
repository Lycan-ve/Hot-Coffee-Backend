package discord

import (
	"log"
	"os"

	"smash-backend/internal/startgg"

	"github.com/bwmarrin/discordgo"
)

// DiscordUserMap: clave = ID de usuario de Discord, valor = username (solo informativo)
type DiscordUserMap map[string]string

func GetGuildMembersMap(s *discordgo.Session, guildID string) DiscordUserMap {
	membersMap := make(DiscordUserMap)

	if guildID == "" || guildID == "members" {
		log.Println("❌ Error: DISCORD_GUILD_ID no está configurado o es inválido.")
		return membersMap
	}

	after := ""
	for {
		batch, err := s.GuildMembers(guildID, after, 1000)
		if err != nil {
			log.Printf("❌ Error al obtener miembros (GuildID: %s): %v\n", guildID, err)
			return make(DiscordUserMap) // lista incompleta = vacía, para no validar a medias
		}
		for _, m := range batch {
			if m.User == nil || m.User.Bot {
				continue
			}
			membersMap[m.User.ID] = m.User.Username
		}
		if len(batch) < 1000 {
			break
		}
		after = batch[len(batch)-1].User.ID
	}

	log.Printf("👥 %d miembros cargados del servidor de Discord\n", len(membersMap))
	return membersMap
}

// toInternalMessages convierte los mensajes que devuelve la API de Discord al
// formato interno que usa el sincronizador de Start.gg (contenido, adjuntos y
// mensajes referenciados/reenviados).
func toInternalMessages(messages []*discordgo.Message) []startgg.DiscordMessage {
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

	return internalMessages
}

// FetchTournamentMessagesFromChannel lee el historial del canal de torneos con la
// sesión ya conectada del bot (no abre una conexión nueva) y lo devuelve en el
// formato interno de startgg. Recorre el canal hacia atrás en lotes de 100
// mensajes hasta llegar al primero del canal (máximo 1000 por seguridad).
func FetchTournamentMessagesFromChannel(s *discordgo.Session, channelID string) []startgg.DiscordMessage {
	if s == nil || channelID == "" {
		log.Println("❌ FetchTournamentMessagesFromChannel: sesión de Discord o ID de canal inválidos.")
		return nil
	}

	const batchSize = 100
	const maxMessages = 1000

	var all []*discordgo.Message
	beforeID := ""
	for len(all) < maxMessages {
		batch, err := s.ChannelMessages(channelID, batchSize, beforeID, "", "")
		if err != nil {
			log.Printf("❌ Error al leer los mensajes del canal de torneos (ID: %s): %v\n", channelID, err)
			break
		}
		all = append(all, batch...)
		if len(batch) < batchSize {
			break // se llegó al primer mensaje del canal
		}
		beforeID = batch[len(batch)-1].ID
	}

	log.Printf("📥 Se leyeron %d mensajes del canal de torneos de Discord.\n", len(all))
	return toInternalMessages(all)
}

// FetchTournamentsFromDiscord mantiene su funcionamiento anterior (canal del
// calendario con DISCORD_CALENDAR_CHANNEL_ID), pero reutiliza la conversión
// compartida toInternalMessages.
func FetchTournamentsFromDiscord() ([]startgg.TournamentInfo, *discordgo.Session) {
	botToken := os.Getenv("DISCORD_BOT_TOKEN")
	channelID := os.Getenv("DISCORD_CALENDAR_CHANNEL_ID")

	if botToken == "" || channelID == "" {
		log.Println("⚠️ DISCORD_BOT_TOKEN o DISCORD_CALENDAR_CHANNEL_ID no están configurados en el .env")
		return nil, nil
	}

	session, err := discordgo.New("Bot " + botToken)
	if err != nil {
		log.Printf("❌ Error al crear la sesión de Discord: %v\n", err)
		return nil, nil
	}

	session.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsGuilds | discordgo.IntentsGuildMembers

	if err := session.Open(); err != nil {
		log.Printf("❌ Error al abrir conexión con Discord: %v\n", err)
		return nil, session
	}

	messages, err := session.ChannelMessages(channelID, 100, "", "", "")
	if err != nil {
		log.Printf("❌ Error al obtener los mensajes del canal de Discord: %v\n", err)
		return nil, session
	}

	log.Printf("📥 Se obtuvieron %d mensajes del canal de Discord.\n", len(messages))

	return startgg.ExtractTournamentsAndImages(toInternalMessages(messages)), session
}
