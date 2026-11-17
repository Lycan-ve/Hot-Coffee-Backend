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

	// Emparejamiento por Glicko-2: compara rating y RD de ambos jugadores,
	// prioriza la menor distancia normalizada y amplía la ventana por espera.
	// Tras 4 min se permite cualquier diferencia para evitar colas bloqueadas.
	adjacentTierAfter = 2 * time.Minute
	anyTierAfter      = 4 * time.Minute

	// Anti-rematch: NO repetir rival si ya jugaron un set hace poco (la cola
	// rota rivales nuevos). Excepciones para no congelar el matchmaking:
	//   - con poca gente activa la ventana se acorta (mejor repetir que no jugar);
	//   - si alguno ya esperó rematchFallbackAfter, se acepta repetir.
	rematchWindow           = 3 * time.Hour
	rematchWindowSmallQueue = 30 * time.Minute
	smallQueuePlayers       = 4
	rematchFallbackAfter    = 3 * time.Minute

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

// Estados de progreso de la búsqueda. Los mensajes se calculan en el backend
// a partir del estado real de la cola, no mediante un carrusel del frontend.
const (
	searchPhaseAnalyzing = "analyzing"
	searchPhaseMeasuring = "measuring"
	searchPhaseContacting = "contacting"
	searchPhasePreparing = "preparing"
)

