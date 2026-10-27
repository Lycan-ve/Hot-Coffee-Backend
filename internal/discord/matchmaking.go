package discord

import (
	"fmt"
	"os"
	"strings"

	"smash-backend/internal/models"

	"github.com/bwmarrin/discordgo"
	"gorm.io/gorm"
)

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

// 1. Comando /ft: Envía la invitación al privado (DM) del rival escogido
func handleFTCommand(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB) {
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

	pointsAtStake := targetFT * 5

	match := models.Matchmaking{
		League:        "2xko_online",
		Player1ID:     hashUserID(author.ID),
		Player1Tag:    author.Username,
		Player2ID:     hashUserID(rivalUser.ID),
		Player2Tag:    rivalUser.Username,
		TargetFT:      targetFT,
		PointsAtStake: pointsAtStake,
		Status:        "PENDING",
	}
	db.Create(&match)

	// Crear canal de DM con el rival
	dmChannel, err := s.UserChannelCreate(rivalUser.ID)
	if err != nil {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ No se pudo enviar mensaje privado al rival. Asegúrate de que tenga los DMs abiertos.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	content := fmt.Sprintf("🎮 **SOLICITUD DE RETA FT %d**\n⚔️ **%s** te ha retado a un duelo.\n💰 Puntos en juego: **%d pts**\n\n¿Aceptas el desafío?",
		targetFT, author.Username, pointsAtStake)

	keyboard := discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "✅ Aceptar",
				Style:    discordgo.SuccessButton,
				CustomID: fmt.Sprintf("ft_accept_%d", match.ID),
			},
			discordgo.Button{
				Label:    "❌ Rechazar",
				Style:    discordgo.DangerButton,
				CustomID: fmt.Sprintf("ft_reject_%d", match.ID),
			},
		},
	}

	// Enviar invitación al DM del rival
	_, err = s.ChannelMessageSendComplex(dmChannel.ID, &discordgo.MessageSend{
		Content:    content,
		Components: []discordgo.MessageComponent{keyboard},
	})

	if err != nil {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ Error al enviar la invitación al privado del rival.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	// Responder al autor en el canal original de manera efímera/pública
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("✅ Invitación enviada al privado de **%s**. Esperando su respuesta...", rivalUser.Username),
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func handleComponentInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB) {
	customID := i.MessageComponentData().CustomID

	// Obtener ID del usuario que interactúa de forma segura (funciona en DM y en Servidor)
	var userIdStr string
	if i.Member != nil && i.Member.User != nil {
		userIdStr = i.Member.User.ID
	} else if i.User != nil {
		userIdStr = i.User.ID
	}

	// 2. ACEPTAR RETA
	if strings.HasPrefix(customID, "ft_accept_") {
		parts := strings.Split(customID, "_")
		matchID := parts[2]

		var match models.Matchmaking
		db.First(&match, matchID)

		if match.Status != "PENDING" {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "⚠️ Esta solicitud ya no está pendiente.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		}

		// Validar que solo el rival desafiado sea quien pueda aceptar la reta
		if hashUserID(userIdStr) != match.Player2ID {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "❌ Solo el jugador retado puede aceptar esta solicitud.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		}

		match.Status = "IN_PROGRESS"
		db.Save(&match)

		// Crear un Hilo (Thread) público dentro del canal de matchmaking
		matchmakingChannel := getMatchmakingChannelID()
		threadName := fmt.Sprintf("⚔️ Reta: %s vs %s (FT %d)", match.Player1Tag, match.Player2Tag, match.TargetFT)

		// USO CORRECTO EN DISCORDGO: s.ThreadStart
		thread, err := s.ThreadStart(
			matchmakingChannel,
			threadName,
			discordgo.ChannelTypeGuildPublicThread,
			1440, // Tiempo de auto-archivo en minutos (24 horas)
		)

		var threadMention string
		if err == nil {
			match.ThreadID = thread.ID
			db.Save(&match)
			threadMention = fmt.Sprintf("<#%s>", thread.ID)

			// Publicar mensaje de inicio dentro del hilo creado
			roundContent := fmt.Sprintf("🎮 **¡RETAS INICIADAS!**\n🥊 **%s** vs **%s**\n💰 Puntos en juego: **%d pts**\n\n¿Quién ganó la **Ronda 1**?",
				match.Player1Tag, match.Player2Tag, match.PointsAtStake)

			keyboard := getRoundKeyboard(match.ID, match.Player1Tag, match.Player2Tag)
			s.ChannelMessageSendComplex(thread.ID, &discordgo.MessageSend{
				Content:    roundContent,
				Components: []discordgo.MessageComponent{keyboard},
			})
		} else {
			threadMention = fmt.Sprintf("<#%s>", matchmakingChannel)
		}

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    fmt.Sprintf("✅ ¡Has aceptado la reta! Se ha creado un hilo para el enfrentamiento: %s", threadMention),
				Components: []discordgo.MessageComponent{},
			},
		})

		// 3. RECHAZAR RETA
	} else if strings.HasPrefix(customID, "ft_reject_") {
		parts := strings.Split(customID, "_")
		matchID := parts[2]

		var match models.Matchmaking
		db.First(&match, matchID)

		if match.Status != "PENDING" {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "⚠️ Esta solicitud ya no está pendiente.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		}

		// Validar que solo el rival desafiado sea quien pueda rechazar
		if hashUserID(userIdStr) != match.Player2ID {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "❌ Solo el jugador retado puede rechazar esta solicitud.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		}

		match.Status = "REJECTED"
		db.Save(&match)

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    "❌ Has rechazado la solicitud de reta.",
				Components: []discordgo.MessageComponent{},
			},
		})

		// 4. MANEJO DE RONDAS / VICTORIAS
	} else if strings.HasPrefix(customID, "ft_win_") {
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

		// --- SI TERMINA EL FT ---
		if match.Score1 >= maxWins || match.Score2 >= maxWins {
			match.Status = "PENDING_REMATCH"
			if match.Score1 > match.Score2 {
				match.WinnerID = match.Player1ID
				match.LoserID = match.Player2ID
			} else {
				match.WinnerID = match.Player2ID
				match.LoserID = match.Player1ID
			}
			db.Save(&match)

			report := fmt.Sprintf("🏁 **¡COMBATE FINALIZADO (FT %d)!**\n🥊 **%s** (%d) vs (%d) **%s**\n🏆 Ganador: **%s**\n\n¿Desean revancha con apuesta doble?",
				match.TargetFT, match.Player1Tag, match.Score1, match.Score2, match.Player2Tag, getWinnerTag(match))

			rematchKeyboard := discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						Label:    "🔄 Sí (Revancha - Apuesta Doble)",
						Style:    discordgo.SuccessButton,
						CustomID: fmt.Sprintf("ft_rematch_yes_%d", match.ID),
					},
					discordgo.Button{
						Label:    "🛑 No (Terminar y dar puntos)",
						Style:    discordgo.DangerButton,
						CustomID: fmt.Sprintf("ft_rematch_no_%d", match.ID),
					},
				},
			}

			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseUpdateMessage,
				Data: &discordgo.InteractionResponseData{
					Content:    report,
					Components: []discordgo.MessageComponent{rematchKeyboard},
				},
			})
			return
		}

		db.Save(&match)

		nextRoundContent := fmt.Sprintf("🎮 **MATCHMAKING FT %d**\n🥊 **%s** (**%d**) vs (**%d**) **%s**\n\n¿Quién gana la siguiente ronda?",
			match.TargetFT, match.Player1Tag, match.Score1, match.Score2, match.Player2Tag)

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    nextRoundContent,
				Components: []discordgo.MessageComponent{getRoundKeyboard(match.ID, match.Player1Tag, match.Player2Tag)},
			},
		})

		// 5. REVANCHA: SÍ
	} else if strings.HasPrefix(customID, "ft_rematch_yes_") {
		parts := strings.Split(customID, "_")
		matchID := parts[3]

		var match models.Matchmaking
		db.First(&match, matchID)

		match.Score1 = 0
		match.Score2 = 0
		match.PointsAtStake *= 2
		match.Status = "IN_PROGRESS"
		db.Save(&match)

		content := fmt.Sprintf("🔄 **¡REVANCHA INICIADA!**\n🔥 Puntos en juego: **%d pts**\n\n🥊 **%s** vs **%s**\n¿Quién gana la **Ronda 1**?",
			match.PointsAtStake, match.Player1Tag, match.Player2Tag)

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    content,
				Components: []discordgo.MessageComponent{getRoundKeyboard(match.ID, match.Player1Tag, match.Player2Tag)},
			},
		})

		// 6. REVANCHA: NO (Otorgar puntos y cerrar)
	} else if strings.HasPrefix(customID, "ft_rematch_no_") {
		parts := strings.Split(customID, "_")
		matchID := parts[3]

		var match models.Matchmaking
		db.First(&match, matchID)

		match.Status = "FINISHED"
		db.Save(&match)

		oldRankW := getPlayerRank(db, match.WinnerID)
		oldRankL := getPlayerRank(db, match.LoserID)

		updatePlayerPoints(db, match)

		newRankW := getPlayerRank(db, match.WinnerID)
		newRankL := getPlayerRank(db, match.LoserID)

		finalMsg := fmt.Sprintf("🏁 **MATCHMAKING CERRADO**\n🏆 Puntos entregados al ganador **%s** (+%d pts).\n*Reporte publicado en <#%s>.*",
			getWinnerTag(match), match.PointsAtStake, getRankingChannelID())

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    finalMsg,
				Components: []discordgo.MessageComponent{},
			},
		})

		sendRankingNotification(s, match, oldRankW, newRankW, oldRankL, newRankL)
	}
}

