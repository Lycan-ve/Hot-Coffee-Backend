package discord

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"smash-backend/internal/models"

	"github.com/bwmarrin/discordgo"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Estados de una reta.
const (
	statusPending      = "PENDING"       // retó; el rival aún no responde
	statusInProgress   = "IN_PROGRESS"   // aceptada; se anotan rondas
	statusPendingAgree = "PENDING_AGREE" // alcanzó el FT; faltan confirmaciones
	statusFinished     = "FINISHED"      // confirmada por ambos; puntos aplicados
	statusRejected     = "REJECTED"
	statusCancelled    = "CANCELLED"
)

const (
	matchmakingLeague = "2xko_online"

	// Puntos que pierde quien pierde una reta, y piso por debajo del cual no se le quitan más.
	// Perder nunca SUBE los puntos: si alguien tiene menos que el piso, simplemente se queda igual.
	loserPenalty   = 5
	minPointsFloor = 5
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
		switch i.Type {
		case discordgo.InteractionApplicationCommand:
			if i.ApplicationCommandData().Name == "ft" {
				handleFTCommand(s, i, db)
			}
		case discordgo.InteractionMessageComponent:
			handleComponentInteraction(s, i, db)
		}
	})
}

// ---------------------------------------------------------------------------
// Utilidades de interacción
// ---------------------------------------------------------------------------

// userError es un error "para mostrarle al usuario" (mensaje efímero), no un fallo interno.
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

func interactionUser(i *discordgo.InteractionCreate) *discordgo.User {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	return i.User
}

// ackComponent confirma el clic a Discord DE INMEDIATO (antes de tocar la base de datos).
// Discord solo da 3 segundos para responder; si se supera, el usuario ve "no respondió a tiempo"
// aunque el servidor sí haya procesado la acción. Después se edita el mensaje con el resultado.
func ackComponent(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	})
	if err != nil {
		log.Printf("❌ No se pudo confirmar la interacción a Discord: %v\n", err)
		return false
	}
	return true
}

// editMessage reemplaza el contenido y los botones del mensaje donde se hizo clic.
func editMessage(s *discordgo.Session, i *discordgo.InteractionCreate, content string, components []discordgo.MessageComponent) {
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:    &content,
		Components: &components,
	})
	if err != nil {
		log.Printf("❌ No se pudo editar el mensaje de la interacción: %v\n", err)
	}
}

// replyEphemeral envía un mensaje que solo ve quien hizo clic (avisos y errores).
func replyEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: content,
		Flags:   discordgo.MessageFlagsEphemeral,
	})
	if err != nil {
		log.Printf("❌ No se pudo enviar el aviso efímero: %v\n", err)
	}
}

func respondError(s *discordgo.Session, i *discordgo.InteractionCreate, err error) {
	var ue userError
	switch {
	case errors.As(err, &ue):
		replyEphemeral(s, i, ue.msg)
	case errors.Is(err, gorm.ErrRecordNotFound):
		replyEphemeral(s, i, "⚠️ Esta reta ya no existe.")
	default:
		log.Printf("❌ Error interno en matchmaking: %v\n", err)
		replyEphemeral(s, i, "❌ Ocurrió un error interno. Intenta de nuevo en unos segundos.")
	}
}

// withMatch carga la reta con bloqueo de fila (SELECT ... FOR UPDATE), ejecuta fn y guarda los cambios
// en una sola transacción. Así dos clics simultáneos no se pisan (p. ej. las dos confirmaciones).
func withMatch(db *gorm.DB, matchID uint64, fn func(tx *gorm.DB, m *models.Matchmaking) error) (models.Matchmaking, error) {
	var match models.Matchmaking
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&match, matchID).Error; err != nil {
			return err
		}
		if err := fn(tx, &match); err != nil {
			return err
		}
		return tx.Save(&match).Error
	})
	return match, err
}