type queueSearchProgress struct {
	Phase           string    `json:"phase"`
	Message         string    `json:"message"`
	CandidateCount  int       `json:"candidateCount"`
	ElapsedSeconds  int64     `json:"elapsedSeconds"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

func newQueueSearchProgress(phase string, candidates int, elapsed time.Duration, now time.Time) queueSearchProgress {
	message := "ANALIZANDO TU RATING…"
	switch phase {
	case searchPhaseMeasuring:
		message = "MIDIENDO EL NIVEL DE LA COLA…"
	case searchPhaseContacting:
		message = "CONTACTANDO RIVALES CERCANOS…"
	case searchPhasePreparing:
		message = "PREPARANDO EL ESCENARIO…"
	}
	if elapsed < 0 {
		elapsed = 0
	}
	return queueSearchProgress{
		Phase:          phase,
		Message:        message,
		CandidateCount: candidates,
		ElapsedSeconds: int64(elapsed.Seconds()),
		UpdatedAt:      now.UTC(),
	}
}

// queueSearchProgressFor cuenta candidatos con las mismas reglas Glicko-2
// que usa tryMatch, además de respetar el anti-rematch.
func queueSearchProgressFor(entry models.MatchmakingQueue, entries []models.MatchmakingQueue, playerRatings map[string]models.MatchmakingPlayer, rematches map[string]bool, now time.Time) queueSearchProgress {
	peerCount := 0
	candidateCount := 0
	for _, peer := range entries {
		if peer.DiscordID == entry.DiscordID {
			continue
		}
		peerCount++

		firstJoinedAt := entry.JoinedAt
		if peer.JoinedAt.Before(firstJoinedAt) ||
			(peer.JoinedAt.Equal(firstJoinedAt) && peer.DiscordID < entry.DiscordID) {
			firstJoinedAt = peer.JoinedAt
		}
		waited := now.Sub(firstJoinedAt)
		if waited < 0 {
			waited = 0
		}
		if !canPairByGlicko(entry, peer, playerRatings, waited, now) {
			continue
		}
		a, b := entry.DiscordID, peer.DiscordID
		if a > b {
			a, b = b, a
		}
		isRecentRematch := rematches[a+"|"+b]
		if isRecentRematch &&
			now.Sub(entry.JoinedAt) < rematchFallbackAfter &&
			now.Sub(peer.JoinedAt) < rematchFallbackAfter {
			continue
		}
		candidateCount++
	}

	phase := searchPhaseAnalyzing
	if peerCount > 0 {
		phase = searchPhaseMeasuring
	}
	if candidateCount > 0 {
		phase = searchPhaseContacting
	}
	return newQueueSearchProgress(phase, candidateCount, now.Sub(entry.JoinedAt), now)
}

// glickoStateOf adapta la fila persistida al modelo matemático. Los defaults
// cubren perfiles de cola sin fila previa de ranking.
func glickoStateOf(p models.MatchmakingPlayer, now time.Time) ranks.Glicko2Rating {
	return ranks.Glicko2At(ranks.Glicko2Rating{
		Rating:     p.GlickoRating,
		RD:         p.GlickoRD,
		Volatility: p.GlickoVolatility,
		LastRatedAt: p.GlickoLastRatedAt,
	}, now)
}

// canPairByGlicko usa la distancia de ratings en desviaciones estándar
// combinadas. La tolerancia se amplía con la espera y mantiene el fallback
// de 4 minutos para que una cola pequeña no se quede bloqueada.
func canPairByGlicko(a, b models.MatchmakingQueue, playerRatings map[string]models.MatchmakingPlayer, waited time.Duration, now time.Time) bool {
	if waited >= anyTierAfter {
		return true
	}
	sigmaWindow := 1.5
	if waited >= adjacentTierAfter {
		sigmaWindow = 2.0
	}
	ra := glickoStateOf(playerRatings[a.DiscordID], now)
	rb := glickoStateOf(playerRatings[b.DiscordID], now)
	return ranks.Glicko2DistanceSigma(ra, rb) <= sigmaWindow
}

var db *gorm.DB

// RegisterRoutes registra la API de la cola y arranca el sweeper de fondo.
func RegisterRoutes(database *gorm.DB) {
	db = database

	http.HandleFunc("/api/matchmaking/queue/join", handleJoin)
	http.HandleFunc("/api/matchmaking/queue/leave", handleLeave)
	http.HandleFunc("/api/matchmaking/queue/state", handleState)
	http.HandleFunc("/api/matchmaking/accept", handleAccept)
	http.HandleFunc("/api/matchmaking/decline", handleDecline)
	http.HandleFunc("/api/matchmaking/rematch", handleRematch)

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
// Incluye récord y racha (de matchmaking_players) para dar contexto del rival.
// Nadie parte con rango: sin los 5 sets de clasificación completados el
// display es "SIN RANGO" (sin emblema ni LP).
func playerCard(discordID, tag, avatar string, points int) map[string]any {
	var mp models.MatchmakingPlayer
	if err := db.Where("discord_id = ?", discordID).First(&mp).Error; err != nil {
		mp = models.MatchmakingPlayer{}
	}
	country, state := auth.LocationOf(discordID)
	glicko := glickoStateOf(mp, time.Now())
	return map[string]any{
		"discordId": discordID,
		"tag":       tag,
		"avatarUrl": avatar,
		"country":   country,
		"state":     state,
		"points":        points + ranks.BasePoints,
		"rank":          ranks.InfoFor(points, mp.Wins+mp.Losses),
		"glickoRating":     glicko.Rating,
		"glickoRD":         glicko.RD,
		"glickoVolatility": glicko.Volatility,
		"wins":      mp.Wins,
		"losses":    mp.Losses,
		"streak":    mp.Streak,
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

// tryMatch recorre la cola por antigüedad y busca el rival elegible más cercano
// según Glicko-2 (rating + RD). La progresión visible sigue siendo LP/rango.
func tryMatch() error {
	return db.Transaction(func(tx *gorm.DB) error {
		matchNow := time.Now()
		var entries []models.MatchmakingQueue
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Order("joined_at ASC, discord_id ASC").Find(&entries).Error; err != nil {
			return err
		}

		// Ratings, RD y volatilidad actuales: Glicko-2 decide la compatibilidad.
		playerRatings := make(map[string]models.MatchmakingPlayer, len(entries))
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			ids = append(ids, e.DiscordID)
		}
		if len(ids) > 0 {
			var players []models.MatchmakingPlayer
			if err := tx.Where("discord_id IN ?", ids).Find(&players).Error; err != nil {
				return err
			}
			for _, p := range players {
				playerRatings[p.DiscordID] = p
			}
		}

		// Anti-rematch: ventana según cuánta gente haya en cola (con poca
		// gente activa se permite repetir antes para no congelar el sistema).
		window := rematchWindow
		if len(entries) <= smallQueuePlayers {
			window = rematchWindowSmallQueue
		}
		rematches := recentOpponents(tx, ids, window)
		isRematch := func(a, b models.MatchmakingQueue) bool {
			x, y := a.DiscordID, b.DiscordID
			if x > y {
				x, y = y, x
			}
			return rematches[x+"|"+y]
		}

		paired := make(map[string]bool, len(entries))
		for i := range entries {
			a := entries[i]
			if paired[a.DiscordID] {
				continue
			}

			// Entre los rivales elegibles, prioriza la menor distancia Glicko-2
			// normalizada por la incertidumbre de ambos jugadores.
			bestJ := -1
			bestDistance := 1e99
			waited := matchNow.Sub(a.JoinedAt)
			for j := i + 1; j < len(entries); j++ {
				b := entries[j]
				if paired[b.DiscordID] || a.DiscordID == b.DiscordID {
					continue
				}
				if !canPairByGlicko(a, b, playerRatings, waited, matchNow) {
					continue
				}
				// No repetir rival reciente… salvo que alguno de los dos ya
				// espere demasiado: repetir es mejor que esperar de más.
				if isRematch(a, b) &&
					matchNow.Sub(a.JoinedAt) < rematchFallbackAfter &&
					matchNow.Sub(b.JoinedAt) < rematchFallbackAfter {
					continue
				}
				distance := ranks.Glicko2DistanceSigma(
					glickoStateOf(playerRatings[a.DiscordID], matchNow),
					glickoStateOf(playerRatings[b.DiscordID], matchNow),
				)
				if distance < bestDistance {
					bestDistance = distance
					bestJ = j
				}
			}
			if bestJ < 0 {
				continue
			}
			b := entries[bestJ]

			// LP se estima y liquida con la economía existente; Glicko-2 solo
			// decide el nivel de habilidad y la selección del rival.
			proposal := models.MatchmakingProposal{
				Player1DiscordID: a.DiscordID,
				Player1Tag:       a.Username,
				Player1Avatar:    a.AvatarURL,
				Player1Points:    a.Points,
				Player2DiscordID: b.DiscordID,
				Player2Tag:       b.Username,
				Player2Avatar:    b.AvatarURL,
				Player2Points:    b.Points,
				TargetFT:         3,
				PointsAtStake:    ranks.MaxStakeOf(a.Points, b.Points),
				Status:           proposalWaiting,
				CreatedAt:        matchNow,
				ExpiresAt:        matchNow.Add(proposalTTL),
			}
			if err := tx.Create(&proposal).Error; err != nil {
				return err
			}
			if err := tx.Where("discord_id IN ?", []string{a.DiscordID, b.DiscordID}).
				Delete(&models.MatchmakingQueue{}).Error; err != nil {
				return err
			}
			paired[a.DiscordID], paired[b.DiscordID] = true, true
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

	// 3) CONFIRMED/LAUNCHING atascadas > 10 min: se cancela todo para que
	//    los jugadores puedan volver a la cola (solo aplica a propuestas
	//    viejas lanzadas al bot; el flujo web pasa directo a LAUNCHED).
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
	_ = body.FT // compatibilidad: la app vieja manda 3 o 5, ahora SIEMPRE es FT 3

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

	// Upsert: si ya estaba en cola (p. ej. reingresó), conserva su puesto.
	var existing models.MatchmakingQueue
	if err := db.Where("discord_id = ?", discordID).First(&existing).Error; err == nil {
		existing.TargetFT = 3
		existing.LastSeen = time.Now()
		db.Save(&existing)
	} else {
		entry := models.MatchmakingQueue{
			DiscordID: discordID,
			Username:  tag,
			AvatarURL: avatar,
			Points:    playerPoints(discordID),
			TargetFT:  3,
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
	now := time.Now()
	db.Model(&models.MatchmakingQueue{}).Where("discord_id = ?", discordID).
		Update("last_seen", now)

	// El emparejamiento y las expiraciones las maneja el sweeper de fondo
	// (cada 10 s): ejecutarlas en cada poll de cada cliente saturaba el pool
	// de conexiones gratuito de Supabase.

	resp := map[string]any{"status": "idle"}

	// 0) Set en curso: la sala del set vive en la propia página (si nació de
	//    la cola web, "web": true) o sigue en el hilo de Discord (si lo
	//    lanzó el bot con una propuesta antigua).
	var active models.Matchmaking
	if err := db.Where("status IN ? AND (player1_discord_id = ? OR player2_discord_id = ?)",
		[]string{"IN_PROGRESS", "PENDING_VERIFY", "PENDING_CLOSE"}, discordID, discordID).
		Order("id DESC").First(&active).Error; err == nil {
		resp["status"] = "matched"
		resp["match"] = map[string]any{
			"id":        active.ID,
			"web":       active.Source == "web",
			"threadId":  active.ThreadID,
			"threadUrl": threadURL(active.ThreadID),
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

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
		// La pantalla mantiene la transición mínima del frontend, pero el
		// texto de esta fase lo determina la propuesta ya creada en el backend.
		resp["search"] = newQueueSearchProgress(searchPhasePreparing, 0, now.Sub(p.CreatedAt), now)
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

	// 2) En cola: buscando rival. Leemos la cola una sola vez para obtener
	// tanto el total de jugadores como los candidatos compatibles por Glicko-2.
	var entries []models.MatchmakingQueue
	if err := db.Order("joined_at ASC, discord_id ASC").Find(&entries).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	var entry *models.MatchmakingQueue
	ids := make([]string, 0, len(entries))
	for i := range entries {
		ids = append(ids, entries[i].DiscordID)
		if entries[i].DiscordID == discordID {
			entry = &entries[i]
		}
	}
	if entry != nil {
		// Ratings y RD para mostrar candidatos con las mismas reglas Glicko-2
		// que utiliza el emparejador.
		playerRatings := make(map[string]models.MatchmakingPlayer, len(ids))
		if len(ids) > 0 {
			var players []models.MatchmakingPlayer
			if err := db.Select("discord_id", "glicko_rating", "glicko_rd", "glicko_volatility", "glicko_last_rated_at").
				Where("discord_id IN ?", ids).Find(&players).Error; err != nil {
				log.Printf("⚠️ WebQueue: no se pudieron leer ratings Glicko-2: %v", err)
				writeErr(w, http.StatusInternalServerError, "bd", "No se pudo consultar el rating de habilidad. Intenta de nuevo.")
				return
			} else {
				for _, player := range players {
					playerRatings[player.DiscordID] = player
				}
			}
		}

		elapsed := now.Sub(entry.JoinedAt)
		rematchWindowForQueue := rematchWindow
		if len(entries) <= smallQueuePlayers {
			rematchWindowForQueue = rematchWindowSmallQueue
		}
		rematches := recentOpponents(db, ids, rematchWindowForQueue)
		resp["status"] = "searching"
		resp["search"] = queueSearchProgressFor(*entry, entries, playerRatings, rematches, now)
		resp["queue"] = map[string]any{
			"ft":      entry.TargetFT,
			"since":   entry.JoinedAt,
			"players": len(entries),
			// Campo legacy para compatibilidad; ya no limita el emparejamiento.
			// El criterio real es la distancia Glicko-2.
			"radius": 100,
			// true cuando se amplía la ventana de habilidad por tiempo de espera.
			"wide": elapsed >= adjacentTierAfter,
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// 3) Propuesta fallida reciente (sets viejos que esperaban al bot).
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
			// Doble aceptación: el set se abre AQUÍ MISMO, 100% en la web
			// (equipo, chat y reporte en la página — sin esperar al bot).
			now := time.Now()
			match := models.Matchmaking{
				League:           matchmakingLeague,
				Source:           "web",
				Player1DiscordID: p.Player1DiscordID,
				Player1Tag:       p.Player1Tag,
				Player1Avatar:    p.Player1Avatar,
				Player2DiscordID: p.Player2DiscordID,
				Player2Tag:       p.Player2Tag,
				Player2Avatar:    p.Player2Avatar,
				TargetFT:         p.TargetFT,
				PointsAtStake:    p.PointsAtStake,
				Status:           "IN_PROGRESS",
				LastRoundAt:      &now,
			}
			if err := tx.Create(&match).Error; err != nil {
				return err
			}
			p.Status = proposalLaunched
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

// ---------------------------------------------------------------------------
// POST /api/matchmaking/rematch — revancha directa tras un set
// ---------------------------------------------------------------------------

// Ventana para pedir revancha: solo tiene sentido justo después de terminar.
const rematchRequestTTL = 15 * time.Minute

// handleRematch crea una propuesta de revancha contra el rival del último set
// terminado. Quien la pide queda ya aceptado; el rival la ve en su pantalla de
// matchmaking (o como aviso en la sala) y la acepta o rechaza ahí. Es la vía
// "oficial" para repetir rival: el anti-revancha de la cola no aplica aquí
// porque ambos tienen que aceptar de nuevo.
func handleRematch(w http.ResponseWriter, r *http.Request) {
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

	// Quien la pide: libre de sets, propuestas y cola.
	if active, err := hasActiveMatch(discordID); err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	} else if active {
		writeErr(w, http.StatusConflict, "set_activo", "ya tienes un set en curso")
		return
	}
	if p, err := activeProposal(discordID); err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	} else if p != nil {
		writeErr(w, http.StatusConflict, "propuesta_abierta", "ya tienes una propuesta pendiente")
		return
	}
	var qn int64
	db.Model(&models.MatchmakingQueue{}).Where("discord_id = ?", discordID).Count(&qn)
	if qn > 0 {
		writeErr(w, http.StatusConflict, "en_cola", "sal de la cola primero para proponer una revancha")
		return
	}

	// Último set FINISHED del jugador (la revancha es contra su rival de ahí),
	// y tiene que ser reciente.
	var last models.Matchmaking
	if err := db.Where("status = ? AND (player1_discord_id = ? OR player2_discord_id = ?)",
		"FINISHED", discordID, discordID).Order("id DESC").First(&last).Error; err != nil {
		writeErr(w, http.StatusNotFound, "sin_set", "no hay ningún set terminado reciente para la revancha")
		return
	}
	if time.Since(last.UpdatedAt) > rematchRequestTTL {
		writeErr(w, http.StatusConflict, "set_viejo", "la revancha solo se puede pedir justo después de terminar un set")
		return
	}
	opp := last.Player1DiscordID
	if last.Player1DiscordID == discordID {
		opp = last.Player2DiscordID
	}
	if opp == discordID {
		writeErr(w, http.StatusBadRequest, "rival", "no se pudo identificar al rival del último set")
		return
	}

	// El rival también tiene que estar libre.
	if active, err := hasActiveMatch(opp); err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	} else if active {
		writeErr(w, http.StatusConflict, "rival_ocupado", "tu rival ya está en otro set")
		return
	}
	if p, err := activeProposal(opp); err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	} else if p != nil {
		writeErr(w, http.StatusConflict, "rival_ocupado", "tu rival ya tiene una propuesta pendiente")
		return
	}
	var qo int64
	db.Model(&models.MatchmakingQueue{}).Where("discord_id = ?", opp).Count(&qo)
	if qo > 0 {
		writeErr(w, http.StatusConflict, "rival_en_cola", "tu rival ya está buscando otro set: búscalo ahí")
		return
	}

	tag1, av1 := playerSnapshot(discordID)
	tag2, av2 := playerSnapshot(opp)
	pts1, pts2 := playerPoints(discordID), playerPoints(opp)
	proposal := models.MatchmakingProposal{
		Player1DiscordID: discordID,
		Player1Tag:       tag1,
		Player1Avatar:    av1,
		Player1Points:    pts1,
		Accept1:          true, // quien la pide ya aceptó
		Player2DiscordID: opp,
		Player2Tag:       tag2,
		Player2Avatar:    av2,
		Player2Points:    pts2,
		TargetFT:         3,
		PointsAtStake:    ranks.MaxStakeOf(pts1, pts2),
		Status:           proposalWaiting,
		CreatedAt:        time.Now(),
		ExpiresAt:        time.Now().Add(proposalTTL),
	}
	if err := db.Create(&proposal).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "proposalId": proposal.ID})
}
