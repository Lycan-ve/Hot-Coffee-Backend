package auth

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"smash-backend/internal/gamechars"
	"smash-backend/internal/models"
	"smash-backend/internal/presence"
	"smash-backend/internal/ranks"

	jwt "github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
)

const (
	stateCookieName = "hc_oauth_state"
	tokenTTL        = 24 * time.Hour
)

var db *gorm.DB

func backendURL() string {
	return strings.TrimRight(os.Getenv("BACKEND_URL"), "/")
}

func frontendURL() string {
	return strings.TrimRight(os.Getenv("FRONTEND_URL"), "/")
}

// ---------------------- Discord OAuth2 ----------------------

func authorizeURL(state string) string {
	v := url.Values{}
	v.Set("client_id", os.Getenv("DISCORD_CLIENT_ID"))
	v.Set("redirect_uri", backendURL()+"/api/auth/discord/callback")
	v.Set("response_type", "code")
	v.Set("scope", "identify guilds.members.read")
	v.Set("state", state)
	return "https://discord.com/oauth2/authorize?" + v.Encode()
}

func exchangeCode(code string) (string, error) {
	v := url.Values{}
	v.Set("client_id", os.Getenv("DISCORD_CLIENT_ID"))
	v.Set("client_secret", os.Getenv("DISCORD_CLIENT_SECRET"))
	v.Set("grant_type", "authorization_code")
	v.Set("code", code)
	v.Set("redirect_uri", backendURL()+"/api/auth/discord/callback")

	res, err := http.PostForm("https://discord.com/api/oauth2/token", v)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("discord no devolvió access_token")
	}
	return out.AccessToken, nil
}

type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Avatar     string `json:"avatar"`
}

func fetchDiscordUser(token string) (discordUser, error) {
	req, _ := http.NewRequest("GET", "https://discord.com/api/users/@me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return discordUser{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return discordUser{}, fmt.Errorf("discord respondió %d al pedir el usuario", res.StatusCode)
	}
	u := discordUser{}
	if err := json.NewDecoder(res.Body).Decode(&u); err != nil {
		return discordUser{}, err
	}
	return u, nil
}

type discordMember struct {
	Nick  string   `json:"nick"`
	Roles []string `json:"roles"`
}

// fetchGuildMember usa el scope guilds.members.read del USUARIO (no del bot).
// Devuelve (nil, nil) si el usuario NO es miembro del servidor.
func fetchGuildMember(token, guildID string) (*discordMember, error) {
	endpoint := fmt.Sprintf("https://discord.com/api/users/@me/guilds/%s/member", guildID)
	req, _ := http.NewRequest("GET", endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, nil // no es miembro del servidor
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discord respondió %d al pedir la membresía", res.StatusCode)
	}
	m := &discordMember{}
	if err := json.NewDecoder(res.Body).Decode(m); err != nil {
		return nil, err
	}
	return m, nil
}

func avatarURL(u discordUser) string {
	if u.Avatar != "" {
		// Los avatares animados de Discord tienen hash con prefijo "a_" y se
		// sirven como .gif (con .png devolverían 404 y el avatar saldría roto).
		ext := ".png"
		if strings.HasPrefix(u.Avatar, "a_") {
			ext = ".gif"
		}
		return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s%s?size=128", u.ID, u.Avatar, ext)
	}
	// Avatar por defecto según el snowflake: (id >> 22) % 6
	id, err := strconv.ParseUint(u.ID, 10, 64)
	if err != nil {
		return "https://cdn.discordapp.com/embed/avatars/0.png"
	}
	return fmt.Sprintf("https://cdn.discordapp.com/embed/avatars/%d.png", (id>>22)%6)
}

// ---------------------- JWT de sesión ----------------------

