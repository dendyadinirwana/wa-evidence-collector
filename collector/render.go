package collector

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gobolditalic"
	"golang.org/x/image/font/gofont/goitalic"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Chat render (a RENDER, not a screenshot).
//
// For every evidence row that has no document, media, or link, the
// collector generates a PNG render of the verbatim message text in the
// style of WhatsApp Web, so the row still has supporting evidence. The
// output is a RENDER drawn from text: it is never a screenshot of the
// WhatsApp app, and the Excel row records it as Jenis Evidence
// "Screenshot chat (render)" so it is never claimed as an original.
//
// The look follows the reference renderer (render_chat_screenshot.py,
// template v3, verified 10 Nov 2026 against a real WhatsApp Web
// screenshot): white background with a faint doodle pattern, a centered
// date pill, grey incoming-style bubbles with a tail and a read-check
// badge, the sender name in green, *bold* and _italic_ WhatsApp markup
// honoured (markup characters not shown), verbatim text including typos,
// per-message timestamps, and an avatar circle with the sender's
// initial at the bottom left.
//
// Rendering is Go-native (golang.org/x/image): Noto Sans from the
// system font directories when present (same faces as the reference),
// DejaVu as a second choice, embedded Go fonts as a last resort, so the
// binary renders on machines without system fonts too. Own messages
// render in the same incoming-bubble style as the reference template;
// there is no green right-aligned variant yet.
//
// Defensive limit: a single message's text is capped at
// renderMaxRunes runes (with a "[...]" marker) so a pathological
// message cannot produce an unbounded image. The full text always stays
// in the JSON/CSV/Excel exports.

const (
	renderWidth        = 1320
	renderBubbleX      = 84
	renderBubbleY      = 68
	renderBubbleW      = 888
	renderPadX         = 30
	renderPadY         = 26
	renderBodySize     = 24.0
	renderSenderSize   = 26.0
	renderTimeSize     = 22.0
	renderPillSize     = 25.0
	renderAvatarSize   = 28.0
	renderLineH        = 33
	renderBlankH       = 12
	renderSenderH      = 44
	renderTimeAreaH    = 42 // 14 + 28, like the reference template
	renderMinHeight    = 400
	renderBottomPad    = 60
	renderBubbleGap    = 16
	renderBubbleRadius = 18
	renderMaxRunes     = 12000
)

var (
	renderBg            = color.RGBA{255, 255, 255, 255}
	renderDoodle        = color.RGBA{238, 238, 238, 255}
	renderPillBg        = color.RGBA{233, 233, 233, 255}
	renderPillText      = color.RGBA{43, 43, 43, 255}
	renderBubble        = color.RGBA{235, 235, 235, 255}
	renderShadow        = color.NRGBA{0, 0, 0, 18}
	renderSenderGreen   = color.RGBA{77, 151, 115, 255}
	renderText          = color.RGBA{17, 17, 17, 255}
	renderTimestampGrey = color.RGBA{118, 118, 118, 255}
	renderCheckRing     = color.RGBA{201, 201, 201, 255}
	renderCheck         = color.RGBA{85, 85, 85, 255}
	renderAvatarBg      = color.RGBA{180, 180, 180, 255}
	renderWhite         = color.RGBA{255, 255, 255, 255}
)

// Segment is one run of text with a single style.
type Segment struct {
	Text   string
	Bold   bool
	Italic bool
}

// RenderMessage is one chat message to render. Timestamp is UTC; it is
// shown in WIB. Sender is shown verbatim (already redacted by the
// collector when redaction is on, so a render never leaks a real name
// or phone number either).
type RenderMessage struct {
	Sender    string
	Timestamp time.Time
	Text      string
}

