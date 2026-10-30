package discord

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"smash-backend/internal/gamechars"
	"smash-backend/internal/models"

	"github.com/bwmarrin/discordgo"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Estados de una reta.
const (
	statusPending       = "PENDING"        // retó; el rival aún no responde
	statusInProgress    = "IN_PROGRESS"    // aceptada; se arma equipo y se juega
	statusPendingVerify = "PENDING_VERIFY" // game reportado; espera Verify Result Submission
	statusPendingClose  = "PENDING_CLOSE"  // propuesta de "Cerrar set" activa; games bloqueados
	statusFinished      = "FINISHED"       // resultado aplicado; puntos asignados
	statusRejected      = "REJECTED"
	statusCancelled     = "CANCELLED"
)

const (
	matchmakingLeague = "2xko_online"

	// Puntos que pierde quien pierde una reta, y piso por debajo del cual no se le quitan más.
	loserPenalty   = 5
	minPointsFloor = 5
)

func getMatchmakingChannelID() string {
	if id := os.Getenv("DISCORD_MATCHMAKING_CHANNEL_ID"); id != "" {
		return id
	}
	return "1564293602606186576"
}

func getMatchmakingResultChannelID() string {
	if id := os.Getenv("DISCORD_MATCHMAKING_RESULT_CHANNEL_ID"); id != "" {
		return id
	}
	return "1564298796425543833"
}

func RegisterMatchmakingHandlers(s *discordgo.Session, db *gorm.DB) {
	s.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		switch i.Type {
		case discordgo.InteractionApplicationCommand:
			if i.ApplicationCommandData().Name == "ft" {
				handleFTCommand(s, i, db)
			}
		case discordgo.InteractionMessageComponent:
			handleComponentInteraction(s, i, db)
		}
	})
}

// ---------------------------------------------------------------------------
// Utilidades de interacción
// ---------------------------------------------------------------------------

// userError es un error "para mostrarle al usuario" (mensaje efímero), no un fallo interno.
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

func interactionUser(i *discordgo.InteractionCreate) *discordgo.User {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	return i.User
}

// ackComponent confirma el clic a Discord DE INMEDIATO (antes de tocar la base de datos).
func ackComponent(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	})
	if err != nil {
		log.Printf("❌ No se pudo confirmar la interacción a Discord: %v\n", err)
		return false
	}
	return true
}

// editMessage reemplaza el contenido y los componentes del mensaje donde se hizo clic.
func editMessage(s *discordgo.Session, i *discordgo.InteractionCreate, content string, components []discordgo.MessageComponent) {
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	emptyEmbeds := []*discordgo.MessageEmbed{}
	_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:    &content,
		Components: &components,
		Embeds:     &emptyEmbeds,
	})
	if err != nil {
		log.Printf("❌ No se pudo editar el mensaje de la interacción: %v\n", err)
	}
}

// keycap: emoji de número (1..10) para la línea de games del set.
func keycap(n int) string {
	switch n {
	case 1:
		return "1️⃣"
	case 2:
		return "2️⃣"
	case 3:
		return "3️⃣"
	case 4:
		return "4️⃣"
	case 5:
		return "5️⃣"
	case 6:
		return "6️⃣"
	case 7:
		return "7️⃣"
	case 8:
		return "8️⃣"
	case 9:
		return "9️⃣"
	case 10:
		return "🔟"
	}
	return ""
}

// roundTrack: el set en UNA sola línea de games numerados con bolitas —
// 1️⃣-2️⃣-3️⃣-4️⃣-5️⃣ / 🟢-🔴-🔴-🟢-⚪. Cubre TODOS los games posibles del set
// (FT3 = 5 bolitas, FT5 = 9: al FT*2-1). Desde la perspectiva del jugador de
// referencia (refSide): 🟢 ganó ese game, 🔴 lo perdió, ⚪ aún no se juega.
func roundTrack(m models.Matchmaking, refSide string) string {
	var hist []int
	if b := []byte(m.RoundHistory); len(b) > 0 {
		if err := json.Unmarshal(b, &hist); err != nil {
			hist = nil
		}
	}
	maxGames := m.TargetFT*2 - 1 // FT3 → 5 games, FT5 → 9 games
	nums, dots := "", ""
	for g := 1; g <= maxGames; g++ {
		if g > 1 {
			nums += "-"
			dots += "-"
		}
		nums += keycap(g)
		switch {
		case g > len(hist):
			dots += "⚪"
		case (hist[g-1] == 1 && refSide == "p1") || (hist[g-1] == 2 && refSide == "p2"):
			dots += "🟢"
		default:
			dots += "🔴"
		}
	}
	return nums + "\n" + dots
}

// hasRoundHistory: true si el set se reportó game a game (los reportes directos
// con "Resultado final" no guardan secuencia).
func hasRoundHistory(m *models.Matchmaking) bool {
	var hist []int
	if b := []byte(m.RoundHistory); len(b) > 0 {
		if err := json.Unmarshal(b, &hist); err != nil {
			hist = nil
		}
	}
	return len(hist) > 0
}

// gamePanelEmbed: pips de victorias por jugador + tarjeta VS con el marcador
// pixel-art al centro (cuando ambos equipos están confirmados).
func gamePanelEmbed(m models.Matchmaking) (*discordgo.MessageEmbed, []*discordgo.File) {
	gameN := m.Score1 + m.Score2 + 1

	embed := &discordgo.MessageEmbed{
		Color: 0x5865F2,
		Title: fmt.Sprintf("GAME %d", gameN),
	}
	files := []*discordgo.File{}

	if m.P1TeamComplete() && m.P2TeamComplete() {
		info := fmt.Sprintf("FT %d · 💰 %d pts", m.TargetFT, m.PointsAtStake)
		if m.RoomCode != "" {
			info += " · 🏠 `" + m.RoomCode + "`"
		}
		// Secuencia única de games: 1️⃣2️⃣3️⃣4️⃣5️⃣ / 🟢🔴🔴🟢⚪ (verde = ganó P1,
		// el jugador destacado del panel; la leyenda va en el footer).
		embed.Description = roundTrack(m, "p1") + "\n" + info
		embed.Footer = &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("🟢 gana %s · 🔴 gana %s", m.Player1Tag, m.Player2Tag),
		}

		// Tarjeta VS: champions de ambos + marcador pixel-art al centro.
		if card := matchCard(m, m.Score1, m.Score2, "match.png"); card != nil {
			embed.Image = &discordgo.MessageEmbedImage{URL: "attachment://match.png"}
			files = append(files, card)
		}
	} else {
		embed.Description = fmt.Sprintf("**%d — %d**\n⏳ Armen sus equipos (selección privada)", m.Score1, m.Score2)
	}

	if m.Player1Avatar != "" {
		embed.Author = &discordgo.MessageEmbedAuthor{Name: m.Player1Tag, IconURL: m.Player1Avatar}
	}
	if m.Player2Avatar != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: m.Player2Avatar}
	}
	return embed, files
}

// refreshGamePanel reemplaza el mensaje donde se hizo clic por el panel VS.
// prefix es un texto opcional que queda sobre el embed ("" = sin texto).
func refreshGamePanel(s *discordgo.Session, i *discordgo.InteractionCreate, prefix string, m models.Matchmaking) {
	embed, files := gamePanelEmbed(m)
	components := getGameKeyboard(m.ID, m.Player1Tag, m.Player2Tag)
	emptyAtts := []*discordgo.MessageAttachment{}
	embeds := []*discordgo.MessageEmbed{embed}
	_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:     &prefix,
		Embeds:      &embeds,
		Components:  &components,
		Files:       files,
		Attachments: &emptyAtts,
	})
	if err != nil {
		log.Printf("❌ No se pudo actualizar el panel de games: %v\n", err)
	}
}

