package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/firmware"
	"xbelite2-configurator/internal/protocol"
)

// FirmwareEnv connects the firmware screen to the world. main wires USB and
// the CDN; -demo and the tests wire fakes.
type FirmwareEnv struct {
	Source Source
	// OpenLink opens the firmware link; the normal controller is already closed.
	OpenLink func() (firmware.Link, error)
	// Reopen closes link (if non-nil) and opens the controller normally.
	Reopen func(link firmware.Link) (device.Controller, error)
}

// Source lists and fetches images: firmware.Catalog or firmware.DemoSource.
type Source interface {
	List(ctx context.Context) ([]firmware.Entry, string)
	Fetch(ctx context.Context, e firmware.Entry, progress func(done, total int64)) ([]byte, error)
}

type fwStage int

const (
	fwBusy fwStage = iota // opening, downloading, reconnecting
	fwList
	fwPath
	fwConfirm
	fwFlashing
	fwFailed
)

type fwScreen struct {
	stage    fwStage
	recovery bool
	link     firmware.Link
	state    firmware.State
	known    bool
	entries  []firmware.Entry
	note     string
	sel      int
	path     string
	img      *firmware.Image
	label    string
	verified bool
	ev       firmware.Event
	busyMsg  string
	errMsg   string
	events   chan tea.Msg
}

type fwOpenedMsg struct {
	link    firmware.Link
	state   firmware.State
	known   bool
	entries []firmware.Entry
	note    string
	err     error
}
type fwImageMsg struct {
	img      *firmware.Image
	label    string
	verified bool
	err      error
}
type fwDownloadMsg struct{ done, total int64 }
type fwLinkMsg struct{ link firmware.Link }
type fwEventMsg struct{ ev firmware.Event }
type fwDoneMsg struct{ err error }
type fwReopenedMsg struct {
	store *Store
	ok    string
	err   error
}

func waitFw(ch chan tea.Msg) tea.Cmd { return func() tea.Msg { return <-ch } }

func probeAndList(env FirmwareEnv, link firmware.Link) tea.Msg {
	st, err := firmware.Probe(link)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	entries, note := env.Source.List(ctx)
	return fwOpenedMsg{link: link, state: st, known: err == nil, entries: entries, note: note}
}

// NewRecovery starts on the firmware screen for a controller stuck in its
// bootloader; the main screen opens once a flash succeeds.
func NewRecovery(env FirmwareEnv, link firmware.Link) Model {
	m := Model{fwEnv: env, profile: 1}
	m.fw = &fwScreen{stage: fwBusy, recovery: true, link: link, busyMsg: "Reading the controller..."}
	m.initCmd = func() tea.Msg { return probeAndList(env, link) }
	return m
}

func (m Model) openFirmware() (tea.Model, tea.Cmd) {
	if len(m.store.Dirty) > 0 {
		m.status = "Write or reload your unsaved changes before updating firmware."
		return m, nil
	}
	if m.fwEnv.OpenLink == nil {
		m.status = "Firmware updates aren't available here."
		return m, nil
	}
	m.fw = &fwScreen{stage: fwBusy, busyMsg: "Switching the controller to firmware mode..."}
	m.status = ""
	env, old := m.fwEnv, m.store.dev
	return m, func() tea.Msg {
		old.Close()
		link, err := env.OpenLink()
		if err != nil {
			return fwOpenedMsg{err: err}
		}
		return probeAndList(env, link)
	}
}

// reopen goes back to normal mode and reloads (or first loads) the profiles.
func (m Model) reopen(ok string) tea.Cmd {
	env, link, store := m.fwEnv, m.fw.link, m.store
	return func() tea.Msg {
		dev, err := env.Reopen(link)
		if err != nil {
			return fwReopenedMsg{err: err}
		}
		if store == nil {
			store, err = NewStore(dev)
		} else {
			err = store.Reconnect(dev)
		}
		if err != nil {
			dev.Close()
			return fwReopenedMsg{err: err}
		}
		return fwReopenedMsg{store: store, ok: ok}
	}
}

func (m Model) leaveFirmware() (tea.Model, tea.Cmd) {
	m.fw.stage, m.fw.busyMsg = fwBusy, "Reconnecting to the controller..."
	return m, m.reopen("Back from the firmware screen.")
}

func (m Model) fetch(e firmware.Entry) tea.Cmd {
	f := m.fw
	f.stage, f.busyMsg = fwBusy, "Downloading "+e.Version.String()+"..."
	ch := make(chan tea.Msg, 16)
	f.events = ch
	src := m.fwEnv.Source
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		b, err := src.Fetch(ctx, e, func(done, total int64) {
			select {
			case ch <- fwDownloadMsg{done, total}:
			default:
			}
		})
		if err != nil {
			ch <- fwImageMsg{err: err}
			return
		}
		img, err := firmware.Parse(b)
		ch <- fwImageMsg{img: img, label: e.Version.String() + " (" + e.Source + ")", verified: true, err: err}
	}()
	return waitFw(ch)
}

