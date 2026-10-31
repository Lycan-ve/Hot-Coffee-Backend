// Package webqueue implementa la COLA DE MATCHMAKING de la web: el jugador
// entra a la cola, el sistema le busca un rival de puntos similares (el radio
// de búsqueda crece con la espera, como en Smash the Peak) y, tras la doble
// aceptación, el BOT de Discord crea el hilo del set con paneles, equipos y
// reporte — el mismo flujo de /ft.
package webqueue

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"smash-backend/internal/auth"
	"smash-backend/internal/models"
	"smash-backend/internal/ranks"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	matchmakingLeague = "2xko_online"

	// Radio de búsqueda en puntos: crece con el tiempo de espera.
	radiusBase   = 25 // 0-30 s: ±25 pts (~1 división)
	radiusGrowth = 25 // +25 pts cada 30 s de espera
	radiusStep   = 30 * time.Second
	radiusMax    = 1 << 20 // a los pocos minutos: contra cualquiera

	// Heartbeat: si el navegador no reporta vida en 45 s, sale de la cola.
	heartbeatTimeout = 45 * time.Second
	// Tiempo para la doble aceptación de la propuesta.
	proposalTTL = 90 * time.Second
	// Frecuencia del barrido de fondo (heartbeat, expiraciones, emparejar).
	sweepInterval = 10 * time.Second
)

// Estados de una propuesta.
const (
	proposalWaiting   = "WAITING"
	proposalConfirmed = "CONFIRMED"
	proposalLaunching = "LAUNCHING"
	proposalLaunched  = "LAUNCHED"
	proposalExpired   = "EXPIRED"
	proposalCancelled = "CANCELLED"
	proposalFailed    = "FAILED"
)

// Estados de reta que cuentan como "set activo" (no puedes hacer cola).
var activeMatchStatuses = []string{"PENDING", "IN_PROGRESS", "PENDING_VERIFY", "PENDING_CLOSE"}

var db *gorm.DB

// RegisterRoutes registra la API de la cola y arranca el sweeper de fondo.
func RegisterRoutes(database *gorm.DB) {
	db = database

	http.HandleFunc("/api/matchmaking/queue/join", handleJoin)
	http.HandleFunc("/api/matchmaking/queue/leave", handleLeave)
	http.HandleFunc("/api/matchmaking/queue/state", handleState)
	http.HandleFunc("/api/matchmaking/accept", handleAccept)
	http.HandleFunc("/api/matchmaking/decline", handleDecline)

	go startSweeper()
	fmt.Println("✅ Rutas de matchmaking web registradas (cola automática).")
}

// ---------------------------------------------------------------------------
// Utilidades
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

