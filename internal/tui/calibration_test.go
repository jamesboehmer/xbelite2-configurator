package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/fixture"
	"xbelite2-configurator/internal/protocol"
)

func calModel(t *testing.T) (Model, *device.Fake) {
	t.Helper()
	dev := device.NewFake(fixture.Pages())
	store, err := NewStore(dev)
	if err != nil {
		t.Fatal(err)
	}
	m := New(store)
	m.demo = true
	return m, dev
}

// tickCal delivers one calibration tick of the current generation.
func tickCal(t *testing.T, m Model) Model {
	t.Helper()
	next, cmd := m.Update(calTickMsg{gen: m.cal.gen})
	return drive(t, next.(Model), cmd)
}

func openCal(t *testing.T, m Model) Model {
	t.Helper()
	m = fwPress(t, m, "s")
	if m.cal == nil || m.cal.stage != calMenu {
		t.Fatalf("calibration menu not open: %+v", m.cal)
	}
	m.cal.restFor = 0
	return m
}

func countLevel(rs []protocol.Motors, level byte) int {
	n := 0
	for _, r := range rs {
		if r.LeftGrip == level {
			n++
		}
	}
	return n
}

func TestCalibrationEndToEnd(t *testing.T) {
	m, dev := calModel(t)
	m = openCal(t, m)
	if !strings.Contains(m.View(), "X 851–3211") {
		t.Fatalf("menu:\n%s", m.View())
	}
	m = fwPress(t, m, "enter") // left stick, waiting for rest
	if m.cal.stage != calRestStart {
		t.Fatalf("stage %d", m.cal.stage)
	}
	m = tickCal(t, m) // resting -> start -> running
	if m.cal.stage != calRunning || len(dev.CalStarts) != 1 || dev.CalStarts[0] != protocol.CalLeftStick {
		t.Fatalf("stage %d, starts %v", m.cal.stage, dev.CalStarts)
	}
	for i := 0; i < 6; i++ {
		m = fwPress(t, m, "n") // demo: complete a step
	}
	if m.cal.stage != calRestEnd {
		t.Fatalf("stage %d after all steps", m.cal.stage)
	}
	m = tickCal(t, m) // resting -> finish
	if m.cal.stage != calResult || dev.Inits != 1 || !m.cal.after.Calibrated {
		t.Fatalf("stage %d, inits %d, after %+v", m.cal.stage, dev.Inits, m.cal.after)
	}
	if countLevel(dev.Rumbles, 0x0a) != 6 || countLevel(dev.Rumbles, 0x1e) != 3 {
		t.Errorf("buzzes: %v", dev.Rumbles)
	}
	m = fwPress(t, m, "enter", "esc") // back to menu, leave
	if m.cal != nil || dev.Commits != 1 || !strings.Contains(m.status, "Calibration finished") {
		t.Fatalf("cal %+v commits %d status %q", m.cal, dev.Commits, m.status)
	}
}

func TestCalibrationCancelFinishesLikeTheApp(t *testing.T) {
	m, dev := calModel(t)
	m = openCal(t, m)
	m = tickCal(t, fwPress(t, m, "enter"))
	m = fwPress(t, m, "esc")
	if m.cal.stage != calConfirm {
		t.Fatalf("stage %d", m.cal.stage)
	}
	m = fwPress(t, m, "y")
	if m.cal.stage != calResult || !m.cal.cancelled || dev.Inits != 1 || countLevel(dev.Rumbles, 0x1e) != 0 {
		t.Fatalf("stage %d cancelled %v inits %d rumbles %v", m.cal.stage, m.cal.cancelled, dev.Inits, dev.Rumbles)
	}
	if !strings.Contains(m.View(), "run it again") {
		t.Fatalf("no re-run advice:\n%s", m.View())
	}
}

func TestCalibrationEscBeforeStartSendsNothing(t *testing.T) {
	m, dev := calModel(t)
	m = openCal(t, m)
	m.cal.restFor = 1 << 62 // never rests
	m = fwPress(t, m, "enter", "esc")
	if m.cal.stage != calMenu || len(dev.CalStarts) != 0 || dev.Inits != 0 {
		t.Fatalf("stage %d starts %v inits %d", m.cal.stage, dev.CalStarts, dev.Inits)
	}
}

func TestCalibrationQuitIgnoredWhileRunning(t *testing.T) {
	m, _ := calModel(t)
	m = tickCal(t, fwPress(t, openCal(t, m), "enter"))
	for _, k := range []string{"ctrl+c", "q"} {
		if _, cmd := m.Update(fwKey(k)); cmd != nil {
			if _, quit := cmd().(tea.QuitMsg); quit {
				t.Errorf("%s quit mid-calibration", k)
			}
		}
	}
}

func TestCalibrationStartFailureReturnsToMenu(t *testing.T) {
	m, dev := calModel(t)
	m = openCal(t, m)
	dev.CalErr = errors.New("unplugged")
	m = tickCal(t, fwPress(t, m, "enter"))
	if m.cal.stage != calMenu || !strings.Contains(m.View(), "unplugged") {
		t.Fatalf("stage %d:\n%s", m.cal.stage, m.View())
	}
}

func TestCalibrationRefusedWithUnsavedChanges(t *testing.T) {
	m, _ := calModel(t)
	m.store.Set(1, protocol.Standard, 1, 0x0C)
	m = fwPress(t, m, "s")
	if m.cal != nil || !strings.Contains(m.status, "unsaved") {
		t.Fatalf("cal %+v status %q", m.cal, m.status)
	}
}

func TestCalibrationStaleTickIgnored(t *testing.T) {
	m, dev := calModel(t)
	m = openCal(t, m)
	m = fwPress(t, m, "enter")
	stale := m.cal.gen - 1
	next, cmd := m.Update(calTickMsg{gen: stale})
	if cmd != nil || len(dev.CalStarts) != 0 {
		t.Fatal("stale tick acted")
	}
	m = fwPress(t, next.(Model), "esc", "esc") // leave
	if _, cmd := m.Update(calTickMsg{gen: 99}); cmd != nil {
		t.Fatal("tick after closing acted")
	}
}
