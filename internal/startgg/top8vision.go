package startgg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"
)

const (
	maxImageBytes = 4_500_000
	maxImageEdge  = 1568
	minMatchScore = 0.6
)

var knownFighters = []string{
	"AHRI", "AKALI", "BLITZCRANK", "BRAUM", "CAITLYN", "DARIUS", "EKKO", "ILLAOI",
	"JINX", "LUX", "SENNA", "TEEMO", "THRESH", "VI", "WARWICK", "YASUO",
}

var fighterAliases = map[string]string{
	"BLITZ": "BLITZCRANK",
	"WW":    "WARWICK",
}

type Top8Entry struct {
	Placement  int      `json:"placement"`
	Tag        string   `json:"tag"`
	Characters []string `json:"characters"`
	Confidence string   `json:"confidence,omitempty"`
}

type top8Payload struct {
	Entries []Top8Entry `json:"entries"`
}

const top8Prompt = `Esta imagen es el gráfico del Top 8 de un torneo de 2XKO (juego de pelea de League of Legends).
Cada jugador del Top 8 aparece con su puesto, su gamertag y un DÚO de dos campeones (retratos, iconos o nombres).

Devuelve ÚNICAMENTE un JSON, sin texto antes ni después y sin bloques de código markdown, con esta forma exacta:
{"entries":[{"placement":1,"tag":"GAMERTAG","characters":["AKALI","YASUO"],"confidence":"high"}]}

Reglas:
- "tag": el gamertag tal como aparece, sin el sponsor/team si está separado.
- "characters": los dos campeones del dúo, nombre en inglés y MAYÚSCULAS, en el orden en que se muestran.
- Si un jugador muestra un solo campeón o no estás seguro, pon los que veas y "confidence":"low". No inventes.
- Si el 5º o el 7º puesto aparecen repetidos (empates), repite el número de puesto tal cual se muestra.
- Incluye solo jugadores que se vean en la imagen. Si la imagen no es un Top 8, devuelve {"entries":[]}.
- Campeones conocidos: AHRI, AKALI, BLITZCRANK, BRAUM, CAITLYN, DARIUS, EKKO, ILLAOI, JINX, LUX, SENNA, TEEMO, THRESH, VI, WARWICK, YASUO. Si ves otro, escríbelo igualmente.`

// ---------- Gemini (Visión Gratuita) ----------

func AnalyzeTop8Image(ctx context.Context, _ *http.Client, apiKey, modelName string, raw []byte) ([]Top8Entry, error) {
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY no está configurada")
	}

	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, fmt.Errorf("error al crear cliente de Gemini: %w", err)
	}
	defer client.Close()

	if modelName == "" {
		modelName = "gemini-1.5-flash"
	}

	model := client.GenerativeModel(modelName)
	model.SetTemperature(0.1)

	imgBytes, mediaType, err := prepareImage(raw)
	if err != nil {
		return nil, err
	}

	format := "png"
	if mediaType == "image/jpeg" {
		format = "jpeg"
	}

	imgBlob := genai.ImageData(format, imgBytes)
	prompt := genai.Text(top8Prompt)

	resp, err := model.GenerateContent(ctx, imgBlob, prompt)
	if err != nil {
		return nil, fmt.Errorf("error al analizar imagen con Gemini: %w", err)
	}

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("Gemini no devolvió ninguna respuesta")
	}

	var rawText string
	for _, part := range resp.Candidates[0].Content.Parts {
		if txt, ok := part.(genai.Text); ok {
			rawText += string(txt)
		}
	}

	return parseTop8JSON(rawText)
}

func parseTop8JSON(text string) ([]Top8Entry, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("el modelo no devolvió JSON: %s", truncate(text, 200))
	}
	var p top8Payload
	if err := json.Unmarshal([]byte(text[start:end+1]), &p); err != nil {
		return nil, fmt.Errorf("JSON del modelo inválido: %w", err)
	}
	normalizeEntries(p.Entries)
	return p.Entries, nil
}

func normalizeEntries(entries []Top8Entry) {
	for i := range entries {
		var chars []string
		for _, c := range entries[i].Characters {
			if name, _ := canonicalFighter(c); name != "" {
				chars = append(chars, name)
			}
		}
		if len(chars) > 2 {
			chars = chars[:2]
		}
		entries[i].Characters = chars
		entries[i].Tag = strings.TrimSpace(entries[i].Tag)
	}
}

// ---------- Imágenes, Caché y Procesamiento ----------