func makeToken(userID string) (string, error) {
	secret := os.Getenv("AUTH_JWT_SECRET")
	if secret == "" {
		return "", fmt.Errorf("AUTH_JWT_SECRET no está configurado")
	}
	claims := jwt.MapClaims{
		"sub": userID,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(tokenTTL).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

func parseToken(raw string) (string, error) {
	secret := os.Getenv("AUTH_JWT_SECRET")
	tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("método de firma inesperado")
		}
		return []byte(secret), nil
	})
	if err != nil || !tok.Valid {
		return "", fmt.Errorf("token inválido o expirado")
	}
	sub, _ := tok.Claims.(jwt.MapClaims)["sub"].(string)
	if sub == "" {
		return "", fmt.Errorf("token sin usuario")
	}
	return sub, nil
}

// UserIDFromRequest valida el header "Authorization: Bearer <jwt>" y
// devuelve el Discord ID del usuario. Versión exportada para otros
// paquetes de la API (p. ej. la cola de matchmaking web).
func UserIDFromRequest(r *http.Request) (string, bool) {
	return bearerDiscordID(r)
}

// PreflightCors escribe los headers CORS y responde OPTIONS si corresponde.
// Devuelve true si la petición ya fue respondida (preflight).
func PreflightCors(w http.ResponseWriter, r *http.Request) bool {
	return setCors(w, r)
}

// ---------------------- Utilidades HTTP ----------------------

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// setCors habilita CORS para el frontend de Vercel y responde OPTIONS.
func setCors(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func bearerDiscordID(r *http.Request) (string, bool) {
	raw := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	if raw == "" || raw == r.Header.Get("Authorization") {
		return "", false
	}
	id, err := parseToken(raw)
	if err != nil {
		return "", false
	}
	return id, true
}

func redirectWithError(w http.ResponseWriter, r *http.Request, code string) {
	http.Redirect(w, r, frontendURL()+"/perfil?error="+url.QueryEscape(code), http.StatusTemporaryRedirect)
}

// ---------------------- Handlers ----------------------

// HandleLogin redirige al usuario a la pantalla de autorización de Discord.
func handleLogin(w http.ResponseWriter, r *http.Request) {
	state := randHex(16)
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    state,
		Path:     "/api/auth",
		MaxAge:   600, // 10 minutos para completar el login
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, authorizeURL(state), http.StatusTemporaryRedirect)
}

// HandleCallback: Discord regresa aquí con ?code=...&state=...
// Valida el state, canjea el código, verifica la membresía del servidor,
// guarda/actualiza el usuario, firma un JWT y manda al frontend a /perfil#token=...
func handleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")

	ck, err := r.Cookie(stateCookieName)
	http.SetCookie(w, &http.Cookie{Name: stateCookieName, Value: "", Path: "/api/auth", MaxAge: -1})
	if err != nil || ck == nil || ck.Value == "" || ck.Value != state || code == "" {
		redirectWithError(w, r, "estado_invalido")
		return
	}

	token, err := exchangeCode(code)
	if err != nil {
		log.Println("❌ OAuth: error canjeando el código:", err)
		redirectWithError(w, r, "token")
		return
	}

	u, err := fetchDiscordUser(token)
	if err != nil {
		log.Println("❌ OAuth: error obteniendo el usuario:", err)
		redirectWithError(w, r, "usuario")
		return
	}

	member, err := fetchGuildMember(token, os.Getenv("DISCORD_GUILD_ID"))
	if err != nil {
		log.Println("❌ OAuth: error verificando membresía:", err)
		redirectWithError(w, r, "servidor")
		return
	}
	if member == nil {
		// No es miembro de Hot Coffee 2XKO → acceso denegado
		redirectWithError(w, r, "no_miembro")
		return
	}

	// Upsert del usuario en la BD
	now := time.Now()
	var existing models.User
	if err := db.Where("discord_id = ?", u.ID).First(&existing).Error; err != nil {
		user := models.User{
			DiscordID:     u.ID,
			Username:      u.Username,
			GlobalName:    u.GlobalName,
			GuildNickname: member.Nick,
			AvatarURL:     avatarURL(u),
			Roles:         strings.Join(member.Roles, ","),
			FirstLogin:    now,
			LastLogin:     now,
		}
		if err := db.Create(&user).Error; err != nil {
			log.Println("❌ OAuth: error creando usuario:", err)
			redirectWithError(w, r, "servidor")
			return
		}
	} else {
		existing.Username = u.Username
		existing.GlobalName = u.GlobalName
		existing.GuildNickname = member.Nick
		existing.AvatarURL = avatarURL(u)
		existing.Roles = strings.Join(member.Roles, ",")
		existing.LastLogin = now
		if err := db.Save(&existing).Error; err != nil {
			log.Println("❌ OAuth: error actualizando usuario:", err)
			redirectWithError(w, r, "servidor")
			return
		}
	}

	jwtStr, err := makeToken(u.ID)
	if err != nil {
		log.Println("❌ OAuth:", err)
		redirectWithError(w, r, "servidor")
		return
	}

	http.Redirect(w, r, frontendURL()+"/perfil#token="+url.QueryEscape(jwtStr), http.StatusTemporaryRedirect)
}

