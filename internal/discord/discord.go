package discord

import (
	"log"
	"os"
	"strings"

	"smash-backend/internal/startgg"

	"github.com/bwmarrin/discordgo"
)

// DiscordUserMap guarda los tags/usernames de los usuarios presentes en el servidor
type DiscordUserMap map[string]string // Key: Tag en minúsculas, Value: Discord User ID

// GetGuildMembersMap obtiene todos los miembros del servidor de Discord
func GetGuildMembersMap(s *discordgo.Session, guildID string) DiscordUserMap {
	membersMap := make(DiscordUserMap)

	members, err := s.GuildMembers(guildID, "", 1000)
	if err != nil {
		log.Printf("❌ Error al obtener miembros del servidor: %v\n", err)
		return membersMap
	}

	for _, m := range members {
		// Guardamos tanto el username como el GlobalName (Display Name)
		membersMap[strings.ToLower(m.User.Username)] = m.User.ID
		if m.User.GlobalName != "" {
			membersMap[strings.ToLower(m.User.GlobalName)] = m.User.ID
		}
	}

	return membersMap
}

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
		return nil, nil
	}

	messages, err := session.ChannelMessages(channelID, 100, "", "", "")
	if err != nil {
		log.Printf("❌ Error al obtener los mensajes del canal de Discord: %v\n", err)
		return nil, session
	}

	log.Printf("📥 Se obtuvieron %d mensajes del canal de Discord.\n", len(messages))

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

	return startgg.ExtractTournamentsAndImages(internalMessages), session
}
