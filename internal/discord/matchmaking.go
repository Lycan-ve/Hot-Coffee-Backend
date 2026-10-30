package discord

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"smash-backend/internal/gamechars"
	"smash-backend/internal/models"

	"github.com/bwmarrin/discordgo"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Estados de una reta.
const (
	statusPending       = "PENDING"        // retó; el rival aún no responde
	statusInProgress    = "IN_PROGRESS"    // aceptada; se arma equipo y se juega
	statusPendingVerify = "PENDING_VERIFY" // game reportado; espera Verify Result Submission
	statusPendingClose  = "PENDING_CLOSE"  // propuesta de "Cerrar set" activa; games bloqueados
	statusFinished      = "FINISHED"       // resultado aplicado; puntos asignados
	statusRejected      = "REJECTED"
	statusCancelled     = "CANCELLED"
)

const (
	matchmakingLeague = "2xko_online"

	// Puntos que pierde quien pierde una reta, y piso por debajo del cual no se le quitan más.
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

// editMessage reemplaza el contenido y los componentes del mensaje donde se hizo clic.
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

// iconFile lee un PNG de Personajes/ o Fusion/ y lo devuelve como adjunto de Discord.
// kind: "char" (personaje) o "fuse" (fusión).
func iconFile(kind, id string) *discordgo.File {
	if id == "" {
		return nil
	}
	var path, name string
	if kind == "fuse" {
		if !gamechars.ValidFuse(id) {
			return nil
		}
		path, name = gamechars.FuseIcon(id), fmt.Sprintf("fuse_%s.png", id)
	} else {
		if !gamechars.ValidCharacter(id) {
			return nil
		}
		path, name = gamechars.CharacterIcon(id), fmt.Sprintf("char_%s.png", id)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("⚠️ No se pudo leer el icono %s: %v\n", path, err)
		return nil
	}
	return &discordgo.File{Name: name, Reader: bytes.NewReader(data)}
}

// teamPanelFiles: los PNG de los picks actuales (Personaje 1, Personaje 2 y Fusión).
func teamPanelFiles(m models.Matchmaking, side string) []*discordgo.File {
	c1, c2, fuse := m.P1Char1, m.P1Char2, m.P1Fuse
	if side == "p2" {
		c1, c2, fuse = m.P2Char1, m.P2Char2, m.P2Fuse
	}
	files := make([]*discordgo.File, 0, 3)
	for _, f := range []*discordgo.File{iconFile("char", c1), iconFile("char", c2), iconFile("fuse", fuse)} {
		if f != nil {
			files = append(files, f)
		}
	}
	return files
}

// editMessageFiles edita el mensaje de la interacción adjuntando archivos.
// El slice vacío de Attachments REEMPLAZA las imágenes anteriores (sin acumular).
func editMessageFiles(s *discordgo.Session, i *discordgo.InteractionCreate, content string, components []discordgo.MessageComponent, files []*discordgo.File) {
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	if files == nil {
		files = []*discordgo.File{}
	}
	empty := []*discordgo.MessageAttachment{}
	_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:     &content,
		Components:  &components,
		Files:       files,
		Attachments: &empty,
	})
	if err != nil {
		log.Printf("❌ No se pudo editar el mensaje de la interacción: %v\n", err)
	}
}

// editThreadMessageFiles edita un mensaje fijo del hilo con adjuntos (fuera de una interacción).
func editThreadMessageFiles(s *discordgo.Session, threadID, messageID, content string, components []discordgo.MessageComponent, files []*discordgo.File) {
	if messageID == "" || threadID == "" {
		return
	}
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	if files == nil {
		files = []*discordgo.File{}
	}
	empty := []*discordgo.MessageAttachment{}
	if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:     threadID,
		ID:          messageID,
		Content:     &content,
		Components:  &components,
		Files:       files,
		Attachments: &empty,
	}); err != nil {
		log.Printf("❌ No se pudo editar el mensaje %s del hilo %s: %v\n", messageID, threadID, err)
	}
}

// replyEphemeral envía un mensaje que solo ve quien hizo clic.
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

// withMatch carga la reta con bloqueo de fila (SELECT ... FOR UPDATE), ejecuta fn y
// guarda los cambios en una sola transacción.
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

// editThreadMessage edita un mensaje fijo del hilo fuera de una interacción.
func editThreadMessage(s *discordgo.Session, threadID, messageID, content string, components []discordgo.MessageComponent) {
	if messageID == "" || threadID == "" {
		return
	}
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    threadID,
		ID:         messageID,
		Content:    &content,
		Components: &components,
	}); err != nil {
		log.Printf("❌ No se pudo editar el mensaje %s del hilo %s: %v\n", messageID, threadID, err)
	}
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

	content := fmt.Sprintf("🎮 **SOLICITUD DE RETA FT %d**\n⚔️ **%s** te ha retado a un duelo.\n💰 Puntos de Matchmaking en juego: **%d pts**\n%s\n¿Aceptas el desafío?",
		targetFT, author.Username, match.PointsAtStake, roomCodeLine(&match))

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

	success := fmt.Sprintf("✅ Invitación enviada al privado de **%s**. Esperando su respuesta...", rival.Username)
	if roomCode != "" {
		success += fmt.Sprintf("\n🏠 Código de sala enviado: `%s`", roomCode)
	}
	reply(success)

}