// roomCodeLine devuelve la línea con el código de la sala (vacía si la reta no tiene código).
// Se incluye en los paneles del hilo para que el rival pueda copiarlo en cualquier momento.
func roomCodeLine(m models.Matchmaking) string {
	if m.RoomCode == "" {
		return ""
	}
	return fmt.Sprintf("🏠 **Código de la sala:** `%s`\n\n", m.RoomCode)
}

// ---------------------------------------------------------------------------
// /ft
// ---------------------------------------------------------------------------

func handleFTCommand(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB) {
	// Respuesta diferida y efímera: el trabajo real (DB + DM) puede tardar más de 3 s.
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})
	if err != nil {
		log.Printf("❌ No se pudo confirmar el comando /ft a Discord: %v\n", err)
		return
	}

	reply := func(content string) {
		if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
			log.Printf("❌ No se pudo responder al comando /ft: %v\n", err)
		}
	}

	author := interactionUser(i)
	data := i.ApplicationCommandData()

	targetFT := 0
	rivalID := ""
	roomCode := ""
	for _, opt := range data.Options {
		switch opt.Name {
		case "objetivo":
			targetFT = int(opt.IntValue())
		case "rival":
			rivalID, _ = opt.Value.(string)
		case "sala":
			roomCode, _ = opt.Value.(string)
		}
	}
	roomCode = strings.TrimSpace(roomCode)

	if author == nil || rivalID == "" {
		reply("❌ Comando inválido.")
		return
	}
	if targetFT != 3 && targetFT != 5 && targetFT != 7 && targetFT != 10 {
		reply("❌ Formato inválido. Usa FT 3, 5, 7 o 10.")
		return
	}

	rival := &discordgo.User{ID: rivalID}
	if data.Resolved != nil {
		if u, ok := data.Resolved.Users[rivalID]; ok && u != nil {
			rival = u
		}
	}
	if rival.Username == "" {
		if u, err := s.User(rivalID); err == nil {
			rival = u
		}
	}

	if author.ID == rival.ID {
		reply("❌ No puedes retarte a ti mismo.")
		return
	}
	if rival.Bot {
		reply("❌ No puedes retar a un bot.")
		return
	}

	match := models.Matchmaking{
		League:           matchmakingLeague,
		Player1DiscordID: author.ID,
		Player1Tag:       author.Username,
		Player1Avatar:    author.AvatarURL("128"),
		Player2DiscordID: rival.ID,
		Player2Tag:       rival.Username,
		Player2Avatar:    rival.AvatarURL("128"),
		TargetFT:         targetFT,
		RoomCode:         roomCode,
		PointsAtStake:    targetFT * 5,
		Status:           statusPending,
	}
	if err := db.Create(&match).Error; err != nil {
		log.Printf("❌ No se pudo crear la reta: %v\n", err)
		reply("❌ No se pudo crear la reta. Intenta de nuevo en unos segundos.")
		return
	}

	content := fmt.Sprintf("🎮 **SOLICITUD DE RETA FT %d**\n⚔️ **%s** te ha retado a un duelo.\n💰 Puntos de Matchmaking en juego: **%d pts**\n\n",
		targetFT, author.Username, match.PointsAtStake)
	if roomCode != "" {
		// El código va en bloque de código para que el rival lo copie con un clic/toque.
		content += fmt.Sprintf("🏠 **Código de la sala:** `%s`\n➡️ *Cópialo y úsalo para entrar a la sala.*\n\n", roomCode)
	}
	content += "¿Aceptas el desafío?"

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

	dm, err := s.UserChannelCreate(rival.ID)
	if err == nil {
		_, err = s.ChannelMessageSendComplex(dm.ID, &discordgo.MessageSend{
			Content:    content,
			Components: []discordgo.MessageComponent{keyboard},
		})
	}
	if err != nil {
		log.Printf("❌ No se pudo enviar el DM de la reta %d: %v\n", match.ID, err)
		db.Model(&match).Update("Status", statusCancelled)
		reply("❌ No se pudo enviar el mensaje privado al rival. Asegúrate de que tenga los DMs abiertos.")
		return
	}

	successMsg := fmt.Sprintf("✅ Invitación enviada al privado de **%s**. Esperando su respuesta...", rival.Username)
	if roomCode != "" {
		successMsg += fmt.Sprintf("\n🏠 Código de sala enviado: `%s`", roomCode)
	}
	reply(successMsg)
}

