package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/protocol"
)

func TestHSVRoundTrip(t *testing.T) {
	for _, rgb := range [][3]byte{{0x70, 0xff, 0x5d}, {255, 0, 0}, {0, 0, 255}, {255, 255, 255}, {0x10, 0x7c, 0x10}, {0, 0, 0}} {
		h, s, v := rgbToHSV(rgb)
		got := hsvToRGB(h, s, v)
		for i := range rgb {
			if d := int(got[i]) - int(rgb[i]); d < -3 || d > 3 {
				t.Errorf("%x -> hsv(%d,%d,%d) -> %x", rgb, h, s, v, got)
				break
			}
		}
	}
}

func TestPickerKeys(t *testing.T) {
	c := newColorPicker([3]byte{255, 0, 0}, true)
	if c.update("#") != pickerNone || !c.typingHex {
		t.Fatal("# should start hex entry")
	}
	for _, k := range []string{"0", "0", "f", "f", "0", "0"} {
		c.update(k)
	}
	if c.update("enter") != pickerChanged || c.rgb() != [3]byte{0, 255, 0} {
		t.Errorf("hex entry gave %x", c.rgb())
	}
	c.update("x")
	if c.custom {
		t.Error("x should switch to default color")
	}
	if c.update("right") != pickerChanged || !c.custom {
		t.Error("adjusting should make the color custom again")
	}
}

func key(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

// press sends keys through the model, running any commands synchronously.
func press(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		next, cmd := m.Update(key(k))
		m = next.(Model)
		if cmd != nil {
			if msg := cmd(); msg != nil {
				next, _ = m.Update(msg)
				m = next.(Model)
			}
		}
	}
	return m
}

func TestColorPickFlow(t *testing.T) {
	dev := device.NewFake(loadPages(t))
	store, err := NewStore(dev)
	if err != nil {
		t.Fatal(err)
	}
	m := New(store)
	m.profile = 2

	// Cancel: previews go out, then the light is handed back; nothing changes.
	m = press(t, m, "c", "right", "esc")
	if len(dev.Previews) != 1 || dev.Ended != 1 || store.Dirty[2] {
		t.Fatalf("cancel: previews=%v ended=%d dirty=%v", dev.Previews, dev.Ended, store.Dirty)
	}

	// Apply a hex color, then write.
	m = press(t, m, "c", "#", "1", "2", "3", "4", "5", "6", "enter", "enter")
	if rgb, custom := store.Color(2); !custom || rgb != [3]byte{0x12, 0x34, 0x56} || !store.Dirty[2] {
		t.Fatalf("apply: %x custom=%v dirty=%v", rgb, custom, store.Dirty)
	}
	if last := dev.Previews[len(dev.Previews)-1]; last != [3]byte{0x12, 0x34, 0x56} {
		t.Errorf("last preview %x", last)
	}
	m = press(t, m, "w")
	_ = m
	if dev.Ended != 2 {
		t.Errorf("write should end the preview, ended=%d", dev.Ended)
	}
	for _, page := range []byte{0x22, 0x28} { // both standard and shift pages
		mp, _ := protocol.ParseMappingPage(dev.Pages[page])
		if rgb, custom := mp.Color(); !custom || rgb != [3]byte{0x12, 0x34, 0x56} {
			t.Errorf("page %#x color %x custom=%v", page, rgb, custom)
		}
	}
}