// ---------------------------------------------------------------------------
// Botones y menús (routing)
// ---------------------------------------------------------------------------

func handleComponentInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB) {
	data := i.MessageComponentData()
	customID := data.CustomID
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
	// Menús de equipo: personaje 1, personaje 2 y fusión (un menú por campo)
	case "c1", "c2", "fuse":
		side := ""
		if len(parts) > 3 {
			side = parts[3]
		}
		handleTeamSelect(s, i, db, user, matchID, parts[1], side, data.Values)
	// Reporte del game (W) → verificación
	case "win":
		side := ""
		if len(parts) > 3 {
			side = parts[3]
		}
		handleReportGame(s, i, db, user, matchID, side)
	case "verify":
		handleVerifyGame(s, i, db, user, matchID)
	case "correct":
		handleCorrectReport(s, i, db, user, matchID)
	case "undo", "reset":
		handleReset(s, i, db, user, matchID)
	case "close":
		handleCloseSet(s, i, db, user, matchID)
	case "acceptclose":
		handleAcceptCloseSet(s, i, db, user, matchID)
	case "declineclose":
		handleDeclineCloseSet(s, i, db, user, matchID)
	}
}

func isCombatant(userID string, m *models.Matchmaking) bool {
	return userID == m.Player1DiscordID || userID == m.Player2DiscordID
}

// isMod dice si quien interactúa es Admin o Moderador del servidor.
func isMod(i *discordgo.InteractionCreate) bool {
	return i.Member != nil && i.Member.Permissions&(discordgo.PermissionAdministrator|discordgo.PermissionManageServer) != 0
}

// ---------------------------------------------------------------------------
// 1. ACEPTAR RETA
// ---------------------------------------------------------------------------

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
		db.Model(&models.Matchmaking{}).Where("id = ?", match.ID).Update("Status", statusPending)
		replyEphemeral(s, i, msg)
	}

	channelID := getMatchmakingChannelID()
	baseMsg, err := s.ChannelMessageSend(channelID,
		fmt.Sprintf("⚔️ **¡RETA ACEPTADA!**\n<@%s> vs <@%s> (FT %d)", match.Player1DiscordID, match.Player2DiscordID, match.TargetFT))
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

	// Paneles del hilo: EQUIPO por jugador (Personaje 1, Personaje 2, Fusión)
	// + panel de games que se activa cuando ambos equipos estén completos.
	teamP1, err1 := s.ChannelMessageSendComplex(thread.ID, &discordgo.MessageSend{
		Content:    teamPanelContent(match, "p1"),
		Components: getPlayerTeamKeyboard(match, "p1"),
	})
	teamP2, err2 := s.ChannelMessageSendComplex(thread.ID, &discordgo.MessageSend{
		Content:    teamPanelContent(match, "p2"),
		Components: getPlayerTeamKeyboard(match, "p2"),
	})
	gamePanel, err3 := s.ChannelMessageSendComplex(thread.ID, &discordgo.MessageSend{
		Content: fmt.Sprintf("⚔️ <@%s> vs <@%s> (FT %d · 💰 **%d pts**)\n"+
			roomCodeLine(&match)+
			"\n⏳ Armen sus equipos en los paneles de arriba: **Personaje 1 + Personaje 2 + Fusión**.\n"+
			"El **REPORT GAME 1** aparece aquí cuando ambos equipos estén completos.",
			match.Player1DiscordID, match.Player2DiscordID, match.TargetFT, match.PointsAtStake),
	})
	if err1 != nil {
		log.Printf("❌ No se pudo publicar el panel de equipo de %s: %v\n", match.Player1Tag, err1)
	}
	if err2 != nil {
		log.Printf("❌ No se pudo publicar el panel de equipo de %s: %v\n", match.Player2Tag, err2)
	}
	if err3 != nil {
		log.Printf("❌ No se pudo publicar el panel de games: %v\n", err3)
	}

	now := time.Now()
	updates := map[string]interface{}{"ThreadID": thread.ID, "LastRoundAt": &now}
	if err1 == nil {
		updates["TeamPanelMessageID1"] = teamP1.ID
	}
	if err2 == nil {
		updates["TeamPanelMessageID2"] = teamP2.ID
	}
	if err3 == nil {
		updates["PanelMessageID"] = gamePanel.ID
	}
	if err := db.Model(&models.Matchmaking{}).Where("id = ?", match.ID).Updates(updates).Error; err != nil {
		log.Printf("❌ No se pudieron guardar los paneles de la reta %d: %v\n", match.ID, err)
	}

	editMessage(s, i, fmt.Sprintf("✅ ¡Has aceptado la reta! Espacio de combate creado: <#%s>", thread.ID), nil)

	if dm, err := s.UserChannelCreate(match.Player1DiscordID); err == nil {
		_, _ = s.ChannelMessageSend(dm.ID, fmt.Sprintf("⚡ **%s** ha aceptado tu reta (FT %d). Entra al hilo para armar tu equipo: <#%s>", match.Player2Tag, match.TargetFT, thread.ID))
	}
}

