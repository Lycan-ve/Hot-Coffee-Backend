package discord

import (
	"fmt"
	"os"
	"strings"

	"smash-backend/internal/models"

	"github.com/bwmarrin/discordgo"
	"gorm.io/gorm"
)

// Constantes de Canales
func getMatchmakingChannelID() string {
	if id := os.Getenv("DISCORD_MATCHMAKING_CHANNEL_ID"); id != "" {
		return id
	}
	return "1564293602606186576"
}

func getRankingChannelID() string {
	if id := os.Getenv("DISCORD_RANKING_CHANNEL_ID"); id != "" {
		return id
	}
	return "1564298796425543833"
}

func RegisterMatchmakingHandlers(s *discordgo.Session, db *gorm.DB) {
	s.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type == discordgo.InteractionApplicationCommand {
			if i.ApplicationCommandData().Name == "ft" {
				handleFTCommand(s, i, db)
			}
		} else if i.Type == discordgo.InteractionMessageComponent {
			handleComponentInteraction(s, i, db)
		}
	})
}

func handleFTCommand(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB) {
	// Verificar que el comando se ejecute en el canal de matchmaking o notificar
	matchmakingChannel := getMatchmakingChannelID()

	options := i.ApplicationCommandData().Options
	targetFT := int(options[0].IntValue())
	rivalUser := options[1].UserValue(s)
	author := i.Member.User

	if author.ID == rivalUser.ID {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ No puedes retarte a ti mismo.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	pointsAtStake := targetFT * 5 // 15, 25, 35, 50 puntos según el FT

	match := models.Matchmaking{
		League:        "2xko_online",
		Player1ID:     hashUserID(author.ID),
		Player1Tag:    author.Username,
		Player2ID:     hashUserID(rivalUser.ID),
		Player2Tag:    rivalUser.Username,
		TargetFT:      targetFT,
		PointsAtStake: pointsAtStake,
		Status:        "IN_PROGRESS",
	}
	db.Create(&match)

	content := fmt.Sprintf("🎮 **NUEVO MATCHMAKING FT %d**\n⚔️ **<@%s>** vs **<@%s>**\n💰 Puntos en juego: **%d pts**\n\n¿Quién ganó la **Ronda 1**?",
		targetFT, author.ID, rivalUser.ID, pointsAtStake)

	keyboard := discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    fmt.Sprintf("Gana %s", author.Username),
				Style:    discordgo.PrimaryButton,
				CustomID: fmt.Sprintf("ft_win_%d_p1", match.ID),
			},
			discordgo.Button{
				Label:    fmt.Sprintf("Gana %s", rivalUser.Username),
				Style:    discordgo.DangerButton,
				CustomID: fmt.Sprintf("ft_win_%d_p2", match.ID),
			},
		},
	}

	// Responder en el canal de Matchmaking (1564293602606186576)
	if i.ChannelID != matchmakingChannel {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: fmt.Sprintf("✅ La reta ha sido creada en el canal <#%s>.", matchmakingChannel),
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})

		s.ChannelMessageSendComplex(matchmakingChannel, &discordgo.MessageSend{
			Content:    content,
			Components: []discordgo.MessageComponent{keyboard},
		})
	} else {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content:    content,
				Components: []discordgo.MessageComponent{keyboard},
			},
		})
	}
}

func handleComponentInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB) {
	customID := i.MessageComponentData().CustomID

	if strings.HasPrefix(customID, "ft_win_") {
		parts := strings.Split(customID, "_")
		matchID := parts[2]
		winnerSide := parts[3]

		var match models.Matchmaking
		db.First(&match, matchID)

		if match.Status != "IN_PROGRESS" {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{Content: "⚠️ Este matchmaking ya ha finalizado.", Flags: discordgo.MessageFlagsEphemeral},
			})
			return
		}

		if winnerSide == "p1" {
			match.Score1++
		} else {
			match.Score2++
		}

		// Cálculo de victorias necesarias según el FT
		maxWins := (match.TargetFT / 2) + 1
		if match.TargetFT == 3 {
			maxWins = 2
		}
		if match.TargetFT == 5 {
			maxWins = 3
		}
		if match.TargetFT == 7 {
			maxWins = 4
		}
		if match.TargetFT == 10 {
			maxWins = 6
		}

		// --- SI EL MATCHMAKING TERMINA ---
		if match.Score1 >= maxWins || match.Score2 >= maxWins {
			match.Status = "FINISHED"
			if match.Score1 > match.Score2 {
				match.WinnerID = match.Player1ID
				match.LoserID = match.Player2ID
			} else {
				match.WinnerID = match.Player2ID
				match.LoserID = match.Player1ID
			}
			db.Save(&match)

			// 1. Obtener posiciones ANTES de actualizar
			oldRankWinner := getPlayerRank(db, match.WinnerID)
			oldRankLoser := getPlayerRank(db, match.LoserID)

			// 2. Transferir puntos
			updatePlayerPoints(db, match)

			// 3. Obtener posiciones DESPUÉS de actualizar
			newRankWinner := getPlayerRank(db, match.WinnerID)
			newRankLoser := getPlayerRank(db, match.LoserID)

			// Notificación corta en el canal de Matchmaking + Opción a Revancha
			matchmakingReport := fmt.Sprintf("🏁 **MATCHMAKING FINALIZADO (FT %d)**\n🥊 **%s** (%d) vs (%d) **%s**\n🏆 Ganador: **%s**\n\n*Los detalles del ranking se han publicado en <#%s>.*",
				match.TargetFT, match.Player1Tag, match.Score1, match.Score2, match.Player2Tag,
				getWinnerTag(match), getRankingChannelID(),
			)

			rematchKeyboard := discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						Label:    "🔄 Pedir Revancha (Apuesta Doble)",
						Style:    discordgo.SuccessButton,
						CustomID: fmt.Sprintf("ft_rematch_%d", match.ID),
					},
				},
			}

			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseUpdateMessage,
				Data: &discordgo.InteractionResponseData{
					Content:    matchmakingReport,
					Components: []discordgo.MessageComponent{rematchKeyboard},
				},
			})

			// Publicar Reporte Completo de Ranking en el Canal 1564298796425543833
			sendRankingNotification(s, match, oldRankWinner, newRankWinner, oldRankLoser, newRankLoser)
			return
		}

		db.Save(&match)

		// Continuar con la siguiente ronda
		nextRoundContent := fmt.Sprintf("🎮 **MATCHMAKING FT %d**\n🥊 **%s** (**%d**) vs (**%d**) **%s**\n\n¿Quién ganó la siguiente ronda?",
			match.TargetFT, match.Player1Tag, match.Score1, match.Score2, match.Player2Tag,
		)

		keyboard := discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    fmt.Sprintf("Gana %s", match.Player1Tag),
					Style:    discordgo.PrimaryButton,
					CustomID: fmt.Sprintf("ft_win_%d_p1", match.ID),
				},
				discordgo.Button{
					Label:    fmt.Sprintf("Gana %s", match.Player2Tag),
					Style:    discordgo.DangerButton,
					CustomID: fmt.Sprintf("ft_win_%d_p2", match.ID),
				},
			},
		}

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    nextRoundContent,
				Components: []discordgo.MessageComponent{keyboard},
			},
		})

	} else if strings.HasPrefix(customID, "ft_rematch_") {
		// --- LÓGICA DE REVANCHA ---
		parts := strings.Split(customID, "_")
		matchID := parts[2]

		var match models.Matchmaking
		db.First(&match, matchID)

		match.Score1 = 0
		match.Score2 = 0
		match.PointsAtStake *= 2 // Apuesta doble
		match.Status = "IN_PROGRESS"
		db.Save(&match)

		rematchContent := fmt.Sprintf("🔄 **¡REVANCHA INICIADA!**\n🔥 Puntos en juego apostados: **%d pts**\n\n🥊 **%s** vs **%s**\n¿Quién gana la **Ronda 1**?",
			match.PointsAtStake, match.Player1Tag, match.Player2Tag,
		)

		keyboard := discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    fmt.Sprintf("Gana %s", match.Player1Tag),
					Style:    discordgo.PrimaryButton,
					CustomID: fmt.Sprintf("ft_win_%d_p1", match.ID),
				},
				discordgo.Button{
					Label:    fmt.Sprintf("Gana %s", match.Player2Tag),
					Style:    discordgo.DangerButton,
					CustomID: fmt.Sprintf("ft_win_%d_p2", match.ID),
				},
			},
		}

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    rematchContent,
				Components: []discordgo.MessageComponent{keyboard},
			},
		})
	}
}

