package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xbelite2-configurator/internal/calibrate"
	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/protocol"
)

const (
	softBuzz     = 0x0a // after each step, as Xbox Accessories does
	strongBuzz   = 0x1e // at the end
	buzzFor      = 300 * time.Millisecond
	buzzGap      = 200 * time.Millisecond
	calTickEvery = 20 * time.Millisecond
)

type calStage int

const (
	calBusy calStage = iota
	calMenu
	calRestStart
	calRunning
	calRestEnd
	calConfirm
	calResult
)

type calScreen struct {
	stage       calStage
	sel         int
	cals        [4]protocol.Calibration
	known       bool
	det         *calibrate.Detector
	rest        *calibrate.Rest
	restFor     time.Duration
	before      protocol.Calibration
	after       protocol.Calibration
	afterOK     bool
	cancelled   bool
	ran         bool
	gen         int
	confirmFrom calStage
	busyMsg     string
	err         string
}

type calReadMsg struct {
	cals [4]protocol.Calibration
	err  error
}
type calStartedMsg struct{ err error }
type calDoneMsg struct {
	after     protocol.Calibration
	readOK    bool
	cancelled bool
	err       error
}
type calClosedMsg struct{ err error }
type calTickMsg struct{ gen int }

func calTick(gen int) tea.Cmd {
	return tea.Tick(calTickEvery, func(time.Time) tea.Msg { return calTickMsg{gen} })
}

// newTicks starts a fresh tick chain; ticks from older chains are ignored.
func (c *calScreen) newTicks() tea.Cmd {
	c.gen++
	return calTick(c.gen)
}

// locked reports whether quitting now could leave a calibration half done.
func (c *calScreen) locked() bool {
	switch c.stage {
	case calRunning, calRestEnd, calConfirm, calBusy:
		return true
	case calMenu, calRestStart, calResult:
		return c.ran // the final COMMIT is still owed
	}
	return false
}

func (m Model) openCalibration() (tea.Model, tea.Cmd) {
	if len(m.store.Dirty) > 0 {
		m.status = "Write or reload your unsaved changes before calibrating."
		return m, nil
	}
	m.cal = &calScreen{stage: calBusy, busyMsg: "Reading calibration...", restFor: calibrate.RestFor}
	m.status = ""
	dev := m.store.dev
	return m, func() tea.Msg {
		var msg calReadMsg
		for i, k := range calibrate.Kinds {
			if msg.cals[i], msg.err = dev.ReadCalibration(k.Target()); msg.err != nil {
				break
			}
		}
		return msg
	}
}

// finishCalibration ends a calibration the way the capture does: strong buzz,
// read, INIT, then two more strong buzzes. A cancelled one only reads and INITs.
func finishCalibration(dev device.Controller, target byte, cancelled bool) tea.Cmd {
	return func() tea.Msg {
		on := func() {
			dev.Rumble(protocol.Motors{LeftTrigger: strongBuzz, RightTrigger: strongBuzz, LeftGrip: strongBuzz, RightGrip: strongBuzz})
		}
		off := func() { dev.Rumble(protocol.Motors{}) }
		if !cancelled {
			on()
			time.Sleep(buzzFor)
			off()
		}
		after, rerr := dev.ReadCalibration(target)
		err := rerr
		if ierr := dev.Init(); err == nil { // INIT even if the read failed
			err = ierr
		}
		if !cancelled {
			for i := 0; i < 2; i++ {
				time.Sleep(buzzGap)
				on()
				time.Sleep(buzzFor)
				off()
			}
		}
		return calDoneMsg{after: after, readOK: rerr == nil, cancelled: cancelled, err: err}
	}
}

func (m Model) updateCalibrationMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	c := m.cal
	switch msg := msg.(type) {
	case calReadMsg:
		c.stage = calMenu
		if msg.err != nil {
			c.err = "Couldn't read calibration: " + msg.err.Error()
		} else {
			c.cals, c.known = msg.cals, true
		}
	case calTickMsg:
		if msg.gen != c.gen {
			return m, nil
		}
		return m.calTickUpdate()
	case calStartedMsg:
		if msg.err != nil {
			// the start frame may have taken effect, so COMMIT is owed on leaving
			c.stage, c.ran, c.err = calMenu, true, "Couldn't start calibration: "+msg.err.Error()
			return m, nil
		}
		c.stage = calRunning
		return m, c.newTicks()
	case calDoneMsg:
		c.stage, c.ran, c.after, c.afterOK, c.cancelled = calResult, true, msg.after, msg.readOK, msg.cancelled
		if msg.err != nil {
			c.err = msg.err.Error()
		}
		if msg.readOK {
			c.cals[c.sel] = msg.after
		}
	case calClosedMsg:
		m.cal = nil
		if msg.err != nil {
			m.status = "Calibration finished, but the final commit failed: " + msg.err.Error()
		} else {
			m.status = "Calibration finished."
		}
	}
	return m, nil
}