func (m Model) loadLocal(path string) tea.Cmd {
	m.fw.stage, m.fw.busyMsg = fwBusy, "Reading "+path+"..."
	return func() tea.Msg {
		b, verified, err := firmware.LoadLocal(path)
		if err != nil {
			return fwImageMsg{err: err}
		}
		img, err := firmware.Parse(b)
		return fwImageMsg{img: img, label: filepath.Base(path), verified: verified, err: err}
	}
}

// startFlash runs Flash in the background. A retry reopens the link first,
// since a failed reset leaves the old one disconnected.
func (m Model) startFlash(retry bool) (tea.Model, tea.Cmd) {
	f := m.fw
	// Only skip the device identity check when the failed run got past it.
	fromBoot := f.recovery || (retry && f.ev.Pass > 0)
	f.stage, f.ev, f.errMsg = fwFlashing, firmware.Event{}, ""
	ch := make(chan tea.Msg, 64)
	f.events = ch
	env, link, img := m.fwEnv, f.link, f.img
	opts := firmware.Options{FromBootloader: fromBoot}
	go func() {
		if retry || link == nil {
			if link != nil {
				link.Close()
			}
			l, err := env.OpenLink()
			if err != nil {
				ch <- fwDoneMsg{err: err}
				return
			}
			link = l
			ch <- fwLinkMsg{l}
		}
		err := firmware.Flash(link, img, opts, func(ev firmware.Event) { ch <- fwEventMsg{ev} })
		ch <- fwDoneMsg{err: err}
	}()
	return m, waitFw(ch)
}

func (m Model) updateFirmwareMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	f := m.fw
	switch msg := msg.(type) {
	case fwOpenedMsg:
		if msg.err != nil {
			f.stage, f.errMsg = fwFailed, "Couldn't open the controller for a firmware update: "+msg.err.Error()
			return m, nil
		}
		f.link, f.state, f.known, f.entries, f.note = msg.link, msg.state, msg.known, msg.entries, msg.note
		f.stage = fwList
	case fwDownloadMsg:
		if msg.total > 0 {
			f.busyMsg = fmt.Sprintf("Downloading... %d%%", msg.done*100/msg.total)
		}
		return m, waitFw(f.events)
	case fwImageMsg:
		if msg.err != nil {
			f.stage = fwList
			m.status = "Firmware: " + msg.err.Error()
			return m, nil
		}
		f.img, f.label, f.verified, f.stage = msg.img, msg.label, msg.verified, fwConfirm
		m.status = ""
	case fwLinkMsg:
		f.link = msg.link
		return m, waitFw(f.events)
	case fwEventMsg:
		f.ev = msg.ev
		return m, waitFw(f.events)
	case fwDoneMsg:
		if msg.err != nil {
			f.stage, f.errMsg = fwFailed, msg.err.Error()
			return m, nil
		}
		f.stage, f.busyMsg = fwBusy, "Reconnecting to the controller..."
		return m, m.reopen(fmt.Sprintf("Firmware %s installed.", f.img.Regions[firmware.RegionApp].Version))
	case fwReopenedMsg:
		if msg.err != nil {
			f.link = nil
			f.stage, f.errMsg = fwFailed, "Couldn't reconnect to the controller: "+msg.err.Error()
			return m, nil
		}
		if m.store == nil {
			m.profile = max(protocol.ProfileForPage(msg.store.dev.ActivePage()), 1)
		}
		m.store, m.fw, m.status = msg.store, nil, msg.ok
	}
	return m, nil
}