// loadIconCell carga un PNG de Personajes/ o Fusion/ y lo reescala (factor ENTERO,
// vecino más cercano: pixel art nítido) para que quepa en un cuadro de size px.
// Devuelve nil si el icono no existe o falla.
func loadIconCell(kind, id string, size int) image.Image {
	if id == "" {
		return nil
	}
	var path string
	if kind == "fuse" {
		if !gamechars.ValidFuse(id) {
			return nil
		}
		path = gamechars.FuseIcon(id)
	} else {
		if !gamechars.ValidCharacter(id) {
			return nil
		}
		path = gamechars.CharacterIcon(id)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("⚠️ No se pudo leer el icono %s: %v\n", path, err)
		return nil
	}
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		log.Printf("⚠️ PNG inválido: %s: %v\n", path, err)
		return nil
	}
	b := src.Bounds()
	k := b.Dx()
	if b.Dy() > k {
		k = b.Dy()
	}
	k = (k + size - 1) / size
	if k < 1 {
		k = 1
	}
	dw, dh := b.Dx()/k, b.Dy()/k
	if dw < 1 || dh < 1 {
		return nil
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		sy := b.Min.Y + y*k + k/2
		for x := 0; x < dw; x++ {
			sx := b.Min.X + x*k + k/2
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

// drawCardBackground pinta fondo + esquinas redondeadas + marco del color de
// acento. Lo comparten el banner de equipo (degradado oscuro) y la tarjeta VS
// (fondo negro). bgTop→bgBottom define el degradado vertical (iguales = plano).
func drawCardBackground(dst *image.RGBA, accent color.RGBA, bgTop, bgBottom color.RGBA) {
	w, h := dst.Bounds().Dx(), dst.Bounds().Dy()
	const (
		radius = 24 // esquinas redondeadas
		frame  = 3  // grosor del marco
	)
	for y := 0; y < h; y++ {
		bg := lerpColor(bgTop, bgBottom, float64(y)/float64(h-1))
		for x := 0; x < w; x++ {
			if !inRoundedRect(x, y, w, h, radius) {
				continue // transparente: esquinas del PNG
			}
			if isFrameEdge(x, y, w, h, radius, frame) {
				dst.Set(x, y, accent)
			} else {
				dst.Set(x, y, bg)
			}
		}
	}
}

// bannerV2 compone los iconos en un PNG horizontal con fondo NEGRO, marco
// redondeado morado e iconos centrados en celdas de 140 px (diseño tester).
func bannerV2(name string, accent color.RGBA, cells ...image.Image) *discordgo.File {
	valid := make([]image.Image, 0, len(cells))
	for _, c := range cells {
		if c != nil {
			valid = append(valid, c)
		}
	}
	if len(valid) == 0 {
		return nil
	}
	const (
		cell = 140 // celda por icono
		pad  = 12  // margen interior
	)
	w, h := pad*2+cell*len(valid), pad*2+cell
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	drawCardBackground(dst, accent, cardBlack, cardBlack)
	for i, c := range valid {
		b := c.Bounds()
		x0 := pad + i*cell + (cell-b.Dx())/2
		y0 := pad + (cell-b.Dy())/2
		draw.Draw(dst, image.Rect(x0, y0, x0+b.Dx(), y0+b.Dy()), c, b.Min, draw.Over)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil
	}
	return &discordgo.File{Name: name, Reader: bytes.NewReader(out.Bytes())}
}

// digitGlyphs: dígitos pixel-art 3×5 para el marcador central de la tarjeta VS.
var digitGlyphs = map[rune][5]string{
	'0': {"111", "101", "101", "101", "111"},
	'1': {"010", "110", "010", "010", "111"},
	'2': {"111", "001", "111", "100", "111"},
	'3': {"111", "001", "111", "001", "111"},
	'4': {"101", "101", "111", "001", "001"},
	'5': {"111", "100", "111", "001", "111"},
	'6': {"111", "100", "111", "101", "111"},
	'7': {"111", "001", "001", "001", "001"},
	'8': {"111", "101", "111", "101", "111"},
	'9': {"111", "101", "111", "001", "111"},
	'-': {"000", "000", "111", "000", "000"},
}

// drawScorePixels dibuja el marcador pixel-art centrado en (cx, cy), ajustando el
// tamaño del píxel para no superar maxW de ancho.
func drawScorePixels(dst *image.RGBA, cx, cy int, text string, maxW int, c color.Color) {
	px := maxW / (4 * len(text))
	if px > 12 {
		px = 12
	}
	if px < 4 {
		px = 4
	}
	w := len(text)*4*px - px
	x0, y0 := cx-w/2, cy-(5*px)/2
	for _, r := range text {
		g, ok := digitGlyphs[r]
		if !ok {
			continue
		}
		for row := 0; row < 5; row++ {
			for col := 0; col < 3; col++ {
				if g[row][col] != '1' {
					continue
				}
				for dy := 0; dy < px; dy++ {
					for dx := 0; dx < px; dx++ {
						dst.Set(x0+col*px+dx, y0+row*px+dy, c)
					}
				}
			}
		}
		x0 += 4 * px
	}
}

// Colores del diseño "tester" de la tarjeta VS: fondo negro, borde morado
// (blurple) y marcador verde.
var (
	cardPurple = color.RGBA{0x58, 0x65, 0xF2, 0xFF} // marco de la tarjeta VS
	cardBlack  = color.RGBA{0x00, 0x00, 0x00, 0xFF} // fondo de la tarjeta VS
	cardGreen  = color.RGBA{0x57, 0xF2, 0x87, 0xFF} // marcador pixel-art
)

// matchCard: tarjeta del enfrentamiento — champions de P1 (izquierda) vs
// champions de P2 (derecha) y el marcador pixel-art al centro. Fondo negro,
// marco morado y score verde (diseño pedazo a pedido del equipo de testers).
func matchCard(m models.Matchmaking, scoreL, scoreR int, name string) *discordgo.File {
	const cell, pad = 120, 14
	const scoreW = 150

	left := []image.Image{loadIconCell("char", m.P1Char1, cell), loadIconCell("char", m.P1Char2, cell)}
	right := []image.Image{loadIconCell("char", m.P2Char1, cell), loadIconCell("char", m.P2Char2, cell)}
	any := false
	for _, img := range append(left, right...) {
		if img != nil {
			any = true
			break
		}
	}
	if !any {
		return nil
	}

	w, h := pad*2+cell*4+scoreW, pad*2+cell
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	drawCardBackground(dst, cardPurple, cardBlack, cardBlack)

	drawCell := func(img image.Image, xBase int) {
		if img == nil {
			return
		}
		b := img.Bounds()
		x0 := xBase + (cell-b.Dx())/2
		y0 := pad + (cell-b.Dy())/2
		draw.Draw(dst, image.Rect(x0, y0, x0+b.Dx(), y0+b.Dy()), img, b.Min, draw.Over)
	}
	drawCell(left[0], pad)
	drawCell(left[1], pad+cell)
	drawCell(right[0], pad+cell*2+scoreW)
	drawCell(right[1], pad+cell*2+scoreW+cell)

	drawScorePixels(dst, pad+cell*2+scoreW/2, h/2,
		fmt.Sprintf("%d-%d", scoreL, scoreR), scoreW-16,
		cardGreen)

	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil
	}
	return &discordgo.File{Name: name, Reader: bytes.NewReader(out.Bytes())}
}

// lerpColor interpola linealmente dos colores (t en [0,1]).
func lerpColor(a, b color.RGBA, t float64) color.RGBA {
	return color.RGBA{
		uint8(float64(a.R) + t*float64(b.R-a.R)),
		uint8(float64(a.G) + t*float64(b.G-a.G)),
		uint8(float64(a.B) + t*float64(b.B-a.B)),
		255,
	}
}

// inRoundedRect dice si (x, y) cae dentro de un rectángulo con esquinas redondeadas.
func inRoundedRect(x, y, w, h, r int) bool {
	if x < 0 || y < 0 || x >= w || y >= h {
		return false
	}
	clamp := func(v, lo, hi int) int {
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	cx, cy := x, y
	if r > 0 {
		cx, cy = clamp(x, r, w-1-r), clamp(y, r, h-1-r)
	}
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

// isFrameEdge dice si el píxel pertenece al marco de la forma.
func isFrameEdge(x, y, w, h, r, f int) bool {
	r2 := r - f
	if r2 < 0 {
		r2 = 0
	}
	return inRoundedRect(x, y, w, h, r) && !inRoundedRect(x-f, y-f, w-2*f, h-2*f, r2)
}

// iconFilePNG exporta un icono como adjunto suelto (p. ej. la fusión en la
// miniatura del embed, vía attachment://).
func iconFilePNG(name, kind, id string, size int) *discordgo.File {
	img := loadIconCell(kind, id, size)
	if img == nil {
		return nil
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil
	}
	return &discordgo.File{Name: name, Reader: bytes.NewReader(out.Bytes())}
}

// teamPanelEmbedWithBanner: la FUSIÓN vive en la miniatura del embed
// (attachment://fusion.png) y el banner lleva SOLO los 2 champions.
func teamPanelEmbedWithBanner(m models.Matchmaking, side string) (*discordgo.MessageEmbed, []*discordgo.File) {
	embed := teamPanelEmbed(m, side)
	c1, c2, fuse := m.P1Char1, m.P1Char2, m.P1Fuse
	if side == "p2" {
		c1, c2, fuse = m.P2Char1, m.P2Char2, m.P2Fuse
	}

	files := []*discordgo.File{}

	// Miniatura: el icono de la FUSIÓN (la identidad del equipo).
	if fuse != "" {
		if f := iconFilePNG("fusion.png", "fuse", fuse, 80); f != nil {
			embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: "attachment://fusion.png"}
			files = append(files, f)
		}
	}

	// Banner: los 2 champions con marco morado sobre negro (diseño tester);
	// la confirmación se marca con el embed dorado, no con el marco.
	if banner := bannerV2("team.png", cardPurple,
		loadIconCell("char", c1, 140), loadIconCell("char", c2, 140)); banner != nil {
		embed.Image = &discordgo.MessageEmbedImage{URL: "attachment://team.png"}
		files = append(files, banner)
	}
	return embed, files
}

// editMessageEmbedFiles edita el mensaje de la interacción con un embed,
// componentes y adjuntos (los adjuntos anteriores se reemplazan).
// Devuelve false si Discord lo rechaza (p. ej. adjuntos al editar el mensaje
// efímero del /ft) para que el llamador reintente sin archivos.
func editMessageEmbedFiles(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed, components []discordgo.MessageComponent, files []*discordgo.File) bool {
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	if files == nil {
		files = []*discordgo.File{}
	}
	empty := ""
	emptyAtts := []*discordgo.MessageAttachment{}
	embeds := []*discordgo.MessageEmbed{embed}
	_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:     &empty,
		Embeds:      &embeds,
		Components:  &components,
		Files:       files,
		Attachments: &emptyAtts,
	})
	if err != nil {
		log.Printf("❌ No se pudo editar el panel de equipo: %v\n", err)
		return false
	}
	return true
}

// replyEphemeral envía un mensaje que solo ve quien hizo clic.
func replyEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: content,
		Flags:   discordgo.MessageFlagsEphemeral,
	})
	if err != nil {
		log.Printf("❌ No se pudo enviar el aviso efímero: %v\n", err)
	}
}

func respondError(s *discordgo.Session, i *discordgo.InteractionCreate, err error) {
	var ue userError
	switch {
	case errors.As(err, &ue):
		replyEphemeral(s, i, ue.msg)
	case errors.Is(err, gorm.ErrRecordNotFound):
		replyEphemeral(s, i, "⚠️ Esta reta ya no existe.")
	default:
		log.Printf("❌ Error interno en matchmaking: %v\n", err)
		replyEphemeral(s, i, "❌ Ocurrió un error interno. Intenta de nuevo en unos segundos.")
	}
}

// withMatch carga la reta con bloqueo de fila (SELECT ... FOR UPDATE), ejecuta fn y
// guarda los cambios en una sola transacción.
func withMatch(db *gorm.DB, matchID uint64, fn func(tx *gorm.DB, m *models.Matchmaking) error) (models.Matchmaking, error) {
	var match models.Matchmaking
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&match, matchID).Error; err != nil {
			return err
		}
		if err := fn(tx, &match); err != nil {
			return err
		}
		return tx.Save(&match).Error
	})
	return match, err
}

