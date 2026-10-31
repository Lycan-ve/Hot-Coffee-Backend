package discord

// Este archivo conecta la COLA WEB con el flujo real de retas del bot:
// cuando dos jugadores aceptan el emparejamiento en la web (propuesta
// CONFIRMED), el worker crea el hilo de Discord con paneles de equipo,
// panel de games y DMs — exactamente igual que al aceptar una reta /ft.

import (
	"fmt"
	"log"
	"time"

	"smash-backend/internal/models"

	"github.com/bwmarrin/discordgo"
	"gorm.io/gorm"
)

const webQueuePollInterval = 10 * time.Second

// StartWebQueueWorker arranca el poller de propuestas confirmadas.
func StartWebQueueWorker(s *discordgo.Session, db *gorm.DB) {
	go func() {
		ticker := time.NewTicker(webQueuePollInterval)
		defer ticker.Stop()
		for range ticker.C {
			launchConfirmedProposals(s, db)
		}
	}()
	log.Println("🌐 Worker de matchmaking web activo: lanza los sets confirmados desde la página.")
}

// launchConfirmedProposals toma una a una las propuestas CONFIRMED y crea su hilo.
func launchConfirmedProposals(s *discordgo.Session, db *gorm.DB) {
	var pending []models.MatchmakingProposal
	if err := db.Where("status = ?", "CONFIRMED").Order("id ASC").Find(&pending).Error; err != nil {
		log.Printf("❌ WebQueue worker: %v\n", err)
		return
	}

	for _, p := range pending {
		// Reclama la propuesta para que otro barrido no la lance dos veces.
		res := db.Model(&models.MatchmakingProposal{}).
			Where("id = ? AND status = ?", p.ID, "CONFIRMED").
			Update("status", "LAUNCHING")
		if res.Error != nil || res.RowsAffected == 0 {
			continue // otro proceso la tomó
		}

		if p.MatchID == 0 {
			log.Printf("❌ WebQueue worker: propuesta %d sin reta asociada (FAILED).\n", p.ID)
			db.Model(&models.MatchmakingProposal{}).Where("id = ?", p.ID).Update("status", "FAILED")
			continue
		}

		// Reclama la reta: PENDING → IN_PROGRESS (la misma transacción que usa /ft).
		match, err := withMatch(db, uint64(p.MatchID), func(_ *gorm.DB, m *models.Matchmaking) error {
			if m.Status != statusPending {
				return fmt.Errorf("reta %d en estado inesperado %s", m.ID, m.Status)
			}
			m.Status = statusInProgress
			return nil
		})
		if err != nil {
			log.Printf("❌ WebQueue worker: no se pudo reclamar la reta %d: %v\n", p.MatchID, err)
			db.Model(&models.MatchmakingProposal{}).Where("id = ?", p.ID).Update("status", "FAILED")
			continue
		}

		match, err = launchMatchThread(s, db, match)
		if err != nil {
			log.Printf("❌ WebQueue worker: no se pudo crear el hilo de la reta %d: %v\n", match.ID, err)
			db.Model(&models.Matchmaking{}).Where("id = ?", match.ID).Update("status", statusCancelled)
			db.Model(&models.MatchmakingProposal{}).Where("id = ?", p.ID).Update("status", "FAILED")
			continue
		}

		// Listo: LAUNCHED con el hilo para que la web muestre el enlace.
		db.Model(&models.MatchmakingProposal{}).Where("id = ?", p.ID).Updates(map[string]any{
			"status":    "LAUNCHED",
			"thread_id": match.ThreadID,
		})

		// Aviso por MD a ambos con el enlace al hilo.
		for _, id := range []string{match.Player1DiscordID, match.Player2DiscordID} {
			if dm, err := s.UserChannelCreate(id); err == nil {
				_, _ = s.ChannelMessageSend(dm.ID, fmt.Sprintf(
					"🌐 **Tu partida de la cola web está lista** (FT %d · 💰 %d pts en juego).\n"+
						"⚔️ Entra al hilo para armar tu equipo y jugar: <#%s>",
					match.TargetFT, match.PointsAtStake, match.ThreadID))
			}
		}
		log.Printf("🌐 WebQueue worker: set %d lanzado para la propuesta %d (hilo %s).\n",
			match.ID, p.ID, match.ThreadID)
	}
}