func discordURLExpiry(raw string) (time.Time, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return time.Time{}, false
	}
	ex := u.Query().Get("ex")
	if ex == "" {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(ex, 16, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

var unsafeKeyChars = regexp.MustCompile(`[^A-Za-z0-9._\-]+`)

func cacheName(key string) string { return unsafeKeyChars.ReplaceAllString(key, "_") }

func CacheImage(ctx context.Context, client *http.Client, rawURL, cacheDir, key string) ([]byte, error) {
	path := filepath.Join(cacheDir, cacheName(key)+".img")
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		return b, nil
	}
	if exp, ok := discordURLExpiry(rawURL); ok && time.Now().After(exp) {
		return nil, fmt.Errorf("el enlace de Discord caducó el %s", exp.Format("2006-01-02 15:04 MST"))
	}

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("descarga fallida (HTTP %d)", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return nil, err
	}
	return b, nil
}

func LoadOrAnalyzeTop8(ctx context.Context, client *http.Client, imageURL, cacheDir, key, apiKey, model string) ([]Top8Entry, error) {
	jsonPath := filepath.Join(cacheDir, cacheName(key)+".json")
	if b, err := os.ReadFile(jsonPath); err == nil {
		var p top8Payload
		if json.Unmarshal(b, &p) == nil && len(p.Entries) > 0 {
			normalizeEntries(p.Entries)
			return p.Entries, nil
		}
	}

	raw, err := CacheImage(ctx, client, imageURL, cacheDir, key)
	if err != nil {
		return nil, err
	}
	entries, err := AnalyzeTop8Image(ctx, client, apiKey, model, raw)
	if err != nil {
		return nil, err
	}
	if len(entries) > 0 {
		if b, err := json.MarshalIndent(top8Payload{Entries: entries}, "", "  "); err == nil {
			_ = os.WriteFile(jsonPath, b, 0o644)
		}
	}
	return entries, nil
}

func prepareImage(raw []byte) ([]byte, string, error) {
	mt := http.DetectContentType(raw)
	switch mt {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil, "", fmt.Errorf("formato no soportado: %s", mt)
	}
	if len(raw) <= maxImageBytes {
		return raw, mt, nil
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("error al decodificar imagen: %w", err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, downscale(src, maxImageEdge), &jpeg.Options{Quality: 90}); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/jpeg", nil
}

func downscale(src image.Image, maxEdge int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	long := w
	if h > long {
		long = h
	}
	if long <= maxEdge {
		return src
	}
	scale := float64(maxEdge) / float64(long)
	nw, nh := int(float64(w)*scale), int(float64(h)*scale)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0 := b.Min.Y + y*h/nh
		y1 := b.Min.Y + (y+1)*h/nh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0 := b.Min.X + x*w/nw
			x1 := b.Min.X + (x+1)*w/nw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, ca := src.At(xx, yy).RGBA()
					r += uint64(cr)
					g += uint64(cg)
					bl += uint64(cb)
					a += uint64(ca)
					n++
				}
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8((r / n) >> 8), G: uint8((g / n) >> 8),
				B: uint8((bl / n) >> 8), A: uint8((a / n) >> 8),
			})
		}
	}
	return dst
}

// ---------- Nombres: Campeones y Matching ----------

func canonicalFighter(raw string) (name string, known bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		if unicode.IsLetter(r) {
			b.WriteRune(r)
		}
	}
	n := b.String()
	if n == "" {
		return "", false
	}
	if alias, ok := fighterAliases[n]; ok {
		return alias, true
	}
	for _, f := range knownFighters {
		if n == f {
			return f, true
		}
	}
	best, bestScore := "", 0.0
	for _, f := range knownFighters {
		if len(f) < 4 {
			continue
		}
		if s := similarity(n, f); s > bestScore {
			best, bestScore = f, s
		}
	}
	if bestScore >= 0.8 {
		return best, true
	}
	return n, false
}

func FormatDuo(chars []string) string { return strings.Join(chars, ", ") }

func normalizeTag(s string) string {
	if i := strings.Index(s, " | "); i >= 0 {
		s = s[i+3:]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func levenshtein(a, b []rune) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

func similarity(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	m := len(ra)
	if len(rb) > m {
		m = len(rb)
	}
	return 1 - float64(levenshtein(ra, rb))/float64(m)
}

func tagScore(a, b string) float64 {
	na, nb := normalizeTag(a), normalizeTag(b)
	if na == "" || nb == "" {
		return 0
	}
	s := similarity(na, nb)
	short, long := na, nb
	if len(short) > len(long) {
		short, long = long, short
	}
	if len([]rune(short)) >= 3 && strings.Contains(long, short) && s < 0.85 {
		s = 0.85
	}
	return s
}

type Candidate struct {
	PlayerID  int
	Tag       string
	Placement int
}

func MatchEntries(entries []Top8Entry, cands []Candidate) (map[int]Top8Entry, []Top8Entry) {
	type pair struct {
		ci, ei int
		score  float64
	}
	var pairs []pair
	for ci, c := range cands {
		for ei, e := range entries {
			s := tagScore(c.Tag, e.Tag)
			if s < minMatchScore {
				continue
			}
			if c.Placement == e.Placement {
				s += 0.05
			}
			pairs = append(pairs, pair{ci, ei, s})
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].score > pairs[j].score })

	matched := make(map[int]Top8Entry)
	usedC := make(map[int]bool)
	usedE := make(map[int]bool)
	for _, p := range pairs {
		if usedC[p.ci] || usedE[p.ei] {
			continue
		}
		usedC[p.ci], usedE[p.ei] = true, true
		matched[cands[p.ci].PlayerID] = entries[p.ei]
	}
	var unmatched []Top8Entry
	for ei, e := range entries {
		if !usedE[ei] {
			unmatched = append(unmatched, e)
		}
	}
	return matched, unmatched
}
