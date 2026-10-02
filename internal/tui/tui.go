// Package tui is the Bubble Tea interface for remapping paddles and buttons.
package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/protocol"
)

// Store holds the pages for all three profiles and tracks unsaved edits.
type Store struct {
	dev   device.Controller
	pages map[byte][]byte
	maps  map[byte]*protocol.MappingPage
	Dirty map[int]bool

	// Live LED preview: the newest wanted state wins, so previews sent from
	// concurrent commands can never leave a stale color showing.
	previewMu   sync.Mutex
	previewWant *[3]byte // nil = end preview
	previewSent *[3]byte
	previewed   bool
}

func NewStore(dev device.Controller) (*Store, error) {
	s := &Store{dev: dev, pages: map[byte][]byte{}, maps: map[byte]*protocol.MappingPage{}, Dirty: map[int]bool{}}
	for _, p := range protocol.Profiles {
		if err := s.Load(p); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) Load(profile int) error {
	for _, page := range protocol.PageIDs(profile).WriteOrder() {
		d, err := s.dev.ReadPage(page)
		if err != nil {
			return err
		}
		s.pages[page] = d
		if protocol.PageSize(page) == protocol.MapSize {
			m, err := protocol.ParseMappingPage(d)
			if err != nil {
				return err
			}
			s.maps[page] = &m
		}
	}
	delete(s.Dirty, profile)
	return nil
}

func (s *Store) Mapping(profile int, mode protocol.Mode) *protocol.MappingPage {
	return s.maps[protocol.PageIDs(profile).Map(mode)]
}

func (s *Store) Set(profile int, mode protocol.Mode, offset int, code byte) {
	m := s.Mapping(profile, mode)
	if m[offset] != code {
		m[offset] = code
		s.Dirty[profile] = true
	}
}

func (s *Store) Reset(profile int, mode protocol.Mode) {
	m := s.Mapping(profile, mode)
	before := *m
	m.ResetButtons()
	if *m != before {
		s.Dirty[profile] = true
	}
}

// Color returns a profile's stored color (from the standard page).
func (s *Store) Color(profile int) ([3]byte, bool) {
	return s.Mapping(profile, protocol.Standard).Color()
}

// SetColor sets the color on both pages of a profile, as Xbox Accessories does.
func (s *Store) SetColor(profile int, rgb [3]byte, custom bool) {
	for _, mode := range []protocol.Mode{protocol.Standard, protocol.Shift} {
		m := s.Mapping(profile, mode)
		before := *m
		m.SetColor(rgb, custom)
		if *m != before {
			s.Dirty[profile] = true
		}
	}
}

// WantPreview records the color the controller should show (nil to stop previewing).
func (s *Store) WantPreview(rgb *[3]byte) {
	s.previewMu.Lock()
	s.previewWant = rgb
	s.previewMu.Unlock()
}

// FlushPreview sends the newest wanted preview state if it differs from what was last sent.
func (s *Store) FlushPreview() error {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	want, sent := s.previewWant, s.previewSent
	if (want == nil && sent == nil) || (want != nil && sent != nil && *want == *sent) {
		return nil
	}
	var err error
	if want == nil {
		err = s.dev.EndPreview()
	} else {
		err = s.dev.PreviewLED(*want)
		s.previewed = true
	}
	if err == nil {
		s.previewSent = want
	}
	return err
}

// EndPreview returns the light to the stored color, if we ever changed it.
func (s *Store) EndPreview() error {
	s.WantPreview(nil)
	if !s.previewed {
		return nil
	}
	return s.FlushPreview()
}

func (s *Store) Write(profile int) error {
	pages := map[byte][]byte{}
	for _, page := range protocol.PageIDs(profile).WriteOrder() {
		if m, ok := s.maps[page]; ok {
			pages[page] = append([]byte(nil), m[:]...)
		} else {
			pages[page] = s.pages[page]
		}
	}
	if err := s.dev.WriteProfile(profile, pages); err != nil {
		return err
	}
	if err := s.Load(profile); err != nil {
		return err
	}
	// The stored color now matches what was previewed; hand the light back.
	return s.EndPreview()
}

// --- Bubble Tea model ---------------------------------------------------------

type tickMsg struct{}
type opDoneMsg struct {
	ok  string
	err error
}
type previewErrMsg struct{ err error }

type Model struct {
	store       *Store
	profile     int
	mode        protocol.Mode
	row         int
	status      string
	confirmQuit bool
	busy        bool
	picking     bool
	pickSel     int
	color       *colorPicker
	width       int
	height      int
}

func New(store *Store) Model {
	profile := protocol.ProfileForPage(store.dev.ActivePage())
	if profile == 0 {
		profile = 1
	}
	return Model{store: store, profile: profile, status: "Loaded profiles from controller."}
}

func tick() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) Init() tea.Cmd { return tick() }

func (m Model) current() protocol.Input { return protocol.Inputs[m.row] }