func (m Model) calTickUpdate() (tea.Model, tea.Cmd) {
	c := m.cal
	a := m.store.dev.Axes()
	switch c.stage {
	case calRestStart:
		if c.rest.Feed(time.Now(), a) {
			c.stage, c.busyMsg = calBusy, "Starting calibration..."
			dev, target := m.store.dev, c.det.Kind().Target()
			return m, func() tea.Msg {
				err := dev.StartCalibration(target)
				if err != nil { // it may have started anyway: leave the controller as the app would
					dev.ReadCalibration(target)
					dev.Init()
				}
				return calStartedMsg{err}
			}
		}
	case calRunning:
		var buzz tea.Cmd
		if c.det.Feed(a) {
			buzz = rumble(m.store.dev, softBuzz, buzzFor)
		}
		if c.det.Done() {
			c.stage, c.rest = calRestEnd, calibrate.NewRest(c.det.Kind(), c.restFor)
		}
		return m, tea.Batch(buzz, calTick(c.gen))
	case calRestEnd:
		if c.rest.Feed(time.Now(), a) {
			c.stage, c.busyMsg = calBusy, "Saving calibration..."
			return m, finishCalibration(m.store.dev, c.det.Kind().Target(), false)
		}
	default:
		return m, nil
	}
	return m, calTick(c.gen)
}

func (m Model) updateCalibrationKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	c := m.cal
	k := msg.String()
	switch c.stage {
	case calMenu:
		switch k {
		case "up", "k":
			c.sel = (c.sel + len(calibrate.Kinds) - 1) % len(calibrate.Kinds)
		case "down", "j":
			c.sel = (c.sel + 1) % len(calibrate.Kinds)
		case "enter":
			if !c.known {
				return m, nil
			}
			kind := calibrate.Kinds[c.sel]
			c.det, c.rest = calibrate.New(kind), calibrate.NewRest(kind, c.restFor)
			c.before, c.err, c.cancelled, c.stage = c.cals[c.sel], "", false, calRestStart
			return m, c.newTicks()
		case "esc", "q":
			if !c.ran {
				m.cal = nil
				return m, nil
			}
			c.stage, c.busyMsg = calBusy, "Saving..."
			dev := m.store.dev
			return m, func() tea.Msg { return calClosedMsg{dev.Commit()} }
		}
	case calRestStart:
		if k == "esc" { // nothing sent yet
			c.stage = calMenu
			c.gen++
		}
	case calRunning, calRestEnd:
		switch k {
		case "esc":
			c.confirmFrom, c.stage = c.stage, calConfirm
			c.gen++
		case "n":
			if m.demo && c.stage == calRunning {
				c.det.Skip()
				if c.det.Done() {
					c.stage, c.rest = calRestEnd, calibrate.NewRest(c.det.Kind(), c.restFor)
				}
				return m, rumble(m.store.dev, softBuzz, buzzFor)
			}
		}
	case calConfirm:
		switch k {
		case "y":
			c.stage, c.busyMsg = calBusy, "Stopping calibration..."
			return m, finishCalibration(m.store.dev, c.det.Kind().Target(), true)
		case "n", "esc":
			c.stage = c.confirmFrom
			c.rest = calibrate.NewRest(c.det.Kind(), c.restFor) // time in the dialog wasn't observed
			c.det.Resume()
			return m, c.newTicks()
		}
	case calResult:
		if k == "enter" || k == "esc" {
			c.stage = calMenu
		}
	}
	return m, nil
}

