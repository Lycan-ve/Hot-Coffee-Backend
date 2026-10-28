package discord

import (
	"fmt"
	"log"
	"time"

	"smash-backend/internal/models"

	"github.com/bwmarrin/discordgo"
	"gorm.io/gorm"
)

// Parámetros del "janitor": barre la BASE DE DATOS cada 30 s (sin timers en
// memoria), así que sobrevive reinicios del bot.
// NOTA: los games esperando verificación NO se auto-verifican (decisión del
// servidor): cualquiera de los dos combatientes puede verificar, incluso si el
// otro está AFK.
const (
	janitorInterval    = 30 * time.Second // frecuencia del barrido
	closeSetDelay      = 2 * time.Minute  // PENDING_CLOSE sin respuesta -> cerrar solo
	idleSetTimeout     = 2 * time.Hour    // IN_PROGRESS sin reportar games -> cancelar
	pendingRetaTimeout = 30 * time.Minute // PENDING sin respuesta al DM -> cancelar
)

func StartMatchmakingJanitor(s *discordgo.Session, db *gorm.DB) {
	go func() {
		ticker := time.NewTicker(janitorInterval)
		defer ticker.Stop()
		for range ticker.C {
			janitorCloseSet(s, db)
			janitorIdleSets(s, db)
			janitorStaleRetas(s, db)
		}
	}()
	log.Println("🧹 Janitor de matchmaking activo: barrido de la BD cada 30 s.")
}

// editPanel actualiza el mensaje del panel de games dentro del hilo (sin interacción activa).
func editPanel(s *discordgo.Session, m *models.Matchmaking, content string, components []discordgo.MessageComponent) {
	if m.PanelMessageID == "" || m.ThreadID == "" {
		return
	}
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    m.ThreadID,
		ID:         m.PanelMessageID,
		Content:    &content,
		Components: &components,
	}); err != nil {
		log.Printf("❌ Janitor: no se pudo editar el panel de la reta %d: %v\n", m.ID, err)
	}
}

// janitorCloseSet: propuestas de cierre sin respuesta a los 2 min -> el set se
// cierra solo (gana el líder con los puntos completos; empate = cancelado sin puntos).
func janitorCloseSet(s *discordgo.Session, db *gorm.DB) {
	deadline := time.Now().Add(-closeSetDelay)
	var stale []models.Matchmaking
	if err := db.Where("status = ? AND close_requested_at IS NOT NULL AND close_requested_at <= ?", statusPendingClose, deadline).
		Find(&stale).Error; err != nil {
		log.Printf("❌ Janitor (cerrar set): %v\n", err)
		return
	}

	for _, m0 := range stale {
		var report *matchReport
		cancelled := false
		match, err := withMatch(db, uint64(m0.ID), func(tx *gorm.DB, m *models.Matchmaking) error {
			if m.Status != statusPendingClose {
				return nil // ya la resolvió alguien
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
			log.Printf("❌ Janitor: no se pudo cerrar el set %d: %v\n", m0.ID, err)
			continue
		}
		if report == nil {
			continue
		}

		if cancelled {
			log.Printf("🧹 Janitor: set %d cerrado en empate -> cancelado sin puntos.\n", match.ID)
			editPanel(s, &match, fmt.Sprintf(
				"🏳️ **SET CERRADO AUTOMÁTICAMENTE EN EMPATE — CANCELADO SIN PUNTOS** (2 min sin respuesta)\n\n"+
					historyBlock(&match)+
					"📍 Consulta el historial de retas en <#%s>.\n\n"+
					"⚠️ **Este hilo se eliminará automáticamente en 5 segundos...**",
				getMatchmakingResultChannelID()), nil)
			go closeMatchThread(s, match)
			continue
		}

		r := *report
		log.Printf("🧹 Janitor: set %d cerrado automáticamente (2 min sin respuesta).\n", match.ID)
		editPanel(s, &match, fmt.Sprintf(
			"🏁 **SET CERRADO AUTOMÁTICAMENTE** (2 minutos sin respuesta)\n\n"+
				historyBlock(&match)+
				"🏆 Ganador: **%s** (+%s pts, total **%d**)\n"+
				"El reporte final está publicado en <#%s>.\n"+
				"📍 Consulta el historial de retas en <#%s>.\n\n"+
				"⚠️ **Este hilo se eliminará automáticamente en 5 segundos...**",
			r.WinnerTag, signed(r.WinnerDelta), r.WinnerPoints,
			getMatchmakingResultChannelID(), getMatchmakingResultChannelID()), nil)

		go func(m models.Matchmaking, r matchReport) {
			sendMatchmakingReport(s, r)
			closeMatchThread(s, m)
		}(match, r)
	}
}

// janitorIdleSets: sets en curso 2 horas sin reportar games -> cancelados sin puntos.
func janitorIdleSets(s *discordgo.Session, db *gorm.DB) {
	deadline := time.Now().Add(-idleSetTimeout)
	var stale []models.Matchmaking
	if err := db.Where("status = ? AND last_round_at IS NOT NULL AND last_round_at <= ?", statusInProgress, deadline).
		Find(&stale).Error; err != nil {
		log.Printf("❌ Janitor (sets abandonados): %v\n", err)
		return
	}

	for _, m := range stale {
		// Cancelación atómica: si otro proceso lo resolvió antes, no hace nada.
		res := db.Model(&models.Matchmaking{}).
			Where("id = ? AND status = ?", m.ID, statusInProgress).
			Update("status", statusCancelled)
		if res.Error != nil || res.RowsAffected == 0 {
			continue
		}
		log.Printf("🧹 Janitor: reta %d cancelada por inactividad (2 h sin reportar games).\n", m.ID)
		editPanel(s, &m, fmt.Sprintf(
			"💀 **SET CANCELADO POR INACTIVIDAD**\n\n"+
				historyBlock(&m)+
				"El set llevaba **2 horas sin reportar games**. Se cancela **sin puntos**.\n\n"+
				"📍 Consulta el historial de retas en <#%s>.\n\n"+
				"⚠️ **Este hilo se eliminará automáticamente en 5 segundos...**",
			getMatchmakingResultChannelID()), nil)
		go closeMatchThread(s, m)
	}
}

// janitorStaleRetas: retas por DM 30 minutos sin respuesta -> canceladas + DM al retador.
func janitorStaleRetas(s *discordgo.Session, db *gorm.DB) {
	deadline := time.Now().Add(-pendingRetaTimeout)
	var stale []models.Matchmaking
	if err := db.Where("status = ? AND created_at <= ?", statusPending, deadline).
		Find(&stale).Error; err != nil {
		log.Printf("❌ Janitor (retas sin responder): %v\n", err)
		return
	}

	for _, m := range stale {
		res := db.Model(&models.Matchmaking{}).
			Where("id = ? AND status = ?", m.ID, statusPending).
			Update("status", statusCancelled)
		if res.Error != nil || res.RowsAffected == 0 {
			continue
		}
		log.Printf("🧹 Janitor: reta %d cancelada (30 min sin respuesta al DM).\n", m.ID)
		if dm, err := s.UserChannelCreate(m.Player1DiscordID); err == nil {
			_, _ = s.ChannelMessageSend(dm.ID, fmt.Sprintf(
				"⌛ Tu reta FT %d contra **%s** expiró sin respuesta (30 minutos) y fue cancelada. Puedes volver a retarlo con /ft.",
				m.TargetFT, m.Player2Tag))
		}
	}
}
