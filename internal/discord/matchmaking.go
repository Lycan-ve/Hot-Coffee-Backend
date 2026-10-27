package discord

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

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

func getMatchmakingResultChannelID() string {
	if id := os.Getenv("DISCORD_MATCHMAKING_RESULT_CHANNEL_ID"); id != "" {
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

// Asegurar que el jugador exista en la BD (si es nuevo, lo registra con su ID de Discord)
// ensurePlayerExists garantiza que el jugador exista en la base de datos buscando por su DiscordID real.
// Devuelve el ID primario de la tabla 'players'.
func ensurePlayerExists(db *gorm.DB, discordIDStr string, username string) int {
	var player models.Player

	// 1. Buscar en la BD usando la columna discord_id
	err := db.First(&player, "discord_id = ? AND league = '2xko_online'", discordIDStr).Error

	if errorsIsRecordNotFound(err) {
		// CASO A: El usuario es completamente nuevo
		playerID := hashUserID(discordIDStr)
		player = models.Player{
			ID:        playerID,
			League:    "2xko_online",
			GamerTag:  username,
			Character: "2XKO_FIGHTER",
			DiscordID: discordIDStr,
			Points:    0,
		}

		if err := db.Create(&player).Error; err != nil {
			log.Printf("❌ Error al crear jugador nuevo (%s): %v\n", username, err)
		} else {
			log.Printf("✨ Nuevo jugador registrado por Reta: %s (Discord ID: %s)\n", username, discordIDStr)
		}
		return playerID
	} else if err == nil {
		// CASO B: El jugador ya existía (por Start.gg o una Reta anterior)
		// Si no tenía GamerTag o cambio de nombre de usuario en Discord, actualizamos la info
		if player.GamerTag == "" || (player.GamerTag != username && player.ProfileURL == "") {
			player.GamerTag = username
			db.Model(&player).Update("gamer_tag", username)
		}
		return player.ID
	}

	// Respaldo en caso de error en la BD
	return hashUserID(discordIDStr)
}

func errorsIsRecordNotFound(err error) bool {
	return err != nil && err.Error() == gorm.ErrRecordNotFound.Error()
}

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

	// Asegurar que ambos existan en la BD
	p1ID := ensurePlayerExists(db, author.ID, author.Username)
	p2ID := ensurePlayerExists(db, rivalUser.ID, rivalUser.Username)

	pointsAtStake := targetFT * 5 // Ejemplo: FT 3 = 15 puntos

	match := models.Matchmaking{
		League:        "2xko_online",
		Player1ID:     p1ID,
		Player1Tag:    author.Username,
		Player2ID:     p2ID,
		Player2Tag:    rivalUser.Username,
		TargetFT:      targetFT,
		PointsAtStake: pointsAtStake,
		Status:        "PENDING",
	}
	db.Create(&match)

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

	var userIdStr string
	if i.Member != nil && i.Member.User != nil {
		userIdStr = i.Member.User.ID
	} else if i.User != nil {
		userIdStr = i.User.ID
	}

	userHash := hashUserID(userIdStr)

	// ----------------------------------------------------
	// 1. ACEPTAR RETA (DM -> Crear Hilo Público)
	// ----------------------------------------------------
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

		if userHash != match.Player2ID {
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
		matchmakingChannel := getMatchmakingChannelID()

		// Mensaje base en canal público
		p1DiscordID := getDiscordIDByHash(db, match.Player1ID)
		p2DiscordID := getDiscordIDByHash(db, match.Player2ID)

		baseMsg, err := s.ChannelMessageSend(
			matchmakingChannel,
			fmt.Sprintf("⚔️ **¡RETA ACEPTADA!**\n<@%s> vs <@%s> (FT %d)", p1DiscordID, p2DiscordID, match.TargetFT),
		)
		if err != nil {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "❌ Error al escribir en el canal de matchmaking público.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		}

		// Crear hilo público
		threadName := fmt.Sprintf("🥊 %s vs %s (FT %d)", match.Player1Tag, match.Player2Tag, match.TargetFT)
		thread, err := s.MessageThreadStart(matchmakingChannel, baseMsg.ID, threadName, 1440)
		if err != nil {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "❌ Error al crear el hilo en Discord.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		}

		match.ThreadID = thread.ID
		db.Save(&match)

		// Mensaje inicial dentro del hilo mencionando a los combatientes
		roundContent := fmt.Sprintf("🎮 **¡MATCHMAKING INICIADO!**\n⚔️ <@%s> vs <@%s>\n💰 Puntos en juego: **%d pts**\n\n🔒 *Solo los combatientes pueden interactuar con este panel.*\n\n¿Quién ganó la **Ronda 1**?",
			p1DiscordID, p2DiscordID, match.PointsAtStake)

		keyboard := getRoundKeyboard(match.ID, match.Player1Tag, match.Player2Tag)
		s.ChannelMessageSendComplex(thread.ID, &discordgo.MessageSend{
			Content:    roundContent,
			Components: []discordgo.MessageComponent{keyboard},
		})

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    fmt.Sprintf("✅ ¡Has aceptado la reta! Espacio de combate creado: <#%s>", thread.ID),
				Components: []discordgo.MessageComponent{},
			},
		})

		if p1DiscordID != "" {
			if dm, err := s.UserChannelCreate(p1DiscordID); err == nil {
				s.ChannelMessageSend(dm.ID, fmt.Sprintf("⚡ **%s** ha aceptado tu reta (FT %d). Entra al hilo para jugar: <#%s>", match.Player2Tag, match.TargetFT, thread.ID))
			}
		}

		// ----------------------------------------------------
		// 2. RECHAZAR RETA
		// ----------------------------------------------------
	} else if strings.HasPrefix(customID, "ft_reject_") {
		parts := strings.Split(customID, "_")
		matchID := parts[2]

		var match models.Matchmaking
		db.First(&match, matchID)

		if userHash != match.Player2ID {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{Content: "❌ Solo el jugador retado puede rechazar esta solicitud.", Flags: discordgo.MessageFlagsEphemeral},
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

		// ----------------------------------------------------
		// 3. ANOTAR RONDA (Control Anti-Sabotaje)
		// ----------------------------------------------------
	} else if strings.HasPrefix(customID, "ft_win_") {
		parts := strings.Split(customID, "_")
		matchID := parts[2]
		winnerSide := parts[3]

		var match models.Matchmaking
		db.First(&match, matchID)

		// 🛑 FILTRO ANTI-SABOTAJE: Rechazar interacción si no es uno de los 2 combatientes
		if userHash != match.Player1ID && userHash != match.Player2ID {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "🚫 **Acceso denegado:** Solo los combatientes de este FT pueden anotar o modificar los marcadores.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		}

		if match.Status != "IN_PROGRESS" {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{Content: "⚠️ Este matchmaking no está en curso.", Flags: discordgo.MessageFlagsEphemeral},
			})
			return
		}

		if winnerSide == "p1" {
			match.Score1++
		} else {
			match.Score2++
		}

		maxWins := match.TargetFT

		if match.Score1 >= maxWins || match.Score2 >= maxWins {
			match.Status = "PENDING_AGREE"
			match.P1Confirmed = false
			match.P2Confirmed = false

			if match.Score1 > match.Score2 {
				match.WinnerID = match.Player1ID
				match.LoserID = match.Player2ID
			} else {
				match.WinnerID = match.Player2ID
				match.LoserID = match.Player1ID
			}
			db.Save(&match)

			confirmContent := fmt.Sprintf(
				"🔍 **PREVISUALIZACIÓN DEL RESULTADO (FT %d)**\n\n"+
					"🥊 Marcador: **%s** `%d - %d` **%s**\n"+
					"🏆 Ganador: **%s**\n\n"+
					"⚠️ **Ambos combatientes deben presionar 'Confirmar y Aceptar' para validar los puntos.**",
				match.TargetFT, match.Player1Tag, match.Score1, match.Score2, match.Player2Tag, getWinnerTag(match),
			)

			confirmKeyboard := discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						Label:    "✅ Confirmar y Aceptar",
						Style:    discordgo.SuccessButton,
						CustomID: fmt.Sprintf("ft_agree_%d", match.ID),
					},
					discordgo.Button{
						Label:    "✏️ Corregir / Reiniciar",
						Style:    discordgo.DangerButton,
						CustomID: fmt.Sprintf("ft_reset_%d", match.ID),
					},
				},
			}

			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseUpdateMessage,
				Data: &discordgo.InteractionResponseData{
					Content:    confirmContent,
					Components: []discordgo.MessageComponent{confirmKeyboard},
				},
			})
			return
		}

		db.Save(&match)

		nextRoundContent := fmt.Sprintf("🎮 **MATCHMAKING FT %d** (Meta: %d victorias)\n🥊 **%s** (**%d**) vs (**%d**) **%s**\n\n¿Quién ganó la siguiente ronda?",
			match.TargetFT, maxWins, match.Player1Tag, match.Score1, match.Score2, match.Player2Tag)

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    nextRoundContent,
				Components: []discordgo.MessageComponent{getRoundKeyboard(match.ID, match.Player1Tag, match.Player2Tag)},
			},
		})

		// ----------------------------------------------------
		// 4. REINICIAR / CORREGIR (Control Anti-Sabotaje)
		// ----------------------------------------------------
	} else if strings.HasPrefix(customID, "ft_undo_") || strings.HasPrefix(customID, "ft_reset_") {
		parts := strings.Split(customID, "_")
		matchID := parts[2]

		var match models.Matchmaking
		db.First(&match, matchID)

		// 🛑 FILTRO ANTI-SABOTAJE
		if userHash != match.Player1ID && userHash != match.Player2ID {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "🚫 **Acceso denegado:** Solo los combatientes pueden corregir el marcador.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		}

		match.Score1 = 0
		match.Score2 = 0
		match.P1Confirmed = false
		match.P2Confirmed = false
		match.Status = "IN_PROGRESS"
		db.Save(&match)

		content := fmt.Sprintf("🔄 **MARCADOR REINICIADO / CORREGIDO**\n🥊 **%s** (0) vs (0) **%s**\n\nRegistren las rondas de nuevo. ¿Quién ganó la **Ronda 1**?",
			match.Player1Tag, match.Player2Tag)

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    content,
				Components: []discordgo.MessageComponent{getRoundKeyboard(match.ID, match.Player1Tag, match.Player2Tag)},
			},
		})

		// ----------------------------------------------------
		// 5. CONFIRMAR RESULTADO (Control Anti-Sabotaje + Autoborrado)
		// ----------------------------------------------------
	} else if strings.HasPrefix(customID, "ft_agree_") {
		parts := strings.Split(customID, "_")
		matchID := parts[2]

		var match models.Matchmaking
		db.First(&match, matchID)

		// 🛑 FILTRO ANTI-SABOTAJE
		if userHash != match.Player1ID && userHash != match.Player2ID {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: "🚫 **Acceso denegado:** Solo los combatientes pueden confirmar el resultado.",
					Flags:   discordgo.MessageFlagsEphemeral,
				},
			})
			return
		}

		if match.Status != "PENDING_AGREE" {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{Content: "⚠️ Este combate no está pendiente de confirmación.", Flags: discordgo.MessageFlagsEphemeral},
			})
			return
		}

		if userHash == match.Player1ID {
			match.P1Confirmed = true
		} else if userHash == match.Player2ID {
			match.P2Confirmed = true
		}

		db.Save(&match)

		if !match.P1Confirmed || !match.P2Confirmed {
			pendingUser := match.Player2Tag
			if match.P2Confirmed {
				pendingUser = match.Player1Tag
			}

			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseUpdateMessage,
				Data: &discordgo.InteractionResponseData{
					Content: fmt.Sprintf(
						"⌛ **CONFIRMACIÓN PARCIAL**\nUn combatiente ha confirmado. Esperando a **%s**...\n\n🥊 Marcador: **%s** `%d - %d` **%s**",
						pendingUser, match.Player1Tag, match.Score1, match.Score2, match.Player2Tag,
					),
					Components: []discordgo.MessageComponent{
						discordgo.ActionsRow{
							Components: []discordgo.MessageComponent{
								discordgo.Button{
									Label:    fmt.Sprintf("✅ Confirmar (%s)", pendingUser),
									Style:    discordgo.SuccessButton,
									CustomID: fmt.Sprintf("ft_agree_%d", match.ID),
								},
								discordgo.Button{
									Label:    "✏️ Corregir Marcador",
									Style:    discordgo.DangerButton,
									CustomID: fmt.Sprintf("ft_reset_%d", match.ID),
								},
							},
						},
					},
				},
			})
			return
		}

		// --- AMBOS COMBATIENTES CONFIRMARON ---
		match.Status = "FINISHED"
		db.Save(&match)

		oldRankW := getPlayerRank(db, match.WinnerID)
		oldRankL := getPlayerRank(db, match.LoserID)

		updatePlayerPointsCustom(db, match)

		newRankW := getPlayerRank(db, match.WinnerID)
		newRankL := getPlayerRank(db, match.LoserID)

		sendRankingNotification(s, match, oldRankW, newRankW, oldRankL, newRankL)

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    fmt.Sprintf("🏁 **¡RETA CONFIRMADA Y PROCESADA!**\n\nLos puntos se han asignado correctamente. El reporte final está publicado en <#%s>.\n\n⚠️ **Este hilo se eliminará automáticamente en 5 segundos...**", getMatchmakingResultChannelID()),
				Components: []discordgo.MessageComponent{},
			},
		})

		// Borrado automático del hilo en 5 segundos (Goroutine)
		if match.ThreadID != "" {
			threadID := match.ThreadID
			go func() {
				time.Sleep(5 * time.Second)
				_, err := s.ChannelDelete(threadID)
				if err != nil {
					log.Printf("❌ Error al eliminar el hilo %s: %v", threadID, err)
				}
			}()
		}
	}
}