// ---------------------------------------------------------------------------
// 2. RECHAZAR RETA
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// 3. SELECCIÓN DE EQUIPO (menús: personaje 1, personaje 2, fusión)
// ---------------------------------------------------------------------------

func handleTeamSelect(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64, kind, side string, values []string) {
	if side != "p1" && side != "p2" {
		return
	}
	if len(values) != 1 {
		replyEphemeral(s, i, "⚠️ Selección no válida.")
		return
	}
	v := values[0]
	if kind == "fuse" && !gamechars.ValidFuse(v) {
		replyEphemeral(s, i, "⚠️ Fusión no válida.")
		return
	}
	if kind != "fuse" && !gamechars.ValidCharacter(v) {
		replyEphemeral(s, i, "⚠️ Personaje no válido.")
		return
	}

	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if side == "p1" && user.ID != m.Player1DiscordID {
			return userError{fmt.Sprintf("🚫 Solo **%s** puede elegir su equipo.", m.Player1Tag)}
		}
		if side == "p2" && user.ID != m.Player2DiscordID {
			return userError{fmt.Sprintf("🚫 Solo **%s** puede elegir su equipo.", m.Player2Tag)}
		}
		if m.Status != statusInProgress {
			return userError{"⚠️ El set ya no está en curso."}
		}

		// Equipo ya confirmado: los menús quedaron ocultos; se ignoran clics de menús viejos.
		if (side == "p1" && m.P1TeamComplete()) || (side == "p2" && m.P2TeamComplete()) {
			return userError{"⚠️ Tu equipo ya está confirmado: la selección de personajes y fusión está cerrada."}
		}

		// Personaje 1 y Personaje 2 deben ser distintos.
		if side == "p1" && ((kind == "c1" && v == m.P1Char2) || (kind == "c2" && v == m.P1Char1)) {
			return userError{"⚠️ Ese ya es tu otro personaje: el **Personaje 1** y el **Personaje 2** deben ser distintos."}
		}
		if side == "p2" && ((kind == "c1" && v == m.P2Char2) || (kind == "c2" && v == m.P2Char1)) {
			return userError{"⚠️ Ese ya es tu otro personaje: el **Personaje 1** y el **Personaje 2** deben ser distintos."}
		}

		switch {
		case kind == "c1" && side == "p1":
			m.P1Char1 = v
		case kind == "c2" && side == "p1":
			m.P1Char2 = v
		case kind == "fuse" && side == "p1":
			m.P1Fuse = v
		case kind == "c1" && side == "p2":
			m.P2Char1 = v
		case kind == "c2" && side == "p2":
			m.P2Char2 = v
		case kind == "fuse" && side == "p2":
			m.P2Fuse = v
		}
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	// ¿El equipo de quien eligió quedó completo (2 personajes + fusión)?
	teamComplete := (side == "p1" && match.P1TeamComplete()) || (side == "p2" && match.P2TeamComplete())

	// Equipo completo → menús ocultos + imágenes; si falta algo, menús visibles + imágenes.
	if teamComplete {
		editMessageFiles(s, i, teamPanelContent(match, side), nil, teamPanelFiles(match, side))
	} else {
		editMessageFiles(s, i, teamPanelContent(match, side), getPlayerTeamKeyboard(match, side), teamPanelFiles(match, side))
	}

	// Ambos equipos completos → activa el panel de games (REPORT GAME 1).
	if match.P1TeamComplete() && match.P2TeamComplete() {
		editThreadMessage(s, match.ThreadID, match.PanelMessageID, gamePanelContent(match),
			[]discordgo.MessageComponent{getGameKeyboard(match.ID, match.Player1Tag, match.Player2Tag)})
	}
}

// ---------------------------------------------------------------------------
// 4. REPORTAR EL GAME (W) → RESULT VERIFICATION
// ---------------------------------------------------------------------------

func handleReportGame(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64, side string) {
	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 **Acceso denegado:** Solo los combatientes de este FT pueden reportar."}
		}
		if m.Status == statusPendingClose {
			return userError{"⚠️ Hay una **propuesta de cierre** en curso: no se pueden reportar games."}
		}
		if m.Status == statusPendingVerify {
			return userError{"⚠️ Hay un game esperando verificación: verifícalo o corrígelo primero."}
		}
		if m.Status != statusInProgress {
			return userError{"⚠️ Este set no está en curso."}
		}
		if !m.P1TeamComplete() || !m.P2TeamComplete() {
			return userError{"⚠️ Ambos equipos deben estar completos (Personaje 1 + Personaje 2 + Fusión) antes de reportar games."}
		}
		switch side {
		case "p1":
			m.PendingGameWinner = 1
		case "p2":
			m.PendingGameWinner = 2
		default:
			return userError{"⚠️ Botón no válido."}
		}
		m.PendingReportedBy = user.ID
		m.Status = statusPendingVerify
		now := time.Now()
		m.LastRoundAt = &now // actividad: reinicia el reloj de inactividad (2 h)
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}
	editMessage(s, i, verificationPanelContent(match), []discordgo.MessageComponent{getVerifyKeyboard(match.ID)})
}

