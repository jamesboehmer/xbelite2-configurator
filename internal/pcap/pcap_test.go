package pcap

import (
	"bytes"
	"os"
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