// searchRadius: cuántos puntos de diferencia tolera el emparejamiento según
// el tiempo que lleva esperando el jugador.
func searchRadius(waited time.Duration) int {
	r := radiusBase + int(waited/radiusStep)*radiusGrowth
	if r > radiusMax {
		r = radiusMax
	}
	return r
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// hasActiveMatch: ¿el usuario tiene una reta en curso (o pendiente)?
func hasActiveMatch(discordID string) (bool, error) {
	var n int64
	err := db.Model(&models.Matchmaking{}).
		Where("status IN ? AND (player1_discord_id = ? OR player2_discord_id = ?)",
			activeMatchStatuses, discordID, discordID).
		Count(&n).Error
	return n > 0, err
}

// activeProposal: propuesta WAITING/CONFIRMED donde participa el usuario.
func activeProposal(discordID string) (*models.MatchmakingProposal, error) {
	var p models.MatchmakingProposal
	err := db.Where("status IN ? AND (player1_discord_id = ? OR player2_discord_id = ?)",
		[]string{proposalWaiting, proposalConfirmed}, discordID, discordID).
		Order("id DESC").First(&p).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// playerSnapshot saca nombre y avatar del usuario de la web (con fallback al
// registro de matchmaking si nunca inició sesión).
func playerSnapshot(discordID string) (tag, avatar string) {
	var u models.User
	if err := db.Where("discord_id = ?", discordID).First(&u).Error; err == nil {
		tag = u.Username
		if u.GlobalName != "" {
			tag = u.GlobalName
		}
		if u.GuildNickname != "" {
			tag = u.GuildNickname
		}
		return tag, u.AvatarURL
	}
	var mp models.MatchmakingPlayer
	if err := db.Where("discord_id = ?", discordID).First(&mp).Error; err == nil {
		return mp.Username, mp.AvatarURL
	}
	return discordID, ""
}

func playerPoints(discordID string) int {
	var mp models.MatchmakingPlayer
	if err := db.Where("discord_id = ?", discordID).First(&mp).Error; err == nil {
		return mp.Points
	}
	return 0
}

// playerCard arma la ficha de un jugador para el VS de la propuesta.
func playerCard(discordID, tag, avatar string, points int) map[string]any {
	return map[string]any{
		"discordId": discordID,
		"tag":       tag,
		"avatarUrl": avatar,
		"points":    points,
		"rank":      ranks.FromPoints(points),
	}
}

func threadURL(threadID string) string {
	guild := os.Getenv("DISCORD_GUILD_ID")
	if guild == "" || threadID == "" {
		return ""
	}
	return "https://discord.com/channels/" + guild + "/" + threadID
}

// requeue reinserta a un jugador en la cola conservando la antigüedad que
// tenía cuando se formó la propuesta (no pierde su puesto por culpa del rival).
func requeue(tx *gorm.DB, discordID string, ft int, joinedAt time.Time) {
	tag, avatar := playerSnapshot(discordID)
	entry := models.MatchmakingQueue{
		DiscordID: discordID,
		Username:  tag,
		AvatarURL: avatar,
		Points:    playerPoints(discordID),
		TargetFT:  ft,
		JoinedAt:  joinedAt,
		LastSeen:  time.Now(),
	}
	tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&entry)
}

// ---------------------------------------------------------------------------
// Emparejador
// ---------------------------------------------------------------------------

// tryMatch empareja (en una transacción con lock de filas) a los jugadores de
// la cola: recorre por antigüedad y junta a cada jugador con el primer rival
// disponible del mismo formato dentro del radio de búsqueda.
func tryMatch() error {
	return db.Transaction(func(tx *gorm.DB) error {
		var entries []models.MatchmakingQueue
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Order("joined_at ASC, discord_id ASC").Find(&entries).Error; err != nil {
			return err
		}

		paired := make(map[string]bool, len(entries))
		for i := range entries {
			a := entries[i]
			if paired[a.DiscordID] {
				continue
			}
			for j := i + 1; j < len(entries); j++ {
				b := entries[j]
				if paired[b.DiscordID] || a.DiscordID == b.DiscordID {
					continue
				}
				if a.TargetFT != b.TargetFT {
					continue // formatos distintos: no se cruzan
				}
				// El radio depende del que MÁS ha esperado (a es más antiguo: orden ASC).
				if abs(a.Points-b.Points) > searchRadius(time.Since(a.JoinedAt)) {
					continue
				}

				// P1 = quien lleva más tiempo esperando.
				proposal := models.MatchmakingProposal{
					Player1DiscordID: a.DiscordID,
					Player1Tag:       a.Username,
					Player1Avatar:    a.AvatarURL,
					Player1Points:    a.Points,
					Player2DiscordID: b.DiscordID,
					Player2Tag:       b.Username,
					Player2Avatar:    b.AvatarURL,
					Player2Points:    b.Points,
					TargetFT:         a.TargetFT,
					PointsAtStake:    a.TargetFT * 5, // igual que /ft
					Status:           proposalWaiting,
					CreatedAt:        time.Now(),
					ExpiresAt:        time.Now().Add(proposalTTL),
				}
				if err := tx.Create(&proposal).Error; err != nil {
					return err
				}
				if err := tx.Where("discord_id IN ?", []string{a.DiscordID, b.DiscordID}).
					Delete(&models.MatchmakingQueue{}).Error; err != nil {
					return err
				}
				paired[a.DiscordID], paired[b.DiscordID] = true, true
				break
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Sweeper de fondo
// ---------------------------------------------------------------------------

func startSweeper() {
	ticker := time.NewTicker(sweepInterval)
	go func() {
		for range ticker.C {
			sweep()
			if err := tryMatch(); err != nil {
				log.Printf("❌ WebQueue (sweeper): %v\n", err)
			}
		}
	}()
	fmt.Println("🔄 Sweeper de la cola web activo (heartbeat, expiraciones, emparejamientos).")
}

// sweep limpia corazones muertos y propuestas vencidas.
func sweep() {
	now := time.Now()

	// 1) Jugadores sin heartbeat (cerraron la pestaña): salen de la cola.
	db.Where("last_seen < ?", now.Add(-heartbeatTimeout)).Delete(&models.MatchmakingQueue{})

	// 2) Propuestas WAITING vencidas: expiran y AMBOS vuelven a la cola
	//    conservando antigüedad.
	var expired []models.MatchmakingProposal
	db.Where("status = ? AND expires_at <= ?", proposalWaiting, now).Find(&expired)
	for _, p := range expired {
		res := db.Model(&models.MatchmakingProposal{}).
			Where("id = ? AND status = ?", p.ID, proposalWaiting).
			Update("status", proposalExpired)
		if res.Error != nil || res.RowsAffected == 0 {
			continue // otro proceso la resolvió
		}
		requeue(db, p.Player1DiscordID, p.TargetFT, p.CreatedAt)
		requeue(db, p.Player2DiscordID, p.TargetFT, p.CreatedAt)
	}

	// 3) CONFIRMED/LAUNCHING atascadas > 10 min (bot caído): se cancela todo
	//    para que los jugadores puedan volver a la cola.
	var stuck []models.MatchmakingProposal
	db.Where("status IN ? AND created_at <= ?",
		[]string{proposalConfirmed, proposalLaunching}, now.Add(-10*time.Minute)).Find(&stuck)
	for _, p := range stuck {
		res := db.Model(&models.MatchmakingProposal{}).
			Where("id = ? AND status IN ?", p.ID,
				[]string{proposalConfirmed, proposalLaunching}).
			Update("status", proposalFailed)
		if res.Error != nil || res.RowsAffected == 0 {
			continue
		}
		if p.MatchID > 0 {
			db.Model(&models.Matchmaking{}).Where("id = ? AND status = ?", p.MatchID, "PENDING").
				Update("status", "CANCELLED")
		}
	}
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/queue/join   { "ft": 3 | 5 }
// ---------------------------------------------------------------------------

func handleJoin(w http.ResponseWriter, r *http.Request) {
	if auth.PreflightCors(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "metodo", "usa POST")
		return
	}
	discordID, ok := auth.UserIDFromRequest(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_autenticado", "inicia sesión con Discord")
		return
	}

	var body struct {
		FT int `json:"ft"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "cuerpo", "JSON inválido")
		return
	}
	if body.FT != 3 && body.FT != 5 {
		writeErr(w, http.StatusBadRequest, "ft", "el formato debe ser FT 3 o FT 5")
		return
	}

	// Guardas: no se hace cola con un set activo o una propuesta abierta.
	if active, err := hasActiveMatch(discordID); err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	} else if active {
		writeErr(w, http.StatusConflict, "set_activo", "ya tienes un set en curso: termínalo o cancélalo primero")
		return
	}
	if p, err := activeProposal(discordID); err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	} else if p != nil {
		writeErr(w, http.StatusConflict, "propuesta_abierta", "responde primero la propuesta que tienes pendiente")
		return
	}

	tag, avatar := playerSnapshot(discordID)

	// Upsert: si ya estaba en cola (p. ej. cambió de formato), conserva su puesto.
	var existing models.MatchmakingQueue
	if err := db.Where("discord_id = ?", discordID).First(&existing).Error; err == nil {
		existing.TargetFT = body.FT
		existing.LastSeen = time.Now()
		db.Save(&existing)
	} else {
		entry := models.MatchmakingQueue{
			DiscordID: discordID,
			Username:  tag,
			AvatarURL: avatar,
			Points:    playerPoints(discordID),
			TargetFT:  body.FT,
			JoinedAt:  time.Now(),
			LastSeen:  time.Now(),
		}
		if err := db.Create(&entry).Error; err != nil {
			writeErr(w, http.StatusInternalServerError, "bd", "error interno")
			return
		}
	}

	if err := tryMatch(); err != nil {
		log.Printf("❌ WebQueue: no se pudo emparejar tras unirse: %v\n", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/queue/leave
// ---------------------------------------------------------------------------

func handleLeave(w http.ResponseWriter, r *http.Request) {
	if auth.PreflightCors(w, r) {
		return
	}
	discordID, ok := auth.UserIDFromRequest(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_autenticado", "inicia sesión con Discord")
		return
	}
	db.Where("discord_id = ?", discordID).Delete(&models.MatchmakingQueue{})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// GET /api/matchmaking/queue/state
// ---------------------------------------------------------------------------

func handleState(w http.ResponseWriter, r *http.Request) {
	if auth.PreflightCors(w, r) {
		return
	}
	discordID, ok := auth.UserIDFromRequest(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_autenticado", "inicia sesión con Discord")
		return
	}

	// Heartbeat: "sigo con la pestaña abierta".
	db.Model(&models.MatchmakingQueue{}).Where("discord_id = ?", discordID).
		Update("last_seen", time.Now())

	// Reacción rápida sin esperar al sweeper de fondo.
	sweep()
	if err := tryMatch(); err != nil {
		log.Printf("❌ WebQueue: %v\n", err)
	}

	resp := map[string]any{"status": "idle"}

	// 1) Propuesta activa (WAITING/CONFIRMED): pantalla de "rival encontrado".
	p, err := activeProposal(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	if p != nil {
		me := playerCard(p.Player1DiscordID, p.Player1Tag, p.Player1Avatar, p.Player1Points)
		opp := playerCard(p.Player2DiscordID, p.Player2Tag, p.Player2Avatar, p.Player2Points)
		acceptedByMe, acceptedByOpp := p.Accept1, p.Accept2
		if p.Side(discordID) == "p2" {
			me, opp = opp, me
			acceptedByMe, acceptedByOpp = p.Accept2, p.Accept1
		}
		status := "found"
		if p.Status == proposalConfirmed {
			status = "confirmed"
		}
		resp["status"] = status
		resp["proposal"] = map[string]any{
			"id":                 p.ID,
			"ft":                 p.TargetFT,
			"pointsAtStake":      p.PointsAtStake,
			"me":                 me,
			"opponent":           opp,
			"acceptedByMe":       acceptedByMe,
			"acceptedByOpponent": acceptedByOpp,
			"expiresAt":          p.ExpiresAt,
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// 2) En cola: buscando rival.
	var entry models.MatchmakingQueue
	if err := db.Where("discord_id = ?", discordID).First(&entry).Error; err == nil {
		var qc int64
		db.Model(&models.MatchmakingQueue{}).Count(&qc)
		resp["status"] = "searching"
		resp["queue"] = map[string]any{
			"ft":      entry.TargetFT,
			"since":   entry.JoinedAt,
			"players": qc,
			"radius":  searchRadius(time.Since(entry.JoinedAt)),
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// 3) Última propuesta LANZADA: muestra el enlace al hilo (si es reciente
	//    o el set sigue activo).
	var launched models.MatchmakingProposal
	if err := db.Where("(player1_discord_id = ? OR player2_discord_id = ?)", discordID, discordID).
		Order("id DESC").First(&launched).Error; err == nil && launched.Status == proposalLaunched {
		show := time.Since(launched.CreatedAt) < 15*time.Minute
		if !show && launched.MatchID > 0 {
			var m models.Matchmaking
			if err := db.Where("id = ? AND status IN ?", launched.MatchID, activeMatchStatuses).
				First(&m).Error; err == nil {
				show = true
			}
		}
		if show {
			resp["status"] = "matched"
			resp["match"] = map[string]any{
				"id":        launched.MatchID,
				"threadId":  launched.ThreadID,
				"threadUrl": threadURL(launched.ThreadID),
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
	}

	// 4) Propuesta fallida reciente (el bot no pudo crear el hilo).
	var failed models.MatchmakingProposal
	if err := db.Where("status = ? AND (player1_discord_id = ? OR player2_discord_id = ?)",
		proposalFailed, discordID, discordID).
		Order("id DESC").First(&failed).Error; err == nil &&
		time.Since(failed.CreatedAt) < 5*time.Minute {
		resp["status"] = "failed"
		writeJSON(w, http.StatusOK, resp)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/accept
// ---------------------------------------------------------------------------

func handleAccept(w http.ResponseWriter, r *http.Request) {
	if auth.PreflightCors(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "metodo", "usa POST")
		return
	}
	discordID, ok := auth.UserIDFromRequest(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_autenticado", "inicia sesión con Discord")
		return
	}

	p, err := activeProposal(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	if p == nil {
		writeErr(w, http.StatusNotFound, "sin_propuesta", "no tienes ninguna propuesta pendiente")
		return
	}
	pID := p.ID

	// Transacción con lock de fila: dos aceptaciones simultáneas no crean
	// dos retas (la segunda ve el estado ya confirmado y no hace nada).
	err = db.Transaction(func(tx *gorm.DB) error {
		var p models.MatchmakingProposal
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&p, pID).Error; err != nil {
			return err
		}
		if p.Status != proposalWaiting {
			return nil // el otro jugador ya la confirmó (o expiró)
		}
		if p.Side(discordID) == "p1" {
			p.Accept1 = true
		} else {
			p.Accept2 = true
		}

		if p.Accept1 && p.Accept2 {
			// Doble aceptación: se confirma y se crea la reta que el bot lanzará.
			match := models.Matchmaking{
				League:           matchmakingLeague,
				Player1DiscordID: p.Player1DiscordID,
				Player1Tag:       p.Player1Tag,
				Player1Avatar:    p.Player1Avatar,
				Player2DiscordID: p.Player2DiscordID,
				Player2Tag:       p.Player2Tag,
				Player2Avatar:    p.Player2Avatar,
				TargetFT:         p.TargetFT,
				PointsAtStake:    p.PointsAtStake,
				Status:           "PENDING", // el bot la reclama (→ IN_PROGRESS) al crear el hilo
			}
			if err := tx.Create(&match).Error; err != nil {
				return err
			}
			p.Status = proposalConfirmed
			p.MatchID = match.ID
		}
		return tx.Save(&p).Error
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/decline
// ---------------------------------------------------------------------------

func handleDecline(w http.ResponseWriter, r *http.Request) {
	if auth.PreflightCors(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "metodo", "usa POST")
		return
	}
	discordID, ok := auth.UserIDFromRequest(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_autenticado", "inicia sesión con Discord")
		return
	}

	p, err := activeProposal(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	if p == nil {
		writeErr(w, http.StatusNotFound, "sin_propuesta", "no tienes ninguna propuesta pendiente")
		return
	}
	pID := p.ID

	err = db.Transaction(func(tx *gorm.DB) error {
		var p models.MatchmakingProposal
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&p, pID).Error; err != nil {
			return err
		}
		if p.Status != proposalWaiting && p.Status != proposalConfirmed {
			return nil
		}
		p.Status = proposalCancelled
		if err := tx.Save(&p).Error; err != nil {
			return err
		}
		// Si ya se había creado la reta, se cancela.
		if p.MatchID > 0 {
			if err := tx.Model(&models.Matchmaking{}).
				Where("id = ? AND status IN ?", p.MatchID,
					[]string{"PENDING", "IN_PROGRESS"}).
				Update("status", "CANCELLED").Error; err != nil {
				return err
			}
		}
		// El rival vuelve a la cola conservando su antigüedad; quien rechazó, sale.
		opp := p.Player2DiscordID
		if p.Side(discordID) == "p2" {
			opp = p.Player1DiscordID
		}
		requeue(tx, opp, p.TargetFT, p.CreatedAt)
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