func (m Model) codeAt(row int) byte {
	return m.store.Mapping(m.profile, m.mode)[protocol.Inputs[row].Offset]
}

func indexOf(code byte) int {
	for i, c := range protocol.OutputOrder {
		if c == code {
			return i
		}
	}
	return 0
}

func (m *Model) setCurrent(code byte, verb string) {
	in := m.current()
	m.store.Set(m.profile, m.mode, in.Offset, code)
	m.status = fmt.Sprintf("%s %s %s (not written yet, press w)", in.Label, verb, protocol.OutputName(code))
}

// preview queues the picker's color (or the end of the preview) for the controller.
func (m Model) preview() tea.Cmd {
	if m.color.custom {
		rgb := m.color.rgb()
		m.store.WantPreview(&rgb)
	} else {
		m.store.WantPreview(nil)
	}
	return m.flushPreview
}

func (m Model) flushPreview() tea.Msg {
	if err := m.store.FlushPreview(); err != nil {
		return previewErrMsg{err}
	}
	return nil
}

func (m Model) updateColor(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.color.update(msg.String()) {
	case pickerChanged:
		return m, m.preview()
	case pickerApply:
		rgb, custom := m.color.rgb(), m.color.custom
		m.store.SetColor(m.profile, rgb, custom)
		m.color = nil
		if custom {
			m.status = fmt.Sprintf("Profile %d color -> %s (previewing; press w to write)", m.profile, hexOf(rgb))
			return m, nil
		}
		m.status = fmt.Sprintf("Profile %d color -> default (press w to write)", m.profile)
		m.store.WantPreview(nil)
		return m, m.flushPreview
	case pickerCancel:
		m.color = nil
		m.status = "Color unchanged."
		// Back to what this profile has in memory (stored, or applied but unwritten).
		if rgb, custom := m.store.Color(m.profile); custom && m.store.Dirty[m.profile] {
			m.store.WantPreview(&rgb)
		} else {
			m.store.WantPreview(nil)
		}
		return m, m.flushPreview
	}
	return m, nil
}

func (m Model) run(ok string, fn func() error) (Model, tea.Cmd) {
	m.busy = true
	m.status = "Talking to controller..."
	return m, func() tea.Msg { return opDoneMsg{ok: ok, err: fn()} }
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		return m, tick()
	case previewErrMsg:
		m.status = "LED preview failed: " + msg.err.Error()
	case opDoneMsg:
		m.busy = false
		if msg.err != nil {
			m.status = "Error: " + msg.err.Error()
		} else {
			m.status = msg.ok
		}
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.busy {
			return m, nil
		}
		if m.color != nil {
			return m.updateColor(msg)
		}
		if m.picking {
			return m.updatePicker(msg)
		}
		return m.updateMain(msg)
	}
	return m, nil
}

func (m Model) updatePicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(protocol.OutputOrder)
	switch msg.String() {
	case "up", "k":
		m.pickSel = (m.pickSel - 1 + n) % n
	case "down", "j":
		m.pickSel = (m.pickSel + 1) % n
	case "enter":
		m.setCurrent(protocol.OutputOrder[m.pickSel], "->")
		m.picking = false
	case "esc", "q":
		m.picking = false
	}
	return m, nil
}

func (m Model) updateMain(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if key != "q" {
		m.confirmQuit = false
	}
	n := len(protocol.Inputs)
	switch key {
	case "up", "k":
		m.row = (m.row - 1 + n) % n
	case "down", "j":
		m.row = (m.row + 1) % n
	case "left", "h", "right", "l":
		step := 1
		if key == "left" || key == "h" {
			step = -1
		}
		o := protocol.OutputOrder
		i := (indexOf(m.codeAt(m.row)) + step + len(o)) % len(o)
		m.setCurrent(o[i], "->")
	case "enter":
		m.picking = true
		m.pickSel = indexOf(m.codeAt(m.row))
	case "0":
		m.setCurrent(0x00, "->")
	case "1", "2", "3":
		m.profile = int(key[0] - '0')
	case "c":
		rgb, custom := m.store.Color(m.profile)
		m.color = newColorPicker(rgb, custom)
		m.status = fmt.Sprintf("Picking color for profile %d (the controller previews it live)", m.profile)
		return m, m.preview()
	case "tab":
		m.mode = 1 - m.mode
	case "d":
		m.store.Reset(m.profile, m.mode)
		m.status = fmt.Sprintf("Profile %d %s reset to defaults (press w to write)", m.profile, m.mode)
	case "r":
		p := m.profile
		return m.run(fmt.Sprintf("Reloaded profile %d.", p), func() error { return m.store.Load(p) })
	case "w":
		if !m.store.Dirty[m.profile] {
			m.status = "Nothing to write for this profile."
			return m, nil
		}
		p := m.profile
		return m.run(fmt.Sprintf("Profile %d written and verified.", p), func() error { return m.store.Write(p) })
	case "q":
		if len(m.store.Dirty) > 0 && !m.confirmQuit {
			m.confirmQuit = true
			var ps []string
			for p := range m.store.Dirty {
				ps = append(ps, fmt.Sprint(p))
			}
			sort.Strings(ps)
			m.status = "Unsaved changes in profile(s) " + strings.Join(ps, ", ") + ". Press q again to quit without writing."
			return m, nil
		}
		return m, tea.Quit
	}
	return m, nil
}

