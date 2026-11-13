package startgg

import (
	"log"
	"net/http"
	"strings"
	"time"

	"smash-backend/internal/models"

	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Nacionalidad desde Start.gg
//
// Start.gg expone la ubicación pública del usuario (user.location) con el
// país (ISO 3166-1 alpha-2, p. ej. "VE") y el estado/región (nombre completo
// o código ISO 3166-2, según cómo lo cargó el jugador). Todo se normaliza a
// los códigos que usa la web para pintar las banderas:
//   - País:   public/Nacionalidad/{cc}.png      (cc en minúsculas)
//   - Estado: public/Estados/{CC}/{subdiv}.png  (ISO 3166-2 sin el prefijo)
// ---------------------------------------------------------------------------

const userLocationQuery = `
query UserLocation($slug: String!) {
  user(slug: $slug) {
    location { country state }
  }
}
`

type userLocationResp struct {
	User *struct {
		Location *struct {
			Country string `json:"country"`
			State   string `json:"state"`
		} `json:"location"`
	} `json:"user"`
}

// slugFromProfileURL extrae el slug de una URL de perfil de Start.gg
// (https://www.start.gg/user/xxxx -> xxxx).
func slugFromProfileURL(profileURL string) string {
	parts := strings.Split(strings.TrimRight(strings.TrimSpace(profileURL), "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

// fetchUserLocation consulta la ubicación pública de un usuario de Start.gg.
func fetchUserLocation(client *http.Client, token, slug string) (country, state string, err error) {
	var r userLocationResp
	if err = gqlDo(client, token, userLocationQuery, map[string]interface{}{"slug": slug}, &r); err != nil {
		return "", "", err
	}
	if r.User == nil || r.User.Location == nil {
		return "", "", nil
	}
	return r.User.Location.Country, r.User.Location.State, nil
}

// SaveUserLocation guarda la nacionalidad (país y estado normalizados) en la
// fila del usuario. SOLO actualiza filas existentes: desde la sync nunca se
// crean usuarios (esa fila la crea el login de Discord) para no romper el
// fallback de perfiles públicos del módulo auth.
// Start.gg manda: si reporta país, sobrescribe lo que hubiera; la elección
// manual en la web queda como respaldo para quien no tiene ubicación ahí.
func SaveUserLocation(db *gorm.DB, discordID, rawCountry, rawState string) {
	country := NormalizeCountry(rawCountry)
	if country == "" || discordID == "" {
		return
	}
	state := NormalizeState(country, rawState)
	if err := db.Model(&models.User{}).
		Where("discord_id = ?", discordID).
		Updates(map[string]any{
			"country":        country,
			"state":          state,
			"country_source": "startgg",
		}).Error; err != nil {
		log.Printf("⚠️ Start.gg (nacionalidad): no se pudo guardar la de %s: %v", discordID, err)
	}
}

// BackfillUserLocations completa la nacionalidad de los usuarios que aún no
// tienen país: localiza su cuenta de Start.gg (players.profile_url vía
// discord_id) y consulta su ubicación pública. Con tope por pasada para
// respetar el rate limit de la API (gqlDo ya duerme 800 ms por pedido).
func BackfillUserLocations(db *gorm.DB, apiToken string, maxPerRun int) {
	if apiToken == "" || maxPerRun <= 0 {
		return
	}
	type pend struct {
		DiscordID  string
		ProfileURL string
	}
	var rows []pend
	if err := db.Table("users").
		Select("DISTINCT players.discord_id AS discord_id, players.profile_url AS profile_url").
		Joins("JOIN players ON players.discord_id = users.discord_id AND players.league = ?", leagueType).
		Where("(users.country = '' OR users.country IS NULL) AND players.profile_url <> '' AND players.discord_id <> ''").
		Limit(maxPerRun).
		Scan(&rows).Error; err != nil {
		log.Printf("⚠️ Start.gg (nacionalidades): no se pudo listar usuarios pendientes: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	log.Printf("🌍 Completando la nacionalidad desde Start.gg para %d usuarios…", len(rows))
	client := &http.Client{Timeout: 30 * time.Second}
	done := 0
	for _, r := range rows {
		slug := slugFromProfileURL(r.ProfileURL)
		if slug == "" {
			continue
		}
		country, state, err := fetchUserLocation(client, apiToken, slug)
		if err != nil {
			log.Printf("   ⚠️ Nacionalidad de %s: %v", r.DiscordID, err)
			continue
		}
		if country != "" {
			SaveUserLocation(db, r.DiscordID, country, state)
			done++
		}
	}
	log.Printf("🌍 Nacionalidades guardadas en esta pasada: %d/%d.", done, len(rows))
}

// ---------------------------------------------------------------------------
// Normalización de nombres -> códigos ISO
// ---------------------------------------------------------------------------

// accents quita diacríticos comunes del español/portugués para comparar.
var accents = map[rune]rune{
	'á': 'a', 'é': 'e', 'í': 'i', 'ó': 'o', 'ú': 'u', 'ü': 'u', 'ñ': 'n',
	'à': 'a', 'è': 'e', 'ì': 'i', 'ò': 'o', 'ù': 'u',
	'â': 'a', 'ê': 'e', 'î': 'i', 'ô': 'o', 'û': 'u',
	'ã': 'a', 'õ': 'o', 'ç': 'c',
}

// fold pasa a minúsculas, quita acentos y signos de puntuación sueltos.
func fold(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if repl, ok := accents[r]; ok {
			r = repl
		}
		if r == '.' || r == ',' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isAlpha(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func isAlnum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// countryNames mapea nombres de país (en español y en inglés, sin acentos)
// a su código ISO 3166-1 alpha-2. Start.gg normalmente manda el código, pero
// algunos perfiles traen el nombre completo.
var countryNames = map[string]string{
	"venezuela": "VE", "estados unidos": "US", "united states": "US", "usa": "US",
	"estados unidos de america": "US", "mexico": "MX", "argentina": "AR",
	"colombia": "CO", "chile": "CL", "peru": "PE", "bolivia": "BO",
	"ecuador": "EC", "brasil": "BR", "brazil": "BR", "uruguay": "UY",
	"paraguay": "PY", "costa rica": "CR", "panama": "PA", "guatemala": "GT",
	"honduras": "HN", "nicaragua": "NI", "el salvador": "SV", "cuba": "CU",
	"republica dominicana": "DO", "dominican republic": "DO",
	"puerto rico": "PR", "espana": "ES", "spain": "ES", "portugal": "PT",
	"francia": "FR", "france": "FR", "reino unido": "GB", "united kingdom": "GB",
	"alemania": "DE", "germany": "DE", "italia": "IT", "italy": "IT",
	"paises bajos": "NL", "netherlands": "NL", "polonia": "PL", "poland": "PL",
	"rumania": "RO", "romania": "RO", "japon": "JP", "japan": "JP",
	"corea del sur": "KR", "south korea": "KR", "korea": "KR", "china": "CN",
	"filipinas": "PH", "philippines": "PH", "australia": "AU",
	"nueva zelanda": "NZ", "new zealand": "NZ", "canada": "CA",
	"antigua y barbuda": "AG", "bahamas": "BS", "barbados": "BB",
	"belice": "BZ", "belize": "BZ", "dominica": "DM", "granada": "GD",
	"grenada": "GD", "guyana": "GY", "haiti": "HT", "jamaica": "JM",
	"san cristobal y nieves": "KN", "saint kitts and nevis": "KN",
	"santa lucia": "LC", "saint lucia": "LC",
	"san vicente y las granadinas": "VC",
	"saint vincent and the grenadines": "VC", "surinam": "SR", "suriname": "SR",
	"trinidad y tobago": "TT", "trinidad and tobago": "TT",
}

// NormalizeCountry devuelve el código ISO 3166-1 alpha-2 del país ("" si no
// se reconoce ni como código ni como nombre).
func NormalizeCountry(v string) string {
	code := strings.ToUpper(strings.TrimSpace(v))
	if len(code) == 2 && isAlpha(code) {
		return code
	}
	return countryNames[fold(v)]
}

// stateNames mapea nombres de estados/regiones (sin acentos) a su código
// ISO 3166-2 SIN el prefijo del país: la convención de public/Estados.
// Venezuela usa una letra (VE-A Distrito Capital … VE-Z Amazonas) y
// Estados Unidos dos (US-CA California…), igual que las carpetas subidas.
var stateNames = map[string]map[string]string{
	"VE": {
		"distrito capital": "A", "capital district": "A",
		"anzoategui": "B", "apure": "C", "aragua": "D", "barinas": "E",
		"bolivar": "F", "carabobo": "G", "cojedes": "H", "falcon": "I",
		"guarico": "J", "lara": "K", "merida": "L", "miranda": "M",
		"monagas": "N", "nueva esparta": "O", "portuguesa": "P", "sucre": "R",
		"tachira": "S", "trujillo": "T", "yaracuy": "U", "zulia": "V",
		"dependencias federales": "W", "federal dependencies": "W",
		"vargas": "X", "la guaira": "X", "guaira": "X", "delta amacuro": "Y",
		"amazonas": "Z",
	},
	"US": {
		"alabama": "AL", "alaska": "AK", "arizona": "AZ", "arkansas": "AR",
		"california": "CA", "colorado": "CO", "connecticut": "CT",
		"delaware": "DE", "florida": "FL", "georgia": "GA", "hawaii": "HI",
		"idaho": "ID", "illinois": "IL", "indiana": "IN", "iowa": "IA",
		"kansas": "KS", "kentucky": "KY", "louisiana": "LA", "maine": "ME",
		"maryland": "MD", "massachusetts": "MA", "michigan": "MI",
		"minnesota": "MN", "mississippi": "MS", "missouri": "MO",
		"montana": "MT", "nebraska": "NE", "nevada": "NV",
		"new hampshire": "NH", "new jersey": "NJ", "new mexico": "NM",
		"new york": "NY", "north carolina": "NC", "north dakota": "ND",
		"ohio": "OH", "oklahoma": "OK", "oregon": "OR", "pennsylvania": "PA",
		"rhode island": "RI", "south carolina": "SC", "south dakota": "SD",
		"tennessee": "TN", "texas": "TX", "utah": "UT", "vermont": "VT",
		"virginia": "VA", "washington": "WA", "west virginia": "WV",
		"wisconsin": "WI", "wyoming": "WY", "district of columbia": "DC",
		"washington dc": "DC", "washington d c": "DC", "puerto rico": "PR",
		"guam": "GU", "virgin islands": "VI", "american samoa": "AS",
		"northern mariana islands": "MP",
	},
}

// NormalizeState devuelve el código de subdivisión (ISO 3166-2 sin prefijo)
// para pintar la bandera del estado ("" si no hay una imagen segura).
func NormalizeState(country, v string) string {
	code := strings.ToUpper(strings.TrimSpace(v))
	if code == "" {
		return ""
	}
	// Ya es un código ("CA", "A", "Z"): se usa tal cual.
	if len(code) <= 3 && isAlnum(code) {
		return code
	}
	if m, ok := stateNames[strings.ToUpper(strings.TrimSpace(country))]; ok {
		return m[fold(v)]
	}
	return ""
}