// ---------------------------------------------------------------------------
// 5. VERIFICAR EL GAME (Verify Result Submission): cualquiera de los dos.
// ---------------------------------------------------------------------------

func handleVerifyGame(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	var report *matchReport

	match, err := withMatch(db, matchID, func(tx *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 Solo los combatientes pueden verificar el reporte."}
		}
		if m.Status != statusPendingVerify {
			return userError{"⚠️ No hay ningún game esperando verificación."}
		}
		// Quien reportó el game SOLO espera: la confirmación es del otro jugador.
		if m.PendingReportedBy != "" && user.ID == m.PendingReportedBy {
			return userError{fmt.Sprintf("⏳ Tú ya reportaste este game: espera la confirmación de **%s**.", pendingConfirmerTag(m))}
		}

		// El game queda REGISTRADO: historial + marcador.
		appendRound(m, fmt.Sprintf("p%d", m.PendingGameWinner))
		if m.PendingGameWinner == 1 {
			m.Score1++
		} else {
			m.Score2++
		}
		m.PendingGameWinner = 0
		m.PendingReportedBy = ""
		now := time.Now()
		m.LastRoundAt = &now

		// ¿Llegó al FT? SIN doble confirmación de puntos: se aplica directo.
		if m.Score1 >= m.TargetFT || m.Score2 >= m.TargetFT {
			if m.Score1 > m.Score2 {
				m.WinnerDiscordID, m.LoserDiscordID = m.Player1DiscordID, m.Player2DiscordID
			} else {
				m.WinnerDiscordID, m.LoserDiscordID = m.Player2DiscordID, m.Player1DiscordID
			}
			m.Status = statusFinished
			r, err := applyMatchResult(tx, m)
			if err != nil {
				return err
			}
			report = &r
		} else {
			m.Status = statusInProgress
		}
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	// FIN del set: puntos aplicados → reporte → hilo borrado (con enlace a resultados).
	if report != nil {
		r := *report
		editMessage(s, i, fmt.Sprintf(
			"🏁 **SET COMPLETADO — RESULTADO APLICADO** (FT %d)\n\n"+
				historyBlock(&r.Match)+
				"🏆 Ganador: **%s** (+%s pts, total **%d**)\n"+
				"El reporte final está publicado en <#%s>.\n"+
				"📍 Consulta el historial de retas en <#%s>.\n\n"+
				"⚠️ **Este hilo se eliminará automáticamente en 5 segundos...**",
			r.Match.TargetFT, r.WinnerTag, signed(r.WinnerDelta), r.WinnerPoints,
			getMatchmakingResultChannelID(), getMatchmakingResultChannelID()), nil)
		go func() {
			sendMatchmakingReport(s, r)
			closeMatchThread(s, r.Match)
		}()
		return
	}

	// Game verificado → panel del siguiente game.
	editMessage(s, i, gamePanelContent(match), []discordgo.MessageComponent{getGameKeyboard(match.ID, match.Player1Tag, match.Player2Tag)})
}

// ---------------------------------------------------------------------------
// 6. CORREGIR EL REPORTE: re-anota el game actual sin castigo.
// ---------------------------------------------------------------------------

func handleCorrectReport(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 Solo los combatientes pueden corregir el reporte."}
		}
		if m.Status != statusPendingVerify {
			return userError{"⚠️ No hay ningún reporte pendiente de verificación."}
		}
		// Quien reportó el game SOLO espera: la confirmación es del otro jugador.
		if m.PendingReportedBy != "" && user.ID == m.PendingReportedBy {
			return userError{fmt.Sprintf("⏳ Tú ya reportaste este game: espera la confirmación de **%s**.", pendingConfirmerTag(m))}
		}
		m.Status = statusInProgress
		m.PendingGameWinner = 0
		m.PendingReportedBy = ""
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}
	editMessage(s, i, "✏️ **Reporte corregido:** re-anota el game actual.\n\n"+gamePanelContent(match),
		[]discordgo.MessageComponent{getGameKeyboard(match.ID, match.Player1Tag, match.Player2Tag)})
}

// ---------------------------------------------------------------------------
// 7. REINICIAR / CORREGIR TODO: reinicia el set y vuelve a armar equipos.
// ---------------------------------------------------------------------------

