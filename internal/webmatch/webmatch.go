// Package webmatch implementa la SALA DEL SET 100% web: armado de equipo
// (2 personajes + fusión), código de sala, chat entre combatientes y reporte
// de games con doble confirmación — sin depender del bot ni de Discord.
// La liquidación de puntos es idéntica a la de /ft: FT 3 equitativo por
// RANGO (las divisiones I/II/III valen igual; vencer a un rango superior
// da más puntos, y caer ante uno inferior cuesta más).
package webmatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"smash-backend/internal/auth"
	"smash-backend/internal/gamechars"
	"smash-backend/internal/models"
	"smash-backend/internal/ranks"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	// Fallback para sets VIEJOS (terminados antes de las reglas por rango),
	// que no guardaban el delta real de cada jugador.
	loserPenalty   = 5
	minPointsFloor = 5

	chatMaxLen = 500 // caracteres del chat
	chatWindow = 100 // mensajes máximos por poll

	sweepInterval  = 30 * time.Second
	closeAutoAfter = 2 * time.Minute  // cierre sin respuesta: gana el líder
	staleAfter     = 3 * time.Hour    // pestaña abandonada: set cancelado
	finishedTTL    = 15 * time.Minute // ventana para ver el resultado y el "gg"
)

// Estados del set (mismos valores que usa el bot).
const (
	statusInProgress    = "IN_PROGRESS"
	statusPendingVerify = "PENDING_VERIFY"
	statusPendingClose  = "PENDING_CLOSE"
	statusFinished      = "FINISHED"
	statusCancelled     = "CANCELLED"
)

// PendingGameWinner especiales: 3 = propuesta de RESULTADO FINAL;
// 4 = propuesta de CORRECCIÓN del último game (requiere confirmación del rival).
const (
	pendingFinal   = 3
	pendingCorrect = 4
)

// Rate-limit del chat: máx. 10 mensajes por ventana de 30 s por jugador.
const (
	chatRateLimit  = 10
	chatRateWindow = 30 * time.Second
)

var (
	chatLimiterMu sync.Mutex
	chatLimiter   = make(map[string][]time.Time)
)

func chatAllowed(discordID string) bool {
	now := time.Now()
	chatLimiterMu.Lock()
	defer chatLimiterMu.Unlock()
	recent := chatLimiter[discordID]
	kept := recent[:0]
	for _, t := range recent {
		if now.Sub(t) < chatRateWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) >= chatRateLimit {
		chatLimiter[discordID] = kept
		return false
	}
	chatLimiter[discordID] = append(kept, now)
	return true
}

var db *gorm.DB

// RegisterRoutes registra la API de la sala del set y arranca su sweeper.
func RegisterRoutes(database *gorm.DB) {
	db = database

	http.HandleFunc("/api/matchmaking/match/state", handleState)
	http.HandleFunc("/api/matchmaking/match/team", handleTeam)
	http.HandleFunc("/api/matchmaking/match/room", handleRoom)
	http.HandleFunc("/api/matchmaking/match/chat", handleChat)
	http.HandleFunc("/api/matchmaking/match/report", handleReport)
	http.HandleFunc("/api/matchmaking/match/report/confirm", handleReportConfirm)
	http.HandleFunc("/api/matchmaking/match/report/reject", handleReportReject)
	http.HandleFunc("/api/matchmaking/match/report/correct", handleReportCorrect)
	http.HandleFunc("/api/matchmaking/match/report/final", handleReportFinal)
	http.HandleFunc("/api/matchmaking/match/close", handleClose)
	http.HandleFunc("/api/matchmaking/match/close/respond", handleCloseRespond)

	go startSweeper()
	fmt.Println("✅ Rutas de la sala del set web registradas (equipo, chat y reporte).")
}

// ---------------------------------------------------------------------------
// Utilidades
// ---------------------------------------------------------------------------

type userErr struct{ msg string }

func (e userErr) Error() string { return e.msg }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

// respondTxErr separa los errores "de regla" (409, se muestran al jugador)
// de los errores internos (500, se loguean).
func respondTxErr(w http.ResponseWriter, err error) {
	var ue userErr
	if errors.As(err, &ue) {
		writeErr(w, http.StatusConflict, "regla", ue.msg)
		return
	}
	log.Printf("❌ WebMatch: %v\n", err)
	writeErr(w, http.StatusInternalServerError, "bd", "error interno")
}

// mustAuth resuelve CORS/preflight y valida el JWT; devuelve false si ya respondió.
func mustAuth(w http.ResponseWriter, r *http.Request) (string, bool) {
	if auth.PreflightCors(w, r) {
		return "", false
	}
	discordID, ok := auth.UserIDFromRequest(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "no_autenticado", "inicia sesión con Discord")
	}
	return discordID, ok
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "cuerpo", "JSON inválido")
		return false
	}
	return true
}