type profileCharacter struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type profileTeam struct {
	Point  profileCharacter `json:"point"`
	Assist profileCharacter `json:"assist"`
	Fuse   profileCharacter `json:"fuse"`
}

type profileFight struct {
	ID                uint        `json:"id"`
	PlayedAt          time.Time   `json:"playedAt"`
	Result            string      `json:"result"`
	ScoreFor          int         `json:"scoreFor"`
	ScoreAgainst      int         `json:"scoreAgainst"`
	TargetFT          int         `json:"targetFT"`
	OpponentTag       string      `json:"opponentTag"`
	OpponentAvatar    string      `json:"opponentAvatar"`
	OpponentDiscordID string      `json:"opponentDiscordId"`
	PlayerTeam        profileTeam `json:"playerTeam"`
	OpponentTeam      profileTeam `json:"opponentTeam"`
}

type profileTournamentRow struct {
	EventID   int    `json:"eventId"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	Points    int    `json:"points"`
	Placement int    `json:"placement"`
	Image     string `json:"image"`
}

type profileTournament struct {
	EventID        int    `json:"eventId"`
	Name           string `json:"name"`
	Slug           string `json:"slug"`
	Image          string `json:"image"`
	Points         int    `json:"points"`
	Placement      int    `json:"placement"`
	PlacementLabel string `json:"placementLabel"`
	IsWinner       bool   `json:"isWinner"`
}

func makeProfileTeam(pointID, assistID, fuseID string) profileTeam {
	return profileTeam{
		Point:  profileCharacter{ID: pointID, Name: gamechars.CharacterName(pointID)},
		Assist: profileCharacter{ID: assistID, Name: gamechars.CharacterName(assistID)},
		Fuse:   profileCharacter{ID: fuseID, Name: gamechars.FuseName(fuseID)},
	}
}

func getProfileFights(discordID string) []profileFight {
	fights := make([]profileFight, 0)
	var records []models.Matchmaking
	if err := db.
		Where("status = ? AND (player1_discord_id = ? OR player2_discord_id = ?)", "FINISHED", discordID, discordID).
		Order("created_at DESC").
		Limit(25).
		Find(&records).Error; err != nil {
		log.Printf("⚠️ No se pudo consultar el historial de peleas: %v", err)
		return fights
	}

	for _, record := range records {
		fight := profileFight{
			ID:       record.ID,
			PlayedAt: record.CreatedAt,
			TargetFT: record.TargetFT,
		}
		if record.Player1DiscordID == discordID {
			fight.ScoreFor = record.Score1
			fight.ScoreAgainst = record.Score2
			fight.OpponentTag = record.Player2Tag
			fight.OpponentAvatar = record.Player2Avatar
			fight.OpponentDiscordID = record.Player2DiscordID
			fight.PlayerTeam = makeProfileTeam(record.P1Char1, record.P1Char2, record.P1Fuse)
			fight.OpponentTeam = makeProfileTeam(record.P2Char1, record.P2Char2, record.P2Fuse)
		} else {
			fight.ScoreFor = record.Score2
			fight.ScoreAgainst = record.Score1
			fight.OpponentTag = record.Player1Tag
			fight.OpponentAvatar = record.Player1Avatar
			fight.OpponentDiscordID = record.Player1DiscordID
			fight.PlayerTeam = makeProfileTeam(record.P2Char1, record.P2Char2, record.P2Fuse)
			fight.OpponentTeam = makeProfileTeam(record.P1Char1, record.P1Char2, record.P1Fuse)
		}
		if record.WinnerDiscordID == discordID {
			fight.Result = "WIN"
		} else {
			fight.Result = "LOSS"
		}
		fights = append(fights, fight)
	}
	return fights
}

func placementLabel(placement, points int) string {
	if placement > 0 {
		return fmt.Sprintf("%d.º puesto", placement)
	}
	// Los resultados históricos guardan puntos, no el puesto exacto. Algunos
	// valores representan puestos empatados, por eso se muestran como rango.
	switch points {
	case 100:
		return "1.º puesto"
	case 70:
		return "2.º puesto"
	case 50:
		return "3.º puesto"
	case 35:
		return "4.º puesto"
	case 20:
		return "5.º–6.º puesto"
	case 10:
		return "7.º–8.º puesto"
	default:
		return "Puesto registrado"
	}
}

func getProfileTournaments(discordID string) []profileTournament {
	const league = "2xko_online"
	byEvent := make(map[int]profileTournament)

	// Compatibilidad con resultados antiguos: se migran visualmente desde el
	// ranking de puntos, donde las posiciones 5-6 y 7-8 estaban agrupadas.
	var players []models.Player
	if err := db.Select("id").Where("discord_id = ? AND league = ?", discordID, league).Find(&players).Error; err != nil {
		log.Printf("⚠️ No se pudo consultar el jugador del ranking de torneos: %v", err)
	}
	playerIDs := make([]int, 0, len(players))
	for _, player := range players {
		playerIDs = append(playerIDs, player.ID)
	}
	if len(playerIDs) > 0 {
		var legacyRows []profileTournamentRow
		err := db.Table("player_results AS pr").
			Select("pr.event_id AS event_id, pe.name AS name, pe.slug AS slug, SUM(pr.points) AS points, 0 AS placement, COALESCE(pe.image, '') AS image").
			Joins("JOIN processed_events AS pe ON pe.event_id = pr.event_id").
			Where("pr.player_id IN ? AND pr.league = ?", playerIDs, league).
			Group("pr.event_id, pe.name, pe.slug, pe.image").
			Order("pr.event_id DESC").
			Scan(&legacyRows).Error
		if err != nil {
			log.Printf("⚠️ No se pudieron consultar los resultados históricos de torneos: %v", err)
		} else {
			for _, row := range legacyRows {
				byEvent[row.EventID] = profileTournament{
					EventID:        row.EventID,
					Name:           row.Name,
					Slug:           row.Slug,
					Image:          row.Image,
					Points:         row.Points,
					Placement:      0,
					PlacementLabel: placementLabel(0, row.Points),
					IsWinner:       row.Points == 100,
				}
			}
		}
	}

	// Los datos nuevos guardan el puesto real, incluso para ubicaciones fuera
	// del Top 8. Tienen prioridad sobre la etiqueta aproximada histórica.
	var exactRows []profileTournamentRow
	if err := db.Table("tournament_placements AS tp").
		Select("tp.event_id AS event_id, pe.name AS name, pe.slug AS slug, tp.points AS points, tp.placement AS placement, COALESCE(pe.image, '') AS image").
		Joins("JOIN processed_events AS pe ON pe.event_id = tp.event_id").
		Where("tp.discord_id = ? AND tp.league = ?", discordID, league).
		Order("tp.event_id DESC").
		Scan(&exactRows).Error; err != nil {
		log.Printf("⚠️ No se pudieron consultar las posiciones de torneos: %v", err)
	} else {
		for _, row := range exactRows {
			byEvent[row.EventID] = profileTournament{
				EventID:        row.EventID,
				Name:           row.Name,
				Slug:           row.Slug,
				Image:          row.Image,
				Points:         row.Points,
				Placement:      row.Placement,
				PlacementLabel: placementLabel(row.Placement, row.Points),
				IsWinner:       row.Placement == 1,
			}
		}
	}

	tournaments := make([]profileTournament, 0, len(byEvent))
	for _, tournament := range byEvent {
		tournaments = append(tournaments, tournament)
	}
	sort.Slice(tournaments, func(i, j int) bool {
		return tournaments[i].EventID > tournaments[j].EventID
	})
	return tournaments
}

// profileRank: rango del jugador para el perfil. Nadie parte con rango:
// sin las 5 partidas de clasificación completadas el display es "SIN RANGO"
// (sin emblema ni LP) mientras el jugador define su rango inicial con los
// 5 sets de clasificación.
func profileRank(mp models.MatchmakingPlayer) ranks.Info {
	return ranks.InfoFor(mp.Points, mp.Wins+mp.Losses)
}

// writeProfile arma el perfil completo (identidad, stats, rango e historial).
// Sirve tanto a /api/auth/me como a la ruta pública de perfiles.
// publicUser expone solo los campos del perfil que usa el frontend. Nunca se
// envían Roles, FirstLogin ni LastLogin (información interna del servidor).
type publicUser struct {
	DiscordID     string `json:"discordId"`
	Username      string `json:"username"`
	GlobalName    string `json:"globalName"`
	GuildNickname string `json:"guildNickname"`
	AvatarURL     string `json:"avatarUrl"`
}

func writeProfile(w http.ResponseWriter, discordID string, includeHistory bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	var user models.User
	if err := db.Where("discord_id = ?", discordID).First(&user).Error; err != nil {
		// El jugador puede existir en los rankings aunque nunca haya iniciado
		// sesión en la web: se reconstruye su identidad pública desde el
		// ranking de matchmaking o el de torneos para que cualquiera pueda
		// ver su perfil.
		var mpFound models.MatchmakingPlayer
		fromMatchmaking := db.Where("discord_id = ?", discordID).First(&mpFound).Error == nil
		var tPlayer models.Player
		fromTournaments := db.Where("discord_id = ?", discordID).Order("points DESC").First(&tPlayer).Error == nil
		if !fromMatchmaking && !fromTournaments {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "usuario_no_encontrado"})
			return
		}
		user = models.User{DiscordID: discordID}
		if fromMatchmaking {
			user.Username = mpFound.Username
			user.AvatarURL = mpFound.AvatarURL
		}
		if fromTournaments {
			user.GlobalName = tPlayer.GamerTag
			if !fromMatchmaking {
				user.Username = tPlayer.GamerTag
				user.AvatarURL = tPlayer.AvatarURL
			}
		}
	}

	// Estadísticas de sets registrados (puede que el usuario aún no tenga partidas).
	var mp models.MatchmakingPlayer
	mpFound := db.Where("discord_id = ?", discordID).First(&mp).Error == nil

	// El ranking de matchmaking parte de Gold I (1.000 pts): los puntos
	// mostrados son el total (base + ganados); los ganados se guardan desde 0.
	mmEarned := mp.Points
	if mmEarned < 0 {
		mmEarned = 0
	}

	// Posición en la tabla (cuántos jugadores tienen más puntos + 1).
	position := 0
	if mpFound {
		var ahead int64
		db.Model(&models.MatchmakingPlayer{}).Where("points > ?", mp.Points).Count(&ahead)
		position = int(ahead) + 1
	}

	// Torneos: puntos acumulados y posición en la tabla de TORNEOS (independiente
	// del matchmaking). La mejor entrada del jugador define sus puntos; la
	// posición es cuántos jugadores tienen más puntos + 1, como en el ranking web.
	tournamentPoints := 0
	var tBest models.Player
	if err := db.Where("discord_id = ? AND league = ?", discordID, "2xko_online").Order("points DESC").First(&tBest).Error; err == nil {
		tournamentPoints = tBest.Points
	}
	tournamentPosition := 0
	if tournamentPoints > 0 {
		var ahead int64
		db.Model(&models.Player{}).Where("league = ? AND points > ?", "2xko_online", tournamentPoints).Count(&ahead)
		tournamentPosition = int(ahead) + 1
	}

	response := map[string]any{
		"user": publicUser{
			DiscordID:     user.DiscordID,
			Username:      user.Username,
			GlobalName:    user.GlobalName,
			GuildNickname: user.GuildNickname,
			AvatarURL:     user.AvatarURL,
		},
		"stats": map[string]any{
			"points":             ranks.BasePoints + mmEarned,
			"wins":               mp.Wins,
			"losses":             mp.Losses,
			"rankingPosition":    position,
			"tournamentPoints":   tournamentPoints,
			"tournamentPosition": tournamentPosition,
		},
		"rank":  profileRank(mp),
		"mains": makeProfileTeam(user.MainChar1, user.MainChar2, user.MainFuse),
	}
	// Estado de Discord en vivo (lo escribe el bot desde el Gateway).
	response["presence"] = presence.Get(discordID)
	if includeHistory {
		response["fights"] = getProfileFights(discordID)
		response["tournaments"] = getProfileTournaments(discordID)
	}
	json.NewEncoder(w).Encode(response)
}

// HandleMe: GET /api/auth/me con header "Authorization: Bearer <token>".
func handleMe(w http.ResponseWriter, r *http.Request) {
	if setCors(w, r) {
		return
	}
	discordID, ok := bearerDiscordID(r)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "no_autenticado"})
		return
	}
	writeProfile(w, discordID, true)
}

// HandlePublicProfile: GET /api/users/profile/{discordId} (para ver perfiles ajenos).
func handlePublicProfile(w http.ResponseWriter, r *http.Request) {
	if setCors(w, r) {
		return
	}
	discordID := strings.TrimPrefix(r.URL.Path, "/api/users/profile/")
	if discordID == "" || strings.Contains(discordID, "/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// Perfil público completo: cualquiera (sin iniciar sesión) puede ver la
	// ficha de cualquier jugador, incluido su historial.
	writeProfile(w, discordID, true)
}

// handleMains: GET/POST /api/auth/mains — equipo principal (mains) del jugador.
// GET devuelve el equipo guardado; POST valida los IDs contra el roster de
// 2XKO y lo guarda en la fila del usuario.
func handleMains(w http.ResponseWriter, r *http.Request) {
	if setCors(w, r) {
		return
	}
	discordID, ok := bearerDiscordID(r)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "no_autenticado"})
		return
	}

	var user models.User
	if err := db.Where("discord_id = ?", discordID).First(&user).Error; err != nil {
		// El JWT solo lo tiene quien inició sesión: la fila debería existir,
		// pero por robustez se crea si falta.
		user = models.User{DiscordID: discordID, FirstLogin: time.Now(), LastLogin: time.Now()}
	}

	if r.Method == http.MethodPost {
		var body struct {
			Char1 string `json:"char1"`
			Char2 string `json:"char2"`
			Fuse  string `json:"fuse"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "cuerpo_invalido",
				"message": "Envía char1, char2 y fuse en formato JSON.",
			})
			return
		}
		if !gamechars.ValidCharacter(body.Char1) || !gamechars.ValidCharacter(body.Char2) || !gamechars.ValidFuse(body.Fuse) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "equipo_invalido",
				"message": "Los personajes y la fusión deben ser del roster de 2XKO.",
			})
			return
		}
		user.MainChar1 = body.Char1
		user.MainChar2 = body.Char2
		user.MainFuse = body.Fuse
		if err := db.Save(&user).Error; err != nil {
			log.Println("❌ Mains: error guardando el equipo:", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "error_interno"})
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"mains": makeProfileTeam(user.MainChar1, user.MainChar2, user.MainFuse),
	})
}

// RegisterRoutes registra todas las rutas de auth y perfiles.
func RegisterRoutes(database *gorm.DB) {
	db = database
	http.HandleFunc("/api/auth/discord/login", handleLogin)
	http.HandleFunc("/api/auth/discord/callback", handleCallback)
	http.HandleFunc("/api/auth/me", handleMe)
	http.HandleFunc("/api/auth/mains", handleMains)
	http.HandleFunc("/api/users/profile/", handlePublicProfile)
	fmt.Println("✅ Rutas de autenticación y perfiles registradas.")
}