func handleReset(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 **Acceso denegado:** Solo los combatientes pueden reiniciar el set."}
		}
		// Un set FINALIZADO no se puede reiniciar: duplicaría los puntos.
		if m.Status != statusInProgress && m.Status != statusPendingVerify && m.Status != statusPendingClose {
			return userError{"⚠️ Este set ya no se puede reiniciar."}
		}
		m.Score1, m.Score2 = 0, 0
		m.RoundHistory = "[]"
		m.PendingGameWinner = 0
		m.PendingReportedBy = ""
		m.WinnerDiscordID, m.LoserDiscordID = "", ""
		m.Status = statusInProgress
		m.CloseRequestedAt = nil
		m.CloseRequestedBy = ""
		// Se vuelve a armar los equipos desde cero.
		m.P1Char1, m.P1Char2, m.P1Fuse = "", "", ""
		m.P2Char1, m.P2Char2, m.P2Fuse = "", "", ""
		now := time.Now()
		m.LastRoundAt = &now
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	// Vuelve a armar equipos: se restauran los menús de ambos paneles de equipo
	// y el panel de games queda esperando de nuevo.
	editMessage(s, i, "🔄 **SET REINICIADO:** armen de nuevo sus equipos en sus paneles. El **GAME 1** aparecerá aquí cuando ambos estén completos.", nil)
	editThreadMessageFiles(s, match.ThreadID, match.TeamPanelMessageID1, teamPanelContent(match, "p1"), getPlayerTeamKeyboard(match, "p1"), nil)
	editThreadMessageFiles(s, match.ThreadID, match.TeamPanelMessageID2, teamPanelContent(match, "p2"), getPlayerTeamKeyboard(match, "p2"), nil)
}

// ---------------------------------------------------------------------------
// 8. PROPONER CIERRE DE SET (🏳️ Cerrar set) — el caso "FT10 que quedó 4-2".
// ---------------------------------------------------------------------------

func handleCloseSet(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	mod := isMod(i)

	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if !mod && !isCombatant(user.ID, m) {
			return userError{"🚫 **Acceso denegado:** Solo los combatientes (o un Admin/Moderador) pueden proponer cerrar el set."}
		}
		if m.Status == statusPendingClose {
			return userError{"⚠️ Ya hay una propuesta de cierre en curso."}
		}
		if m.Status == statusPendingVerify {
			return userError{"⚠️ Hay un game esperando verificación: verifícalo o corrígelo antes de cerrar el set."}
		}
		if m.Status != statusInProgress {
			return userError{"⚠️ Este set no está en curso."}
		}
		now := time.Now()
		m.Status = statusPendingClose
		m.CloseRequestedAt = &now
		m.CloseRequestedBy = user.ID
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	other := match.Player2Tag
	if user.ID == match.Player2DiscordID {
		other = match.Player1Tag
	}

	outcome := fmt.Sprintf("Si se cierra, gana **%s** con los **%d pts** completos del FT.", getLeaderTag(match), match.PointsAtStake)
	if match.Score1 == match.Score2 {
		outcome = "El marcador está empatado: si se cierra así, el set se **cancela sin puntos**."
	}

	content := fmt.Sprintf("🏳️ **PROPUESTA DE CIERRE DE SET**\n\n"+
		historyBlock(&match)+
		"%s\n\n"+
		"⚠️ **%s: tienes 2 minutos para responder.**\n"+
		"✅ **Aceptar cierre** = el set termina YA.\n"+
		"▶️ **Seguir jugando** = el set continúa intacto.\n"+
		"⏳ Si no respondes, el sistema cerrará el set solo a los 2 minutos. Un **Admin/Moderador** también puede cerrarlo dándole la victoria a quien va ganando.",
		outcome, other)
	editMessage(s, i, content, []discordgo.MessageComponent{getCloseKeyboard(match.ID)})
}

// ---------------------------------------------------------------------------
// 9. ACEPTAR EL CIERRE (combatiente contrario, o Admin/Moderador de inmediato).
// ---------------------------------------------------------------------------