// ParseWhatsAppMarkup splits text into paragraphs of styled segments,
// honouring *bold* and _italic_ WhatsApp markup (the markup characters
// themselves are not kept). It mirrors the reference renderer's parser:
// a marker toggles its style, an unmatched marker styles the tail, and
// a marker of the other kind inside a styled run is literal text.
// Blank lines become nil paragraphs (paragraph gaps).
func ParseWhatsAppMarkup(text string) [][]Segment {
	var paras [][]Segment
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			paras = append(paras, nil)
			continue
		}
		var segs []Segment
		var buf strings.Builder
		var mode byte // 0, '*', or '_'
		flush := func() {
			if buf.Len() == 0 {
				return
			}
			segs = append(segs, Segment{Text: buf.String(), Bold: mode == '*', Italic: mode == '_'})
			buf.Reset()
		}
		for i := 0; i < len(line); i++ {
			ch := line[i]
			if (ch == '*' || ch == '_') && (mode == 0 || mode == ch) {
				flush()
				if mode == 0 {
					mode = ch
				} else {
					mode = 0
				}
				continue
			}
			buf.WriteByte(ch)
		}
		flush()
		paras = append(paras, segs)
	}
	return paras
}

// RenderFileName is HHMM_<chat-label>_chat.png for a message time (WIB).
// The chat label is the hashed label, never a real JID.
func RenderFileName(t time.Time, chatJID string) string {
	return t.In(wib).Format("1504") + "_" + ChatShort(chatJID) + "_chat.png"
}

// RenderDatePill is the centered date label for a message time (WIB),
// e.g. "11 November 2026".
func RenderDatePill(t time.Time) string {
	return t.In(wib).Format("2 January 2006")
}

// hasLink reports whether text carries an http(s) URL.
func hasLink(s string) bool {
	return strings.Contains(s, "http://") || strings.Contains(s, "https://")
}

// avatarInitial is the first rune of the sender string, or "?" when empty.
func avatarInitial(sender string) string {
	for _, r := range sender {
		return string(r)
	}
	return "?"
}

// ---------------------------------------------------------------------------
// Fonts
// ---------------------------------------------------------------------------

type fontStyle int

const (
	styleRegular fontStyle = iota
	styleBold
	styleItalic
	styleBoldItalic
)

type fontFaceKey struct {
	style fontStyle
	size  float64
}

// chatFonts holds the four parsed TTFs plus a cache of sized faces.
type chatFonts struct {
	regular, bold, italic, boldItalic *opentype.Font
	faces                             map[fontFaceKey]font.Face
}

func (f *chatFonts) face(style fontStyle, size float64) font.Face {
	if f.faces == nil {
		f.faces = map[fontFaceKey]font.Face{}
	}
	key := fontFaceKey{style, size}
	if face, ok := f.faces[key]; ok {
		return face
	}
	var src *opentype.Font
	switch style {
	case styleBold:
		src = f.bold
	case styleItalic:
		src = f.italic
	case styleBoldItalic:
		src = f.boldItalic
	default:
		src = f.regular
	}
	face, err := opentype.NewFace(src, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		// Fall back to the regular face at the same size.
		face, _ = opentype.NewFace(f.regular, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	}
	f.faces[key] = face
	return face
}

// measure returns the width of s in pixels for the given style and size.
func (f *chatFonts) measure(style fontStyle, size float64, s string) float64 {
	d := &font.Drawer{Face: f.face(style, size)}
	return float64(d.MeasureString(s)) / 64
}

var (
	fontsOnce   sync.Once
	fontsCached *chatFonts
	fontsErr    error
)

// loadChatFonts resolves the render fonts once per process: Noto Sans
// from the system directories first (same faces as the reference
// renderer), DejaVu second, embedded Go fonts last. WA_RENDER_FONT_DIR
// overrides the system directories.
func loadChatFonts() (*chatFonts, error) {
	fontsOnce.Do(func() {
		fontsCached, fontsErr = loadChatFontsUncached()
	})
	return fontsCached, fontsErr
}

func loadChatFontsUncached() (*chatFonts, error) {
	if dir := os.Getenv("WA_RENDER_FONT_DIR"); dir != "" {
		if cf, err := loadFontSet(dir,
			"NotoSans-Regular.ttf", "NotoSans-Bold.ttf",
			"NotoSans-Italic.ttf", "NotoSans-BoldItalic.ttf"); err == nil {
			return cf, nil
		}
	}
	if cf, err := loadFontSet("/usr/share/fonts/truetype/noto",
		"NotoSans-Regular.ttf", "NotoSans-Bold.ttf",
		"NotoSans-Italic.ttf", "NotoSans-BoldItalic.ttf"); err == nil {
		return cf, nil
	}
	if cf, err := loadFontSet("/usr/share/fonts/truetype/dejavu",
		"DejaVuSans.ttf", "DejaVuSans-Bold.ttf",
		"DejaVuSans-Oblique.ttf", "DejaVuSans-BoldOblique.ttf"); err == nil {
		return cf, nil
	}
	// Embedded Go fonts: always available, no system dependency.
	reg, err := opentype.Parse(goregular.TTF)
	if err != nil {
		return nil, fmt.Errorf("parse embedded regular font: %w", err)
	}
	bold, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return nil, fmt.Errorf("parse embedded bold font: %w", err)
	}
	ital, err := opentype.Parse(goitalic.TTF)
	if err != nil {
		return nil, fmt.Errorf("parse embedded italic font: %w", err)
	}
	boldItal, err := opentype.Parse(gobolditalic.TTF)
	if err != nil {
		return nil, fmt.Errorf("parse embedded bold-italic font: %w", err)
	}
	return &chatFonts{regular: reg, bold: bold, italic: ital, boldItalic: boldItal}, nil
}