func getRoundKeyboard(matchID uint, p1Tag, p2Tag string) discordgo.ActionsRow {
	return discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    fmt.Sprintf("Gana %s", p1Tag),
				Style:    discordgo.PrimaryButton,
				CustomID: fmt.Sprintf("ft_win_%d_p1", matchID),
			},
			discordgo.Button{
				Label:    fmt.Sprintf("Gana %s", p2Tag),
				Style:    discordgo.DangerButton,
				CustomID: fmt.Sprintf("ft_win_%d_p2", matchID),
			},
		},
	}
}

func sendRankingNotification(s *discordgo.Session, match models.Matchmaking, oldRankW, newRankW, oldRankL, newRankL int) {
	winnerTag := getWinnerTag(match)
	loserTag := getLoserTag(match)

	embed := &discordgo.MessageEmbed{
		Title:       "📊 REPORTE DE MATCHMAKING & RANKING",
		Color:       0x00ff00,
		Description: fmt.Sprintf("Se completó el encuentro **FT %d** entre **%s** y **%s**.", match.TargetFT, match.Player1Tag, match.Player2Tag),
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "🥊 Marcador Final",
				Value:  fmt.Sprintf("**%s** `%d - %d` **%s**", match.Player1Tag, match.Score1, match.Score2, match.Player2Tag),
				Inline: false,
			},
			{
				Name:   fmt.Sprintf("🏆 %s (Ganador)", winnerTag),
				Value:  fmt.Sprintf("💰 **+%d Puntos**\n📈 Posición: **#%d** → **#%d**", match.PointsAtStake, oldRankW, newRankW),
				Inline: true,
			},
			{
				Name:   fmt.Sprintf("💀 %s (Perdedor)", loserTag),
				Value:  fmt.Sprintf("📉 **-%d Puntos**\n📉 Posición: **#%d** → **#%d**", match.PointsAtStake, oldRankL, newRankL),
				Inline: true,
			},
		},
	}

	s.ChannelMessageSendEmbed(getRankingChannelID(), embed)
}

func getPlayerRank(db *gorm.DB, playerID int) int {
	var count int64
	db.Raw(`
		SELECT COUNT(*) + 1 
		FROM players p1 
		WHERE p1.league = '2xko_online' AND p1.points > (
			SELECT COALESCE(p2.points, 0) FROM players p2 WHERE p2.id = ? AND p2.league = '2xko_online'
		)`, playerID).Scan(&count)
	return int(count)
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

func getWinnerTag(m models.Matchmaking) string {
	if m.WinnerID == m.Player1ID {
		return m.Player1Tag
	}
	return m.Player2Tag
}

func getLoserTag(m models.Matchmaking) string {
	if m.WinnerID == m.Player1ID {
		return m.Player2Tag
	}
	return m.Player1Tag
}