func handleAcceptCloseSet(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	mod := isMod(i)

	var report *matchReport
	cancelled := false

	match, err := withMatch(db, matchID, func(tx *gorm.DB, m *models.Matchmaking) error {
		if m.Status != statusPendingClose {
			return userError{"⚠️ No hay ninguna propuesta de cierre pendiente."}
		}
		// Un mod puede aceptar (incluso su propia propuesta); un jugador no puede
		// aceptarse a sí mismo y solo el otro combatiente puede aceptar.
		if !mod && (user.ID == m.CloseRequestedBy || !isCombatant(user.ID, m)) {
			return userError{"🚫 Solo el otro combatiente (o un Admin/Moderador) puede aceptar el cierre."}
		}
		r, applied, err := finishWithLeader(tx, m)
		if err != nil {
			return err
		}
		report = &r
		cancelled = !applied
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	publishClosedSet(s, i, match, report, cancelled)
}

// ---------------------------------------------------------------------------
// 10. RECHAZAR EL CIERRE (▶️ Seguir jugando): el set se reactiva intacto.
// ---------------------------------------------------------------------------

func handleDeclineCloseSet(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 Solo los combatientes pueden reactivar el set."}
		}
		if m.Status != statusPendingClose {
			return userError{"⚠️ No hay ninguna propuesta de cierre pendiente."}
		}
		m.Status = statusInProgress
		m.CloseRequestedAt = nil
		m.CloseRequestedBy = ""
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	content := "▶️ **SEGUIMOS JUGANDO** — la propuesta de cierre fue rechazada.\n\n" + gamePanelContent(match)
	editMessage(s, i, content, []discordgo.MessageComponent{getGameKeyboard(match.ID, match.Player1Tag, match.Player2Tag)})
}

// publishClosedSet muestra el resultado del cierre y publica el reporte + limpia el hilo.
func publishClosedSet(s *discordgo.Session, i *discordgo.InteractionCreate, match models.Matchmaking, report *matchReport, cancelled bool) {
	if cancelled {
		editMessage(s, i, fmt.Sprintf(
			"🏳️ **SET CERRADO EN EMPATE — CANCELADO SIN PUNTOS**\n\n"+
				historyBlock(&match)+
				"📍 Consulta el historial de retas en <#%s>.\n\n"+
				"⚠️ **Este hilo se eliminará automáticamente en 5 segundos...**",
			getMatchmakingResultChannelID()), nil)
		go closeMatchThread(s, match)
		return
	}

	r := *report
	editMessage(s, i, fmt.Sprintf(
		"🏁 **SET CERRADO — RESULTADO APLICADO**\n\n"+
			historyBlock(&r.Match)+
			"🏆 Ganador: **%s** (+%s pts, total **%d**)\n"+
			"El reporte final está publicado en <#%s>.\n"+
			"📍 Consulta el historial de retas en <#%s>.\n\n"+
			"⚠️ **Este hilo se eliminará automáticamente en 5 segundos...**",
		r.WinnerTag, signed(r.WinnerDelta), r.WinnerPoints,
		getMatchmakingResultChannelID(), getMatchmakingResultChannelID()), nil)

	go func() {
		sendMatchmakingReport(s, r)
		closeMatchThread(s, r.Match)
	}()
}

// ---------------------------------------------------------------------------
// Puntos y ranking de MATCHMAKING
// ---------------------------------------------------------------------------

type matchReport struct {
	Match                     models.Matchmaking
	WinnerTag, LoserTag       string
	WinnerDelta, LoserDelta   int
	WinnerPoints, LoserPoints int
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

// finishWithLeader cierra un set anticipadamente: gana quien va ARRIBA con los
// puntos COMPLETOS del FT; si hay empate, el set se cancela sin puntos.
func finishWithLeader(tx *gorm.DB, m *models.Matchmaking) (matchReport, bool, error) {
	if m.Score1 == m.Score2 {
		m.Status = statusCancelled
		return matchReport{Match: *m}, false, nil
	}
	if m.Score1 > m.Score2 {
		m.WinnerDiscordID, m.LoserDiscordID = m.Player1DiscordID, m.Player2DiscordID
	} else {
		m.WinnerDiscordID, m.LoserDiscordID = m.Player2DiscordID, m.Player1DiscordID
	}
	m.Status = statusFinished
	r, err := applyMatchResult(tx, m)
	return r, true, err
}

// ---------------------------------------------------------------------------
// Teclados (botones y menús)
// ---------------------------------------------------------------------------

// getPlayerTeamKeyboard: menús del equipo de UN jugador (personaje 1, personaje 2 y fusión).
func getPlayerTeamKeyboard(m models.Matchmaking, side string) []discordgo.MessageComponent {
	one := 1
	tag := m.Player1Tag
	if side == "p2" {
		tag = m.Player2Tag
	}

	charOpts := make([]discordgo.SelectMenuOption, 0, len(gamechars.Characters))
	for _, c := range gamechars.Characters {
		charOpts = append(charOpts, discordgo.SelectMenuOption{Label: c.Name, Value: c.ID})
	}
	fuseOpts := make([]discordgo.SelectMenuOption, 0, len(gamechars.Fuses))
	for _, f := range gamechars.Fuses {
		fuseOpts = append(fuseOpts, discordgo.SelectMenuOption{Label: f.Name, Value: f.ID})
	}

	row := func(sm discordgo.SelectMenu) discordgo.ActionsRow {
		return discordgo.ActionsRow{Components: []discordgo.MessageComponent{sm}}
	}

	return []discordgo.MessageComponent{
		row(discordgo.SelectMenu{
			MenuType:    discordgo.StringSelectMenu,
			CustomID:    fmt.Sprintf("ft_c1_%d_%s", m.ID, side),
			Placeholder: fmt.Sprintf("🎮 Personaje 1 de %s", tag),
			MinValues:   &one,
			MaxValues:   one,
			Options:     charOpts,
		}),
		row(discordgo.SelectMenu{
			MenuType:    discordgo.StringSelectMenu,
			CustomID:    fmt.Sprintf("ft_c2_%d_%s", m.ID, side),
			Placeholder: fmt.Sprintf("🎮 Personaje 2 de %s", tag),
			MinValues:   &one,
			MaxValues:   one,
			Options:     charOpts,
		}),
		row(discordgo.SelectMenu{
			MenuType:    discordgo.StringSelectMenu,
			CustomID:    fmt.Sprintf("ft_fuse_%d_%s", m.ID, side),
			Placeholder: fmt.Sprintf("🔀 Fusión de %s", tag),
			MinValues:   &one,
			MaxValues:   one,
			Options:     fuseOpts,
		}),
	}
}

// getGameKeyboard: botones de reporte del game (estilo start.gg).
func getGameKeyboard(matchID uint, p1Tag, p2Tag string) discordgo.ActionsRow {
	return discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    fmt.Sprintf("W %s", p1Tag),
				Style:    discordgo.SuccessButton,
				CustomID: fmt.Sprintf("ft_win_%d_p1", matchID),
			},
			discordgo.Button{
				Label:    fmt.Sprintf("W %s", p2Tag),
				Style:    discordgo.SuccessButton,
				CustomID: fmt.Sprintf("ft_win_%d_p2", matchID),
			},
			discordgo.Button{
				Label:    "⏪ Reiniciar / Corregir",
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("ft_undo_%d", matchID),
			},
			discordgo.Button{
				Label:    "🏳️ Cerrar set",
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("ft_close_%d", matchID),
			},
		},
	}
}