// currentMatch: el set más reciente del usuario que sigue activo (o terminó
// hace poco, para que la sala pueda mostrar el resultado final).
func currentMatch(discordID string) (*models.Matchmaking, error) {
	var m models.Matchmaking
	err := db.Where(
		"(player1_discord_id = ? OR player2_discord_id = ?) AND "+
			"(status IN ? OR (status IN ? AND updated_at > ?))",
		discordID, discordID,
		[]string{statusInProgress, statusPendingVerify, statusPendingClose},
		[]string{statusFinished, statusCancelled},
		time.Now().Add(-finishedTTL),
	).Order("id DESC").First(&m).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// activeMatch: el set en curso del usuario (para acciones de la sala).
func activeMatch(discordID string) (*models.Matchmaking, error) {
	var m models.Matchmaking
	err := db.Where("status IN ? AND (player1_discord_id = ? OR player2_discord_id = ?)",
		[]string{statusInProgress, statusPendingVerify, statusPendingClose},
		discordID, discordID).
		Order("id DESC").First(&m).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func mySide(m *models.Matchmaking, discordID string) string {
	if discordID == m.Player1DiscordID {
		return "p1"
	}
	return "p2"
}

func isCombatant(m *models.Matchmaking, discordID string) bool {
	return discordID == m.Player1DiscordID || discordID == m.Player2DiscordID
}

func sideTag(m *models.Matchmaking, discordID string) string {
	if discordID == m.Player1DiscordID {
		return m.Player1Tag
	}
	return m.Player2Tag
}

func sideInfoOf(m *models.Matchmaking, discordID string) (tag, avatar string) {
	if discordID == m.Player1DiscordID {
		return m.Player1Tag, m.Player1Avatar
	}
	return m.Player2Tag, m.Player2Avatar
}

func playerPoints(discordID string) int {
	var mp models.MatchmakingPlayer
	if err := db.Where("discord_id = ?", discordID).First(&mp).Error; err == nil {
		return mp.Points
	}
	return 0
}

// playerGames: sets terminados del jugador (0 si nunca jugó).
func playerGames(discordID string) int {
	var mp models.MatchmakingPlayer
	if err := db.Where("discord_id = ?", discordID).First(&mp).Error; err == nil {
		return mp.Wins + mp.Losses
	}
	return 0
}

// playerRank: rango para la sala. Nadie parte con rango: sin los 5 sets de
// clasificación completados el display es "SIN RANGO" (sin emblema ni LP).
func playerRank(pointsWon, games int) ranks.Info {
	return ranks.InfoFor(pointsWon, games)
}

func roundHistory(m *models.Matchmaking) []int {
	// Siempre NO nulo: un slice nil se serializa como JSON "null" y rompe
	// el frontend de la sala (match.roundHistory.length).
	hist := []int{}
	if b := []byte(m.RoundHistory); len(b) > 0 {
		_ = json.Unmarshal(b, &hist)
	}
	return hist
}

func appendRound(m *models.Matchmaking, winner int) {
	hist := roundHistory(m)
	hist = append(hist, winner)
	b, _ := json.Marshal(hist)
	m.RoundHistory = string(b)
}

// ---------------------------------------------------------------------------
// Payloads del estado
// ---------------------------------------------------------------------------

func teamPayload(c1, c2, fuse string) map[string]any {
	return map[string]any{
		"char1":     c1,
		"char2":     c2,
		"fuse":      fuse,
		"char1Name": gamechars.CharacterName(c1),
		"char2Name": gamechars.CharacterName(c2),
		"fuseName":  gamechars.FuseName(fuse),
		"complete":  c1 != "" && c2 != "" && fuse != "",
	}
}

func matchPayload(m *models.Matchmaking, discordID string) map[string]any {
	side := mySide(m, discordID)

	meID, oppID := m.Player1DiscordID, m.Player2DiscordID
	myScore, oppScore := m.Score1, m.Score2
	meTag, meAvatar := m.Player1Tag, m.Player1Avatar
	oppTag, oppAvatar := m.Player2Tag, m.Player2Avatar
	// Privacidad igual que el bot: el rival solo ve tu equipo COMPLETO.
	meTeam := teamPayload(m.P1Char1, m.P1Char2, m.P1Fuse)
	oppTeam := teamPayload(m.P2Char1, m.P2Char2, m.P2Fuse)
	if side == "p2" {
		meID, oppID = m.Player2DiscordID, m.Player1DiscordID
		myScore, oppScore = m.Score2, m.Score1
		meTag, meAvatar = m.Player2Tag, m.Player2Avatar
		oppTag, oppAvatar = m.Player1Tag, m.Player1Avatar
		meTeam = teamPayload(m.P2Char1, m.P2Char2, m.P2Fuse)
		if !m.P1TeamComplete() {
			oppTeam = teamPayload("", "", "")
		}
	} else if !m.P2TeamComplete() {
		oppTeam = teamPayload("", "", "")
	}

	// Los puntos se leen DESPUES del intercambio p1/p2: antes quedaban
	// cruzados cuando quien consultaba era el P2 (rango y puntos del rival).
	mePts, oppPts := playerPoints(meID), playerPoints(oppID)
	meCountry, meState := auth.LocationOf(meID)
	oppCountry, oppState := auth.LocationOf(oppID)
	return map[string]any{
		"id":            m.ID,
		"ft":            m.TargetFT,
		"pointsAtStake": m.PointsAtStake,
		"roomCode":      m.RoomCode,
		"status":        m.Status,
		"mySide":        side,
		"me": map[string]any{
			"discordId": meID,
			"tag":       meTag,
			"avatarUrl": meAvatar,
			"country":   meCountry,
			"state":     meState,
			"points":    mePts + ranks.BasePoints,
			"rank":      playerRank(mePts, playerGames(meID)),
			"score":     myScore,
			"team":      meTeam,
		},
		"opponent": map[string]any{
			"discordId": oppID,
			"tag":       oppTag,
			"avatarUrl": oppAvatar,
			"country":   oppCountry,
			"state":     oppState,
			"points":    oppPts + ranks.BasePoints,
			"rank":      playerRank(oppPts, playerGames(oppID)),
			"score":     oppScore,
			"team":      oppTeam,
		},
		"roundHistory": roundHistory(m),
		"pending":      pendingPayload(m, discordID),
		"close":        closePayload(m, discordID),
		"finished":     finishedPayload(m, discordID, myScore, oppScore),
	}
}

func pendingPayload(m *models.Matchmaking, discordID string) map[string]any {
	if m.Status != statusPendingVerify || m.PendingGameWinner == 0 {
		return nil
	}
	p := map[string]any{
		"byMe":        m.PendingReportedBy == discordID,
		"proposerTag": sideTag(m, m.PendingReportedBy),
	}
	if m.PendingGameWinner == pendingFinal {
		mine, opp := m.FinalPick1, m.FinalPick2
		if mySide(m, discordID) == "p2" {
			mine, opp = m.FinalPick2, m.FinalPick1
		}
		p["kind"] = "final"
		p["finalMine"] = mine
		p["finalOpp"] = opp
		return p
	}
	if m.PendingGameWinner == pendingCorrect {
		p["kind"] = "correct"
		return p
	}
	p["kind"] = "game"
	if m.PendingGameWinner == 1 {
		p["winnerSide"] = "p1"
	} else {
		p["winnerSide"] = "p2"
	}
	return p
}

func closePayload(m *models.Matchmaking, discordID string) map[string]any {
	if m.Status != statusPendingClose || m.CloseRequestedBy == "" {
		return nil
	}
	leaderID := m.Player2DiscordID
	if m.Score1 > m.Score2 {
		leaderID = m.Player1DiscordID
	}
	return map[string]any{
		"byMe":        m.CloseRequestedBy == discordID,
		"requestedBy": sideTag(m, m.CloseRequestedBy),
		"leader":      sideTag(m, leaderID),
		"leaderIsMe":  leaderID == discordID,
		"tie":         m.Score1 == m.Score2,
		"requestedAt": m.CloseRequestedAt,
	}
}

func finishedPayload(m *models.Matchmaking, discordID string, myScore, oppScore int) map[string]any {
	if m.Status != statusFinished && m.Status != statusCancelled {
		return nil
	}
	if m.Status == statusCancelled {
		return map[string]any{
			"status":   statusCancelled,
			"myScore":  myScore,
			"oppScore": oppScore,
		}
	}
	won := m.WinnerDiscordID == discordID
	points := playerPoints(discordID)
	// Delta real aplicado al terminar el set (reglas por rango); los sets
	// viejos no lo guardan y usan el stake/penalty fijo de entonces.
	delta := m.WinnerDelta
	if delta == 0 {
		delta = m.PointsAtStake
	}
	if !won {
		delta = m.LoserDelta
		if delta == 0 {
			delta = -loserPenalty
		}
	}
	return map[string]any{
		"status":    statusFinished,
		"won":       won,
		"myScore":   myScore,
		"oppScore":  oppScore,
		"delta":     delta,
		"myPoints":  points + ranks.BasePoints,
		"rank":      playerRank(points, playerGames(discordID)),
		"winnerTag": sideTag(m, m.WinnerDiscordID),
	}
}

// ---------------------------------------------------------------------------
// GET /api/matchmaking/match/state?after=<id>
// ---------------------------------------------------------------------------

func handleState(w http.ResponseWriter, r *http.Request) {
	discordID, ok := mustAuth(w, r)
	if !ok {
		return
	}

	m, err := currentMatch(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}

	resp := map[string]any{"match": nil, "messages": []any{}, "lastMessageId": 0}
	if m == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)

	var msgs []models.MatchmakingMessage
	if after > 0 {
		if err := db.Where("match_id = ? AND id > ?", m.ID, after).
			Order("id ASC").Limit(chatWindow).Find(&msgs).Error; err != nil {
			writeErr(w, http.StatusInternalServerError, "bd", "error interno")
			return
		}
	} else {
		// Primer poll: los ÚLTIMOS mensajes (el chat crece hacia abajo).
		if err := db.Where("match_id = ?", m.ID).
			Order("id DESC").Limit(chatWindow).Find(&msgs).Error; err != nil {
			writeErr(w, http.StatusInternalServerError, "bd", "error interno")
			return
		}
		for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
			msgs[i], msgs[j] = msgs[j], msgs[i]
		}
	}

	list := make([]map[string]any, 0, len(msgs))
	last := after
	for _, msg := range msgs {
		list = append(list, map[string]any{
			"id":        msg.ID,
			"mine":      msg.SenderID == discordID,
			"senderTag": sideTag(m, msg.SenderID),
			"content":   msg.Content,
			"createdAt": msg.CreatedAt,
		})
		if uint64(msg.ID) > last {
			last = uint64(msg.ID)
		}
	}

	// Propuesta de revancha activa: la sala terminada la muestra como banner
	// (quien la pidió espera; el rival la ve para ir a aceptarla al matchmaking).
	var rp models.MatchmakingProposal
	if err := db.Where("status IN ? AND (player1_discord_id = ? OR player2_discord_id = ?)",
		[]string{"WAITING", "CONFIRMED"}, discordID, discordID).
		Order("id DESC").First(&rp).Error; err == nil {
		oppTag := rp.Player2Tag
		if rp.Side(discordID) == "p2" {
			oppTag = rp.Player1Tag
		}
		resp["rematch"] = map[string]any{
			"id":          rp.ID,
			"byMe":        rp.Side(discordID) == "p1",
			"opponentTag": oppTag,
			"status":      rp.Status,
		}
	}

	resp["match"] = matchPayload(m, discordID)
	resp["messages"] = list
	resp["lastMessageId"] = last
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/match/team   { "char1", "char2", "fuse" }
// ---------------------------------------------------------------------------

func handleTeam(w http.ResponseWriter, r *http.Request) {
	discordID, ok := mustAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "metodo", "usa POST")
		return
	}

	var body struct {
		Char1 string `json:"char1"`
		Char2 string `json:"char2"`
		Fuse  string `json:"fuse"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if !gamechars.ValidCharacter(body.Char1) || !gamechars.ValidCharacter(body.Char2) {
		writeErr(w, http.StatusBadRequest, "personajes", "personaje inválido")
		return
	}
	if body.Char1 == body.Char2 {
		writeErr(w, http.StatusBadRequest, "personajes", "el Personaje 1 y el Personaje 2 deben ser distintos")
		return
	}
	if !gamechars.ValidFuse(body.Fuse) {
		writeErr(w, http.StatusBadRequest, "fusion", "fusión inválida")
		return
	}

	m, err := activeMatch(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "sin_set", "no tienes ningún set en curso")
		return
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		var mm models.Matchmaking
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&mm, m.ID).Error; err != nil {
			return err
		}
		if mm.Status != statusInProgress {
			return userErr{"el set ya no está en curso"}
		}
		if mySide(&mm, discordID) == "p1" {
			// Con games jugados el equipo queda sellado (igual que el bot).
			if mm.P1TeamComplete() && mm.Score1+mm.Score2 > 0 {
				return userErr{"ya hay games jugados: el equipo ya no se puede cambiar"}
			}
			mm.P1Char1, mm.P1Char2, mm.P1Fuse = body.Char1, body.Char2, body.Fuse
		} else {
			if mm.P2TeamComplete() && mm.Score1+mm.Score2 > 0 {
				return userErr{"ya hay games jugados: el equipo ya no se puede cambiar"}
			}
			mm.P2Char1, mm.P2Char2, mm.P2Fuse = body.Char1, body.Char2, body.Fuse
		}
		return tx.Save(&mm).Error
	})
	if err != nil {
		respondTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/match/room   { "room": "código de sala 2XKO" }
// ---------------------------------------------------------------------------

func handleRoom(w http.ResponseWriter, r *http.Request) {
	discordID, ok := mustAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "metodo", "usa POST")
		return
	}

	var body struct {
		Room string `json:"room"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	room := strings.TrimSpace(body.Room)
	if len(room) > 32 {
		writeErr(w, http.StatusBadRequest, "sala", "código demasiado largo")
		return
	}

	m, err := activeMatch(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "sin_set", "no tienes ningún set en curso")
		return
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		var mm models.Matchmaking
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&mm, m.ID).Error; err != nil {
			return err
		}
		if !isCombatant(&mm, discordID) {
			return userErr{"no eres parte de este set"}
		}
		mm.RoomCode = room
		return tx.Save(&mm).Error
	})
	if err != nil {
		respondTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/match/chat   { "content": "..." }
// ---------------------------------------------------------------------------

func handleChat(w http.ResponseWriter, r *http.Request) {
	discordID, ok := mustAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "metodo", "usa POST")
		return
	}

	var body struct {
		Content string `json:"content"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	content := strings.TrimSpace(body.Content)
	if content == "" {
		writeErr(w, http.StatusBadRequest, "mensaje", "el mensaje está vacío")
		return
	}
	if len([]rune(content)) > chatMaxLen {
		writeErr(w, http.StatusBadRequest, "mensaje", "máximo 500 caracteres")
		return
	}
	if !chatAllowed(discordID) {
		writeErr(w, http.StatusTooManyRequests, "spam", "vas muy rápido: espera unos segundos antes de escribir de nuevo")
		return
	}

	m, err := currentMatch(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "sin_set", "no tienes ningún set en curso")
		return
	}

	msg := models.MatchmakingMessage{
		MatchID:  m.ID,
		SenderID: discordID,
		Content:  content,
	}
	if err := db.Create(&msg).Error; err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/match/report   { "winner": "me" | "opponent" }
// ---------------------------------------------------------------------------

func handleReport(w http.ResponseWriter, r *http.Request) {
	discordID, ok := mustAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "metodo", "usa POST")
		return
	}

	var body struct {
		Winner string `json:"winner"` // "me" | "opponent"
	}
	if !decodeBody(w, r, &body) {
		return
	}

	m, err := activeMatch(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "sin_set", "no tienes ningún set en curso")
		return
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		var mm models.Matchmaking
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&mm, m.ID).Error; err != nil {
			return err
		}
		if mm.Status == statusPendingVerify {
			return userErr{"ya hay un reporte pendiente: confírmalo o recházalo primero"}
		}
		if mm.Status != statusInProgress {
			return userErr{"el set ya no está en curso"}
		}
		if !mm.P1TeamComplete() || !mm.P2TeamComplete() {
			return userErr{"ambos equipos deben estar completos antes de reportar"}
		}

		winner := 0
		switch body.Winner {
		case "me":
			winner = 1
			if mySide(&mm, discordID) == "p2" {
				winner = 2
			}
		case "opponent":
			winner = 2
			if mySide(&mm, discordID) == "p2" {
				winner = 1
			}
		default:
			return userErr{"reporte inválido"}
		}

		mm.PendingGameWinner = winner
		mm.PendingReportedBy = discordID
		mm.Status = statusPendingVerify
		return tx.Save(&mm).Error
	})
	if err != nil {
		respondTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/match/report/confirm
// ---------------------------------------------------------------------------

func handleReportConfirm(w http.ResponseWriter, r *http.Request) {
	discordID, ok := mustAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "metodo", "usa POST")
		return
	}

	m, err := activeMatch(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "sin_set", "no tienes ningún set en curso")
		return
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		var mm models.Matchmaking
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&mm, m.ID).Error; err != nil {
			return err
		}
		if mm.Status != statusPendingVerify {
			return userErr{"no hay ningún reporte pendiente"}
		}
		if !isCombatant(&mm, discordID) {
			return userErr{"no eres parte de este set"}
		}
		if mm.PendingReportedBy == discordID {
			return userErr{"espera a que tu rival confirme tu reporte"}
		}

		switch mm.PendingGameWinner {
		case 1, 2:
			// Game a game: se anota y se mira si llegó al FT.
			appendRound(&mm, mm.PendingGameWinner)
			if mm.PendingGameWinner == 1 {
				mm.Score1++
			} else {
				mm.Score2++
			}
			mm.PendingGameWinner = 0
			mm.PendingReportedBy = ""
			mm.Status = statusInProgress
			now := time.Now()
			mm.LastRoundAt = &now
			if mm.Score1 >= mm.TargetFT || mm.Score2 >= mm.TargetFT {
				return finishMatch(tx, &mm)
			}
		case pendingFinal:
			return applyFinal(tx, &mm)
		case pendingCorrect:
			// Corrección confirmada por el rival: se deshace el último game.
			hist := roundHistory(&mm)
			if len(hist) == 0 {
				return userErr{"no hay games para corregir"}
			}
			last := hist[len(hist)-1]
			hist = hist[:len(hist)-1]
			b, _ := json.Marshal(hist)
			mm.RoundHistory = string(b)
			if last == 1 && mm.Score1 > 0 {
				mm.Score1--
			}
			if last == 2 && mm.Score2 > 0 {
				mm.Score2--
			}
			mm.PendingGameWinner = 0
			mm.PendingReportedBy = ""
			mm.Status = statusInProgress
			now := time.Now()
			mm.LastRoundAt = &now
			return tx.Save(&mm).Error
		default:
			return userErr{"reporte inválido"}
		}
		return tx.Save(&mm).Error
	})
	if err != nil {
		respondTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/match/report/reject
// (rechaza el del rival o cancela el propio)
// ---------------------------------------------------------------------------

func handleReportReject(w http.ResponseWriter, r *http.Request) {
	discordID, ok := mustAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "metodo", "usa POST")
		return
	}

	m, err := activeMatch(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "sin_set", "no tienes ningún set en curso")
		return
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		var mm models.Matchmaking
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&mm, m.ID).Error; err != nil {
			return err
		}
		if mm.Status != statusPendingVerify {
			return userErr{"no hay ningún reporte pendiente"}
		}
		if !isCombatant(&mm, discordID) {
			return userErr{"no eres parte de este set"}
		}
		mm.PendingGameWinner = 0
		mm.PendingReportedBy = ""
		mm.FinalPick1, mm.FinalPick2 = -1, -1
		mm.Status = statusInProgress
		return tx.Save(&mm).Error
	})
	if err != nil {
		respondTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/match/report/correct  (deshace el ÚLTIMO game)
// ---------------------------------------------------------------------------

func handleReportCorrect(w http.ResponseWriter, r *http.Request) {
	discordID, ok := mustAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "metodo", "usa POST")
		return
	}

	m, err := activeMatch(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "sin_set", "no tienes ningún set en curso")
		return
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		var mm models.Matchmaking
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&mm, m.ID).Error; err != nil {
			return err
		}
		if mm.Status != statusInProgress {
			return userErr{"solo se puede corregir con el set en curso"}
		}
		if !isCombatant(&mm, discordID) {
			return userErr{"no eres parte de este set"}
		}
		if len(roundHistory(&mm)) == 0 {
			return userErr{"todavía no hay games para corregir"}
		}
		// La corrección ya no es unilateral: se propone y el RIVAL la confirma
		// (igual que un reporte de game), para evitar disputas.
		mm.PendingGameWinner = pendingCorrect
		mm.PendingReportedBy = discordID
		mm.Status = statusPendingVerify
		return tx.Save(&mm).Error
	})
	if err != nil {
		respondTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/match/report/final   { "myScore", "oppScore" }
// ---------------------------------------------------------------------------

func handleReportFinal(w http.ResponseWriter, r *http.Request) {
	discordID, ok := mustAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "metodo", "usa POST")
		return
	}

	var body struct {
		MyScore  int `json:"myScore"`
		OppScore int `json:"oppScore"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	m, err := activeMatch(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "sin_set", "no tienes ningún set en curso")
		return
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		var mm models.Matchmaking
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&mm, m.ID).Error; err != nil {
			return err
		}
		if mm.Status == statusPendingVerify {
			return userErr{"ya hay un reporte pendiente: confírmalo o recházalo primero"}
		}
		if mm.Status != statusInProgress {
			return userErr{"el set ya no está en curso"}
		}
		if !mm.P1TeamComplete() || !mm.P2TeamComplete() {
			return userErr{"ambos equipos deben estar completos antes de reportar"}
		}

		myScore, oppScore := body.MyScore, body.OppScore
		if myScore < 0 || oppScore < 0 || myScore > mm.TargetFT || oppScore > mm.TargetFT {
			return userErr{fmt.Sprintf("los games van de 0 a %d", mm.TargetFT)}
		}
		if (myScore == mm.TargetFT) == (oppScore == mm.TargetFT) {
			return userErr{fmt.Sprintf("en FT %d, uno debe terminar con %d y el otro con menos", mm.TargetFT, mm.TargetFT)}
		}

		// Se guarda en perspectiva P1/P2 y queda PENDIENTE para que el rival
		// lo confirme (doble verificación, como en el flujo del bot).
		if mySide(&mm, discordID) == "p1" {
			mm.FinalPick1, mm.FinalPick2 = myScore, oppScore
		} else {
			mm.FinalPick1, mm.FinalPick2 = oppScore, myScore
		}
		mm.PendingGameWinner = pendingFinal
		mm.PendingReportedBy = discordID
		mm.Status = statusPendingVerify
		return tx.Save(&mm).Error
	})
	if err != nil {
		respondTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/match/close  (proponer cierre anticipado)
// ---------------------------------------------------------------------------

// absorbPending anota el game pendiente de verificación (mismo criterio del
// bot al cerrar) y devuelve el set a IN_PROGRESS.
func absorbPending(mm *models.Matchmaking) {
	if mm.PendingGameWinner == 1 || mm.PendingGameWinner == 2 {
		appendRound(mm, mm.PendingGameWinner)
		if mm.PendingGameWinner == 1 {
			mm.Score1++
		} else {
			mm.Score2++
		}
	}
	mm.PendingGameWinner = 0
	mm.PendingReportedBy = ""
	mm.FinalPick1, mm.FinalPick2 = -1, -1
	mm.Status = statusInProgress
}

func handleClose(w http.ResponseWriter, r *http.Request) {
	discordID, ok := mustAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "metodo", "usa POST")
		return
	}

	m, err := activeMatch(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "sin_set", "no tienes ningún set en curso")
		return
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		var mm models.Matchmaking
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&mm, m.ID).Error; err != nil {
			return err
		}
		if mm.Status == statusPendingVerify {
			absorbPending(&mm)
		}
		if mm.Status != statusInProgress {
			return userErr{"el set ya no está en curso"}
		}
		if !isCombatant(&mm, discordID) {
			return userErr{"no eres parte de este set"}
		}
		now := time.Now()
		mm.Status = statusPendingClose
		mm.CloseRequestedAt = &now
		mm.CloseRequestedBy = discordID
		return tx.Save(&mm).Error
	})
	if err != nil {
		respondTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// POST /api/matchmaking/match/close/respond   { "accept": true|false }
// ---------------------------------------------------------------------------

func handleCloseRespond(w http.ResponseWriter, r *http.Request) {
	discordID, ok := mustAuth(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "metodo", "usa POST")
		return
	}

	var body struct {
		Accept bool `json:"accept"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	m, err := activeMatch(discordID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bd", "error interno")
		return
	}
	if m == nil {
		writeErr(w, http.StatusNotFound, "sin_set", "no tienes ningún set en curso")
		return
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		var mm models.Matchmaking
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&mm, m.ID).Error; err != nil {
			return err
		}
		if mm.Status != statusPendingClose {
			return userErr{"no hay ninguna propuesta de cierre pendiente"}
		}
		if !isCombatant(&mm, discordID) {
			return userErr{"no eres parte de este set"}
		}

		if !body.Accept {
			mm.Status = statusInProgress
			mm.CloseRequestedAt = nil
			mm.CloseRequestedBy = ""
			return tx.Save(&mm).Error
		}
		if mm.CloseRequestedBy == discordID {
			return userErr{"espera la respuesta de tu rival (a los 2 min se cierra solo)"}
		}
		// Aceptado: gana quien va ARRIBA con los puntos COMPLETOS del FT;
		// empate = set cancelado sin puntos (igual que el bot).
		return finishWithLeader(tx, &mm)
	})
	if err != nil {
		respondTxErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Liquidación (idéntica a la del bot)
// ---------------------------------------------------------------------------

// finishMatch marca FINISHED y liquida los puntos según el marcador.
func finishMatch(tx *gorm.DB, m *models.Matchmaking) error {
	if m.Score1 > m.Score2 {
		m.WinnerDiscordID, m.LoserDiscordID = m.Player1DiscordID, m.Player2DiscordID
	} else {
		m.WinnerDiscordID, m.LoserDiscordID = m.Player2DiscordID, m.Player1DiscordID
	}
	m.Status = statusFinished
	if err := tx.Save(m).Error; err != nil {
		return err
	}
	return applyMatchResult(tx, m)
}

// finishWithLeader cierra anticipadamente: gana quien va ARRIBA con los
// puntos COMPLETOS del FT; empate = CANCELADO sin puntos.
func finishWithLeader(tx *gorm.DB, m *models.Matchmaking) error {
	if m.Score1 == m.Score2 {
		m.Status = statusCancelled
		return tx.Save(m).Error
	}
	return finishMatch(tx, m)
}

// applyFinal aplica un resultado final ya confirmado por el rival.
func applyFinal(tx *gorm.DB, m *models.Matchmaking) error {
	if m.FinalPick1 < 0 || m.FinalPick2 < 0 || m.FinalPick1 > m.TargetFT || m.FinalPick2 > m.TargetFT {
		return userErr{"resultado inválido"}
	}
	if (m.FinalPick1 != m.TargetFT) == (m.FinalPick2 != m.TargetFT) {
		return userErr{fmt.Sprintf("en FT %d, uno debe terminar con %d y el otro con menos", m.TargetFT, m.TargetFT)}
	}
	m.Score1, m.Score2 = m.FinalPick1, m.FinalPick2
	m.RoundHistory = "[]" // reporte directo: sin secuencia game a game
	m.FinalPick1, m.FinalPick2 = -1, -1
	m.PendingGameWinner = 0
	m.PendingReportedBy = ""
	now := time.Now()
	m.LastRoundAt = &now
	return finishMatch(tx, m)
}

// applyMatchResult suma los puntos al ganador y resta al perdedor SOLO en
// matchmaking_players (réplica exacta del bot: FT 3 equitativo por RANGO —
// las divisiones I/II/III valen igual, con bonus/castigo por diferencia de
// rango, racha de victorias y piso de 5).
func applyMatchResult(tx *gorm.DB, m *models.Matchmaking) error {
	// Garantiza que ambos existan (se crean la primera vez que juegan un set).
	for _, id := range []string{m.Player1DiscordID, m.Player2DiscordID} {
		tag, avatar := sideInfoOf(m, id)
		p := models.MatchmakingPlayer{DiscordID: id, Username: tag, AvatarURL: avatar}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&p).Error; err != nil {
			return err
		}
	}

	// Bloquea ambas filas, siempre en el mismo orden (evita deadlocks cruzados).
	var rows []models.MatchmakingPlayer
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("discord_id IN ?", []string{m.WinnerDiscordID, m.LoserDiscordID}).
		Order("discord_id").Find(&rows).Error; err != nil {
		return err
	}

	byID := make(map[string]*models.MatchmakingPlayer, len(rows))
	for idx := range rows {
		byID[rows[idx].DiscordID] = &rows[idx]
	}
	winner, loser := byID[m.WinnerDiscordID], byID[m.LoserDiscordID]
	if winner == nil || loser == nil {
		return errors.New("no se encontraron los jugadores del set")
	}

	// CLASIFICACIÓN: quien no completó sus 5 sets aún no tiene rango. Sus LP
	// suben/bajan con deltas FIJOS de clasificación y el neto de esos 5 sets
	// decide el rango inicial. El rival ya clasificado cobra con las reglas
	// normales (el clasificante cuenta con el nivel de sus puntos actuales).
	winnerPlacing := winner.Wins+winner.Losses < ranks.PlacementGames
	loserPlacing := loser.Wins+loser.Losses < ranks.PlacementGames

	var stake, penalty int
	if winnerPlacing {
		stake = ranks.PlacementWin
	} else {
		// Puntos por RANGO (FT 3, partidas equitativas): las divisiones I/II/III
		// no importan. Vencer a un rango superior da más puntos; caer ante uno
		// inferior cuesta más. Se recalculan AQUÍ con los rangos reales del final.
		stake = ranks.WinPoints(winner.Points, loser.Points)
		// Bonos de LP: racha caliente (la 3ª victoria seguida y las siguientes
		// pagan extra; winner.Streak aún es la racha ANTES de este set) y set
		// perfecto sin ceder games. Se incluyen en WinnerDelta para que la sala
		// muestre el LP real ganado.
		stake += ranks.StreakBonus(winner.Streak)
		winScore, loseScore := m.Score1, m.Score2
		if winScore < loseScore {
			winScore, loseScore = loseScore, winScore
		}
		if ranks.IsFlawless(winScore, loseScore, m.TargetFT) {
			stake += ranks.FlawlessBonus
		}
	}
	if loserPlacing {
		penalty = ranks.PlacementLoss
	} else {
		penalty = ranks.LosePoints(winner.Points, loser.Points)
	}

	loserNew := loser.Points - penalty
	if loserPlacing {
		// Piso de la clasificación: con 0V-5D se aterriza en Plata II y no se
		// baja más por perder en clasificación.
		if loserNew < ranks.PlacementFloor {
			loserNew = ranks.PlacementFloor
		}
	} else {
		if loserNew < ranks.MinPointsFloor {
			loserNew = ranks.MinPointsFloor
		}
		if loserNew > loser.Points { // perder nunca sube los puntos
			loserNew = loser.Points
		}
		// Escudo anti-descenso: si la derrota lo bajaría de división y tiene el
		// escudo (lo gana al ascender), cae solo a 0 LP de SU división y lo gasta.
		if oldIdx := ranks.DivisionIndexOf(loser.Points); ranks.DivisionIndexOf(loserNew) < oldIdx && loser.DemotionShield {
			if floor := ranks.DivisionFloor(oldIdx); loserNew < floor {
				loserNew = floor
			}
			loser.DemotionShield = false
		}
	}

	m.PointsAtStake = stake
	m.WinnerDelta = stake
	m.LoserDelta = loserNew - loser.Points

	// Ascender de división (re)otorga el escudo anti-descenso (ya clasificado).
	if !winnerPlacing && ranks.DivisionIndexOf(winner.Points+stake) > ranks.DivisionIndexOf(winner.Points) {
		winner.DemotionShield = true
	}
	winner.Points += stake
	winner.Wins++
	winner.Streak++ // racha de victorias: 3+ empareja contra un rango superior

	loser.Points = loserNew
	loser.Losses++
	loser.Streak = 0

	// Mantiene actualizados nombre y avatar.
	if tag, avatar := sideInfoOf(m, winner.DiscordID); tag != "" {
		winner.Username = tag
		if avatar != "" {
			winner.AvatarURL = avatar
		}
	}
	if tag, avatar := sideInfoOf(m, loser.DiscordID); tag != "" {
		loser.Username = tag
		if avatar != "" {
			loser.AvatarURL = avatar
		}
	}

	if err := tx.Save(winner).Error; err != nil {
		return err
	}
	if err := tx.Save(loser).Error; err != nil {
		return err
	}
	// El set ya estaba guardado como FINISHED: persistimos el stake real y
	// los deltas aplicados para que la sala muestre los puntos exactos.
	return tx.Save(m).Error
}

// ---------------------------------------------------------------------------
// Sweeper: cierres sin respuesta y sets abandonados
// ---------------------------------------------------------------------------

func startSweeper() {
	ticker := time.NewTicker(sweepInterval)
	go func() {
		for range ticker.C {
			sweep()
		}
	}()
	fmt.Println("🔄 Sweeper de la sala web activo (cierres y sets abandonados).")
}

func sweep() {
	now := time.Now()

	// 1) Cierre propuesto sin respuesta: a los 2 min gana quien va arriba
	//    (solo sets web; los del bot los cuida su janitor).
	var closing []models.Matchmaking
	if err := db.Where("status = ? AND source = ? AND close_requested_at <= ?",
		statusPendingClose, "web", now.Add(-closeAutoAfter)).Find(&closing).Error; err != nil {
		log.Printf("❌ WebMatch (sweeper cierres): %v\n", err)
	}
	for _, m := range closing {
		err := db.Transaction(func(tx *gorm.DB) error {
			var mm models.Matchmaking
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&mm, m.ID).Error; err != nil {
				return err
			}
			if mm.Status != statusPendingClose {
				return nil // ya fue resuelto
			}
			return finishWithLeader(tx, &mm)
		})
		if err != nil {
			log.Printf("❌ WebMatch (auto-cierre set %d): %v\n", m.ID, err)
		}
	}

	// 2) Sets web abandonados (pestaña cerrada, nadie reporta): se cancelan.
	if err := db.Model(&models.Matchmaking{}).
		Where("status IN ? AND source = ? AND last_round_at <= ?",
			[]string{statusInProgress, statusPendingVerify}, "web", now.Add(-staleAfter)).
		Update("status", statusCancelled).Error; err != nil {
		log.Printf("❌ WebMatch (cancelar abandonados): %v\n", err)
	}

	// 3) Chat de sets terminados hace más de 7 días: se borra para que la
	//    tabla no crezca para siempre.
	chatCutoff := now.Add(-7 * 24 * time.Hour)
	var oldIDs []uint
	if err := db.Model(&models.Matchmaking{}).
		Where("status IN ? AND updated_at < ?", []string{statusFinished, statusCancelled}, chatCutoff).
		Limit(500).Pluck("id", &oldIDs).Error; err != nil {
		log.Printf("❌ WebMatch (limpieza de chat): %v\n", err)
	} else if len(oldIDs) > 0 {
		db.Where("match_id IN ?", oldIDs).Delete(&models.MatchmakingMessage{})
	}
}
