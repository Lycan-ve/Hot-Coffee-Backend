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
	"strconv"
	"strings"
	"time"

	"smash-backend/internal/models"
	"smash-backend/internal/ranks"

	jwt "github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
)

const (
	stateCookieName = "hc_oauth_state"
	tokenTTL        = 7 * 24 * time.Hour
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
		return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png?size=128", u.ID, u.Avatar)
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

// writeProfile arma la respuesta de perfil: usuario + stats de matchmaking + rango.
func writeProfile(w http.ResponseWriter, discordID string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	var user models.User
	if err := db.Where("discord_id = ?", discordID).First(&user).Error; err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "usuario_no_encontrado"})
		return
	}

	// Estadísticas de matchmaking (puede que el usuario nunca haya jugado un /ft)
	var mp models.MatchmakingPlayer
	db.Where("discord_id = ?", discordID).First(&mp)

	// Posición en el ranking de matchmaking (cuántos tienen más puntos + 1)
	position := 0
	if mp.Points > 0 {
		var ahead int64
		db.Model(&models.MatchmakingPlayer{}).Where("points > ?", mp.Points).Count(&ahead)
		position = int(ahead) + 1
	}

	json.NewEncoder(w).Encode(map[string]any{
		"user": user,
		"stats": map[string]any{
			"points":          mp.Points,
			"wins":            mp.Wins,
			"losses":          mp.Losses,
			"rankingPosition": position,
		},
		"rank": ranks.FromPoints(mp.Points),
	})
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
	writeProfile(w, discordID)
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
	writeProfile(w, discordID)
}

// RegisterRoutes registra todas las rutas de auth y perfiles.
func RegisterRoutes(database *gorm.DB) {
	db = database
	http.HandleFunc("/api/auth/discord/login", handleLogin)
	http.HandleFunc("/api/auth/discord/callback", handleCallback)
	http.HandleFunc("/api/auth/me", handleMe)
	http.HandleFunc("/api/users/profile/", handlePublicProfile)
	fmt.Println("✅ Rutas de autenticación y perfiles registradas.")
}