// loadFontSet parses the four named TTF files from dir. All four must
// load; a missing style is an error so callers fall through to the next
// candidate set instead of mixing families.
func loadFontSet(dir, regular, bold, italic, boldItalic string) (*chatFonts, error) {
	load := func(name string) (*opentype.Font, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		return opentype.Parse(b)
	}
	reg, err := load(regular)
	if err != nil {
		return nil, err
	}
	b, err := load(bold)
	if err != nil {
		return nil, err
	}
	it, err := load(italic)
	if err != nil {
		return nil, err
	}
	bi, err := load(boldItalic)
	if err != nil {
		return nil, err
	}
	return &chatFonts{regular: reg, bold: b, italic: it, boldItalic: bi}, nil
}

// ---------------------------------------------------------------------------
// Wrapping
// ---------------------------------------------------------------------------

type styledWord struct {
	text         string
	bold, italic bool
}

type wrappedLine struct {
	words []styledWord
	blank bool
}

func styleFor(w styledWord) fontStyle {
	switch {
	case w.bold && w.italic:
		return styleBoldItalic
	case w.bold:
		return styleBold
	case w.italic:
		return styleItalic
	default:
		return styleRegular
	}
}

// wrapSegments wraps one paragraph into lines at most maxW pixels wide,
// mirroring the reference renderer: words are split on single spaces
// (runs of spaces collapse), the space width comes from the regular face.
func wrapSegments(segs []Segment, maxW float64, f *chatFonts) []wrappedLine {
	var words []styledWord
	for _, s := range segs {
		for _, w := range strings.Split(s.Text, " ") {
			if w == "" {
				continue
			}
			words = append(words, styledWord{text: w, bold: s.Bold, italic: s.Italic})
		}
	}
	if len(words) == 0 {
		return nil
	}
	spaceW := f.measure(styleRegular, renderBodySize, " ")
	var lines []wrappedLine
	cur := []styledWord{}
	curW := 0.0
	for _, w := range words {
		ww := f.measure(styleFor(w), renderBodySize, w.text)
		add := ww
		if len(cur) > 0 {
			add += spaceW
		}
		if len(cur) > 0 && curW+add > maxW {
			lines = append(lines, wrappedLine{words: cur})
			cur = []styledWord{w}
			curW = ww
		} else {
			cur = append(cur, w)
			curW += add
		}
	}
	if len(cur) > 0 {
		lines = append(lines, wrappedLine{words: cur})
	}
	return lines
}

// ---------------------------------------------------------------------------
// Layout
// ---------------------------------------------------------------------------

type laidOutMessage struct {
	msg     RenderMessage
	lines   []wrappedLine
	bubbleH int
}

func layoutMessage(msg RenderMessage, f *chatFonts) laidOutMessage {
	text := msg.Text
	if n := len([]rune(text)); n > renderMaxRunes {
		text = string([]rune(text)[:renderMaxRunes]) + "\n[...]"
	}
	paras := ParseWhatsAppMarkup(text)
	maxW := float64(renderBubbleW - renderPadX*2)
	var lines []wrappedLine
	for _, p := range paras {
		if p == nil {
			lines = append(lines, wrappedLine{blank: true})
			continue
		}
		lines = append(lines, wrapSegments(p, maxW, f)...)
	}
	contentH := renderSenderH + renderTimeAreaH
	for _, ln := range lines {
		if ln.blank {
			contentH += renderBlankH
		} else {
			contentH += renderLineH
		}
	}
	return laidOutMessage{msg: msg, lines: lines, bubbleH: contentH + renderPadY*2}
}