func (m Model) updateFirmwareKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := m.fw
	k := msg.String()
	switch f.stage {
	case fwList:
		n := len(f.entries) + 1
		switch k {
		case "up", "k":
			f.sel = (f.sel - 1 + n) % n
		case "down", "j":
			f.sel = (f.sel + 1) % n
		case "enter":
			if f.sel == len(f.entries) {
				f.stage, f.path = fwPath, ""
				return m, nil
			}
			return m, m.fetch(f.entries[f.sel])
		case "esc":
			if !f.recovery {
				return m.leaveFirmware()
			}
		case "q":
			if f.recovery {
				return m, tea.Quit
			}
			return m.leaveFirmware()
		}
	case fwPath:
		switch msg.Type {
		case tea.KeyEsc:
			f.stage = fwList
		case tea.KeyEnter:
			return m, m.loadLocal(f.path)
		case tea.KeyBackspace:
			if r := []rune(f.path); len(r) > 0 {
				f.path = string(r[:len(r)-1])
			}
		case tea.KeySpace:
			f.path += " "
		case tea.KeyRunes:
			f.path += string(msg.Runes)
		}
	case fwConfirm:
		switch k {
		case "y":
			return m.startFlash(false)
		case "n", "esc":
			f.stage = fwList
		}
	case fwFailed:
		switch k {
		case "r", "enter":
			if f.img != nil {
				return m.startFlash(true)
			}
		case "esc":
			if !f.recovery {
				return m.leaveFirmware()
			}
		case "q":
			if f.recovery {
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func (f *fwScreen) view(status string) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Xbox Elite Series 2 firmware") + "\n\n")
	if f.recovery {
		b.WriteString(boldStyle.Render(" The controller is in firmware-update mode. Flash a firmware to recover it.") + "\n\n")
	}
	if f.known {
		boot := "unknown"
		if !f.state.Boot.IsZero() {
			boot = f.state.Boot.String()
		}
		app := "none"
		if !f.state.App.IsZero() {
			app = f.state.App.String()
		}
		fmt.Fprintf(&b, " Controller firmware: %s   bootloader: %s\n\n", app, boot)
	} else if f.stage != fwBusy {
		b.WriteString(" Controller firmware: unknown\n\n")
	}
	switch f.stage {
	case fwBusy:
		b.WriteString(" " + f.busyMsg + "\n")
	case fwList:
		for i, e := range f.entries {
			line := fmt.Sprintf("%-14s %-20s", e.Version, e.Source)
			if f.known && e.Version == f.state.App {
				line += " installed"
			}
			if i == f.sel {
				line = selStyle.Render(line)
			}
			b.WriteString("   " + line + "\n")
		}
		line := fmt.Sprintf("%-35s", "Local file...")
		if f.sel == len(f.entries) {
			line = selStyle.Render(line)
		}
		b.WriteString("   " + line + "\n")
		if f.note != "" {
			b.WriteString("\n " + dimStyle.Render(f.note) + "\n")
		}
	case fwPath:
		b.WriteString(" Path to a .bin (relative to the current directory):\n " + f.path + "█\n")
	case fwConfirm:
		b.WriteString(f.confirmView())
	case fwFlashing:
		b.WriteString(f.progressView())
	case fwFailed:
		b.WriteString(" " + boldStyle.Render("Firmware update failed: "+f.errMsg) + "\n\n")
		b.WriteString(" The controller is probably in firmware-update mode. Retrying is safe:\n" +
			" pages already written are skipped, and the bootloader is never touched.\n")
	}
	b.WriteString("\n " + boldStyle.Render(status) + "\n\n")
	b.WriteString(dimStyle.Render(f.help()))
	return b.String()
}

func (f *fwScreen) confirmView() string {
	var b strings.Builder
	to := f.img.Regions[firmware.RegionApp].Version
	fmt.Fprintf(&b, " Flash firmware %s from %s\n", to, f.label)
	if f.known && !f.state.App.IsZero() {
		fmt.Fprintf(&b, " Installed now:  %s\n", f.state.App)
		switch to.Compare(f.state.App) {
		case -1:
			b.WriteString(boldStyle.Render(" This is a DOWNGRADE.") + "\n")
		case 0:
			b.WriteString(" Same version: only pages that differ are rewritten.\n")
		}
	}
	if !f.verified {
		b.WriteString(boldStyle.Render(" UNVERIFIED: the file name carries no SHA-256, so it can't be checked against Microsoft's release.") + "\n")
	}
	if boot := f.img.Regions[firmware.RegionBoot]; boot != nil && f.known && !f.state.Boot.IsZero() && boot.Version != f.state.Boot {
		fmt.Fprintf(&b, " The image's bootloader is %s and the controller's is %s. The bootloader is\n never written, so the controller keeps %s.\n", boot.Version, f.state.Boot, f.state.Boot)
	}
	b.WriteString("\n This takes about 4 minutes. Keep the USB cable connected.\n\n")
	b.WriteString(boldStyle.Render(" Press y to flash, n to cancel.") + "\n")
	return b.String()
}

func (f *fwScreen) progressView() string {
	var b strings.Builder
	b.WriteString(boldStyle.Render(" Flashing. Don't unplug the controller.") + "\n\n")
	switch f.ev.Pass {
	case 1:
		b.WriteString(" Pass 1 of 2: preparing storage (region 4)\n")
	case 2:
		b.WriteString(" Pass 2 of 2: firmware (region 3)\n")
	default:
		b.WriteString(" Starting...\n")
	}
	if f.ev.Total > 0 {
		const width = 40
		n := f.ev.Done * width / f.ev.Total
		fmt.Fprintf(&b, " [%s%s] %d/%d pages (%d written, %d already up to date)\n",
			strings.Repeat("█", n), strings.Repeat("░", width-n), f.ev.Done, f.ev.Total, f.ev.Written, f.ev.Skipped)
	}
	if f.ev.Rebooting {
		b.WriteString(" Rebooting the controller...\n")
	}
	return b.String()
}

func (f *fwScreen) help() string {
	switch f.stage {
	case fwList:
		if f.recovery {
			return "↑/↓ select   enter choose   q quit"
		}
		return "↑/↓ select   enter choose   esc back"
	case fwPath:
		return "type a path   enter load   esc back"
	case fwConfirm:
		return "y flash   n cancel"
	case fwFailed:
		retry := ""
		if f.img != nil {
			retry = "r retry   "
		}
		if f.recovery {
			return retry + "q quit"
		}
		return retry + "esc back"
	}
	return ""
}