// ---------------------------------------------------------------------------
// Botones
// ---------------------------------------------------------------------------

func handleComponentInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB) {
	customID := i.MessageComponentData().CustomID
	if !strings.HasPrefix(customID, "ft_") {
		return
	}

	parts := strings.Split(customID, "_")
	if len(parts) < 3 {
		return
	}
	matchID, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return
	}

	// 1) Confirmar a Discord ANTES de cualquier consulta a la base de datos.
	if !ackComponent(s, i) {
		return
	}

	user := interactionUser(i)
	if user == nil {
		return
	}

	switch parts[1] {
	case "accept":
		handleAccept(s, i, db, user, matchID)
	case "reject":
		handleReject(s, i, db, user, matchID)
	case "win":
		side := ""
		if len(parts) > 3 {
			side = parts[3]
		}
		handleWin(s, i, db, user, matchID, side)
	case "undo", "reset":
		handleReset(s, i, db, user, matchID)
	case "agree":
		handleAgree(s, i, db, user, matchID)
	}
}

func isCombatant(userID string, m *models.Matchmaking) bool {
	return userID == m.Player1DiscordID || userID == m.Player2DiscordID
}

// 1. ACEPTAR RETA
func handleAccept(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	// Se "reclama" la reta cambiando el estado dentro de la transacción: un doble clic no crea dos hilos.
	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if user.ID != m.Player2DiscordID {
			return userError{"❌ Solo el jugador retado puede aceptar esta solicitud."}
		}
		if m.Status != statusPending {
			return userError{"⚠️ Esta solicitud ya no está pendiente."}
		}
		m.Status = statusInProgress
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	revert := func(msg string) {
		db.Model(&match).Update("Status", statusPending)
		replyEphemeral(s, i, msg)
	}

	channelID := getMatchmakingChannelID()
	baseContent := fmt.Sprintf("⚔️ **¡RETA ACEPTADA!**\n<@%s> vs <@%s> (FT %d)", match.Player1DiscordID, match.Player2DiscordID, match.TargetFT)
	if match.RoomCode != "" {
		baseContent += fmt.Sprintf("\n🏠 Sala: `%s`", match.RoomCode)
	}
	baseMsg, err := s.ChannelMessageSend(channelID, baseContent)
	if err != nil {
		log.Printf("❌ No se pudo escribir en el canal de matchmaking: %v\n", err)
		revert("❌ Error al escribir en el canal de matchmaking público.")
		return
	}

	threadName := fmt.Sprintf("🥊 %s vs %s (FT %d)", match.Player1Tag, match.Player2Tag, match.TargetFT)
	thread, err := s.MessageThreadStart(channelID, baseMsg.ID, threadName, 1440)
	if err != nil {
		log.Printf("❌ No se pudo crear el hilo: %v\n", err)
		_ = s.ChannelMessageDelete(channelID, baseMsg.ID)
		revert("❌ Error al crear el hilo en Discord.")
		return
	}

	if err := db.Model(&match).Update("ThreadID", thread.ID).Error; err != nil {
		log.Printf("❌ No se pudo guardar el ThreadID de la reta %d: %v — el hilo no podrá cerrarse automáticamente al finalizar.\n", match.ID, err)
	}

	roundContent := fmt.Sprintf("🎮 **¡MATCHMAKING INICIADO!**\n⚔️ <@%s> vs <@%s>\n💰 Puntos de Matchmaking en juego: **%d pts**\n\n%s🔒 *Solo los combatientes pueden interactuar con este panel.*\n\n¿Quién ganó la **Ronda 1**?",
		match.Player1DiscordID, match.Player2DiscordID, match.PointsAtStake, roomCodeLine(match))

	_, err = s.ChannelMessageSendComplex(thread.ID, &discordgo.MessageSend{
		Content:    roundContent,
		Components: []discordgo.MessageComponent{getRoundKeyboard(match.ID, match.Player1Tag, match.Player2Tag)},
	})
	if err != nil {
		log.Printf("❌ No se pudo publicar el panel en el hilo: %v\n", err)
	}

	editMessage(s, i, fmt.Sprintf("✅ ¡Has aceptado la reta! Espacio de combate creado: <#%s>", thread.ID), nil)

	if dm, err := s.UserChannelCreate(match.Player1DiscordID); err == nil {
		dmMsg := fmt.Sprintf("⚡ **%s** ha aceptado tu reta (FT %d). Entra al hilo para jugar: <#%s>", match.Player2Tag, match.TargetFT, thread.ID)
		if match.RoomCode != "" {
			dmMsg += fmt.Sprintf("\n🏠 Código de la sala: `%s`", match.RoomCode)
		}
		_, _ = s.ChannelMessageSend(dm.ID, dmMsg)
	}
}

