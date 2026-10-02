package tui

import (
	"bytes"
	"testing"

	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/fixture"
	"xbelite2-configurator/internal/protocol"
)

func loadPages(t *testing.T) map[byte][]byte {
	t.Helper()
	return fixture.Pages()
}

func TestFinalProfile1State(t *testing.T) {
	m, err := protocol.ParseMappingPage(loadPages(t)[0x20])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Y", "B", "X", "A"}
	for i, in := range protocol.Inputs[:4] {
		if got := protocol.OutputName(m[in.Offset]); got != want[i] {
			t.Errorf("%s = %s, want %s", in.Label, got, want[i])
		}
	}
}

func TestUntouchedProfilesAreDefaults(t *testing.T) {
	pages := loadPages(t)
	for _, page := range []byte{0x22, 0x24, 0x28, 0x2A} {
		for _, in := range protocol.Inputs {
			if pages[page][in.Offset] != in.Default {
				t.Errorf("page %#x offset %d = %#x, want %#x", page, in.Offset, pages[page][in.Offset], in.Default)
			}
		}
	}
}

func TestEditAndWriteRoundtrip(t *testing.T) {
	pages := loadPages(t)
	dev := device.NewFake(pages)
	store, err := NewStore(dev)
	if err != nil {
		t.Fatal(err)
	}
	store.Set(2, protocol.Standard, 1, 0x0C) // P1 -> LB on profile 2
	if !store.Dirty[2] || len(store.Dirty) != 1 {
		t.Fatalf("dirty = %v", store.Dirty)
	}
	if err := store.Write(2); err != nil {
		t.Fatal(err)
	}
	if len(store.Dirty) != 0 {
		t.Errorf("still dirty after write: %v", store.Dirty)
	}
	written := map[byte][]byte{}
	for _, w := range dev.Writes {
		written[w[0][0]] = w[1]
	}
	if len(written) != 4 || written[0x22] == nil || written[0x23] == nil || written[0x28] == nil || written[0x29] == nil {
		t.Fatalf("wrong pages written: %v", written)
	}
	if written[0x22][0] != 0x11 || written[0x22][1] != 0x0C {
		t.Errorf("page 0x22 header %x", written[0x22][:2])
	}
	if !bytes.Equal(written[0x22][2:], pages[0x22][2:]) || !bytes.Equal(written[0x23][1:], pages[0x23][1:]) {
		t.Error("untouched bytes changed")
	}
}