// AJUSTE: Puntos del ganador completos y penalización de 5 pts al perdedor (mínimo de puntos = 5)
func updatePlayerPointsCustom(db *gorm.DB, match models.Matchmaking) {
	var pWinner, pLoser models.Player
	db.First(&pWinner, "id = ?", match.WinnerID)
	db.First(&pLoser, "id = ?", match.LoserID)

	loserPenalty := 5     // Resta de puntos fija para el perdedor
	minPointsAllowed := 5 // Puntaje mínimo permitido para cualquier jugador

	// 1. Sumar puntos al ganador
	if pWinner.ID != 0 {
		pWinner.Points += match.PointsAtStake
		db.Save(&pWinner)
	}

	// 2. Descontar puntos al perdedor asegurando el límite mínimo de 5 puntos
	if pLoser.ID != 0 {
		pLoser.Points -= loserPenalty

		// Si el resultado cae por debajo del mínimo de 5 puntos, se fija en 5
		if pLoser.Points < minPointsAllowed {
			pLoser.Points = minPointsAllowed
		}
		db.Save(&pLoser)
	}
}

// Teclado con opción de "Corregir Marcador" durante las rondas
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
			discordgo.Button{
				Label:    "⏪ Reiniciar / Corregir",
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("ft_undo_%d", matchID),
			},
		},
	}
}

