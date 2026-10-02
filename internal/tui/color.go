package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// colorPicker edits a profile color in HSV so hue survives passing through gray.
type colorPicker struct {
	h, s, v   int      // 0-359, 0-100, 0-100
	exact     *[3]byte // set from hex/preset/stored color; cleared when a slider moves
	custom    bool
	row       int // pickerRows index
	preset    int
	typingHex bool
	hexInput  string
	err       string
}

var pickerRows = []string{"Hue", "Saturation", "Brightness", "Presets"}

const (
	rowHue = iota
	rowSat
	rowVal
	rowPresets
)

var presets = []struct {
	name string
	rgb  [3]byte
}{
	{"Xbox green", [3]byte{0x10, 0x7c, 0x10}},
	{"White", [3]byte{0xff, 0xff, 0xff}},
	{"Red", [3]byte{0xff, 0x00, 0x00}},
	{"Orange", [3]byte{0xff, 0x80, 0x00}},
	{"Yellow", [3]byte{0xff, 0xdc, 0x00}},
	{"Green", [3]byte{0x00, 0xff, 0x00}},
	{"Cyan", [3]byte{0x00, 0xff, 0xff}},
	{"Blue", [3]byte{0x00, 0x40, 0xff}},
	{"Purple", [3]byte{0xa0, 0x00, 0xff}},
	{"Pink", [3]byte{0xff, 0x00, 0xa0}},
}

func newColorPicker(rgb [3]byte, custom bool) *colorPicker {
	c := &colorPicker{custom: custom}
	if custom {
		c.setRGB(rgb)
	} else {
		c.h, c.s, c.v = 120, 100, 100
	}
	return c
}

func (c *colorPicker) rgb() [3]byte {
	if c.exact != nil {
		return *c.exact
	}
	return hsvToRGB(c.h, c.s, c.v)
}

func (c *colorPicker) setRGB(rgb [3]byte) {
	c.exact = &rgb
	h, s, v := rgbToHSV(rgb)
	if s == 0 || v == 0 {
		h = c.h // hue is meaningless for grays; keep the slider where it was
	}
	c.h, c.s, c.v = h, s, v
}

type pickerAction int

const (
	pickerNone pickerAction = iota
	pickerChanged
	pickerApply
	pickerCancel
)

func (c *colorPicker) update(key string) pickerAction {
	c.err = ""
	if c.typingHex {
		switch {
		case key == "enter":
			v, err := strconv.ParseUint(c.hexInput, 16, 32)
			if len(c.hexInput) != 6 || err != nil {
				c.err = "enter 6 hex digits, e.g. 70ff5d"
				return pickerNone
			}
			c.setRGB([3]byte{byte(v >> 16), byte(v >> 8), byte(v)})
			c.custom, c.typingHex = true, false
			return pickerChanged
		case key == "esc":
			c.typingHex = false
		case key == "backspace":
			if c.hexInput != "" {
				c.hexInput = c.hexInput[:len(c.hexInput)-1]
			}
		case len(key) == 1 && strings.Contains("0123456789abcdefABCDEF", key) && len(c.hexInput) < 6:
			c.hexInput += strings.ToLower(key)
		}
		return pickerNone
	}

	step := 1
	switch key {
	case "up", "k":
		c.row = (c.row - 1 + len(pickerRows)) % len(pickerRows)
	case "down", "j":
		c.row = (c.row + 1) % len(pickerRows)
	case "shift+left", "H":
		step = -10
		return c.adjust(step)
	case "shift+right", "L":
		step = 10
		return c.adjust(step)
	case "left", "h":
		return c.adjust(-step)
	case "right", "l":
		return c.adjust(step)
	case "#":
		c.typingHex, c.hexInput = true, ""
	case "x":
		c.custom = !c.custom
		return pickerChanged
	case "enter":
		return pickerApply
	case "esc", "q", "c":
		return pickerCancel
	}
	return pickerNone
}

func (c *colorPicker) adjust(step int) pickerAction {
	c.exact = nil
	switch c.row {
	case rowHue:
		c.h = ((c.h+step*3)%360 + 360) % 360
	case rowSat:
		c.s = clamp(c.s+step, 0, 100)
	case rowVal:
		c.v = clamp(c.v+step, 0, 100)
	case rowPresets:
		dir := 1
		if step < 0 {
			dir = -1
		}
		c.preset = (c.preset + dir + len(presets)) % len(presets)
		c.setRGB(presets[c.preset].rgb)
	}
	c.custom = true
	return pickerChanged
}

func clamp(v, lo, hi int) int { return max(lo, min(hi, v)) }

// --- rendering ------------------------------------------------------------------

const barWidth = 36

func hexOf(rgb [3]byte) string { return fmt.Sprintf("#%02x%02x%02x", rgb[0], rgb[1], rgb[2]) }

// markerColor picks black or white for text drawn on top of rgb.
func markerColor(rgb [3]byte) lipgloss.Color {
	lum := 0.299*float64(rgb[0]) + 0.587*float64(rgb[1]) + 0.114*float64(rgb[2])
	if lum > 140 {
		return lipgloss.Color("#000000")
	}
	return lipgloss.Color("#ffffff")
}

