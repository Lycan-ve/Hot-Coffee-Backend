package auth

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"smash-backend/internal/models"
)

// Plataformas de juego válidas. La comunidad de 2XKO juega en PC y en
// consolas, así que la plataforma es parte de la identidad del perfil.
var platformNames = map[string]string{
	"PC":   "PC",
	"PS5":  "PlayStation 5",
	"XBOX": "Xbox",
}

// Redes sociales permitidas: clave -> prefijo de la URL pública. Solo se
// guardan identificadores, nunca URLs libres, para evitar spam o phishing.
var socialDomains = map[string]string{
	"twitter":   "https://twitter.com/",
	"twitch":    "https://twitch.tv/",
	"youtube":   "https://youtube.com/@",
	"instagram": "https://instagram.com/",
	"tiktok":    "https://tiktok.com/@",
	"startgg":   "https://start.gg/",
}

const (
	maxBioLength = 200
	maxPlatforms = 3
	maxSocials   = 4
)

// Identificadores admitidos en redes: letras, números, punto, guion y
// guion bajo. Cubre usuarios de Twitter/Twitch/Instagram, de TikTok y
// slugs de start.gg.
var handlePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,40}$`)

// socialLink es la forma pública de una red social guardada.
type socialLink struct {
	Kind   string `json:"kind"`
	Handle string `json:"handle"`
	URL    string `json:"url"`
}

// socialRequest es la forma con la que la web envía una red.
type socialRequest struct {
	Kind   string `json:"kind"`
	Handle string `json:"handle"`
}

func validPlatform(code string) bool {
	_, ok := platformNames[code]
	return ok
}

func containsStr(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// platformsOf convierte la lista guardada ("PC,PS5") en códigos válidos.
func platformsOf(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		code := strings.ToUpper(strings.TrimSpace(part))
		if code == "" || !validPlatform(code) || containsStr(out, code) {
			continue
		}
		out = append(out, code)
	}
	return out
}

// socialsOf convierte la lista guardada ("twitch:lycan,...") en enlaces.
func socialsOf(raw string) []socialLink {
	out := []socialLink{}
	for _, part := range strings.Split(raw, ",") {
		pieces := strings.SplitN(strings.TrimSpace(part), ":", 2)
		if len(pieces) != 2 {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(pieces[0]))
		prefix, ok := socialDomains[kind]
		if !ok || pieces[1] == "" {
			continue
		}
		out = append(out, socialLink{Kind: kind, Handle: pieces[1], URL: prefix + pieces[1]})
	}
	return out
}

// handleProfile: GET/POST /api/auth/profile — preferencias visibles del
// jugador: bio corta, plataformas de juego (PC/PS5/XBOX) y redes sociales.
// Todo se expone también en el perfil público /api/users/profile/{id}.
func handleProfile(w http.ResponseWriter, r *http.Request) {
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
		// Igual que en handleMains/country: la fila debería existir tras el
		// login, se crea por robustez.
		user = models.User{DiscordID: discordID, FirstLogin: time.Now(), LastLogin: time.Now()}
	}

	if r.Method == http.MethodPost {
		var body struct {
			Bio       string          `json:"bio"`
			Platforms []string        `json:"platforms"`
			Socials   []socialRequest `json:"socials"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "cuerpo_invalido",
				"message": "Envía bio, platforms y socials en formato JSON.",
			})
			return
		}

		// Bio: una línea, sin espacios redundantes y con límite duro.
		bio := strings.Join(strings.Fields(body.Bio), " ")
		if len([]rune(bio)) > maxBioLength {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "bio_muy_larga",
				"message": "La bio no puede pasar de 200 caracteres.",
			})
			return
		}

		// Plataformas: solo códigos válidos, sin repetidos, máximo 3.
		platforms := []string{}
		for _, part := range body.Platforms {
			code := strings.ToUpper(strings.TrimSpace(part))
			if code == "" {
				continue
			}
			if !validPlatform(code) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{
					"error":   "plataforma_invalida",
					"message": "Las plataformas admitidas son PC, PS5 y XBOX.",
				})
				return
			}
			if !containsStr(platforms, code) {
				platforms = append(platforms, code)
			}
		}
		if len(platforms) > maxPlatforms {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "demasiadas_plataformas",
				"message": "Máximo 3 plataformas.",
			})
			return
		}

		// Redes: solo dominios de la lista y usuarios con formato válido.
		socials := []string{}
		seen := map[string]bool{}
		for _, s := range body.Socials {
			kind := strings.ToLower(strings.TrimSpace(s.Kind))
			handle := strings.TrimSpace(s.Handle)
			if kind == "" && handle == "" {
				continue // red limpiada por el usuario
			}
			if _, isAllowed := socialDomains[kind]; !isAllowed {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{
					"error":   "red_invalida",
					"message": "Esa red social no está disponible.",
				})
				return
			}
			if !handlePattern.MatchString(handle) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{
					"error":   "usuario_invalido",
					"message": "El usuario de la red tiene caracteres no válidos.",
				})
				return
			}
			if !seen[kind] {
				seen[kind] = true
				socials = append(socials, kind+":"+handle)
			}
		}
		if len(socials) > maxSocials {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "demasiadas_redes",
				"message": "Máximo 4 redes sociales.",
			})
			return
		}

		user.Bio = bio
		user.Platforms = strings.Join(platforms, ",")
		user.Socials = strings.Join(socials, ",")
		if err := db.Save(&user).Error; err != nil {
			log.Println("❌ Profile: error guardando el perfil:", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "error_interno"})
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"bio":       user.Bio,
		"platforms": platformsOf(user.Platforms),
		"socials":   socialsOf(user.Socials),
	})
}