// ---------------------------------------------------------------------------
// Drawing helpers (anti-aliased via golang.org/x/image/vector)
// ---------------------------------------------------------------------------

const kappa = 0.5522847498307936 // circle approximation constant

// fillPath draws one shape with a rasterizer sized to the shape's
// bounding box. The vector rasterizer's buffers are width x height
// samples and Draw touches the whole buffer, so a full-image rasterizer
// per shape would make every doodle cost a full image pass; sizing the
// rasterizer to the shape keeps the work proportional to the shape's
// area. build receives the translation (ox, oy) that maps image
// coordinates into the rasterizer's space. Shapes partly outside the
// image are clipped to it.
func fillPath(dst *image.RGBA, src image.Image, bounds image.Rectangle, build func(z *vector.Rasterizer, ox, oy float32)) {
	bounds = bounds.Intersect(dst.Bounds())
	if bounds.Empty() {
		return
	}
	z := vector.NewRasterizer(bounds.Dx(), bounds.Dy())
	build(z, -float32(bounds.Min.X), -float32(bounds.Min.Y))
	z.Draw(dst, bounds, src, image.Point{})
}

// rectFromFloat is the integer rectangle containing the float rectangle,
// padded by 1px so boundary coordinates stay inside the rasterizer.
func rectFromFloat(x0, y0, x1, y1 float32) image.Rectangle {
	return image.Rect(
		int(math.Floor(float64(x0)))-1, int(math.Floor(float64(y0)))-1,
		int(math.Ceil(float64(x1)))+1, int(math.Ceil(float64(y1)))+1)
}

func circlePath(z *vector.Rasterizer, cx, cy, r float32) {
	k := kappa * r
	z.MoveTo(cx+r, cy)
	z.CubeTo(cx+r, cy+k, cx+k, cy+r, cx, cy+r)
	z.CubeTo(cx-k, cy+r, cx-r, cy+k, cx-r, cy)
	z.CubeTo(cx-r, cy-k, cx-k, cy-r, cx, cy-r)
	z.CubeTo(cx+k, cy-r, cx+r, cy-k, cx+r, cy)
	z.ClosePath()
}

func fillCircle(dst *image.RGBA, cx, cy, r float32, col color.Color) {
	fillPath(dst, image.NewUniform(col), rectFromFloat(cx-r, cy-r, cx+r, cy+r),
		func(z *vector.Rasterizer, ox, oy float32) {
			circlePath(z, cx+ox, cy+oy, r)
		})
}

func roundedRectPath(z *vector.Rasterizer, x, y, w, h, r float32) {
	k := kappa * r
	z.MoveTo(x+r, y)
	z.LineTo(x+w-r, y)
	z.CubeTo(x+w-r+k, y, x+w, y+r-k, x+w, y+r)
	z.LineTo(x+w, y+h-r)
	z.CubeTo(x+w, y+h-r+k, x+w-r+k, y+h, x+w-r, y+h)
	z.LineTo(x+r, y+h)
	z.CubeTo(x+r-k, y+h, x, y+h-r+k, x, y+h-r)
	z.LineTo(x, y+r)
	z.CubeTo(x, y+r-k, x+r-k, y, x+r, y)
	z.ClosePath()
}

func fillRoundedRect(dst *image.RGBA, x, y, w, h, r float32, col color.Color) {
	fillPath(dst, image.NewUniform(col), rectFromFloat(x, y, x+w, y+h),
		func(z *vector.Rasterizer, ox, oy float32) {
			roundedRectPath(z, x+ox, y+oy, w, h, r)
		})
}