// ---------------------------------------------------------------------------
// /ft
// ---------------------------------------------------------------------------

func handleFTCommand(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB) {
	// Respuesta diferida y efímera: el trabajo real (DB + DM) puede tardar más de 3 s.
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})
	if err != nil {
		log.Printf("❌ No se pudo confirmar el comando /ft a Discord: %v\n", err)
		return
	}

	reply := func(content string) {
		if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
			log.Printf("❌ No se pudo responder al comando /ft: %v\n", err)
		}
	}

	author := interactionUser(i)
	data := i.ApplicationCommandData()

	targetFT := 0
	rivalID := ""
	roomCode := ""
	for _, opt := range data.Options {
		switch opt.Name {
		case "objetivo":
			targetFT = int(opt.IntValue())
		case "rival":
			rivalID, _ = opt.Value.(string)
		case "sala":
			roomCode, _ = opt.Value.(string)
		}
	}

	if author == nil || rivalID == "" {
		reply("❌ Comando inválido.")
		return
	}
	// Solo FT 3 y FT 5: los formatos 7 y 10 están desactivados (la línea de
	// bolitas y los keycaps 1..10 solo cubren bien hasta FT5 = 9 games).
	if targetFT != 3 && targetFT != 5 {
		reply("❌ Formato inválido. Usa FT 3 o FT 5.")
		return
	}

	rival := &discordgo.User{ID: rivalID}
	if data.Resolved != nil {
		if u, ok := data.Resolved.Users[rivalID]; ok && u != nil {
			rival = u
		}
	}
	if rival.Username == "" {
		if u, err := s.User(rivalID); err == nil {
			rival = u
		}
	}

	if author.ID == rival.ID {
		reply("❌ No puedes retarte a ti mismo.")
		return
	}
	if rival.Bot {
		reply("❌ No puedes retar a un bot.")
		return
	}

	match := models.Matchmaking{
		League:           matchmakingLeague,
		Player1DiscordID: author.ID,
		Player1Tag:       author.Username,
		Player1Avatar:    author.AvatarURL("128"),
		Player2DiscordID: rival.ID,
		Player2Tag:       rival.Username,
		Player2Avatar:    rival.AvatarURL("128"),
		TargetFT:         targetFT,
		RoomCode:         roomCode,
		PointsAtStake:    targetFT * 5,
		Status:           statusPending,
	}
	if err := db.Create(&match).Error; err != nil {
		log.Printf("❌ No se pudo crear la reta: %v\n", err)
		reply("❌ No se pudo crear la reta. Intenta de nuevo en unos segundos.")
		return
	}

	content := fmt.Sprintf("🎮 **SOLICITUD DE RETA FT %d**\n⚔️ **%s** te ha retado a un duelo.\n💰 Puntos de Matchmaking en juego: **%d pts**\n%s\n¿Aceptas el desafío?",
		targetFT, author.Username, match.PointsAtStake, roomCodeLine(&match))

	keyboard := discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "✅ Aceptar",
				Style:    discordgo.SuccessButton,
				CustomID: fmt.Sprintf("ft_accept_%d", match.ID),
			},
			discordgo.Button{
				Label:    "❌ Rechazar",
				Style:    discordgo.DangerButton,
				CustomID: fmt.Sprintf("ft_reject_%d", match.ID),
			},
		},
	}

	dm, err := s.UserChannelCreate(rival.ID)
	if err == nil {
		_, err = s.ChannelMessageSendComplex(dm.ID, &discordgo.MessageSend{
			Content:    content,
			Components: []discordgo.MessageComponent{keyboard},
		})
	}
	if err != nil {
		log.Printf("❌ No se pudo enviar el DM de la reta %d: %v\n", match.ID, err)
		db.Model(&match).Update("Status", statusCancelled)
		reply("❌ No se pudo enviar el mensaje privado al rival. Asegúrate de que tenga los DMs abiertos.")
		return
	}

	success := fmt.Sprintf("✅ Invitación enviada al privado de **%s**. Esperando su respuesta...", rival.Username)
	if roomCode != "" {
		success += fmt.Sprintf("\n🏠 Código de sala enviado: `%s`", roomCode)
	}

	// El RETADOR arma su equipo AQUÍ MISMO, en la respuesta del /ft (efímera:
	// solo él la ve). No tiene que ir a ningún MD ni esperar la aceptación.
	// Su rival no ve nada; el equipo se revela en el hilo solo al confirmar los 3.
	success += "\n🧩 **Arma tu equipo abajo mientras esperas** — nadie más lo ve."
	embed := teamPanelEmbed(match, "p1")
	embeds := []*discordgo.MessageEmbed{embed}
	components := getPlayerTeamKeyboard(match, "p1")
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:    &success,
		Embeds:     &embeds,
		Components: &components,
	}); err != nil {
		log.Printf("❌ No se pudo enviar el panel de selección del /ft: %v\n", err)
	}

}

// ---------------------------------------------------------------------------
// Botones y menús (routing)
// ---------------------------------------------------------------------------

func handleComponentInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB) {
	data := i.MessageComponentData()
	customID := data.CustomID
	if !strings.HasPrefix(customID, "ft_") {
		return
	}

	parts := strings.Split(customID, "_")
	if len(parts) < 3 {
		return
	}
	matchID, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return
	}

	// 1) Confirmar a Discord ANTES de cualquier consulta a la base de datos.
	if !ackComponent(s, i) {
		return
	}

	user := interactionUser(i)
	if user == nil {
		return
	}

	switch parts[1] {
	case "accept":
		handleAccept(s, i, db, user, matchID)
	case "reject":
		handleReject(s, i, db, user, matchID)
	// Menús de equipo: personaje 1, personaje 2 y fusión
	case "c1", "c2", "fuse":
		side := ""
		if len(parts) > 3 {
			side = parts[3]
		}
		handleTeamSelect(s, i, db, user, matchID, parts[1], side, data.Values)
	// Corregir la selección de equipo: borra los picks y reabre los menús
	case "reset":
		side := ""
		if len(parts) > 3 {
			side = parts[3]
		}
		handleTeamReset(s, i, db, user, matchID, side)
	// Reporte del game (W): 1 botón, se anota directo
	case "win":
		side := ""
		if len(parts) > 3 {
			side = parts[3]
		}
		handleReportGame(s, i, db, user, matchID, side)
	// Corregir: deshace el ÚLTIMO game
	case "correct":
		handleCorrectReport(s, i, db, user, matchID)
	// Resultado final del set: 2 dropdowns
	case "fullset":
		handleFullSetPanel(s, i, db, user, matchID)
	case "fs1", "fs2":
		handleFullSetSelect(s, i, db, user, matchID, parts[1], data.Values)
	case "back":
		handleFullSetBack(s, i, db, user, matchID)
	case "close":
		handleCloseSet(s, i, db, user, matchID)
	case "acceptclose":
		handleAcceptCloseSet(s, i, db, user, matchID)
	case "declineclose":
		handleDeclineCloseSet(s, i, db, user, matchID)
	}
}

func isCombatant(userID string, m *models.Matchmaking) bool {
	return userID == m.Player1DiscordID || userID == m.Player2DiscordID
}

// isMod dice si quien interactúa es Admin o Moderador del servidor.
func isMod(i *discordgo.InteractionCreate) bool {
	return i.Member != nil && i.Member.Permissions&(discordgo.PermissionAdministrator|discordgo.PermissionManageServer) != 0
}

// ---------------------------------------------------------------------------
// 1. ACEPTAR RETA
// ---------------------------------------------------------------------------