// Envío de Embed al canal de Ranking (1564298796425543833)
func sendRankingNotification(s *discordgo.Session, match models.Matchmaking, oldRankW, newRankW, oldRankL, newRankL int) {
	winnerTag := getWinnerTag(match)
	loserTag := getLoserTag(match)

	winnerRankDiff := formatRankChange(oldRankW, newRankW)
	loserRankDiff := formatRankChange(oldRankL, newRankL)

	embed := &discordgo.MessageEmbed{
		Title:       "📊 REPORTE DE MATCHMAKING & RANKING",
		Color:       0x00ff00, // Verde
		Description: fmt.Sprintf("Se ha completado el encuentro **FT %d** entre **%s** y **%s**.", match.TargetFT, match.Player1Tag, match.Player2Tag),
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "🥊 Marcador Final",
				Value:  fmt.Sprintf("**%s** `%d - %d` **%s**", match.Player1Tag, match.Score1, match.Score2, match.Player2Tag),
				Inline: false,
			},
			{
				Name:   fmt.Sprintf("🏆 %s (Ganador)", winnerTag),
				Value:  fmt.Sprintf("💰 **+%d Puntos**\n📈 Ranking: Posición **#%d** → **#%d** (%s)", match.PointsAtStake, oldRankW, newRankW, winnerRankDiff),
				Inline: true,
			},
			{
				Name:   fmt.Sprintf("💀 %s (Perdedor)", loserTag),
				Value:  fmt.Sprintf("📉 **-%d Puntos**\n📉 Ranking: Posición **#%d** → **#%d** (%s)", match.PointsAtStake, oldRankL, newRankL, loserRankDiff),
				Inline: true,
			},
		},
	}

	s.ChannelMessageSendEmbed(getRankingChannelID(), embed)
}

func getPlayerRank(db *gorm.DB, playerID int) int {
	var count int64
	// Cuenta cuántos jugadores tienen más puntos que el jugador actual
	db.Raw(`
		SELECT COUNT(*) + 1 
		FROM players p1 
		WHERE p1.league = '2xko_online' AND p1.points > (
			SELECT COALESCE(p2.points, 0) FROM players p2 WHERE p2.id = ? AND p2.league = '2xko_online'
		)`, playerID).Scan(&count)
	return int(count)
}

func formatRankChange(oldRank, newRank int) string {
	if oldRank > newRank {
		return fmt.Sprintf("▲ Ascendió %d pos", oldRank-newRank)
	} else if oldRank < newRank {
		return fmt.Sprintf("▼ Descendió %d pos", newRank-oldRank)
	}
	return "➡️ Se mantiene"
}

func updatePlayerPoints(db *gorm.DB, match models.Matchmaking) {
	var pWinner, pLoser models.Player
	db.First(&pWinner, "id = ?", match.WinnerID)
	db.First(&pLoser, "id = ?", match.LoserID)

	if pWinner.ID != 0 {
		pWinner.Points += match.PointsAtStake
		db.Save(&pWinner)
	}
	if pLoser.ID != 0 {
		pLoser.Points -= match.PointsAtStake
		if pLoser.Points < 0 {
			pLoser.Points = 0
		}
		db.Save(&pLoser)
	}
}

// hashUserID convierte el ID de usuario de Discord (string) a un número entero único para la BD.
func hashUserID(discordID string) int {
	h := 0
	for _, c := range discordID {
		h = 31*h + int(c)
	}
	if h < 0 {
		h = -h
	}
	return h
}

// getWinnerTag devuelve el GamerTag del jugador que ganó la reta.
func getWinnerTag(m models.Matchmaking) string {
	if m.WinnerID == m.Player1ID {
		return m.Player1Tag
	}
	return m.Player2Tag
}

// getLoserTag devuelve el GamerTag del jugador que perdió la reta.
func getLoserTag(m models.Matchmaking) string {
	if m.WinnerID == m.Player1ID {
		return m.Player2Tag
	}
	return m.Player1Tag
}
