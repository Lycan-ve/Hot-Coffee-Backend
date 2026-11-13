package auth

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"smash-backend/internal/models"
)

// countries lista los países disponibles para la bandera de nacionalidad
// (ISO 3166-1 alpha-2). La liga vive en el continente americano, pero se
// acepta el resto del mundo para no dejar fuera a nadie de la comunidad.
var countries = map[string]string{
	"AG": "Antigua y Barbuda", "AR": "Argentina", "BS": "Bahamas",
	"BB": "Barbados", "BZ": "Belice", "BO": "Bolivia", "BR": "Brasil",
	"CA": "Canadá", "CL": "Chile", "CO": "Colombia", "CR": "Costa Rica",
	"CU": "Cuba", "DM": "Dominica", "DO": "República Dominicana",
	"EC": "Ecuador", "SV": "El Salvador", "GD": "Granada", "GT": "Guatemala",
	"GY": "Guyana", "HT": "Haití", "HN": "Honduras", "JM": "Jamaica",
	"MX": "México", "NI": "Nicaragua", "PA": "Panamá", "PY": "Paraguay",
	"PE": "Perú", "KN": "San Cristóbal y Nieves", "LC": "Santa Lucía",
	"VC": "San Vicente y las Granadinas", "SR": "Surinam",
	"TT": "Trinidad y Tobago", "US": "Estados Unidos", "UY": "Uruguay",
	"VE": "Venezuela",
	"ES": "España", "PT": "Portugal", "FR": "Francia", "GB": "Reino Unido",
	"DE": "Alemania", "IT": "Italia", "NL": "Países Bajos", "PL": "Polonia",
	"RO": "Rumanía", "JP": "Japón", "KR": "Corea del Sur", "CN": "China",
	"PH": "Filipinas", "AU": "Australia", "NZ": "Nueva Zelanda",
}

// ValidCountry dice si el código es un país de la lista.
func ValidCountry(code string) bool {
	_, ok := countries[strings.ToUpper(strings.TrimSpace(code))]
	return ok
}

// CountryOf devuelve el código de país del usuario ("" si nunca lo eligió).
// Lo usan webqueue y webmatch para pintar la bandera en el matchmaking.
func CountryOf(discordID string) string {
	var u models.User
	if err := db.Where("discord_id = ?", discordID).First(&u).Error; err == nil {
		return strings.ToUpper(strings.TrimSpace(u.Country))
	}
	return ""
}

// handleCountry: GET/POST /api/auth/country — nacionalidad del jugador.
// POST acepta {"country":"VE"} ("" la quita); GET devuelve la actual.
func handleCountry(w http.ResponseWriter, r *http.Request) {
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
		// pero por robustez se crea si falta (igual que en handleMains).
		user = models.User{DiscordID: discordID, FirstLogin: time.Now(), LastLogin: time.Now()}
	}

	if r.Method == http.MethodPost {
		var body struct {
			Country string `json:"country"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "cuerpo_invalido",
				"message": "Envía country en formato JSON.",
			})
			return
		}
		code := strings.ToUpper(strings.TrimSpace(body.Country))
		if code != "" && !ValidCountry(code) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error":   "pais_invalido",
				"message": "Ese país no está en la lista disponible.",
			})
			return
		}
		user.Country = code
		if err := db.Save(&user).Error; err != nil {
			log.Println("❌ Country: error guardando el país:", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "error_interno"})
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"country": strings.ToUpper(strings.TrimSpace(user.Country))})
}
