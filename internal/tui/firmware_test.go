package tui

import (
	"errors"
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
		switch msg.(type) {
		case tickMsg, calTickMsg:
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

// infoFailLink fails the first op 00 request, as a timeout in the preamble would.
type infoFailLink struct {
	*firmware.FakeBootloader
	failed bool
}

func (l *infoFailLink) Send(frame []byte) error {
	if !l.failed && len(frame) > 4 && frame[0] == firmware.CmdFirmware && frame[1]&0x80 == 0 && frame[4] == firmware.OpInfo {
		l.failed = true
		l.FakeBootloader.Sent = append(l.FakeBootloader.Sent, frame)
		return errors.New("injected info failure")
	}
	return l.FakeBootloader.Send(frame)
}

func countInfo(bl *firmware.FakeBootloader) int {
	n := 0
	for _, s := range bl.Sent {
		if len(s) > 4 && s[0] == firmware.CmdFirmware && s[1]&0x80 == 0 && s[4] == firmware.OpInfo {
			n++
		}
	}
	return n
}

func TestFirmwareRetryBeforeAnyPassRechecksDevice(t *testing.T) {
	env, bl := fwEnv(t)
	link := &infoFailLink{FakeBootloader: bl}
	env.OpenLink = func() (firmware.Link, error) { return link, nil }
	m := fwPress(t, newFwModel(t, env), "f", "enter", "y")
	if m.fw == nil || m.fw.stage != fwFailed || m.fw.ev.Pass != 0 {
		t.Fatalf("expected failure before any pass: %+v", m.fw)
	}
	before := countInfo(bl)
	m = fwPress(t, m, "r")
	if m.fw != nil {
		t.Fatalf("retry didn't finish: %s", m.fw.errMsg)
	}
	if countInfo(bl) <= before {
		t.Error("retry skipped the device identity check (no op 00)")
	}
}

func TestFirmwareMidPassRetrySkipsPreamble(t *testing.T) {
	env, bl := fwEnv(t)
	bl.FailWrite = 10
	m := fwPress(t, newFwModel(t, env), "f", "enter", "y")
	if m.fw == nil || m.fw.stage != fwFailed || m.fw.ev.Pass == 0 {
		t.Fatalf("expected a mid-pass failure: %+v", m.fw)
	}
	before := countInfo(bl)
	bl.FailWrite = 0
	m = fwPress(t, m, "r")
	if m.fw != nil {
		t.Fatalf("retry didn't finish: %s", m.fw.errMsg)
	}
	if countInfo(bl) != before {
		t.Error("mid-pass retry sent op 00")
	}
}

func countFrames(bl *firmware.FakeBootloader, op byte) int {
	n := 0
	for _, s := range bl.Sent {
		if len(s) > 4 && s[0] == firmware.CmdFirmware && s[1]&0x80 == 0 && s[4] == op {
			n++
		}
	}
	return n
}

func TestFirmwareDeclinedImageIsNeverFlashedByRetry(t *testing.T) {
	env, bl := fwEnv(t)
	env.Reopen = func(firmware.Link) (device.Controller, error) { return nil, errors.New("unplugged") }
	m := fwPress(t, newFwModel(t, env), "f", "enter", "n")
	if m.fw.stage != fwList || m.fw.img != nil {
		t.Fatalf("decline kept the image: %+v", m.fw)
	}
	m = fwPress(t, m, "esc")
	if m.fw == nil || m.fw.stage != fwFailed {
		t.Fatalf("expected a failed reopen: %+v", m.fw)
	}
	m = fwPress(t, m, "enter", "r")
	if m.fw == nil || m.fw.stage != fwFailed {
		t.Fatalf("stage %+v", m.fw)
	}
	if countFrames(bl, firmware.OpStart) != 0 || bl.Resets != 0 {
		t.Errorf("a flash started: %d op 02, %d resets", countFrames(bl, firmware.OpStart), bl.Resets)
	}
	if v := m.View(); strings.Contains(v, "firmware-update mode") {
		t.Errorf("reconnect failure mentions the bootloader:\n%s", v)
	}
}

func TestFirmwareReconnectRetryOnlyReopens(t *testing.T) {
	env, bl := fwEnv(t)
	ok := env.Reopen
	calls := 0
	env.Reopen = func(l firmware.Link) (device.Controller, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("unplugged")
		}
		return ok(l)
	}
	m := fwPress(t, newFwModel(t, env), "f", "enter", "y")
	if m.fw == nil || m.fw.stage != fwFailed || m.fw.failed != failReconnect {
		t.Fatalf("expected a reconnect failure: %+v", m.fw)
	}
	v := m.View()
	if !strings.Contains(v, "Couldn't reconnect to the controller") || !strings.Contains(v, "Press r to try again.") || strings.Contains(v, "firmware-update mode") {
		t.Errorf("view:\n%s", v)
	}
	infos, starts, resets := countInfo(bl), countFrames(bl, firmware.OpStart), bl.Resets
	m = fwPress(t, m, "r")
	if m.fw != nil || calls != 2 {
		t.Fatalf("fw %+v, %d reopen calls", m.fw, calls)
	}
	if !strings.Contains(m.status, "installed") {
		t.Errorf("status %q", m.status)
	}
	if countInfo(bl) != infos || countFrames(bl, firmware.OpStart) != starts || bl.Resets != resets {
		t.Error("retry flashed again")
	}
}

func TestRecoveryBanner(t *testing.T) {
	env, bl := fwEnv(t)
	bl.InBootloader = true
	m := NewRecovery(env, bl)
	m = drive(t, m, m.initCmd)
	if v := m.View(); !strings.Contains(v, "didn't answer normally") || strings.Contains(v, "firmware-update mode") {
		t.Errorf("app version known:\n%s", v)
	}
}

func TestFirmwareConfirmUnknownInstalled(t *testing.T) {
	env, _ := fwEnv(t)
	m := fwPress(t, newFwModel(t, env), "f", "enter")
	m.fw.known = false
	if v := m.View(); !strings.Contains(v, "Installed now:  unknown (couldn't read the controller)") {
		t.Errorf("view:\n%s", v)
	}
}

// After a failure inside a pass the controller is in its bootloader, where a
// normal reconnect can't work, so esc switches to recovery instead.
func TestFirmwareEscAfterMidPassFailureEntersRecovery(t *testing.T) {
	env, bl := fwEnv(t)
	reopens := 0
	reopen := env.Reopen
	env.Reopen = func(l firmware.Link) (device.Controller, error) { reopens++; return reopen(l) }
	bl.FailWrite = 10
	m := fwPress(t, newFwModel(t, env), "f", "enter", "y", "esc")
	if reopens != 0 {
		t.Fatalf("esc tried a normal reconnect %d times", reopens)
	}
	if m.fw == nil || m.fw.stage != fwList || !m.fw.recovery {
		t.Fatalf("esc didn't enter recovery: %+v", m.fw)
	}
	if v := m.View(); !strings.Contains(v, "firmware-update mode") {
		t.Fatalf("view:\n%s", v)
	}
	bl.FailWrite = 0
	m = fwPress(t, m, "enter", "y")
	if m.fw != nil || reopens != 1 {
		t.Fatalf("recovery flash didn't finish (reopens %d): %+v", reopens, m.fw)
	}
}

// Before any pass the controller is still in normal mode, so esc reconnects.
func TestFirmwareEscBeforeAnyPassReconnects(t *testing.T) {
	env, bl := fwEnv(t)
	link := &infoFailLink{FakeBootloader: bl}
	env.OpenLink = func() (firmware.Link, error) { return link, nil }
	m := fwPress(t, newFwModel(t, env), "f", "enter", "y", "esc")
	if m.fw != nil {
		t.Fatalf("esc didn't return to the main screen: %+v", m.fw)
	}
}