func handleAccept(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	// Se "reclama" la reta cambiando el estado dentro de la transacción: un doble clic no crea dos hilos.
	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if user.ID != m.Player2DiscordID {
			return userError{"❌ Solo el jugador retado puede aceptar esta solicitud."}
		}
		if m.Status != statusPending {
			return userError{"⚠️ Esta solicitud ya no está pendiente."}
		}
		m.Status = statusInProgress
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	revert := func(msg string) {
		db.Model(&models.Matchmaking{}).Where("id = ?", match.ID).Update("Status", statusPending)
		replyEphemeral(s, i, msg)
	}

	channelID := getMatchmakingChannelID()
	baseMsg, err := s.ChannelMessageSend(channelID,
		fmt.Sprintf("⚔️ **¡RETA ACEPTADA!**\n<@%s> vs <@%s> (FT %d)", match.Player1DiscordID, match.Player2DiscordID, match.TargetFT))
	if err != nil {
		log.Printf("❌ No se pudo escribir en el canal de matchmaking: %v\n", err)
		revert("❌ Error al escribir en el canal de matchmaking público.")
		return
	}

	threadName := fmt.Sprintf("🥊 %s vs %s (FT %d)", match.Player1Tag, match.Player2Tag, match.TargetFT)
	thread, err := s.MessageThreadStart(channelID, baseMsg.ID, threadName, 1440)
	if err != nil {
		log.Printf("❌ No se pudo crear el hilo: %v\n", err)
		_ = s.ChannelMessageDelete(channelID, baseMsg.ID)
		revert("❌ Error al crear el hilo en Discord.")
		return
	}

	// Selección PRIVADA: la ficha con menús va por MD — el rival no ve tus picks
	// hasta que confirmas los 3. En el hilo solo quedan paneles de estado.
	setupDM := func(side string) bool {
		return sendTeamPanelDM(s, match, side)
	}
	// El retado (P2) siempre elige por MD. El retador (P1) ya eligió desde el
	// panel del /ft en el 99% de los casos: MD SOLO como fallback si no terminó.
	p2DM := setupDM("p2")
	p1DM := false
	if !match.P1TeamComplete() {
		p1DM = setupDM("p1")
	}

	// Paneles públicos del hilo: estado SIN picks; si el MD falló, ahí van los menús.
	pubPanel := func(side string, viaDM bool) *discordgo.MessageSend {
		// El retador ya confirmó desde el /ft: su ficha se revela de una vez.
		if side == "p1" && match.P1TeamComplete() {
			embed, files := teamPanelEmbedWithBanner(match, "p1")
			return &discordgo.MessageSend{
				Embeds: []*discordgo.MessageEmbed{embed},
				Files:  files,
			}
		}
		tag := match.Player1Tag
		if side == "p2" {
			tag = match.Player2Tag
		}
		foot := "Revisa tus mensajes privados para elegir tu equipo"
		if side == "p1" && viaDM {
			foot = "Termina de armar tu equipo en el panel del /ft o en tu MD"
		} else if side == "p1" && !viaDM {
			foot = "No pude enviarte MD: elige tu equipo en el panel del /ft o en este panel"
		} else if side == "p2" && !viaDM {
			foot = "No pude enviarte MD: elige tu equipo en este panel"
		}
		send := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{{
			Color:       0x5865F2,
			Title:       "👤 " + tag,
			Description: "🤖 Armando equipo...",
			Footer:      &discordgo.MessageEmbedFooter{Text: foot},
		}}}
		if !viaDM {
			send.Components = getPlayerTeamKeyboard(match, side)
		}
		return send
	}
	teamP1, err1 := s.ChannelMessageSendComplex(thread.ID, pubPanel("p1", p1DM))
	teamP2, err2 := s.ChannelMessageSendComplex(thread.ID, pubPanel("p2", p2DM))

	gpEmbed, gpFiles := gamePanelEmbed(match)
	gamePanel, err3 := s.ChannelMessageSendComplex(thread.ID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{gpEmbed},
		Files:  gpFiles,
	})
	if !p1DM && !match.P1TeamComplete() {
		log.Printf("⚠️ No se pudo enviar el panel de equipo por MD a %s: usa el panel del /ft.\n", match.Player1Tag)
	}
	if !p2DM {
		log.Printf("⚠️ No se pudo enviar el panel de equipo por MD a %s: menús públicos de fallback.\n", match.Player2Tag)
	}
	if err1 != nil {
		log.Printf("❌ No se pudo publicar el panel de equipo de %s: %v\n", match.Player1Tag, err1)
	}
	if err2 != nil {
		log.Printf("❌ No se pudo publicar el panel de equipo de %s: %v\n", match.Player2Tag, err2)
	}
	if err3 != nil {
		log.Printf("❌ No se pudo publicar el panel de games: %v\n", err3)
	}

	now := time.Now()
	updates := map[string]interface{}{"ThreadID": thread.ID, "LastRoundAt": &now}
	if err1 == nil {
		updates["TeamPanelMessageID1"] = teamP1.ID
	}
	if err2 == nil {
		updates["TeamPanelMessageID2"] = teamP2.ID
	}
	if err3 == nil {
		updates["PanelMessageID"] = gamePanel.ID
	}
	if err := db.Model(&models.Matchmaking{}).Where("id = ?", match.ID).Updates(updates).Error; err != nil {
		log.Printf("❌ No se pudieron guardar los paneles de la reta %d: %v\n", match.ID, err)
	}

	editMessage(s, i, fmt.Sprintf("✅ ¡Has aceptado la reta! Espacio de combate creado: <#%s>", thread.ID), nil)

	if dm, err := s.UserChannelCreate(match.Player1DiscordID); err == nil {
		if match.P1TeamComplete() {
			_, _ = s.ChannelMessageSend(dm.ID, fmt.Sprintf("⚡ **%s** ha aceptado tu reta (FT %d). Tu equipo ya está confirmado — ¡entra al hilo: <#%s>", match.Player2Tag, match.TargetFT, thread.ID))
		} else {
			_, _ = s.ChannelMessageSend(dm.ID, fmt.Sprintf("⚡ **%s** ha aceptado tu reta (FT %d). Termina de armar tu equipo (panel del /ft o este MD) — hilo: <#%s>", match.Player2Tag, match.TargetFT, thread.ID))
		}
	}
}

// ---------------------------------------------------------------------------
// 2. RECHAZAR RETA
// ---------------------------------------------------------------------------

func handleReject(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	_, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if user.ID != m.Player2DiscordID {
			return userError{"❌ Solo el jugador retado puede rechazar esta solicitud."}
		}
		if m.Status != statusPending {
			return userError{"⚠️ Esta solicitud ya no está pendiente."}
		}
		m.Status = statusRejected
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}
	editMessage(s, i, "❌ Has rechazado la solicitud de reta.", nil)
}

// ---------------------------------------------------------------------------
// 3. SELECCIÓN DE EQUIPO (menús: personaje 1, personaje 2, fusión)
// ---------------------------------------------------------------------------

func handleTeamSelect(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64, kind, side string, values []string) {
	if side != "p1" && side != "p2" {
		return
	}
	if len(values) != 1 {
		replyEphemeral(s, i, "⚠️ Selección no válida.")
		return
	}
	v := values[0]
	if kind == "fuse" && !gamechars.ValidFuse(v) {
		replyEphemeral(s, i, "⚠️ Fusión no válida.")
		return
	}
	if kind != "fuse" && !gamechars.ValidCharacter(v) {
		replyEphemeral(s, i, "⚠️ Personaje no válido.")
		return
	}

	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if side == "p1" && user.ID != m.Player1DiscordID {
			return userError{fmt.Sprintf("🚫 Solo **%s** puede elegir su equipo.", m.Player1Tag)}
		}
		if side == "p2" && user.ID != m.Player2DiscordID {
			return userError{fmt.Sprintf("🚫 Solo **%s** puede elegir su equipo.", m.Player2Tag)}
		}
		// El RETADOR puede armar su equipo desde el panel del /ft mientras la
		// reta sigue pendiente; el retado solo después de aceptar.
		if m.Status == statusPending && side == "p2" {
			return userError{"⚠️ Aún no has aceptado esta reta."}
		}
		if m.Status != statusInProgress && !(m.Status == statusPending && side == "p1") {
			return userError{"⚠️ El set ya no está en curso."}
		}

		// Equipo ya confirmado: los menús quedaron ocultos; se ignoran clics de menús viejos.
		if (side == "p1" && m.P1TeamComplete()) || (side == "p2" && m.P2TeamComplete()) {
			return userError{"⚠️ Tu equipo ya está confirmado: la selección de personajes y fusión está cerrada."}
		}

		// Personaje 1 y Personaje 2 deben ser distintos.
		if side == "p1" && ((kind == "c1" && v == m.P1Char2) || (kind == "c2" && v == m.P1Char1)) {
			return userError{"⚠️ Ese ya es tu otro personaje: el **Personaje 1** y el **Personaje 2** deben ser distintos."}
		}
		if side == "p2" && ((kind == "c1" && v == m.P2Char2) || (kind == "c2" && v == m.P2Char1)) {
			return userError{"⚠️ Ese ya es tu otro personaje: el **Personaje 1** y el **Personaje 2** deben ser distintos."}
		}

		switch {
		case kind == "c1" && side == "p1":
			m.P1Char1 = v
		case kind == "c2" && side == "p1":
			m.P1Char2 = v
		case kind == "fuse" && side == "p1":
			m.P1Fuse = v
		case kind == "c1" && side == "p2":
			m.P2Char1 = v
		case kind == "c2" && side == "p2":
			m.P2Char2 = v
		case kind == "fuse" && side == "p2":
			m.P2Fuse = v
		}
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	// ¿El equipo de quien eligió quedó completo (2 personajes + fusión)?
	teamComplete := (side == "p1" && match.P1TeamComplete()) || (side == "p2" && match.P2TeamComplete())

	// Panel donde se hizo clic (MD privado, público o el efímero del /ft).
	// Teclado progresivo: mientras faltan picks muestra solo los menús pendientes
	// + "🔄 Corregir"; al completar se cierra la selección y queda el botón.
	embed, files := teamPanelEmbedWithBanner(match, side)
	keyboard := getPlayerTeamKeyboard(match, side)
	if !editMessageEmbedFiles(s, i, embed, keyboard, files) {
		// El efímero del /ft a veces rechaza adjuntos al editar → sin banner.
		editMessageEmbedFiles(s, i, teamPanelEmbed(match, side), keyboard, nil)
	}
	if teamComplete {
		// Solo al CONFIRMAR se revela el equipo en el panel público del hilo
		// (durante la reta pendiente el hilo aún no existe: no pasa nada).
		refreshPublicTeamPanel(s, match, side)
	}

	// Ambos equipos completos → activa el panel de games (tarjeta VS + botones W).
	if match.P1TeamComplete() && match.P2TeamComplete() {
		refreshGamePanelThread(s, match)
	}
}

// handleTeamReset: "🔄 Corregir selección" — borra los picks del jugador y
// reabre los menús (para miss-clicks). Solo mientras no haya games jugados.
func handleTeamReset(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64, side string) {
	if side != "p1" && side != "p2" {
		return
	}
	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if side == "p1" && user.ID != m.Player1DiscordID {
			return userError{fmt.Sprintf("🚫 Solo **%s** puede corregir su selección.", m.Player1Tag)}
		}
		if side == "p2" && user.ID != m.Player2DiscordID {
			return userError{fmt.Sprintf("🚫 Solo **%s** puede corregir su selección.", m.Player2Tag)}
		}
		// El retador puede corregir desde el panel del /ft con la reta pendiente.
		if m.Status == statusPending && side == "p2" {
			return userError{"⚠️ Aún no has aceptado esta reta."}
		}
		if m.Status != statusInProgress && !(m.Status == statusPending && side == "p1") {
			return userError{"⚠️ El set ya no está en curso."}
		}
		// Con games jugados el equipo queda sellado (el marcador y la tarjeta VS
		// ya se mostraron con esos personajes).
		if m.Score1+m.Score2 > 0 {
			return userError{"⚠️ Ya hay games jugados en este set: el equipo ya no se puede cambiar."}
		}
		if side == "p1" {
			m.P1Char1, m.P1Char2, m.P1Fuse = "", "", ""
		} else {
			m.P2Char1, m.P2Char2, m.P2Fuse = "", "", ""
		}
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	// El panel donde se hizo clic vuelve al estado de selección (menús completos).
	// Si se corrigió desde el hilo público, los menús van por MD (privacidad);
	// si el MD falla, quedan en ese panel como fallback.
	embed, files := teamPanelEmbedWithBanner(match, side)
	keyboard := getPlayerTeamKeyboard(match, side)
	if i.ChannelID == match.ThreadID && sendTeamPanelDM(s, match, side) {
		keyboard = nil // sigue eligiendo por MD; el hilo queda solo como estado
	}
	if !editMessageEmbedFiles(s, i, embed, keyboard, files) {
		editMessageEmbedFiles(s, i, teamPanelEmbed(match, side), keyboard, nil)
	}

	// El hilo refleja el cambio: el panel público vuelve a "Armando equipo..."
	// y el panel de games (si la tarjeta VS ya se había mostrado) vuelve a esperar.
	refreshPublicTeamPanel(s, match, side)
	if !match.P1TeamComplete() || !match.P2TeamComplete() {
		refreshGamePanelThread(s, match)
	}
}

