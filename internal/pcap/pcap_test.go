package pcap

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xbelite2-configurator/internal/fixture"
)

// Capture files aren't committed. Point XBE2_CAPTURE at a USBPcap capture of
// Xbox Accessories saving a profile (buttonremap.pcapng) to run this.
func TestCaptureMatchesFixture(t *testing.T) {
	path := os.Getenv("XBE2_CAPTURE")
	if path == "" {
		t.Skip("set XBE2_CAPTURE to a capture file to run")
	}
	pages, err := LastPages(path)
	if err != nil {
		t.Fatal(err)
	}
	for page, want := range fixture.Pages() {
		if !bytes.Equal(pages[page], want) {
			t.Errorf("page %#x: got %x, want %x", page, pages[page], want)
		}
	}
	var out strings.Builder
	if err := Dump(&out, path); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WRITE page 0x20 size 56") {
		t.Error("decoded dump has no profile 1 write")
	}
}

func TestReadClassicPcap(t *testing.T) {
	le := binary.LittleEndian
	hdr := make([]byte, 24)
	le.PutUint32(hdr, 0xa1b2c3d4)
	le.PutUint16(hdr[4:], 2)
	le.PutUint16(hdr[6:], 4)
	le.PutUint32(hdr[16:], 65535)
	le.PutUint32(hdr[20:], 249)
	data := []byte{0x0c, 0x30, 0x34, 0x01, 0x00}
	usb := make([]byte, 27)
	le.PutUint16(usb, 27)
	le.PutUint16(usb[19:], 16) // device address
	usb[21], usb[22] = 0x02, 1 // EP 0x02 OUT, interrupt
	le.PutUint32(usb[23:], uint32(len(data)))
	pkt := append(usb, data...)
	rec := make([]byte, 16)
	le.PutUint32(rec, 100)
	le.PutUint32(rec[8:], uint32(len(pkt)))
	le.PutUint32(rec[12:], uint32(len(pkt)))
	path := filepath.Join(t.TempDir(), "c.pcap")
	if err := os.WriteFile(path, append(append(hdr, rec...), pkt...), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].FromHost || got[0].Device != 16 || !bytes.Equal(got[0].Data, data) {
		t.Fatalf("got %+v", got)
	}
}