// 2. RECHAZAR RETA
func handleReject(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	_, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if user.ID != m.Player2DiscordID {
			return userError{"❌ Solo el jugador retado puede rechazar esta solicitud."}
		}
		if m.Status != statusPending {
			return userError{"⚠️ Esta solicitud ya no está pendiente."}
		}
		m.Status = statusRejected
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}
	editMessage(s, i, "❌ Has rechazado la solicitud de reta.", nil)
}

// 3. ANOTAR RONDA
func handleWin(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64, side string) {
	reached := false

	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 **Acceso denegado:** Solo los combatientes de este FT pueden anotar o modificar los marcadores."}
		}
		if m.Status != statusInProgress {
			return userError{"⚠️ Este matchmaking no está en curso."}
		}

		switch side {
		case "p1":
			m.Score1++
		case "p2":
			m.Score2++
		default:
			return userError{"⚠️ Botón no válido."}
		}

		if m.Score1 >= m.TargetFT || m.Score2 >= m.TargetFT {
			reached = true
			m.Status = statusPendingAgree
			m.P1Confirmed = false
			m.P2Confirmed = false
			if m.Score1 > m.Score2 {
				m.WinnerDiscordID, m.LoserDiscordID = m.Player1DiscordID, m.Player2DiscordID
			} else {
				m.WinnerDiscordID, m.LoserDiscordID = m.Player2DiscordID, m.Player1DiscordID
			}
		}
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	if reached {
		confirmContent := fmt.Sprintf(
			"🔍 **PREVISUALIZACIÓN DEL RESULTADO (FT %d)**\n\n"+
				"🥊 Marcador: **%s** `%d - %d` **%s**\n"+
				"🏆 Ganador: **%s**\n\n"+
				"⚠️ **Ambos combatientes deben presionar 'Confirmar y Aceptar' para validar los puntos.**",
			match.TargetFT, match.Player1Tag, match.Score1, match.Score2, match.Player2Tag, getWinnerTag(match),
		)
		keyboard := discordgo.ActionsRow{
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
		editMessage(s, i, confirmContent, []discordgo.MessageComponent{keyboard})
		return
	}

	nextRound := fmt.Sprintf("🎮 **MATCHMAKING FT %d** (Meta: %d victorias)\n🥊 **%s** (**%d**) vs (**%d**) **%s**\n\n%s¿Quién gana la siguiente ronda?",
		match.TargetFT, match.TargetFT, match.Player1Tag, match.Score1, match.Score2, match.Player2Tag, roomCodeLine(match))
	editMessage(s, i, nextRound, []discordgo.MessageComponent{getRoundKeyboard(match.ID, match.Player1Tag, match.Player2Tag)})
}