// sendTeamPanelDM envía la ficha de selección con sus menús por mensaje privado.
// Devuelve false si el MD falla (el llamador decide el fallback).
func sendTeamPanelDM(s *discordgo.Session, m models.Matchmaking, side string) bool {
	playerID := m.Player1DiscordID
	if side == "p2" {
		playerID = m.Player2DiscordID
	}
	dm, err := s.UserChannelCreate(playerID)
	if err != nil {
		return false
	}
	embed, files := teamPanelEmbedWithBanner(m, side)
	_, err = s.ChannelMessageSendComplex(dm.ID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{embed},
		Components: getPlayerTeamKeyboard(m, side),
		Files:      files,
	})
	return err == nil
}

// refreshPublicTeamPanel sincroniza el panel público del hilo con el estado del
// equipo: ficha dorada al confirmar, "🤖 Armando equipo..." mientras elige
// (también si corrige la selección después de haber confirmado).
func refreshPublicTeamPanel(s *discordgo.Session, m models.Matchmaking, side string) {
	msgID := m.TeamPanelMessageID1
	if side == "p2" {
		msgID = m.TeamPanelMessageID2
	}
	if m.ThreadID == "" || msgID == "" {
		return
	}

	complete := (side == "p1" && m.P1TeamComplete()) || (side == "p2" && m.P2TeamComplete())
	var embed *discordgo.MessageEmbed
	var files []*discordgo.File
	var components *[]discordgo.MessageComponent
	if complete {
		embed, files = teamPanelEmbedWithBanner(m, side)
		// El dueño puede corregir desde el hilo (mientras no haya games).
		btns := getTeamResetKeyboard(m, side)
		components = &btns
	} else {
		tag := m.Player1Tag
		if side == "p2" {
			tag = m.Player2Tag
		}
		embed = &discordgo.MessageEmbed{
			Color:       0x5865F2,
			Title:       "👤 " + tag,
			Description: "🤖 Armando equipo...",
		}
		// Components nil = sin cambios: si este panel es el fallback público con
		// menús, estos se conservan; si era solo de estado, sigue limpio.
	}

	emptyAtts := []*discordgo.MessageAttachment{}
	embeds := []*discordgo.MessageEmbed{embed}
	if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:     m.ThreadID,
		ID:          msgID,
		Embeds:      &embeds,
		Files:       files,
		Components:  components,
		Attachments: &emptyAtts,
	}); err != nil {
		log.Printf("❌ No se pudo sincronizar el panel público de equipo: %v\n", err)
	}
}

// refreshGamePanelThread actualiza el panel de games del hilo sin interacción
// (p. ej. al completarse ambos equipos).
func refreshGamePanelThread(s *discordgo.Session, m models.Matchmaking) {
	if m.ThreadID == "" || m.PanelMessageID == "" {
		return
	}
	embed, files := gamePanelEmbed(m)
	components := getGameKeyboard(m.ID, m.Player1Tag, m.Player2Tag)
	empty := ""
	emptyAtts := []*discordgo.MessageAttachment{}
	embeds := []*discordgo.MessageEmbed{embed}
	if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:     m.ThreadID,
		ID:          m.PanelMessageID,
		Content:     &empty,
		Embeds:      &embeds,
		Components:  &components,
		Files:       files,
		Attachments: &emptyAtts,
	}); err != nil {
		log.Printf("❌ No se pudo actualizar el panel de games del hilo: %v\n", err)
	}
}

// ---------------------------------------------------------------------------
// 4. REPORTAR EL GAME (W) → RESULT VERIFICATION
// ---------------------------------------------------------------------------

func handleReportGame(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64, side string) {
	var report *matchReport

	match, err := withMatch(db, matchID, func(tx *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 Solo los combatientes pueden reportar."}
		}
		if m.Status == statusPendingClose {
			return userError{"⚠️ Hay una propuesta de cierre en curso."}
		}
		// Sets viejos atascados en PENDING_VERIFY (doble confirmación): se anota el
		// game pendiente y se sigue con el flujo de 1 botón.
		if m.Status == statusPendingVerify {
			if m.PendingGameWinner == 1 || m.PendingGameWinner == 2 {
				appendRound(m, fmt.Sprintf("p%d", m.PendingGameWinner))
				if m.PendingGameWinner == 1 {
					m.Score1++
				} else {
					m.Score2++
				}
			}
			m.PendingGameWinner = 0
			m.PendingReportedBy = ""
			m.Status = statusInProgress
		}
		if m.Status != statusInProgress {
			return userError{"⚠️ Este set no está en curso."}
		}
		if !m.P1TeamComplete() || !m.P2TeamComplete() {
			return userError{"⚠️ Ambos equipos deben estar completos antes de reportar."}
		}
		switch side {
		case "p1", "p2":
			appendRound(m, side)
			if side == "p1" {
				m.Score1++
			} else {
				m.Score2++
			}
		default:
			return userError{"⚠️ Botón no válido."}
		}
		now := time.Now()
		m.LastRoundAt = &now

		// ¿Llegó al FT? Se aplica directo: 1 botón, sin doble confirmación.
		if m.Score1 >= m.TargetFT || m.Score2 >= m.TargetFT {
			if m.Score1 > m.Score2 {
				m.WinnerDiscordID, m.LoserDiscordID = m.Player1DiscordID, m.Player2DiscordID
			} else {
				m.WinnerDiscordID, m.LoserDiscordID = m.Player2DiscordID, m.Player1DiscordID
			}
			m.Status = statusFinished
			r, err := applyMatchResult(tx, m)
			if err != nil {
				return err
			}
			report = &r
		}
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	if report != nil {
		publishFinished(s, i, *report)
		return
	}
	refreshGamePanel(s, i, "", match)
}

// ---------------------------------------------------------------------------
// 6. CORREGIR EL REPORTE: re-anota el game actual sin castigo.
// ---------------------------------------------------------------------------

// CORREGIR: deshace el ÚLTIMO game reportado (no reinicia el set).
func handleCorrectReport(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 Solo los combatientes pueden corregir."}
		}
		if m.Status == statusPendingClose {
			return userError{"⚠️ Hay una propuesta de cierre en curso."}
		}
		if m.Status != statusInProgress {
			return userError{"⚠️ Solo se puede corregir con el set en curso."}
		}
		var hist []int
		if b := []byte(m.RoundHistory); len(b) > 0 {
			if err := json.Unmarshal(b, &hist); err != nil {
				hist = nil
			}
		}
		if len(hist) == 0 {
			return userError{"⚠️ Todavía no hay games para corregir."}
		}
		last := hist[len(hist)-1]
		hist = hist[:len(hist)-1]
		b, _ := json.Marshal(hist)
		m.RoundHistory = string(b)
		if last == 1 && m.Score1 > 0 {
			m.Score1--
		}
		if last == 2 && m.Score2 > 0 {
			m.Score2--
		}
		now := time.Now()
		m.LastRoundAt = &now
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}
	replyEphemeral(s, i, "✏️ **Corregido:** se deshizo el último game.")
	refreshGamePanel(s, i, "", match)
}

// ---------------------------------------------------------------------------
// 8. PROPONER CIERRE DE SET (🏳️ Cerrar set) — el caso "FT10 que quedó 4-2".
// ---------------------------------------------------------------------------

func handleCloseSet(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	mod := isMod(i)

	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if !mod && !isCombatant(user.ID, m) {
			return userError{"🚫 **Acceso denegado:** Solo los combatientes (o un Admin/Moderador) pueden proponer cerrar el set."}
		}
		if m.Status == statusPendingClose {
			return userError{"⚠️ Ya hay una propuesta de cierre en curso."}
		}
		// Sets viejos atascados en PENDING_VERIFY: se absorbe el game pendiente
		// (mismo criterio que al reportar con W) y se permite cerrar el set.
		if m.Status == statusPendingVerify {
			if m.PendingGameWinner == 1 || m.PendingGameWinner == 2 {
				appendRound(m, fmt.Sprintf("p%d", m.PendingGameWinner))
				if m.PendingGameWinner == 1 {
					m.Score1++
				} else {
					m.Score2++
				}
			}
			m.PendingGameWinner = 0
			m.PendingReportedBy = ""
			m.Status = statusInProgress
		}
		if m.Status != statusInProgress {
			return userError{"⚠️ Este set no está en curso."}
		}
		now := time.Now()
		m.Status = statusPendingClose
		m.CloseRequestedAt = &now
		m.CloseRequestedBy = user.ID
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	other := match.Player2Tag
	if user.ID == match.Player2DiscordID {
		other = match.Player1Tag
	}

	outcome := fmt.Sprintf("Si se cierra, gana **%s** con los **%d pts** completos del FT.", getLeaderTag(match), match.PointsAtStake)
	if match.Score1 == match.Score2 {
		outcome = "El marcador está empatado: si se cierra así, el set se **cancela sin puntos**."
	}

	content := fmt.Sprintf("🏳️ **PROPUESTA DE CIERRE DE SET**\n\n"+
		historyBlock(&match)+
		"%s\n\n"+
		"⚠️ **%s: tienes 2 minutos para responder.**\n"+
		"✅ **Aceptar cierre** = el set termina YA.\n"+
		"▶️ **Seguir jugando** = el set continúa intacto.\n"+
		"⏳ Si no respondes, el sistema cerrará el set solo a los 2 minutos. Un **Admin/Moderador** también puede cerrarlo dándole la victoria a quien va ganando.",
		outcome, other)
	editMessage(s, i, content, []discordgo.MessageComponent{getCloseKeyboard(match.ID)})
}