// sendRankingNotification publica el reporte completo del FT en el canal de ranking
func sendRankingNotification(s *discordgo.Session, match models.Matchmaking, oldRankW, newRankW, oldRankL, newRankL int) {
	winnerTag := getWinnerTag(match)
	loserTag := getLoserTag(match)
	loserPenalty := 5 // Penalización fija para el perdedor

	// Calcular diferencia de posiciones (Ascenso/Descenso)
	winnerDiffStr := formatRankChange(oldRankW, newRankW)
	loserDiffStr := formatRankChange(oldRankL, newRankL)

	embed := &discordgo.MessageEmbed{
		Title:       "📊 REPORTE DE MATCHMAKING & RANKING",
		Color:       0x00FF00, // Verde brillante
		Description: fmt.Sprintf("Se ha completado oficialmente el encuentro **FT %d** entre **%s** y **%s**.", match.TargetFT, match.Player1Tag, match.Player2Tag),
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "🥊 Marcador Final",
				Value:  fmt.Sprintf("**%s** `%d - %d` **%s**", match.Player1Tag, match.Score1, match.Score2, match.Player2Tag),
				Inline: false,
			},
			{
				Name: fmt.Sprintf("🏆 %s (Ganador)", winnerTag),
				Value: fmt.Sprintf(
					"💰 Puntos: **+%d Puntos**\n📈 Ranking: Posición **#%d** → **#%d** (%s)",
					match.PointsAtStake, oldRankW, newRankW, winnerDiffStr,
				),
				Inline: true,
			},
			{
				Name: fmt.Sprintf("💀 %s (Perdedor)", loserTag),
				Value: fmt.Sprintf(
					"📉 Puntos: **-%d Puntos**\n📉 Ranking: Posición **#%d** → **#%d** (%s)",
					loserPenalty, oldRankL, newRankL, loserDiffStr,
				),
				Inline: true,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: "Hot Coffee 2XKO Discord Ranking • Actualización automática",
		},
	}

	s.ChannelMessageSendEmbed(getMatchmakingResultChannelID(), embed)
}

// Auxiliar para formatear de forma clara si un jugador subió, bajó o mantuvo su puesto
func formatRankChange(oldRank, newRank int) string {
	if oldRank > newRank {
		return fmt.Sprintf("▲ Ascendió %d pos.", oldRank-newRank)
	} else if oldRank < newRank {
		return fmt.Sprintf("▼ Descendió %d pos.", newRank-oldRank)
	}
	return "➡️ Se mantiene"
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

func getDiscordIDByHash(db *gorm.DB, hashedID int) string {
	var player models.Player
	if err := db.Where("id = ? AND league = '2xko_online'", hashedID).First(&player).Error; err == nil {
		return player.DiscordID
	}
	return ""
}