// 4. REINICIAR / CORREGIR
func handleReset(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 **Acceso denegado:** Solo los combatientes pueden corregir el marcador."}
		}
		// Una reta ya FINALIZADA no se puede reiniciar: evitaría duplicar los puntos.
		if m.Status != statusInProgress && m.Status != statusPendingAgree {
			return userError{"⚠️ Este combate ya no se puede corregir."}
		}
		m.Score1, m.Score2 = 0, 0
		m.P1Confirmed, m.P2Confirmed = false, false
		m.WinnerDiscordID, m.LoserDiscordID = "", ""
		m.Status = statusInProgress
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	content := fmt.Sprintf("🔄 **MARCADOR REINICIADO / CORREGIDO**\n🥊 **%s** (0) vs (0) **%s**\n\n%sRegistren las rondas de nuevo. ¿Quién ganó la **Ronda 1**?",
		match.Player1Tag, match.Player2Tag, roomCodeLine(match))
	editMessage(s, i, content, []discordgo.MessageComponent{getRoundKeyboard(match.ID, match.Player1Tag, match.Player2Tag)})
}

// 5. CONFIRMAR RESULTADO
func handleAgree(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	var report *matchReport

	match, err := withMatch(db, matchID, func(tx *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 **Acceso denegado:** Solo los combatientes pueden confirmar el resultado."}
		}
		if m.Status == statusFinished {
			return userError{"⚠️ Este combate ya fue procesado y confirmado con anterioridad."}
		}
		if m.Status != statusPendingAgree {
			return userError{"⚠️ Este combate no está pendiente de confirmación."}
		}

		if user.ID == m.Player1DiscordID {
			if m.P1Confirmed {
				return userError{fmt.Sprintf("✅ Ya confirmaste este resultado. Falta que confirme **%s**.", m.Player2Tag)}
			}
			m.P1Confirmed = true
		} else {
			if m.P2Confirmed {
				return userError{fmt.Sprintf("✅ Ya confirmaste este resultado. Falta que confirme **%s**.", m.Player1Tag)}
			}
			m.P2Confirmed = true
		}

		if !m.P1Confirmed || !m.P2Confirmed {
			return nil // confirmación parcial
		}

		// Ambos confirmaron: se marca FINISHED y se aplican los puntos en la MISMA transacción.
		m.Status = statusFinished
		r, err := applyMatchResult(tx, m)
		if err != nil {
			return err
		}
		report = &r
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	// Falta la confirmación del otro combatiente.
	if report == nil {
		pendingUser := match.Player2Tag
		if match.P2Confirmed {
			pendingUser = match.Player1Tag
		}
		keyboard := discordgo.ActionsRow{
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
		}
		editMessage(s, i, fmt.Sprintf(
			"⌛ **CONFIRMACIÓN PARCIAL**\nUn combatiente ha confirmado. Esperando a **%s**...\n\n🥊 Marcador: **%s** `%d - %d` **%s**",
			pendingUser, match.Player1Tag, match.Score1, match.Score2, match.Player2Tag,
		), []discordgo.MessageComponent{keyboard})
		return
	}

	editMessage(s, i, fmt.Sprintf(
		"🏁 **¡RETA CONFIRMADA Y PROCESADA!**\n\nLos puntos del ranking de matchmaking se han asignado correctamente. El reporte final está publicado en <#%s>.\n\n⚠️ **Este hilo se eliminará automáticamente en 5 segundos...**",
		getMatchmakingResultChannelID(),
	), nil)

	// Reporte y limpieza del hilo en segundo plano (ya no hay ninguna interacción esperando).
	r := *report
	go func() {
		sendMatchmakingReport(s, r)
		closeMatchThread(s, r.Match)
	}()
}

// closeMatchThread cierra el hilo de una reta terminada, tras la demora anunciada al usuario.
// Estrategia en cascada:
//  1. Borra el hilo (requiere permiso "Gestionar hilos" / MANAGE_THREADS).
//  2. Si falla (p. ej. 50013 Missing Permissions), archiva el hilo como fallback:
//     el creador del hilo (el bot) puede archivarlo aunque no tenga ese permiso.
func closeMatchThread(s *discordgo.Session, m models.Matchmaking) {
	if m.ThreadID == "" {
		log.Printf("⚠️ Reta %d sin ThreadID guardado: no se puede cerrar el hilo automáticamente.\n", m.ID)
		return
	}

	time.Sleep(5 * time.Second)

	if _, err := s.ChannelDelete(m.ThreadID); err != nil {
		log.Printf("⚠️ No se pudo borrar el hilo %s: %v — se intenta archivar como fallback.\n", m.ThreadID, err)
		archiveFallback(s, m.ThreadID)
		return
	}
	log.Printf("🗑️ Hilo %s (reta %d) eliminado automáticamente.\n", m.ThreadID, m.ID)
}

// archiveFallback archiva (y bloquea, si el permiso lo permite) un hilo que no se pudo borrar.
func archiveFallback(s *discordgo.Session, threadID string) {
	archived, locked := true, true

	// Intento 1: archivar Y bloquear en la misma petición.
	if _, err := s.ChannelEditComplex(threadID, &discordgo.ChannelEdit{
		Archived: &archived,
		Locked:   &locked,
	}); err == nil {
		log.Printf("🔒 Hilo %s archivado y bloqueado (fallback de borrado).\n", threadID)
		return
	}

	// Intento 2: solo archivar — el creador del hilo puede archivar sin MANAGE_THREADS.
	if _, err := s.ChannelEditComplex(threadID, &discordgo.ChannelEdit{Archived: &archived}); err != nil {
		log.Printf("❌ No se pudo cerrar el hilo %s: %v (revisa el permiso 'Gestionar hilos' del bot).\n", threadID, err)
		return
	}
	log.Printf("🗂️ Hilo %s archivado (el bloqueo requiere 'Gestionar hilos').\n", threadID)
}

// ---------------------------------------------------------------------------
// Ranking de MATCHMAKING (tabla matchmaking_players, separada de los torneos)
// ---------------------------------------------------------------------------

type matchReport struct {
	Match                     models.Matchmaking
	WinnerTag, LoserTag       string
	WinnerDelta, LoserDelta   int // cambio real de puntos (+x / -y / 0)
	WinnerPoints, LoserPoints int // total nuevo
	OldRankW, NewRankW        int
	OldRankL, NewRankL        int
}

// matchmakingRank devuelve la posición (empates comparten posición) de un jugador con esos puntos.
func matchmakingRank(tx *gorm.DB, points int) int {
	var higher int64
	tx.Model(&models.MatchmakingPlayer{}).Where("points > ?", points).Count(&higher)
	return int(higher) + 1
}

func sideInfo(m *models.Matchmaking, discordID string) (tag, avatar string) {
	if discordID == m.Player1DiscordID {
		return m.Player1Tag, m.Player1Avatar
	}
	return m.Player2Tag, m.Player2Avatar
}

// applyMatchResult suma los puntos al ganador y resta al perdedor SOLO en matchmaking_players.
// Debe llamarse dentro de la transacción que marca la reta como FINISHED.
func applyMatchResult(tx *gorm.DB, m *models.Matchmaking) (matchReport, error) {
	report := matchReport{Match: *m}

	// Garantiza que ambos existan (se crean la primera vez que juegan una reta confirmada).
	for _, id := range []string{m.Player1DiscordID, m.Player2DiscordID} {
		tag, avatar := sideInfo(m, id)
		p := models.MatchmakingPlayer{DiscordID: id, Username: tag, AvatarURL: avatar}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&p).Error; err != nil {
			return report, err
		}
	}

	// Bloquea ambas filas, siempre en el mismo orden, para evitar bloqueos cruzados entre retas.
	var rows []models.MatchmakingPlayer
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("discord_id IN ?", []string{m.WinnerDiscordID, m.LoserDiscordID}).
		Order("discord_id").
		Find(&rows).Error
	if err != nil {
		return report, err
	}

	byID := make(map[string]*models.MatchmakingPlayer, len(rows))
	for idx := range rows {
		byID[rows[idx].DiscordID] = &rows[idx]
	}
	winner, loser := byID[m.WinnerDiscordID], byID[m.LoserDiscordID]
	if winner == nil || loser == nil {
		return report, errors.New("no se encontraron los jugadores de matchmaking de esta reta")
	}

	report.OldRankW = matchmakingRank(tx, winner.Points)
	report.OldRankL = matchmakingRank(tx, loser.Points)

	oldWinnerPts, oldLoserPts := winner.Points, loser.Points

	winner.Points += m.PointsAtStake
	winner.Wins++

	loserNew := loser.Points - loserPenalty
	if loserNew < minPointsFloor {
		loserNew = minPointsFloor
	}
	if loserNew > loser.Points { // perder nunca sube los puntos
		loserNew = loser.Points
	}
	loser.Points = loserNew
	loser.Losses++

	// Mantiene actualizados nombre y avatar de Discord.
	if tag, avatar := sideInfo(m, winner.DiscordID); tag != "" {
		winner.Username = tag
		if avatar != "" {
			winner.AvatarURL = avatar
		}
	}
	if tag, avatar := sideInfo(m, loser.DiscordID); tag != "" {
		loser.Username = tag
		if avatar != "" {
			loser.AvatarURL = avatar
		}
	}

	if err := tx.Save(winner).Error; err != nil {
		return report, err
	}
	if err := tx.Save(loser).Error; err != nil {
		return report, err
	}

	report.NewRankW = matchmakingRank(tx, winner.Points)
	report.NewRankL = matchmakingRank(tx, loser.Points)
	report.WinnerTag, report.LoserTag = winner.Username, loser.Username
	report.WinnerPoints, report.LoserPoints = winner.Points, loser.Points
	report.WinnerDelta = winner.Points - oldWinnerPts
	report.LoserDelta = loser.Points - oldLoserPts
	return report, nil
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
			discordgo.Button{
				Label:    "⏪ Reiniciar / Corregir",
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("ft_undo_%d", matchID),
			},
		},
	}
}