// ---------------------------------------------------------------------------
// 9. ACEPTAR EL CIERRE (combatiente contrario, o Admin/Moderador de inmediato).
// ---------------------------------------------------------------------------

func handleAcceptCloseSet(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	mod := isMod(i)

	var report *matchReport
	cancelled := false

	match, err := withMatch(db, matchID, func(tx *gorm.DB, m *models.Matchmaking) error {
		if m.Status != statusPendingClose {
			return userError{"⚠️ No hay ninguna propuesta de cierre pendiente."}
		}
		// Un mod puede aceptar (incluso su propia propuesta); un jugador no puede
		// aceptarse a sí mismo y solo el otro combatiente puede aceptar.
		if !mod && (user.ID == m.CloseRequestedBy || !isCombatant(user.ID, m)) {
			return userError{"🚫 Solo el otro combatiente (o un Admin/Moderador) puede aceptar el cierre."}
		}
		r, applied, err := finishWithLeader(tx, m)
		if err != nil {
			return err
		}
		report = &r
		cancelled = !applied
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	publishClosedSet(s, i, match, report, cancelled)
}

// ---------------------------------------------------------------------------
// 10. RECHAZAR EL CIERRE (▶️ Seguir jugando): el set se reactiva intacto.
// ---------------------------------------------------------------------------

func handleDeclineCloseSet(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 Solo los combatientes pueden reactivar el set."}
		}
		if m.Status != statusPendingClose {
			return userError{"⚠️ No hay ninguna propuesta de cierre pendiente."}
		}
		m.Status = statusInProgress
		m.CloseRequestedAt = nil
		m.CloseRequestedBy = ""
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}

	refreshGamePanel(s, i, "▶️ **Seguimos jugando** — la propuesta de cierre fue rechazada.", match)
}

// publishClosedSet muestra el resultado del cierre y publica el reporte + limpia el hilo.
func publishClosedSet(s *discordgo.Session, i *discordgo.InteractionCreate, match models.Matchmaking, report *matchReport, cancelled bool) {
	if cancelled {
		editMessage(s, i, fmt.Sprintf(
			"🏳️ **SET CERRADO EN EMPATE — CANCELADO SIN PUNTOS**\n\n"+
				historyBlock(&match)+
				"📍 Consulta el historial de retas en <#%s>.\n\n"+
				"⚠️ **Este hilo se eliminará automáticamente en 5 segundos...**",
			getMatchmakingResultChannelID()), nil)
		go closeMatchThread(s, match)
		return
	}

	r := *report
	editMessage(s, i, fmt.Sprintf(
		"🏁 **SET CERRADO — RESULTADO APLICADO**\n\n"+
			historyBlock(&r.Match)+
			"🏆 Ganador: **%s** (+%s pts, total **%d**)\n"+
			"El reporte final está publicado en <#%s>.\n"+
			"📍 Consulta el historial de retas en <#%s>.\n\n"+
			"⚠️ **Este hilo se eliminará automáticamente en 5 segundos...**",
		r.WinnerTag, signed(r.WinnerDelta), r.WinnerPoints,
		getMatchmakingResultChannelID(), getMatchmakingResultChannelID()), nil)

	go func() {
		sendMatchmakingReport(s, r)
		closeMatchThread(s, r.Match)
	}()
}

// ---------------------------------------------------------------------------
// Puntos y ranking de MATCHMAKING
// ---------------------------------------------------------------------------

type matchReport struct {
	Match                     models.Matchmaking
	WinnerTag, LoserTag       string
	WinnerDelta, LoserDelta   int
	WinnerPoints, LoserPoints int
	OldRankW, NewRankW        int
	OldRankL, NewRankL        int
}

// matchmakingRank devuelve la posición (empates comparten posición) de un jugador con esos puntos.
func matchmakingRank(tx *gorm.DB, points int) int {
	var higher int64
	tx.Model(&models.MatchmakingPlayer{}).Where("points > ?", points).Count(&higher)
	return int(higher) + 1
}

func sideInfo(m *models.Matchmaking, discordID string) (tag, avatar string) {
	if discordID == m.Player1DiscordID {
		return m.Player1Tag, m.Player1Avatar
	}
	return m.Player2Tag, m.Player2Avatar
}

// applyMatchResult suma los puntos al ganador y resta al perdedor SOLO en matchmaking_players.
// Debe llamarse dentro de la transacción que marca la reta como FINISHED.
func applyMatchResult(tx *gorm.DB, m *models.Matchmaking) (matchReport, error) {
	report := matchReport{Match: *m}

	// Garantiza que ambos existan (se crean la primera vez que juegan una reta confirmada).
	for _, id := range []string{m.Player1DiscordID, m.Player2DiscordID} {
		tag, avatar := sideInfo(m, id)
		p := models.MatchmakingPlayer{DiscordID: id, Username: tag, AvatarURL: avatar}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&p).Error; err != nil {
			return report, err
		}
	}

	// Bloquea ambas filas, siempre en el mismo orden, para evitar bloqueos cruzados entre retas.
	var rows []models.MatchmakingPlayer
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("discord_id IN ?", []string{m.WinnerDiscordID, m.LoserDiscordID}).
		Order("discord_id").
		Find(&rows).Error
	if err != nil {
		return report, err
	}

	byID := make(map[string]*models.MatchmakingPlayer, len(rows))
	for idx := range rows {
		byID[rows[idx].DiscordID] = &rows[idx]
	}
	winner, loser := byID[m.WinnerDiscordID], byID[m.LoserDiscordID]
	if winner == nil || loser == nil {
		return report, errors.New("no se encontraron los jugadores de matchmaking de esta reta")
	}

	report.OldRankW = matchmakingRank(tx, winner.Points)
	report.OldRankL = matchmakingRank(tx, loser.Points)

	oldWinnerPts, oldLoserPts := winner.Points, loser.Points

	winner.Points += m.PointsAtStake
	winner.Wins++

	loserNew := loser.Points - loserPenalty
	if loserNew < minPointsFloor {
		loserNew = minPointsFloor
	}
	if loserNew > loser.Points { // perder nunca sube los puntos
		loserNew = loser.Points
	}
	loser.Points = loserNew
	loser.Losses++

	// Mantiene actualizados nombre y avatar de Discord.
	if tag, avatar := sideInfo(m, winner.DiscordID); tag != "" {
		winner.Username = tag
		if avatar != "" {
			winner.AvatarURL = avatar
		}
	}
	if tag, avatar := sideInfo(m, loser.DiscordID); tag != "" {
		loser.Username = tag
		if avatar != "" {
			loser.AvatarURL = avatar
		}
	}

	if err := tx.Save(winner).Error; err != nil {
		return report, err
	}
	if err := tx.Save(loser).Error; err != nil {
		return report, err
	}

	report.NewRankW = matchmakingRank(tx, winner.Points)
	report.NewRankL = matchmakingRank(tx, loser.Points)
	report.WinnerTag, report.LoserTag = winner.Username, loser.Username
	report.WinnerPoints, report.LoserPoints = winner.Points, loser.Points
	report.WinnerDelta = winner.Points - oldWinnerPts
	report.LoserDelta = loser.Points - oldLoserPts
	return report, nil
}

// finishWithLeader cierra un set anticipadamente: gana quien va ARRIBA con los
// puntos COMPLETOS del FT; si hay empate, el set se cancela sin puntos.
func finishWithLeader(tx *gorm.DB, m *models.Matchmaking) (matchReport, bool, error) {
	if m.Score1 == m.Score2 {
		m.Status = statusCancelled
		return matchReport{Match: *m}, false, nil
	}
	if m.Score1 > m.Score2 {
		m.WinnerDiscordID, m.LoserDiscordID = m.Player1DiscordID, m.Player2DiscordID
	} else {
		m.WinnerDiscordID, m.LoserDiscordID = m.Player2DiscordID, m.Player1DiscordID
	}
	m.Status = statusFinished
	r, err := applyMatchResult(tx, m)
	return r, true, err
}

// ---------------------------------------------------------------------------
// Teclados (botones y menús)
// ---------------------------------------------------------------------------

// getPlayerTeamKeyboard: menús de selección PROGRESIVOS — cada menú desaparece
// al elegirse (elegiste P1 → solo quedan P2 y fusión; elegiste P2 → solo la
// fusión). Si hay al menos una elección, aparece "🔄 Corregir selección" para
// empezar de cero (miss-clicks). Equipo completo → solo queda el botón.
func getPlayerTeamKeyboard(m models.Matchmaking, side string) []discordgo.MessageComponent {
	c1, c2, fuse := m.P1Char1, m.P1Char2, m.P1Fuse
	complete := m.P1TeamComplete()
	if side == "p2" {
		c1, c2, fuse = m.P2Char1, m.P2Char2, m.P2Fuse
		complete = m.P2TeamComplete()
	}

	// Equipo confirmado: la selección se cierra; queda la opción de corregir
	// (útil si el miss-click fue justo en la fusión, el último menú).
	if complete {
		return getTeamResetKeyboard(m, side)
	}

	one := 1
	charOpts := make([]discordgo.SelectMenuOption, 0, len(gamechars.Characters))
	for _, c := range gamechars.Characters {
		charOpts = append(charOpts, discordgo.SelectMenuOption{Label: c.Name, Value: c.ID})
	}
	fuseOpts := make([]discordgo.SelectMenuOption, 0, len(gamechars.Fuses))
	for _, f := range gamechars.Fuses {
		fuseOpts = append(fuseOpts, discordgo.SelectMenuOption{Label: f.Name, Value: f.ID})
	}

	row := func(sm discordgo.SelectMenu) discordgo.ActionsRow {
		return discordgo.ActionsRow{Components: []discordgo.MessageComponent{sm}}
	}

	rows := []discordgo.MessageComponent{}
	if c1 == "" {
		rows = append(rows, row(discordgo.SelectMenu{
			MenuType:    discordgo.StringSelectMenu,
			CustomID:    fmt.Sprintf("ft_c1_%d_%s", m.ID, side),
			Placeholder: "1️⃣ Elige tu Personaje 1",
			MinValues:   &one,
			MaxValues:   one,
			Options:     charOpts,
		}))
	}
	if c2 == "" {
		rows = append(rows, row(discordgo.SelectMenu{
			MenuType:    discordgo.StringSelectMenu,
			CustomID:    fmt.Sprintf("ft_c2_%d_%s", m.ID, side),
			Placeholder: "2️⃣ Elige tu Personaje 2",
			MinValues:   &one,
			MaxValues:   one,
			Options:     charOpts,
		}))
	}
	if fuse == "" {
		rows = append(rows, row(discordgo.SelectMenu{
			MenuType:    discordgo.StringSelectMenu,
			CustomID:    fmt.Sprintf("ft_fuse_%d_%s", m.ID, side),
			Placeholder: "🔀 Elige tu Fusión",
			MinValues:   &one,
			MaxValues:   one,
			Options:     fuseOpts,
		}))
	}
	// Ya hay al menos una elección → botón para corregir miss-clicks.
	if c1 != "" || c2 != "" || fuse != "" {
		rows = append(rows, getTeamResetKeyboard(m, side)...)
	}
	return rows
}

