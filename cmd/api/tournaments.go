package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// ---------- Series de torneos ----------
//
// El ranking numera los torneos procesados (1, 2, 3...) y la web los agrupa
// por SERIE para que nadie se confunda: "Hot Coffee Event #12" y
// "Hot Coffee Event #15" son la misma serie (Hot Coffee), igual que
// "TNS Redemption #4" es TNS. Los nombres de serie son CORTOS a propósito.

type seriesDef struct {
	Key   string // slug de la serie (para filtrar)
	Name  string // nombre corto que se muestra
	Match string // texto que identifica la serie en el slug/nombre
}

var seriesTable = []seriesDef{
	{Key: "hot-coffee", Name: "Hot Coffee", Match: "hot-coffee"},
	{Key: "tns", Name: "TNS", Match: "tns"},
	{Key: "tacos-tuesday", Name: "Tacos Tuesday", Match: "taco"},
	{Key: "paragon", Name: "Paragon", Match: "paragon"},
}

// seriesOf devuelve (key, nombre corto) de la serie de un torneo según su
// slug y su nombre. Si no cae en ninguna serie conocida, se deriva del slug:
// tokens sin números ni sufijos de edición ("event", "redemption", etc.).
func seriesOf(slug, name string) (string, string) {
	s := strings.ToLower(slug + " " + name)
	for _, sd := range seriesTable {
		if strings.Contains(s, sd.Match) {
			return sd.Key, sd.Name
		}
	}

	// Serie genérica a partir del slug: "showdown-weekly-7" -> "Showdown".
	editionTokens := map[string]bool{
		"event": true, "redemption": true, "weekly": true, "edition": true,
		"season": true, "online": true, "torneo": true, "cup": true,
	}
	var tokens []string
	for _, tok := range strings.Split(strings.ToLower(strings.ReplaceAll(slug, "_", "-")), "-") {
		if tok == "" || allDigits(tok) || editionTokens[tok] {
			continue
		}
		tokens = append(tokens, tok)
	}
	if len(tokens) == 0 {
		return strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(name)
	}
	key := strings.Join(tokens, "-")
	return key, titleTokens(tokens)
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// titleTokens: ["tacos","tuesday"] -> "Tacos Tuesday"; ["tns"] -> "TNS".
func titleTokens(tokens []string) string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if len(t) <= 3 {
			out = append(out, strings.ToUpper(t))
			continue
		}
		out = append(out, strings.ToUpper(t[:1])+t[1:])
	}
	return strings.Join(out, " ")
}

// processedEventRow: fila mínima de processed_events.
type processedEventRow struct {
	EventID int
	Slug    string
	Name    string
	Image   string
}

// HandleGetTournaments lista los torneos procesados con la MISMA numeración
// que usa el ranking (orden por event_id) más la serie a la que pertenecen.
// La web lo usa para el banner de "torneos procesados" y el filtro por serie.
func HandleGetTournaments(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	var rows []processedEventRow
	if err := db.Raw(`SELECT event_id, slug, name, image FROM processed_events ORDER BY event_id ASC`).Scan(&rows).Error; err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "error_interno"})
		return
	}

	type tournamentInfo struct {
		Number    int    `json:"number"`
		EventID   int    `json:"eventId"`
		Name      string `json:"name"`
		Slug      string `json:"slug"`
		Image     string `json:"image"`
		SeriesKey string `json:"seriesKey"`
		Series    string `json:"series"`
	}

	out := make([]tournamentInfo, 0, len(rows))
	for i, e := range rows {
		key, display := seriesOf(e.Slug, e.Name)
		out = append(out, tournamentInfo{
			Number:    i + 1,
			EventID:   e.EventID,
			Name:      e.Name,
			Slug:      e.Slug,
			Image:     e.Image,
			SeriesKey: key,
			Series:    display,
		})
	}
	json.NewEncoder(w).Encode(map[string]any{"tournaments": out})
}

// seriesEvents: event_id de todos los torneos de una serie (key de seriesOf).
func seriesEvents(key string) []int {
	var rows []processedEventRow
	if err := db.Raw(`SELECT event_id, slug, name, image FROM processed_events`).Scan(&rows).Error; err != nil {
		return nil
	}
	var ids []int
	for _, e := range rows {
		if k, _ := seriesOf(e.Slug, e.Name); k == key {
			ids = append(ids, e.EventID)
		}
	}
	return ids
}