func sendMatchmakingReport(s *discordgo.Session, r matchReport) {
	m := r.Match

	embed := &discordgo.MessageEmbed{
		Title:       "📊 REPORTE DE MATCHMAKING",
		Color:       0x00FF00,
		Description: fmt.Sprintf("Se ha completado oficialmente la reta **FT %d** entre **%s** y **%s**.", m.TargetFT, m.Player1Tag, m.Player2Tag),
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "🥊 Marcador Final",
				Value:  fmt.Sprintf("**%s** `%d - %d` **%s**", m.Player1Tag, m.Score1, m.Score2, m.Player2Tag),
				Inline: false,
			},
			{
				Name: fmt.Sprintf("🏆 %s (Ganador)", r.WinnerTag),
				Value: fmt.Sprintf("💰 **%s pts** (total: **%d**)\n📈 Posición: **#%d** → **#%d** (%s)",
					signed(r.WinnerDelta), r.WinnerPoints, r.OldRankW, r.NewRankW, formatRankChange(r.OldRankW, r.NewRankW)),
				Inline: true,
			},
			{
				Name: fmt.Sprintf("💀 %s (Perdedor)", r.LoserTag),
				Value: fmt.Sprintf("📉 **%s pts** (total: **%d**)\n📉 Posición: **#%d** → **#%d** (%s)",
					signed(r.LoserDelta), r.LoserPoints, r.OldRankL, r.NewRankL, formatRankChange(r.OldRankL, r.NewRankL)),
				Inline: true,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: "Hot Coffee 2XKO • Ranking de Matchmaking",
		},
	}

	if _, err := s.ChannelMessageSendEmbed(getMatchmakingResultChannelID(), embed); err != nil {
		log.Printf("❌ No se pudo publicar el reporte de matchmaking: %v\n", err)
	}
}

func signed(n int) string {
	if n == 0 {
		return "±0"
	}
	return fmt.Sprintf("%+d", n)
}

func formatRankChange(oldRank, newRank int) string {
	if oldRank > newRank {
		return fmt.Sprintf("▲ Ascendió %d pos.", oldRank-newRank)
	} else if oldRank < newRank {
		return fmt.Sprintf("▼ Descendió %d pos.", newRank-oldRank)
	}
	return "➡️ Se mantiene"
}

func getWinnerTag(m models.Matchmaking) string {
	if m.WinnerDiscordID == m.Player1DiscordID {
		return m.Player1Tag
	}
	return m.Player2Tag
}