func swatch(rgb [3]byte, width int) string {
	return lipgloss.NewStyle().Background(lipgloss.Color(hexOf(rgb))).Render(strings.Repeat(" ", width))
}

// bar draws a gradient where cell i has color at(i); the cell nearest the
// current value gets a marker.
func bar(at func(i int) [3]byte, sel int) string {
	var b strings.Builder
	for i := 0; i < barWidth; i++ {
		rgb := at(i)
		st := lipgloss.NewStyle().Background(lipgloss.Color(hexOf(rgb)))
		cell := " "
		if i == sel {
			st = st.Foreground(markerColor(rgb)).Bold(true)
			cell = "┃"
		}
		b.WriteString(st.Render(cell))
	}
	return b.String()
}

func pos(v, maxV int) int { return int(math.Round(float64(v) / float64(maxV) * float64(barWidth-1))) }

func (c *colorPicker) view(title string) string {
	var b strings.Builder
	b.WriteString(boldStyle.Render(title) + "\n\n")

	rgb := c.rgb()
	if c.custom {
		sw := swatch(rgb, 10)
		b.WriteString(sw + "  " + boldStyle.Render(hexOf(rgb)) + "\n" + sw + "\n\n")
	} else {
		b.WriteString(dimStyle.Render("Default color (no custom color stored)") + "\n\n\n")
	}

	bars := []string{
		bar(func(i int) [3]byte { return hsvToRGB(i*360/barWidth, max(c.s, 60), max(c.v, 60)) }, pos(c.h, 359)),
		bar(func(i int) [3]byte { return hsvToRGB(c.h, i*100/(barWidth-1), max(c.v, 40)) }, pos(c.s, 100)),
		bar(func(i int) [3]byte { return hsvToRGB(c.h, c.s, i*100/(barWidth-1)) }, pos(c.v, 100)),
	}
	values := []string{fmt.Sprintf("%3d°", c.h), fmt.Sprintf("%3d%%", c.s), fmt.Sprintf("%3d%%", c.v)}
	for i, name := range pickerRows {
		cursor := "  "
		if i == c.row {
			cursor = "› "
		}
		label := fmt.Sprintf("%s%-11s", cursor, name)
		if i == c.row {
			label = boldStyle.Render(label)
		}
		if i < len(bars) {
			b.WriteString(label + bars[i] + " " + values[i] + "\n")
			continue
		}
		var sw []string
		for j, p := range presets {
			mark := " "
			if i == c.row && j == c.preset {
				mark = "▲"
			}
			sw = append(sw, swatch(p.rgb, 2)+mark)
		}
		b.WriteString(label + strings.Join(sw, "") + "\n")
		if i == c.row {
			b.WriteString(strings.Repeat(" ", 13) + dimStyle.Render(presets[c.preset].name) + "\n")
		} else {
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	if c.typingHex {
		b.WriteString("Hex: #" + c.hexInput + "▏  " + dimStyle.Render("enter set · esc back") + "\n")
	} else {
		b.WriteString(dimStyle.Render("# type hex · x default color") + "\n")
	}
	if c.err != "" {
		b.WriteString(c.err + "\n")
	} else {
		b.WriteString("\n")
	}
	b.WriteString(dimStyle.Render("↑/↓ row · ←/→ adjust (shift ×10)\nenter apply · esc cancel"))
	return boxStyle.Render(b.String())
}

// --- color maths ---------------------------------------------------------------

func hsvToRGB(h, s, v int) [3]byte {
	hf, sf, vf := float64(h%360), float64(s)/100, float64(v)/100
	c := vf * sf
	x := c * (1 - math.Abs(math.Mod(hf/60, 2)-1))
	m := vf - c
	var r, g, b float64
	switch {
	case hf < 60:
		r, g, b = c, x, 0
	case hf < 120:
		r, g, b = x, c, 0
	case hf < 180:
		r, g, b = 0, c, x
	case hf < 240:
		r, g, b = 0, x, c
	case hf < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	to := func(f float64) byte { return byte(math.Round((f + m) * 255)) }
	return [3]byte{to(r), to(g), to(b)}
}

func rgbToHSV(rgb [3]byte) (h, s, v int) {
	r, g, b := float64(rgb[0])/255, float64(rgb[1])/255, float64(rgb[2])/255
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	d := mx - mn
	var hf float64
	switch {
	case d == 0:
		hf = 0
	case mx == r:
		hf = 60 * math.Mod((g-b)/d, 6)
	case mx == g:
		hf = 60 * ((b-r)/d + 2)
	default:
		hf = 60 * ((r-g)/d + 4)
	}
	if hf < 0 {
		hf += 360
	}
	sf := 0.0
	if mx > 0 {
		sf = d / mx
	}
	return int(math.Round(hf)) % 360, int(math.Round(sf * 100)), int(math.Round(mx * 100))
}
