package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/protocol"
)

const (
	vibStep    = 5
	vibMax     = 100
	testLevel  = 0x32 // Xbox Accessories' test buzz
	testLength = 500 * time.Millisecond
	vibBarLen  = 20
)

var motorRows = []struct {
	label string
	get   func(protocol.Motors) byte
	set   func(*protocol.Motors, byte)
}{
	{"Left grip", func(m protocol.Motors) byte { return m.LeftGrip }, func(m *protocol.Motors, v byte) { m.LeftGrip = v }},
	{"Right grip", func(m protocol.Motors) byte { return m.RightGrip }, func(m *protocol.Motors, v byte) { m.RightGrip = v }},
	{"Left trigger", func(m protocol.Motors) byte { return m.LeftTrigger }, func(m *protocol.Motors, v byte) { m.LeftTrigger = v }},
	{"Right trigger", func(m protocol.Motors) byte { return m.RightTrigger }, func(m *protocol.Motors, v byte) { m.RightTrigger = v }},
}

type vibPanel struct{ row int }

type rumbleErrMsg struct{ err error }

// rumble runs all four motors at level for d, then stops them, off the UI goroutine.
func rumble(dev device.Controller, level byte, d time.Duration) tea.Cmd {
	return func() tea.Msg {
		if err := dev.Rumble(protocol.Motors{LeftTrigger: level, RightTrigger: level, LeftGrip: level, RightGrip: level}); err != nil {
			return rumbleErrMsg{err}
		}
		time.Sleep(d)
		if err := dev.Rumble(protocol.Motors{}); err != nil {
			return rumbleErrMsg{err}
		}
		return nil
	}
}

func clampVib(v int) byte { return byte(max(0, min(vibMax, v))) }

func (m Model) updateVibration(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	v := m.store.Vibration(m.profile)
	r := motorRows[m.vib.row]
	set := func(val int) {
		r.set(&v, clampVib(val))
		m.store.SetVibration(m.profile, v)
		m.status = fmt.Sprintf("%s vibration %d%% (not written yet, press w)", r.label, r.get(v))
	}
	n := len(motorRows)
	switch msg.String() {
	case "up", "k":
		m.vib.row = (m.vib.row - 1 + n) % n
	case "down", "j":
		m.vib.row = (m.vib.row + 1) % n
	case "left", "h":
		set(int(clampVib(int(r.get(v)))) - vibStep)
	case "right", "l":
		set(int(clampVib(int(r.get(v)))) + vibStep)
	case "0":
		set(0)
	case "t":
		return m, rumble(m.store.dev, testLevel, testLength)
	case "esc", "enter", "v":
		m.vib = nil
	case "w":
		m.vib = nil
		return m.updateMain(msg)
	}
	return m, nil
}

func (p *vibPanel) view(title string, v protocol.Motors, note string) string {
	var b strings.Builder
	b.WriteString(boldStyle.Render(title) + "\n")
	for i, r := range motorRows {
		val := int(clampVib(int(r.get(v))))
		n := val * vibBarLen / vibMax
		line := fmt.Sprintf("%-14s %s%s %3d%%", r.label, strings.Repeat("█", n), strings.Repeat("░", vibBarLen-n), val)
		if i == p.row {
			line = selStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	if note != "" {
		b.WriteString(dimStyle.Render(note) + "\n")
	}
	b.WriteString(dimStyle.Render("↑/↓ motor  ←/→ ±5%  0 off  t test  esc done"))
	return boxStyle.Render(b.String())
}
