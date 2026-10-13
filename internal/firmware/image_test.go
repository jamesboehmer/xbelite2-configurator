package firmware

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRegion(t uint32, addr uint32, pages int, fill byte) Region {
	return Region{Type: t, Addr: addr, Version: Version{5, 23, 6, 0}, Data: bytes.Repeat([]byte{fill}, pages*PageSize)}
}

func testImageBytes() []byte {
	return Build(Version{5, 23, 6, 0},
		testRegion(RegionStaging, AppBase, 3, 0x44),
		testRegion(RegionApp, AppBase, 3, 0x33),
		testRegion(RegionBoot, 0x10005000, 1, 0x22))
}

func TestParseBuildRoundtrip(t *testing.T) {
	b := testImageBytes()
	img, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if img.Version != (Version{5, 23, 6, 0}) {
		t.Errorf("version %s", img.Version)
	}
	app := img.Regions[RegionApp]
	if app == nil || app.Addr != AppBase || app.Pages() != 3 || app.Data[0] != 0x33 {
		t.Fatalf("app region %+v", app)
	}
	if img.Regions[RegionBoot].Addr != 0x10005000 {
		t.Error("boot region address")
	}
	if img.SHA256 != sha256.Sum256(b) {
		t.Error("sha256")
	}
}

func TestParseRejects(t *testing.T) {
	good := testImageBytes()
	le := binary.LittleEndian
	cases := map[string]func([]byte) []byte{
		"too short":        func(b []byte) []byte { return b[:0x80] },
		"not a firmware":   func(b []byte) []byte { copy(b, "zip!"); return b },
		"045e:0b12":        func(b []byte) []byte { le.PutUint16(b[0x0a:], 0x0b12); return b },
		"missing region 3": func(b []byte) []byte { le.PutUint32(b[0x80:], 9); return b },
		"multiple of 4096": func(b []byte) []byte { le.PutUint32(b[0x4c:], 0x1800); return b },
		"outside the file": func(b []byte) []byte { le.PutUint32(b[0x48:], 0xfffff000); return b },
		"load address":     func(b []byte) []byte { le.PutUint32(b[0x50:], 0x10000000); return b },
	}
	for want, mutate := range cases {
		_, err := Parse(mutate(append([]byte(nil), good...)))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	a, b := Version{5, 21, 3242, 0}, Version{5, 23, 6, 0}
	if a.Compare(b) != -1 || b.Compare(a) != 1 || a.Compare(a) != 0 {
		t.Error("compare")
	}
	if a.String() != "5.21.3242.0" || !(Version{}).IsZero() || a.IsZero() {
		t.Error("string/zero")
	}
}

func TestFilename(t *testing.T) {
	const name = "DELP_045E_0B00_v_5_23_6_0_h_JxWi1z5Qk2QjWtZUAC3puP7TebjR789X3GyCFqNKjtg=.bin"
	v, sum, ok := ParseFilename("/some/dir/" + name)
	if !ok || v != (Version{5, 23, 6, 0}) {
		t.Fatalf("parse: %v %v", v, ok)
	}
	if hex.EncodeToString(sum[:]) != "2715a2d73e509364235ad654002de9b8fed379b8d1efcf57dc6c8216a34a8ed8" {
		t.Errorf("hash %x", sum)
	}
	if Filename(v, sum) != name {
		t.Errorf("Filename = %s", Filename(v, sum))
	}
	for _, bad := range []string{"firmware.bin", "DELP_045E_0B00_v_5_23_6_0_h_short.bin", "DELP_045E_0B12_v_5_23_6_0_h_JxWi1z5Qk2QjWtZUAC3puP7TebjR789X3GyCFqNKjtg=.bin"} {
		if _, _, ok := ParseFilename(bad); ok {
			t.Errorf("%s parsed", bad)
		}
	}
}

// Point XBE2_FW_BIN at an official 5.23.6.0 .bin to check the real layout.
func TestParseRealImage(t *testing.T) {
	path := os.Getenv("XBE2_FW_BIN")
	if path == "" {
		t.Skip("set XBE2_FW_BIN to an official 5.23.6.0 image to run")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	img, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	_, sum, ok := ParseFilename(filepath.Base(path))
	if !ok || sum != img.SHA256 {
		t.Error("SHA-256 doesn't match the filename")
	}
	if img.Regions[RegionApp].Version != (Version{5, 23, 6, 0}) || img.Regions[RegionStaging].Version != (Version{5, 23, 330, 0}) {
		t.Errorf("region versions %s %s", img.Regions[RegionApp].Version, img.Regions[RegionStaging].Version)
	}
	if r := img.Regions[RegionBoot]; r.Addr != 0x10005000 || len(r.Data) != 0x22000 {
		t.Errorf("boot region %#x %#x", r.Addr, len(r.Data))
	}
	if img.Regions[RegionApp].Pages() != 126 {
		t.Error("app pages")
	}
}