// strokeLine draws a line as a filled quad (vector has no stroke API).
func strokeLine(dst *image.RGBA, x0, y0, x1, y1, width float32, col color.Color) {
	dx, dy := x1-x0, y1-y0
	length := float32(math.Hypot(float64(dx), float64(dy)))
	if length == 0 {
		return
	}
	px, py := -dy/length*width/2, dx/length*width/2
	// The perpendicular offset is at most width/2; pad by width.
	bounds := rectFromFloat(
		float32(math.Min(float64(x0), float64(x1)))-width,
		float32(math.Min(float64(y0), float64(y1)))-width,
		float32(math.Max(float64(x0), float64(x1)))+width,
		float32(math.Max(float64(y0), float64(y1)))+width)
	fillPath(dst, image.NewUniform(col), bounds, func(z *vector.Rasterizer, ox, oy float32) {
		z.MoveTo(x0+px+ox, y0+py+oy)
		z.LineTo(x1+px+ox, y1+py+oy)
		z.LineTo(x1-px+ox, y1-py+oy)
		z.LineTo(x0-px+ox, y0-py+oy)
		z.ClosePath()
	})
}

// textBaseline converts a top y (PIL-style) to a Go font baseline.
func textBaseline(top float64, face font.Face) int {
	return int(math.Round(top + float64(face.Metrics().Ascent)/64))
}

// ---------------------------------------------------------------------------
// Render
// ---------------------------------------------------------------------------

// RenderChat draws msgs as WhatsApp-Web-style bubbles into a PNG at dst.
// One bubble per message, stacked with a gap; a single date pill sits at
// the top and the avatar (initial of the last message's sender) at the
// bottom left. The output is a RENDER from text, not a screenshot.
func RenderChat(msgs []RenderMessage, datePill, dst string) error {
	if len(msgs) == 0 {
		return fmt.Errorf("no messages to render")
	}
	fonts, err := loadChatFonts()
	if err != nil {
		return fmt.Errorf("load render fonts: %w", err)
	}
	laid := make([]laidOutMessage, len(msgs))
	height := renderBubbleY + renderBottomPad
	for i, m := range msgs {
		laid[i] = layoutMessage(m, fonts)
		height += laid[i].bubbleH
		if i > 0 {
			height += renderBubbleGap
		}
	}
	if height < renderMinHeight {
		height = renderMinHeight
	}

	img := image.NewRGBA(image.Rect(0, 0, renderWidth, height))
	drawBackground(img)
	if datePill != "" {
		drawDatePill(img, fonts, datePill)
	}
	y := renderBubbleY
	for i, lm := range laid {
		if i > 0 {
			y += renderBubbleGap
		}
		drawBubble(img, fonts, lm, y)
		y += lm.bubbleH
	}
	drawAvatar(img, fonts, avatarInitial(laid[len(laid)-1].msg.Sender), height)

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("create render dir: %w", err)
	}
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return fmt.Errorf("encode %s: %w", dst, err)
	}
	return f.Close()
}

func drawBackground(img *image.RGBA) {
	draw.Draw(img, img.Bounds(), image.NewUniform(renderBg), image.Point{}, draw.Src)
	rnd := rand.New(rand.NewSource(9))
	W, H := img.Bounds().Dx(), img.Bounds().Dy()
	for gy := 0; gy < H; gy += 110 {
		for gx := 0; gx < W; gx += 130 {
			x := float32(gx + rnd.Intn(41) - 20)
			y := float32(gy + rnd.Intn(41) - 20)
			switch rnd.Intn(4) {
			case 0: // circle outline, 16x16
				fillCircle(img, x+8, y+8, 8, renderDoodle)
				fillCircle(img, x+8, y+8, 6, renderBg)
			case 1: // X, 14x14
				strokeLine(img, x, y, x+14, y+14, 2, renderDoodle)
				strokeLine(img, x+14, y, x, y+14, 2, renderDoodle)
			case 2: // plus, 16x16
				strokeLine(img, x+8, y, x+8, y+16, 2, renderDoodle)
				strokeLine(img, x, y+8, x+16, y+8, 2, renderDoodle)
			case 3: // arc, bottom half of an 18x16 ellipse
				arcDoodle(img, x+9, y+8, 9, 8)
			}
		}
	}
}

// arcDoodle draws the bottom half of an ellipse as a polyline.
func arcDoodle(img *image.RGBA, cx, cy, rx, ry float32) {
	const steps = 12
	var px, py float32
	for i := 0; i <= steps; i++ {
		t := float64(i) / steps * math.Pi
		x := cx + rx*float32(math.Cos(t))
		y := cy + ry*float32(math.Sin(t))
		if i > 0 {
			strokeLine(img, px, py, x, y, 2, renderDoodle)
		}
		px, py = x, y
	}
}