// getTeamResetKeyboard: "🔄 Corregir selección" — borra los picks del jugador y
// reabre los 3 menús (solo funciona mientras no haya games jugados).
func getTeamResetKeyboard(m models.Matchmaking, side string) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "🔄 Corregir selección",
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("ft_reset_%d_%s", m.ID, side),
			},
		}},
	}
}

// getGameKeyboard: fila 1 = botones W (protagonistas) · fila 2 = utilitarios.
func getGameKeyboard(matchID uint, p1Tag, p2Tag string) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    fmt.Sprintf("W %s", p1Tag),
				Style:    discordgo.SuccessButton,
				CustomID: fmt.Sprintf("ft_win_%d_p1", matchID),
			},
			discordgo.Button{
				Label:    fmt.Sprintf("W %s", p2Tag),
				Style:    discordgo.SuccessButton,
				CustomID: fmt.Sprintf("ft_win_%d_p2", matchID),
			},
		}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "✏️ Corregir",
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("ft_correct_%d", matchID),
			},
			discordgo.Button{
				Label:    "📋 Resultado final",
				Style:    discordgo.PrimaryButton,
				CustomID: fmt.Sprintf("ft_fullset_%d", matchID),
			},
			discordgo.Button{
				Label:    "🏳️ Cerrar set",
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("ft_close_%d", matchID),
			},
		}},
	}
}

// getFullSetKeyboard: 2 dropdowns (games de cada jugador, 0..FT) + volver.
func getFullSetKeyboard(m models.Matchmaking) []discordgo.MessageComponent {
	one := 1
	opts := make([]discordgo.SelectMenuOption, 0, m.TargetFT+1)
	for n := 0; n <= m.TargetFT; n++ {
		opts = append(opts, discordgo.SelectMenuOption{Label: strconv.Itoa(n), Value: strconv.Itoa(n)})
	}
	row := func(sm discordgo.SelectMenu) discordgo.ActionsRow {
		return discordgo.ActionsRow{Components: []discordgo.MessageComponent{sm}}
	}
	return []discordgo.MessageComponent{
		row(discordgo.SelectMenu{
			MenuType:    discordgo.StringSelectMenu,
			CustomID:    fmt.Sprintf("ft_fs1_%d", m.ID),
			Placeholder: fmt.Sprintf("🏆 Games de %s", m.Player1Tag),
			MinValues:   &one,
			MaxValues:   one,
			Options:     opts,
		}),
		row(discordgo.SelectMenu{
			MenuType:    discordgo.StringSelectMenu,
			CustomID:    fmt.Sprintf("ft_fs2_%d", m.ID),
			Placeholder: fmt.Sprintf("🏆 Games de %s", m.Player2Tag),
			MinValues:   &one,
			MaxValues:   one,
			Options:     opts,
		}),
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "◀️ Volver a reportar por games",
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("ft_back_%d", m.ID),
			},
		}},
	}
}

// getCloseKeyboard: panel de propuesta de cierre (rondas bloqueadas mientras esté activo).
func getCloseKeyboard(matchID uint) discordgo.ActionsRow {
	return discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "✅ Aceptar cierre",
				Style:    discordgo.SuccessButton,
				CustomID: fmt.Sprintf("ft_acceptclose_%d", matchID),
			},
			discordgo.Button{
				Label:    "▶️ Seguir jugando",
				Style:    discordgo.SecondaryButton,
				CustomID: fmt.Sprintf("ft_declineclose_%d", matchID),
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Contenido de los paneles
// ---------------------------------------------------------------------------

// teamPanelEmbed: ficha del jugador. La FUSIÓN manda: al confirmar, encabeza el
// título y el embed pasa a dorado; mientras, slots con estado (✔ / por elegir).
func teamPanelEmbed(m models.Matchmaking, side string) *discordgo.MessageEmbed {
	tag, avatar, c1, c2, fuse := m.Player1Tag, m.Player1Avatar, m.P1Char1, m.P1Char2, m.P1Fuse
	complete := m.P1TeamComplete()
	if side == "p2" {
		tag, avatar, c1, c2, fuse = m.Player2Tag, m.Player2Avatar, m.P2Char1, m.P2Char2, m.P2Fuse
		complete = m.P2TeamComplete()
	}

	embed := &discordgo.MessageEmbed{}
	if avatar != "" {
		embed.Author = &discordgo.MessageEmbedAuthor{Name: tag, IconURL: avatar}
	}

	if complete {
		embed.Color = 0xFEE75C // dorado: equipo listo
		embed.Title = fmt.Sprintf("⚡ %s · FT %d", gamechars.FuseName(fuse), m.TargetFT)
		embed.Description = fmt.Sprintf("🔒 **%s + %s**", gamechars.CharacterName(c1), gamechars.CharacterName(c2))
		return embed
	}

	charSlot := func(label, id string) string {
		if n := gamechars.CharacterName(id); n != "" {
			return fmt.Sprintf("%s — **%s** ✔", label, n)
		}
		return fmt.Sprintf("%s — *por elegir*", label)
	}
	fuseSlot := "🔀 Fusión — *por elegir*"
	if n := gamechars.FuseName(fuse); n != "" {
		fuseSlot = fmt.Sprintf("🔀 Fusión — **%s** ✔", n)
	}
	embed.Color = 0x5865F2
	embed.Title = fmt.Sprintf("🧩 ARMA TU EQUIPO · FT %d", m.TargetFT)
	embed.Description = fmt.Sprintf("%s\n%s\n%s", charSlot("1️⃣ Personaje 1", c1), charSlot("2️⃣ Personaje 2", c2), fuseSlot)
	embed.Footer = &discordgo.MessageEmbedFooter{
		Text: "La fusión define tu estilo — se muestra en la portada del embed",
	}
	return embed
}

// historyBlock construye el resumen del set con el ORDEN de las victorias:
//
//	game 1: **A** W - L **B**
//	**Score** `2 - 0`
func historyBlock(m *models.Matchmaking) string {
	var hist []int
	if b := []byte(m.RoundHistory); len(b) > 0 {
		if err := json.Unmarshal(b, &hist); err != nil {
			hist = nil
		}
	}
	if len(hist) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, side := range hist {
		res := "L - W"
		if side == 1 {
			res = "W - L"
		}
		fmt.Fprintf(&sb, "game %d: **%s** %s **%s**\n", i+1, m.Player1Tag, res, m.Player2Tag)
	}
	fmt.Fprintf(&sb, "**Score** `%d - %d`\n\n", m.Score1, m.Score2)
	return sb.String()
}

// roomCodeLine devuelve la línea del código de la sala (vacía si la reta no tiene sala).
// La sala es OPCIONAL en /ft: sin código no agrega nada.
func roomCodeLine(m *models.Matchmaking) string {
	if m.RoomCode == "" {
		return ""
	}
	return fmt.Sprintf("🏠 **Sala:** `%s`\n", m.RoomCode)
}

func fullSetPanelContent(m models.Matchmaking) string {
	p1, p2 := "—", "—"
	if m.FinalPick1 >= 0 {
		p1 = strconv.Itoa(m.FinalPick1)
	}
	if m.FinalPick2 >= 0 {
		p2 = strconv.Itoa(m.FinalPick2)
	}
	return fmt.Sprintf("📋 **RESULTADO FINAL** — FT %d\n%s: **%s** · %s: **%s**\nElige los games de cada jugador (0 a %d); el set cierra con los dos.",
		m.TargetFT, m.Player1Tag, p1, m.Player2Tag, p2, m.TargetFT)
}

// publishFinished muestra el resultado del set terminado y limpia el hilo.
func publishFinished(s *discordgo.Session, i *discordgo.InteractionCreate, r matchReport) {
	editMessage(s, i, fmt.Sprintf(
		"🏁 **SET FINALIZADO** — %s `%d - %d` %s\n"+
			"🏆 **%s** %s pts (total **%d**)\n"+
			"📊 Reporte en <#%s> · este hilo se borra en 5 s.",
		r.Match.Player1Tag, r.Match.Score1, r.Match.Score2, r.Match.Player2Tag,
		r.WinnerTag, signed(r.WinnerDelta), r.WinnerPoints,
		getMatchmakingResultChannelID()), nil)
	go func() {
		sendMatchmakingReport(s, r)
		closeMatchThread(s, r.Match)
	}()
}

// appendRound agrega una victoria al historial (1 = Player1, 2 = Player2).
func appendRound(m *models.Matchmaking, side string) {
	var hist []int
	_ = json.Unmarshal([]byte(m.RoundHistory), &hist) // vacío/inválido = historial nuevo
	switch side {
	case "p1":
		hist = append(hist, 1)
	case "p2":
		hist = append(hist, 2)
	}
	b, _ := json.Marshal(hist)
	m.RoundHistory = string(b)
}

// getLeaderTag devuelve el tag de quien va ganando el marcador.
func getLeaderTag(m models.Matchmaking) string {
	if m.Score1 > m.Score2 {
		return m.Player1Tag
	}
	return m.Player2Tag
}

// ---------------------------------------------------------------------------
// Reporte al canal de resultados
// ---------------------------------------------------------------------------

// sendMatchmakingReport publica el resultado en el canal de resultados.
// Diseño "Matchup Card": tarjeta VS con los champions de ambos + marcador
// pixel-art al centro; la fusión del ganador en la miniatura; puntos/ranking
// en la descripción.
func sendMatchmakingReport(s *discordgo.Session, r matchReport) {
	m := r.Match

	// Equipo del ganador (para el footer y la miniatura).
	wc1, wc2, wfuse := m.P2Char1, m.P2Char2, m.P2Fuse
	if m.WinnerDiscordID == m.Player1DiscordID {
		wc1, wc2, wfuse = m.P1Char1, m.P1Char2, m.P1Fuse
	}

	desc := fmt.Sprintf("**%s %d — %d %s**\n", m.Player1Tag, m.Score1, m.Score2, m.Player2Tag)
	// Secuencia de games desde la perspectiva del GANADOR (el autor del embed):
	// solo si el set se reportó game a game (los "Resultado final" no tienen orden).
	foot := "⚡ " + gamechars.TeamLabel(wc1, wc2, wfuse)
	if hasRoundHistory(&m) {
		ref := "p2"
		if m.WinnerDiscordID == m.Player1DiscordID {
			ref = "p1"
		}
		desc += roundTrack(m, ref) + "\n"
		foot += " · 🟢 gana " + r.WinnerTag
	}
	desc += fmt.Sprintf("🏆 **%s** %s pts → **%d** · %s\n❌ %s %s pts → **%d** · %s",
		r.WinnerTag, signed(r.WinnerDelta), r.WinnerPoints, rankLine(r.OldRankW, r.NewRankW),
		r.LoserTag, signed(r.LoserDelta), r.LoserPoints, rankLine(r.OldRankL, r.NewRankL))

	wTag, wAvatar := sideInfo(&m, m.WinnerDiscordID)
	embed := &discordgo.MessageEmbed{
		Color:       0xFEE75C, // dorado
		Title:       fmt.Sprintf("🏆 VICTORIA · FT %d", m.TargetFT),
		Description: desc,
	}
	if wAvatar != "" {
		embed.Author = &discordgo.MessageEmbedAuthor{Name: wTag, IconURL: wAvatar}
	}

	files := []*discordgo.File{}
	// Miniatura: la fusión del ganador (portada del equipo).
	if f := iconFilePNG("fusion.png", "fuse", wfuse, 80); f != nil {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: "attachment://fusion.png"}
		files = append(files, f)
	}
	// Tarjeta VS: champions de ambos + marcador pixel-art (negro/morado/verde).
	if card := matchCard(m, m.Score1, m.Score2, "match.png"); card != nil {
		embed.Image = &discordgo.MessageEmbedImage{URL: "attachment://match.png"}
		files = append(files, card)
	}
	embed.Footer = &discordgo.MessageEmbedFooter{
		Text: foot,
	}

	if _, err := s.ChannelMessageSendComplex(getMatchmakingResultChannelID(), &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		Files:  files,
	}); err != nil {
		log.Printf("❌ No se pudo publicar el reporte de matchmaking: %v\n", err)
	}
}