func calValues(k calibrate.Kind, c protocol.Calibration) string {
	state := "default"
	if c.Calibrated {
		state = "user calibrated"
	}
	v := c.Values
	if k.Stick() {
		return fmt.Sprintf("X %d–%d  Y %d–%d  (%s)", v[0], v[1], v[2], v[3], state)
	}
	return fmt.Sprintf("%d %d %d %d  (%s)", v[0], v[1], v[2], v[3], state)
}

func liveView(k calibrate.Kind, a protocol.Axes) string {
	if !k.Stick() {
		v := a.LT
		if k == calibrate.RightTrigger {
			v = a.RT
		}
		n := min(int(v), 1023) * 30 / 1023
		return fmt.Sprintf(" [%s%s] %4d\n", strings.Repeat("█", n), strings.Repeat("░", 30-n), v)
	}
	x, y := a.LX, a.LY
	if k == calibrate.RightStick {
		x, y = a.RX, a.RY
	}
	const size = 11
	col := (int(x) + 32768) * (size - 1) / 65535
	row := (32767 - int(y)) * (size - 1) / 65535
	var b strings.Builder
	for r := 0; r < size; r++ {
		b.WriteString(" ")
		for c := 0; c < size; c++ {
			if r == row && c == col {
				b.WriteString("● ")
			} else {
				b.WriteString("· ")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (c *calScreen) view(a protocol.Axes, demo bool, status string) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Stick and trigger calibration") + "\n\n")
	switch c.stage {
	case calBusy:
		b.WriteString(" " + c.busyMsg + "\n")
	case calMenu:
		for i, k := range calibrate.Kinds {
			vals := "unknown"
			if c.known {
				vals = calValues(k, c.cals[i])
			}
			line := fmt.Sprintf("%-14s %s", k, vals)
			if i == c.sel {
				line = selStyle.Render(line)
			}
			b.WriteString("   " + line + "\n")
		}
		if c.err != "" {
			b.WriteString("\n " + boldStyle.Render(c.err) + "\n")
		}
	case calRestStart, calRunning, calRestEnd, calConfirm:
		kind := c.det.Kind()
		what := "stick"
		if !kind.Stick() {
			what = "trigger"
		}
		b.WriteString(" " + boldStyle.Render(kind.String()) + "\n\n")
		for i, s := range c.det.Steps() {
			mark := "  "
			switch {
			case i < c.det.Step():
				mark = "✓ "
			case i == c.det.Step() && c.stage == calRunning:
				mark = "› "
			}
			b.WriteString(" " + mark + s + "\n")
		}
		b.WriteString("\n")
		switch c.stage {
		case calRestStart:
			b.WriteString(boldStyle.Render(fmt.Sprintf(" Let go of the %s and leave it at rest...", what)) + "\n")
		case calRunning:
			b.WriteString(" " + c.det.Progress() + "\n")
			if demo {
				b.WriteString(dimStyle.Render(" (demo: press n to complete this step)") + "\n")
			}
		case calRestEnd:
			b.WriteString(boldStyle.Render(fmt.Sprintf(" Done. Let go of the %s...", what)) + "\n")
		case calConfirm:
			b.WriteString(boldStyle.Render(" Stop calibrating? The result may be partial; run it again afterwards. y stop  n keep going") + "\n")
		}
		b.WriteString("\n" + liveView(kind, a))
	case calResult:
		kind := calibrate.Kinds[c.sel]
		b.WriteString(" " + boldStyle.Render(kind.String()) + "\n\n")
		b.WriteString(" Before: " + calValues(kind, c.before) + "\n")
		if c.afterOK {
			b.WriteString(" After:  " + calValues(kind, c.after) + "\n")
		} else {
			b.WriteString(" After: unknown\n")
		}
		if c.err != "" {
			b.WriteString("\n " + boldStyle.Render("Error: "+c.err) + "\n")
		}
		if c.cancelled {
			b.WriteString("\n " + boldStyle.Render("Calibration was stopped early and may be partial; run it again.") + "\n")
		}
	}
	b.WriteString("\n " + boldStyle.Render(status) + "\n\n")
	b.WriteString(dimStyle.Render(c.help()))
	return b.String()
}

func (c *calScreen) help() string {
	switch c.stage {
	case calMenu:
		return "↑/↓ select   enter calibrate   esc done"
	case calRestStart:
		return "esc back"
	case calRunning, calRestEnd:
		return "esc stop"
	case calResult:
		return "enter back"
	}
	return ""
}