func drawDatePill(img *image.RGBA, f *chatFonts, pill string) {
	face := f.face(styleBold, renderPillSize)
	tw := f.measure(styleBold, renderPillSize, pill)
	pw := int(tw + 56)
	ph := 50
	px := (renderWidth - pw) / 2
	py := 12
	fillRoundedRect(img, float32(px), float32(py), float32(pw), float32(ph), 25, renderPillBg)
	d := &font.Drawer{Dst: img, Src: image.NewUniform(renderPillText), Face: face}
	d.Dot = fixed.P(px+28, textBaseline(float64(py+9), face))
	d.DrawString(pill)
}

func drawBubble(img *image.RGBA, f *chatFonts, lm laidOutMessage, topY int) {
	bx, by, bw := float32(renderBubbleX), float32(topY), float32(renderBubbleW)
	bh := float32(lm.bubbleH)
	// Shadow, then body, then tail (same color as the body).
	fillRoundedRect(img, bx+3, by+4, bw, bh, renderBubbleRadius, renderShadow)
	fillRoundedRect(img, bx, by, bw, bh, renderBubbleRadius, renderBubble)
	fillPath(img, image.NewUniform(renderBubble), rectFromFloat(bx-10, by-2, bx+10, by+22),
		func(z *vector.Rasterizer, ox, oy float32) {
			z.MoveTo(bx+2+ox, by+2+oy)
			z.LineTo(bx-10+ox, by-2+oy)
			z.LineTo(bx+10+ox, by+22+oy)
			z.ClosePath()
		})
	// Read-check badge: ring + check, top right of the bubble.
	cx, cy := bx+bw-38, by+38
	fillCircle(img, cx, cy, 22, renderCheckRing)
	fillCircle(img, cx, cy, 20, renderBubble)
	strokeLine(img, cx-9, cy-4, cx, cy+6, 3, renderCheck)
	strokeLine(img, cx, cy+6, cx+9, cy-4, 3, renderCheck)

	// Sender name in green.
	x := renderBubbleX + renderPadX
	y := topY + renderPadY
	if lm.msg.Sender != "" {
		face := f.face(styleBold, renderSenderSize)
		d := &font.Drawer{Dst: img, Src: image.NewUniform(renderSenderGreen), Face: face}
		d.Dot = fixed.P(x, textBaseline(float64(y), face))
		d.DrawString(lm.msg.Sender)
	}
	y += renderSenderH
	// Body lines.
	for _, ln := range lm.lines {
		if ln.blank {
			y += renderBlankH
			continue
		}
		xx := float64(x)
		for _, w := range ln.words {
			face := f.face(styleFor(w), renderBodySize)
			d := &font.Drawer{Dst: img, Src: image.NewUniform(renderText), Face: face}
			d.Dot = fixed.P(int(math.Round(xx)), textBaseline(float64(y), face))
			d.DrawString(w.text)
			xx += f.measure(styleFor(w), renderBodySize, w.text+" ")
		}
		y += renderLineH
	}
	// Timestamp, right-aligned at the bottom of the bubble.
	y += 8
	ts := lm.msg.Timestamp.In(wib).Format("3:04 PM")
	face := f.face(styleRegular, renderTimeSize)
	tw := f.measure(styleRegular, renderTimeSize, ts)
	d := &font.Drawer{Dst: img, Src: image.NewUniform(renderTimestampGrey), Face: face}
	d.Dot = fixed.P(renderBubbleX+renderBubbleW-renderPadX-int(math.Round(tw)), textBaseline(float64(y), face))
	d.DrawString(ts)
}

func drawAvatar(img *image.RGBA, f *chatFonts, initial string, height int) {
	ax, ay := 8, height-88
	fillCircle(img, float32(ax+37), float32(ay+37), 37, renderWhite)
	fillCircle(img, float32(ax+37), float32(ay+37), 34, renderAvatarBg)
	face := f.face(styleBold, renderAvatarSize)
	iw := f.measure(styleBold, renderAvatarSize, initial)
	d := &font.Drawer{Dst: img, Src: image.NewUniform(renderWhite), Face: face}
	x := ax + int(math.Round((74-iw)/2))
	d.Dot = fixed.P(x, textBaseline(float64(ay+18), face))
	d.DrawString(initial)
}