// rankLine: "#2 → #2" (con ▲/▼ y cuántas posiciones si cambió) — versión compacta.
func rankLine(oldRank, newRank int) string {
	switch {
	case oldRank == newRank:
		return fmt.Sprintf("#%d → #%d", oldRank, newRank)
	case oldRank > newRank:
		return fmt.Sprintf("#%d → #%d ▲%d", oldRank, newRank, oldRank-newRank)
	default:
		return fmt.Sprintf("#%d → #%d ▼%d", oldRank, newRank, newRank-oldRank)
	}
}

func signed(n int) string {
	if n == 0 {
		return "±0"
	}
	return fmt.Sprintf("%+d", n)
}

// RESULTADO FINAL: reportar todo el set con 2 dropdowns (0..FT).

func handleFullSetPanel(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 Solo los combatientes pueden reportar el set."}
		}
		if m.Status != statusInProgress {
			return userError{"⚠️ Este set no está en curso."}
		}
		if !m.P1TeamComplete() || !m.P2TeamComplete() {
			return userError{"⚠️ Ambos equipos deben estar completos antes de reportar."}
		}
		m.FinalPick1, m.FinalPick2 = -1, -1
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}
	editMessage(s, i, fullSetPanelContent(match), getFullSetKeyboard(match))
}

func handleFullSetSelect(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64, kind string, values []string) {
	if kind != "fs1" && kind != "fs2" || len(values) != 1 {
		replyEphemeral(s, i, "⚠️ Selección no válida.")
		return
	}
	v, err := strconv.Atoi(values[0])
	if err != nil || v < 0 {
		replyEphemeral(s, i, "⚠️ Selección no válida.")
		return
	}

	var report *matchReport

	match, err2 := withMatch(db, matchID, func(tx *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 Solo los combatientes pueden reportar el set."}
		}
		if m.Status != statusInProgress {
			return userError{"⚠️ Este set no está en curso."}
		}
		if v > m.TargetFT {
			return userError{fmt.Sprintf("⚠️ Valor fuera de rango (0 a %d).", m.TargetFT)}
		}
		if kind == "fs1" {
			m.FinalPick1 = v
		} else {
			m.FinalPick2 = v
		}
		// Falta un dropdown: se muestra el progreso y se espera el otro.
		if m.FinalPick1 < 0 || m.FinalPick2 < 0 {
			return nil
		}
		// Resultado válido: exactamente uno llega al FT.
		if (m.FinalPick1 != m.TargetFT) == (m.FinalPick2 != m.TargetFT) {
			return userError{fmt.Sprintf("⚠️ En FT %d, uno debe terminar con **%d** y el otro con menos.", m.TargetFT, m.TargetFT)}
		}
		m.Score1, m.Score2 = m.FinalPick1, m.FinalPick2
		m.RoundHistory = "[]" // reporte directo: sin historial game a game
		if m.Score1 > m.Score2 {
			m.WinnerDiscordID, m.LoserDiscordID = m.Player1DiscordID, m.Player2DiscordID
		} else {
			m.WinnerDiscordID, m.LoserDiscordID = m.Player2DiscordID, m.Player1DiscordID
		}
		m.Status = statusFinished
		now := time.Now()
		m.LastRoundAt = &now
		r, err := applyMatchResult(tx, m)
		if err != nil {
			return err
		}
		report = &r
		return nil
	})
	if err2 != nil {
		respondError(s, i, err2)
		return
	}
	if report != nil {
		publishFinished(s, i, *report)
		return
	}
	editMessage(s, i, fullSetPanelContent(match), getFullSetKeyboard(match))
}

func handleFullSetBack(s *discordgo.Session, i *discordgo.InteractionCreate, db *gorm.DB, user *discordgo.User, matchID uint64) {
	match, err := withMatch(db, matchID, func(_ *gorm.DB, m *models.Matchmaking) error {
		if !isCombatant(user.ID, m) {
			return userError{"🚫 Solo los combatientes pueden usar el panel."}
		}
		if m.Status != statusInProgress {
			return userError{"⚠️ Este set no está en curso."}
		}
		m.FinalPick1, m.FinalPick2 = -1, -1
		return nil
	})
	if err != nil {
		respondError(s, i, err)
		return
	}
	refreshGamePanel(s, i, "", match)
}

// ---------------------------------------------------------------------------
// Cierre del hilo (borrado con fallback de archivado)
// ---------------------------------------------------------------------------

// closeMatchThread cierra el hilo de la reta terminada, tras la demora anunciada al usuario.
// Estrategia en cascada:
//  1. Borra el hilo (requiere permiso "Gestionar hilos" / MANAGE_THREADS).
//  2. Si falla (p. ej. 50013 Missing Permissions), archiva el hilo como fallback:
//     el creador del hilo (el bot) puede archivarlo aunque no tenga ese permiso.
func closeMatchThread(s *discordgo.Session, m models.Matchmaking) {
	if m.ThreadID == "" {
		log.Printf("⚠️ Reta %d sin ThreadID guardado: no se puede cerrar el hilo automáticamente.\n", m.ID)
		return
	}

	time.Sleep(5 * time.Second)

	if _, err := s.ChannelDelete(m.ThreadID); err != nil {
		log.Printf("⚠️ No se pudo borrar el hilo %s: %v — se intenta archivar como fallback.\n", m.ThreadID, err)
		archiveFallback(s, m.ThreadID)
		return
	}
	log.Printf("🗑️ Hilo %s (reta %d) eliminado automáticamente.\n", m.ThreadID, m.ID)
}

// archiveFallback archiva (y bloquea, si el permiso lo permite) un hilo que no se pudo borrar.
func archiveFallback(s *discordgo.Session, threadID string) {
	archived, locked := true, true

	// Intento 1: archivar Y bloquear en la misma petición.
	if _, err := s.ChannelEditComplex(threadID, &discordgo.ChannelEdit{
		Archived: &archived,
		Locked:   &locked,
	}); err == nil {
		log.Printf("🔒 Hilo %s archivado y bloqueado (fallback de borrado).\n", threadID)
		return
	}

	// Intento 2: solo archivar — el creador del hilo puede archivar sin MANAGE_THREADS.
	if _, err := s.ChannelEditComplex(threadID, &discordgo.ChannelEdit{Archived: &archived}); err != nil {
		log.Printf("❌ No se pudo cerrar el hilo %s: %v (revisa el permiso 'Gestionar hilos' del bot).\n", threadID, err)
		return
	}
	log.Printf("🗂️ Hilo %s archivado (el bloqueo requiere 'Gestionar hilos').\n", threadID)
}
