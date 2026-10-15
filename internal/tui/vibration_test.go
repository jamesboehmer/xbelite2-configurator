package tui

import (
	"strings"
	"testing"

	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/fixture"
	"xbelite2-configurator/internal/protocol"
)

func vibModel(t *testing.T) (Model, *device.Fake) {
	t.Helper()
	dev := device.NewFake(fixture.Pages())
	store, err := NewStore(dev)
	if err != nil {
		t.Fatal(err)
	}
	return New(store), dev
}

func TestVibrationEditWritesBothPages(t *testing.T) {
	m, dev := vibModel(t)
	m = fwPress(t, m, "v")
	if m.vib == nil || !strings.Contains(m.View(), "Left grip") {
		t.Fatalf("panel not open:\n%s", m.View())
	}
	m = fwPress(t, m, "left") // left grip 100 -> 95
	if !m.store.Dirty[1] || !strings.Contains(m.View(), " 95%") {
		t.Fatalf("edit not applied:\n%s", m.View())
	}
	m = fwPress(t, m, "esc", "w")
	written := map[byte][]byte{}
	for _, w := range dev.Writes {
		written[w[0][0]] = w[1]
	}
	for _, page := range []byte{0x20, 0x26} {
		if got := written[page][28:32]; got[0] != 100 || got[1] != 95 || got[2] != 100 || got[3] != 100 {
			t.Errorf("page %#x bytes 28-31 = %v", page, got)
		}
	}
}

func TestVibrationOutOfRangeValueIsClamped(t *testing.T) {
	pages := fixture.Pages()
	pages[0x20][28] = 0xff // right grip
	dev := device.NewFake(pages)
	store, err := NewStore(dev)
	if err != nil {
		t.Fatal(err)
	}
	m := fwPress(t, New(store), "v", "down", "right") // right grip
	_ = m.View()                                      // must not panic
	if v := m.store.Vibration(1).RightGrip; v != 100 {
		t.Errorf("right grip %d, want clamped to 100", v)
	}
	m = fwPress(t, m, "0")
	if v := m.store.Vibration(1).RightGrip; v != 0 {
		t.Errorf("0 key: %d", v)
	}
	m = fwPress(t, m, "left")
	if v := m.store.Vibration(1).RightGrip; v != 0 {
		t.Errorf("below 0: %d", v)
	}
}

func TestVibrationTestBuzz(t *testing.T) {
	m, dev := vibModel(t)
	fwPress(t, m, "v", "t")
	want := []protocol.Motors{{LeftTrigger: 50, RightTrigger: 50, LeftGrip: 50, RightGrip: 50}, {}}
	if len(dev.Rumbles) != 2 || dev.Rumbles[0] != want[0] || dev.Rumbles[1] != want[1] {
		t.Fatalf("rumbles %v", dev.Rumbles)
	}
}

func TestVibrationNoteForInactiveProfile(t *testing.T) {
	m, _ := vibModel(t)
	m = fwPress(t, m, "2", "v")
	if !strings.Contains(m.View(), "saved settings of the active profile") {
		t.Fatalf("missing note:\n%s", m.View())
	}
}

func TestVibrationPanelWriteKey(t *testing.T) {
	m, dev := vibModel(t)
	m = fwPress(t, m, "v", "left", "w")
	if m.vib != nil || len(dev.Writes) == 0 {
		t.Fatalf("panel %v, writes %d", m.vib, len(dev.Writes))
	}
}