// getVerifyKeyboard: doble confirmación del game (RESULT VERIFICATION).
func getVerifyKeyboard(matchID uint) discordgo.ActionsRow {
	return discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "✅ Verify Result Submission",
				Style:    discordgo.SuccessButton,
				CustomID: fmt.Sprintf("ft_verify_%d", matchID),
			},
			discordgo.Button{
				Label:    "✏️ Corregir Reporte",
				Style:    discordgo.DangerButton,
				CustomID: fmt.Sprintf("ft_correct_%d", matchID),
			},
		},
	}
}

// getCloseKeyboard: panel de propuesta de cierre (rondas bloqueadas mientras esté activo).
func getCloseKeyboard(matchID uint) discordgo.ActionsRow {
	return discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "✅ Aceptar cierre",
				Style:    discordgo.SuccessButton,
				CustomID: fmt.Sprintf("ft_acceptclose_%d", matchID),
			},
			discordgo.Button{
				Label:    "▶️ Seguir jugando",
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("ft_declineclose_%d", matchID),
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Contenido de los paneles
// ---------------------------------------------------------------------------

// teamPanelContent: panel de equipo de un jugador (personaje 1, personaje 2 y fusión).
func teamPanelContent(m models.Matchmaking, side string) string {
	tag, c1, c2, fuse := m.Player1Tag, m.P1Char1, m.P1Char2, m.P1Fuse
	complete := m.P1TeamComplete()
	if side == "p2" {
		tag, c1, c2, fuse = m.Player2Tag, m.P2Char1, m.P2Char2, m.P2Fuse
		complete = m.P2TeamComplete()
	}
	pick := func(id string) string {
		if n := gamechars.CharacterName(id); n != "" {
			return n
		}
		return "—"
	}
	f := "—"
	if n := gamechars.FuseName(fuse); n != "" {
		f = n
	}
	if complete {
		return fmt.Sprintf("🔒 **EQUIPO DE %s** — CONFIRMADO (FT %d)\n"+
			"🎮 **Personaje 1:** %s\n"+
			"🎮 **Personaje 2:** %s\n"+
			"🔀 **Fusión:** %s\n\n"+
			"✅ Equipo cerrado: la selección de personajes y fusión quedó oculta.\n"+
			"¿Error? Usa **⏪ Reiniciar / Corregir** en el panel de games para re-armar los equipos.",
			tag, m.TargetFT, pick(c1), pick(c2), f)
	}
	return fmt.Sprintf("🧑‍🤝‍🧑 **EQUIPO DE %s** — FT %d\n"+
		"🎮 **Personaje 1:** %s\n"+
		"🎮 **Personaje 2:** %s\n"+
		"🔀 **Fusión:** %s\n\n"+
		"Elige tu **Personaje 1**, tu **Personaje 2** (distinto) y tu **Fusión** en los menús. Se ocultan al confirmar los 3.",
		tag, m.TargetFT, pick(c1), pick(c2), f)
}

// gamePanelContent: panel de reporte del game N (estilo start.gg).
func gamePanelContent(m models.Matchmaking) string {
	gameN := m.Score1 + m.Score2 + 1
	return fmt.Sprintf("📋 **REPORT GAME %d** — FT %d (Meta: %d victorias)\n"+
		"▸ **%s** · %s\n"+
		"▸ **%s** · %s\n"+
		roomCodeLine(&m)+
		"\n"+historyBlock(&m)+
		"¿Quién ganó el **GAME %d**? Presiona **W**.",
		gameN, m.TargetFT, m.TargetFT,
		m.Player1Tag, gamechars.TeamLabel(m.P1Char1, m.P1Char2, m.P1Fuse),
		m.Player2Tag, gamechars.TeamLabel(m.P2Char1, m.P2Char2, m.P2Fuse),
		gameN)
}

// verificationPanelContent: RESULT VERIFICATION del game reportado.
func verificationPanelContent(m models.Matchmaking) string {
	gameN := m.Score1 + m.Score2 + 1
	res := "L - W"
	if m.PendingGameWinner == 1 {
		res = "W - L"
	}
	return fmt.Sprintf("🔍 **RESULT VERIFICATION — GAME %d**\n"+
		"game %d: **%s** %s **%s**\n\n"+
		"⏳ **%s** ya reportó: solo falta que **%s** presione **✅ Verify Result Submission**.\n"+
		"✏️ ¿Reporte equivocado? **%s** puede presionar **Corregir Reporte** para re-anotar el game sin castigo.",
		gameN, gameN, m.Player1Tag, res, m.Player2Tag,
		pendingReporterTag(&m), pendingConfirmerTag(&m), pendingConfirmerTag(&m))
}

// pendingReporterTag / pendingConfirmerTag: quién reportó el game pendiente
// y quién debe confirmarlo (el otro jugador).
func pendingReporterTag(m *models.Matchmaking) string {
	if m.PendingReportedBy == m.Player1DiscordID {
		return m.Player1Tag
	}
	return m.Player2Tag
}

func pendingConfirmerTag(m *models.Matchmaking) string {
	if m.PendingReportedBy == m.Player1DiscordID {
		return m.Player2Tag
	}
	return m.Player1Tag
}

// historyBlock construye el resumen del set con el ORDEN de las victorias:
//
//	game 1: **A** W - L **B**
//	**Score** `2 - 0`
func historyBlock(m *models.Matchmaking) string {
	var hist []int
	if b := []byte(m.RoundHistory); len(b) > 0 {
		if err := json.Unmarshal(b, &hist); err != nil {
			hist = nil
		}
	}
	if len(hist) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, side := range hist {
		res := "L - W"
		if side == 1 {
			res = "W - L"
		}
		fmt.Fprintf(&sb, "game %d: **%s** %s **%s**\n", i+1, m.Player1Tag, res, m.Player2Tag)
	}
	fmt.Fprintf(&sb, "**Score** `%d - %d`\n\n", m.Score1, m.Score2)
	return sb.String()
}

// roomCodeLine devuelve la línea del código de la sala (vacía si la reta no tiene sala).
// La sala es OPCIONAL en /ft: sin código no agrega nada.
func roomCodeLine(m *models.Matchmaking) string {
	if m.RoomCode == "" {
		return ""
	}
	return fmt.Sprintf("🏠 **Código de la sala:** `%s`\n➡️ *Cópialo y úsalo para entrar a la sala.*\n", m.RoomCode)
}

// appendRound agrega una victoria al historial (1 = Player1, 2 = Player2).
func appendRound(m *models.Matchmaking, side string) {
	var hist []int
	_ = json.Unmarshal([]byte(m.RoundHistory), &hist) // vacío/inválido = historial nuevo
	switch side {
	case "p1":
		hist = append(hist, 1)
	case "p2":
		hist = append(hist, 2)
	}
	b, _ := json.Marshal(hist)
	m.RoundHistory = string(b)
}

// getLeaderTag devuelve el tag de quien va ganando el marcador.
func getLeaderTag(m models.Matchmaking) string {
	if m.Score1 > m.Score2 {
		return m.Player1Tag
	}
	return m.Player2Tag
}

func getWinnerTag(m models.Matchmaking) string {
	if m.WinnerDiscordID == m.Player1DiscordID {
		return m.Player1Tag
	}
	return m.Player2Tag
}

// ---------------------------------------------------------------------------
// Reporte al canal de resultados
// ---------------------------------------------------------------------------

func sendMatchmakingReport(s *discordgo.Session, r matchReport) {
	m := r.Match

	embed := &discordgo.MessageEmbed{
		Title: "📊 REPORTE DE MATCHMAKING",
		Color: 0x00FF00,
		Description: fmt.Sprintf("Se ha completado oficialmente la reta **FT %d** entre **%s** y **%s**.\n\n%s",
			m.TargetFT, m.Player1Tag, m.Player2Tag, historyBlock(&m)),
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "🥊 Marcador Final",
				Value:  fmt.Sprintf("**%s** `%d - %d` **%s**", m.Player1Tag, m.Score1, m.Score2, m.Player2Tag),
				Inline: false,
			},
			{
				Name: "🧑‍🤝‍🧑 Equipos (2XKO)",
				Value: fmt.Sprintf("**%s:** %s\n**%s:** %s",
					m.Player1Tag, gamechars.TeamLabel(m.P1Char1, m.P1Char2, m.P1Fuse),
					m.Player2Tag, gamechars.TeamLabel(m.P2Char1, m.P2Char2, m.P2Fuse)),
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

// ---------------------------------------------------------------------------
// Cierre del hilo (borrado con fallback de archivado)
// ---------------------------------------------------------------------------

// closeMatchThread cierra el hilo de la reta terminada, tras la demora anunciada al usuario.
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
