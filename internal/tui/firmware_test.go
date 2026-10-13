package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/firmware"
	"xbelite2-configurator/internal/fixture"
	"xbelite2-configurator/internal/protocol"
)

var installed = firmware.Version{Major: 5, Minor: 24, Build: 4}

func fwEnv(t *testing.T) (FirmwareEnv, *firmware.FakeBootloader) {
	t.Helper()
	img, err := firmware.Parse(firmware.DemoImage(installed))
	if err != nil {
		t.Fatal(err)
	}
	bl := firmware.NewFakeBootloader(img)
	dev := device.NewFake(fixture.Pages())
	return FirmwareEnv{
		Source:   firmware.DemoSource{},
		OpenLink: func() (firmware.Link, error) { return bl, nil },
		Reopen:   func(firmware.Link) (device.Controller, error) { return dev, nil },
	}, bl
}

func newFwModel(t *testing.T, env FirmwareEnv) Model {
	t.Helper()
	store, err := NewStore(device.NewFake(fixture.Pages()))
	if err != nil {
		t.Fatal(err)
	}
	m := New(store)
	m.fwEnv = env
	return m
}

// drive feeds each command's message back into the model until none is left.
func drive(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for i := 0; cmd != nil; i++ {
		if i > 100000 {
			t.Fatal("model never settled")
		}
		msg := cmd()
		if _, ok := msg.(tickMsg); ok {
			return m
		}
		var next tea.Model
		next, cmd = m.Update(msg)
		m = next.(Model)
	}
	return m
}

func fwKey(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func fwPress(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		next, cmd := m.Update(fwKey(k))
		m = drive(t, next.(Model), cmd)
	}
	return m
}

func TestFirmwareUpdateEndToEnd(t *testing.T) {
	env, bl := fwEnv(t)
	m := fwPress(t, newFwModel(t, env), "f")
	if m.fw == nil || m.fw.stage != fwList {
		t.Fatalf("firmware screen not open: %+v", m.fw)
	}
	if v := m.View(); !strings.Contains(v, "5.24.4.0") || !strings.Contains(v, "5.23.6.0") || !strings.Contains(v, "5.21.3242.0") {
		t.Fatalf("list view:\n%s", v)
	}
	m = fwPress(t, m, "enter")
	if m.fw.stage != fwConfirm || !strings.Contains(m.View(), "DOWNGRADE") {
		t.Fatalf("confirm view:\n%s", m.View())
	}
	m = fwPress(t, m, "y")
	if m.fw != nil {
		t.Fatalf("still on firmware screen: stage %d, %s", m.fw.stage, m.fw.errMsg)
	}
	if !strings.Contains(m.status, "5.23.6.0 installed") {
		t.Errorf("status %q", m.status)
	}
	if bl.Versions[firmware.RegionApp] != (firmware.Version{Major: 5, Minor: 23, Build: 6}) {
		t.Errorf("fake runs %s", bl.Versions[firmware.RegionApp])
	}
}

func TestFirmwareRefusesWithUnsavedChanges(t *testing.T) {
	env, _ := fwEnv(t)
	m := newFwModel(t, env)
	m.store.Set(1, protocol.Standard, 1, 0x0C)
	m = fwPress(t, m, "f")
	if m.fw != nil || !strings.Contains(m.status, "unsaved") {
		t.Fatalf("fw %v status %q", m.fw, m.status)
	}
}

func TestFirmwareFailureOffersRetry(t *testing.T) {
	env, bl := fwEnv(t)
	bl.FailWrite = 10
	m := fwPress(t, newFwModel(t, env), "f", "enter", "y")
	if m.fw == nil || m.fw.stage != fwFailed || !strings.Contains(m.View(), "Retrying is safe") {
		t.Fatalf("view:\n%s", m.View())
	}
	bl.FailWrite = 0
	m = fwPress(t, m, "r")
	if m.fw != nil {
		t.Fatalf("retry didn't finish: %s", m.fw.errMsg)
	}
}

func TestFirmwareQuitIgnoredWhileFlashing(t *testing.T) {
	env, _ := fwEnv(t)
	m := newFwModel(t, env)
	m.fw = &fwScreen{stage: fwFlashing}
	for _, k := range []string{"ctrl+c", "q", "esc"} {
		if _, cmd := m.Update(fwKey(k)); cmd != nil {
			t.Errorf("%s returned a command while flashing", k)
		}
	}
}

func TestFirmwareLocalFileNotAnImage(t *testing.T) {
	env, bl := fwEnv(t)
	path := filepath.Join(t.TempDir(), "notes.bin")
	os.WriteFile(path, []byte("hello"), 0o644)
	m := fwPress(t, newFwModel(t, env), "f", "down", "down", "enter", path, "enter")
	if m.fw.stage != fwList || !strings.Contains(m.status, "too short") {
		t.Fatalf("stage %d status %q", m.fw.stage, m.status)
	}
	for _, s := range bl.Sent {
		if s[0] == firmware.CmdFirmware && s[1]&0x80 != 0 {
			t.Fatal("wrote to the controller")
		}
	}
}

func TestRecoveryStartsOnFirmwareScreen(t *testing.T) {
	env, bl := fwEnv(t)
	bl.InBootloader = true
	bl.Versions[firmware.RegionApp] = firmware.Version{}
	m := NewRecovery(env, bl)
	m = drive(t, m, m.initCmd)
	if v := m.View(); !strings.Contains(v, "firmware-update mode") {
		t.Fatalf("view:\n%s", v)
	}
	m = fwPress(t, m, "esc") // can't leave recovery without a controller
	if m.fw == nil {
		t.Fatal("esc left recovery")
	}
	m = fwPress(t, m, "enter", "y")
	if m.fw != nil || m.store == nil {
		t.Fatalf("recovery didn't finish: %+v", m.fw)
	}
}