var (
	titleStyle = lipgloss.NewStyle().Bold(true)
	selStyle   = lipgloss.NewStyle().Reverse(true)
	dimStyle   = lipgloss.NewStyle().Faint(true)
	boldStyle  = lipgloss.NewStyle().Bold(true)
	boxStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)

const help = "↑/↓ select   ←/→ change output   enter pick from list   0 disable\n" +
	"1/2/3 profile   tab standard/shift   c color   d reset profile   r reload   w write   q quit"

func (m Model) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Xbox Elite Series 2 remapper") + "\n\n")

	var tabs []string
	activeProfile := protocol.ProfileForPage(m.store.dev.ActivePage())
	for _, p := range protocol.Profiles {
		label := fmt.Sprintf(" Profile %d", p)
		if m.store.Dirty[p] {
			label += "*"
		}
		if p == activeProfile {
			label += " (active)"
		}
		label += " "
		if p == m.profile {
			label = selStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	tabs = append(tabs, "  ")
	for _, mode := range []protocol.Mode{protocol.Standard, protocol.Shift} {
		label := " " + mode.String() + " "
		if mode == m.mode {
			label = selStyle.Render(label)
		}
		tabs = append(tabs, label)
	}
	b.WriteString(" " + strings.Join(tabs, " ") + "\n")

	mp := m.store.Mapping(m.profile, m.mode)
	if rgb, custom := m.store.Color(m.profile); custom {
		b.WriteString(" Color " + swatch(rgb, 4) + " " + hexOf(rgb))
	} else {
		b.WriteString(" Color default")
	}
	b.WriteString(dimStyle.Render(fmt.Sprintf("  (c to change)   page flags 0x%02x", mp.Flags())) + "\n\n")

	var rows strings.Builder
	rows.WriteString(boldStyle.Render("  Paddles") + "\n")
	for i, in := range protocol.Inputs {
		if i == protocol.PaddleCount {
			rows.WriteString("\n" + boldStyle.Render("  Buttons") + "\n")
		}
		code := mp[in.Offset]
		line := fmt.Sprintf("%-18s ->  %-12s", in.Label, protocol.OutputName(code))
		if i == m.row {
			line = selStyle.Render(line)
		}
		if code != in.Default {
			line += dimStyle.Render("  (changed from default)")
		}
		rows.WriteString("    " + line + "\n")
	}
	body := rows.String()
	if m.color != nil {
		body = lipgloss.JoinHorizontal(lipgloss.Top, body, "  ", m.color.view(fmt.Sprintf("Profile %d color", m.profile)))
	} else if m.picking {
		var p strings.Builder
		p.WriteString(boldStyle.Render(m.current().Label) + "\n")
		for i, c := range protocol.OutputOrder {
			l := fmt.Sprintf("%-14s", protocol.OutputName(c))
			if i == m.pickSel {
				l = selStyle.Render(l)
			}
			p.WriteString(l + "\n")
		}
		body = lipgloss.JoinHorizontal(lipgloss.Top, body, "  ", boxStyle.Render(strings.TrimRight(p.String(), "\n")))
	}
	b.WriteString(body + "\n")

	held := "-"
	if p := m.store.dev.Pressed(); len(p) > 0 {
		held = strings.Join(p, " ")
	}
	b.WriteString(dimStyle.Render(" Held: "+held+"\n       (press a paddle to see which P-number it is)") + "\n\n")
	b.WriteString(" " + boldStyle.Render(m.status) + "\n\n")
	b.WriteString(dimStyle.Render(help))
	return b.String()
}

// Run loads all profiles and starts the UI.
func Run(dev device.Controller) error {
	store, err := NewStore(dev)
	if err != nil {
		return err
	}
	useTrueColorUnderSudo()
	_, err = tea.NewProgram(New(store), tea.WithAltScreen()).Run()
	if perr := store.EndPreview(); err == nil && perr != nil {
		err = fmt.Errorf("could not end LED preview: %w", perr)
	}
	return err
}

// sudo strips COLORTERM, so color detection drops to 256 colors and the
// picker's gradients get banded. A terminal that reports 256 colors inside a
// sudo session on macOS/Linux is almost always a truecolor one.
func useTrueColorUnderSudo() {
	if os.Getenv("SUDO_USER") != "" && lipgloss.ColorProfile() == termenv.ANSI256 {
		lipgloss.SetColorProfile(termenv.TrueColor)
	}
}
