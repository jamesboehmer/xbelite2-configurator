# Firmware Update Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a user update, downgrade, or recover a wired Xbox Elite Series 2's firmware from the configurator's TUI on macOS and Linux.

**Architecture:** A new I/O-free `internal/firmware` package parses `rdhm` images, encodes the GIP cmd `0x0c` protocol (including GIP chunked transfer), finds official images (CDN manifest + built-in list + cache), and runs the flash as a state machine over a small `Link` interface. `internal/device` gets a USB `FirmwareLink` that works without the `0x4D` handshake and survives re-enumeration. The TUI gets a firmware screen (`f`) and a recovery mode. A `FakeBootloader` drives tests and `-demo`; a replay test proves byte-exactness against the real capture.

**Tech Stack:** Go 1.27 (stdlib only: `crypto/sha1`, `crypto/sha256`, `net/http`, `math/rand/v2`), gousb, Bubble Tea, lipgloss.

**Spec:** `docs/superpowers/specs/2026-10-12-firmware-update-design.md`

## Global Constraints

- Only VID:PID `045e:0b00` images; only USB (wired).
- Region type 2 (bootloader) is **never** sent in op `02` or written with op `04`. Passes are region 4, then region 3.
- Host frame bytes must match the capture: request builders and chunk headers are golden-tested against capture hex.
- Timeouts: 2 s per control reply and per window ACK; 5 s for the op `04` status; 30 s to re-acquire after a reset.
- No blind retries of writes. Any timeout or unexpected status aborts with pass, page, and cause.
- After each page write, re-query its hash with op `03` and require a match.
- Commit nothing containing serials, account data, or Microsoft firmware bytes. `.tmp/` is gitignored. Real-file tests read `XBE2_FW_CAPTURE` / `XBE2_FW_BIN` and skip when unset.
- No new module dependencies.
- Commit messages: plain imperative summaries in the repo's style ("Add …"). **No `Co-Authored-By` or other AI attribution** (user's global instruction).
- Match the surrounding code: sparse comments, short doc comments, small helpers, `t.Fatal`/`t.Errorf` tests without assertion libraries.
- Run `gofmt -w` on every Go file you touch before committing (some code blocks here are hand-aligned).
- **Refinement of the spec:** op `00` (info) was only observed in normal mode, so `Probe` and bootloader detection use op `01` region queries only; `Flash` sends op `00` only when not starting from the bootloader.

## Review Focus

1. **The controller never comes back after a reset** (unplugged, or a slow re-enumeration): `Flash` must return an error naming the pass, and the TUI must show the retry-is-safe message, not hang. Test: `TestFlashResetFailureIsReported` (Task 5), `TestFirmwareFailureOffersRetry` (Task 9).
2. **A local file that isn't a firmware image** (a typo'd path, a zip, a truncated download): clear error, the screen stays usable, and nothing is sent to the controller. Test: `TestFirmwareLocalFileNotAnImage` (Task 9).
3. **The manifest is unreachable or returns garbage/404:** the built-in list still shows, with a note. Test: `TestCatalogManifestUnreachable` and `TestCatalogManifestGarbage` (Task 7).
4. **A stale or partial file in the cache** (an interrupted earlier download): it must not be used, and it gets re-downloaded. Test: `TestCatalogFetchIgnoresCorruptCache` (Task 7).
5. **`ctrl+c` or `q` pressed mid-flash:** ignored, so the user can't leave the controller half-flashed by reflex. Test: `TestFirmwareQuitIgnoredWhileFlashing` (Task 9).

---

## File Structure

| File | Responsibility |
|---|---|
| `.gitignore` | add `/.tmp/` |
| `internal/protocol/protocol.go` | `AckFor` echoes the system flag |
| `internal/pcap/pcap.go` | classic-pcap support; `Transfer.Device` (USB address) |
| `internal/firmware/image.go` | `Version`, `Region`, `Image`, `Parse`, `Build`, `ParseFilename`, `Filename` |
| `internal/firmware/wire.go` | cmd `0x0c` request builders, varints, `ChunkPage`, reply/ACK parsers |
| `internal/firmware/fake.go` | `FakeBootloader`, `DemoImage`, `DemoSource` |
| `internal/firmware/flash.go` | `Link`, `Flash`, `Probe`, `Event`, `Options`, `State` |
| `internal/firmware/catalog.go` | `Entry`, `Catalog` (manifest, built-in list, cache), `LoadLocal` |
| `internal/firmware/*_test.go` | unit tests, fake-driven flasher tests, replay test |
| `internal/device/device.go` | split `open` into `openRaw` + handshake; forward firmware frames; idempotent `Close` |
| `internal/device/firmware.go` | `FirmwareLink` (USB `firmware.Link`) |
| `internal/tui/firmware.go` | firmware screen, `FirmwareEnv`, `Source`, recovery |
| `internal/tui/tui.go` | `f` key, routing to the firmware screen, `Store.Reconnect`, `Run`/`RunRecovery` |
| `internal/tui/firmware_test.go` | screen tests against the fake |
| `main.go` | wire real/demo `FirmwareEnv`; recovery at startup |
| `FIRMWARE.md`, `README.md` | protocol doc rewrite; user docs |

---

### Task 1: Fix `AckFor` for system frames and ignore `.tmp/`

**Files:**
- Modify: `internal/protocol/protocol.go:246-250`
- Modify: `.gitignore`
- Test: `internal/protocol/protocol_test.go`

**Interfaces:**
- Produces: `protocol.AckFor(pkt []byte) []byte` now sets ACK payload byte 2 to `pkt[1] & FlagSystem`.

- [ ] **Step 1: Write the failing test**

Append to `internal/protocol/protocol_test.go`:

```go
// The firmware capture ACKs system frames with the system flag echoed:
// device `0c 30 73 3c ...` -> host `01 20 73 09 00 0c 20 3c 00 00 00 00 00`.
func TestAckForSystemFrame(t *testing.T) {
	dev := append([]byte{0x0c, 0x30, 0x73, 0x3c}, make([]byte, 0x3c)...)
	eqHex(t, AckFor(dev), "01207309000c203c0000000000")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/protocol/ -run TestAckForSystemFrame`
Expected: FAIL. The output shows `01207309000c003c0000000000`.

- [ ] **Step 3: Implement**

Replace `AckFor` in `internal/protocol/protocol.go`:

```go
// AckFor ACKs a device frame that had FlagNeedsAck, echoing its system flag.
// Matches captures: `4d 10 03 3c ...` -> `01 20 03 09 00 4d 00 3c 00 00 00 00 00`,
// `0c 30 73 3c ...` -> `01 20 73 09 00 0c 20 3c 00 00 00 00 00`.
func AckFor(pkt []byte) []byte {
	return Frame(CmdAck, FlagSystem, pkt[2], []byte{0x00, pkt[0], pkt[1] & FlagSystem, pkt[3], 0, 0, 0, 0, 0})
}
```

Append to `.gitignore`:

```
# local firmware capture and images (account IDs, Microsoft's binaries)
/.tmp/
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/protocol/ && git status --short`
Expected: `ok`. `.tmp/` no longer appears in `git status`.

- [ ] **Step 5: Commit**

```bash
git add internal/protocol/protocol.go internal/protocol/protocol_test.go .gitignore
git commit -m "Echo the system flag in ACKs and ignore local firmware files"
```

---

### Task 2: Firmware image parsing

**Files:**
- Create: `internal/firmware/image.go`
- Test: `internal/firmware/image_test.go`

**Interfaces:**
- Produces:
  - `type Version struct{ Major, Minor uint16; Build uint32; Rev uint16 }` with `String() string` ("5.23.6.0"), `IsZero() bool`, and `Compare(Version) int`.
  - `const RegionStaging, RegionApp, RegionBoot uint32 = 4, 3, 2`. `const AppBase = 0x10027000`, `PageSize = 0x1000`, `VendorID = 0x045e`, `ProductID = 0x0b00`.
  - `type Region struct{ Type, Addr uint32; Version Version; Data []byte }` with `Pages() int`.
  - `type Image struct{ Version Version; Regions map[uint32]*Region; SHA256 [32]byte }`.
  - `Parse(b []byte) (*Image, error)` and `Build(v Version, regions ...Region) []byte`.
  - `ParseFilename(name string) (Version, [32]byte, bool)` and `Filename(v Version, sum [32]byte) string`.

- [ ] **Step 1: Write the failing tests**

Create `internal/firmware/image_test.go`:

```go
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
		"too short":       func(b []byte) []byte { return b[:0x80] },
		"not a firmware":  func(b []byte) []byte { copy(b, "zip!"); return b },
		"045e:0b12":       func(b []byte) []byte { le.PutUint16(b[0x0a:], 0x0b12); return b },
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/firmware/`
Expected: FAIL to compile with `undefined: Build`.

- [ ] **Step 3: Implement**

Create `internal/firmware/image.go`:

```go
// Package firmware parses Elite Series 2 firmware images, finds official ones,
// and flashes them with the GIP firmware-update command 0x0c. It does no USB
// I/O itself; see Link. See ../../FIRMWARE.md.
package firmware

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
)

// Region types in an rdhm image.
const (
	RegionStaging uint32 = 4 // pass 1: NV/calibration programmer, loaded into the app slot
	RegionApp     uint32 = 3 // pass 2: the controller firmware
	RegionBoot    uint32 = 2 // bootloader; never written
)

const (
	AppBase   = 0x10027000 // the one app slot both passes write
	PageSize  = 0x1000
	VendorID  = 0x045e
	ProductID = 0x0b00

	headerSize = 0x100
	maxRegions = 3
)

// Version is major.minor.build.rev. Images and region queries carry rev 0.
type Version struct {
	Major, Minor uint16
	Build        uint32
	Rev          uint16
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d.%d", v.Major, v.Minor, v.Build, v.Rev)
}

func (v Version) IsZero() bool { return v == (Version{}) }

// Compare returns -1, 0 or 1.
func (v Version) Compare(o Version) int {
	a := [4]uint64{uint64(v.Major), uint64(v.Minor), uint64(v.Build), uint64(v.Rev)}
	b := [4]uint64{uint64(o.Major), uint64(o.Minor), uint64(o.Build), uint64(o.Rev)}
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

// Region is one entry of an image's region table.
type Region struct {
	Type    uint32
	Addr    uint32
	Version Version
	Data    []byte
}

func (r *Region) Pages() int { return len(r.Data) / PageSize }

// Image is a parsed rdhm firmware file.
type Image struct {
	Version Version
	Regions map[uint32]*Region
	SHA256  [32]byte
}

// Parse validates an rdhm image. Header (little-endian): magic, header
// version 1, VID, PID, version, and a region count at 0x1c; region
// descriptors are 0x40 bytes each from 0x40.
func Parse(b []byte) (*Image, error) {
	if len(b) < headerSize {
		return nil, errors.New("firmware file is too short")
	}
	if string(b[:4]) != "rdhm" {
		return nil, errors.New("not a firmware image (no rdhm header)")
	}
	le := binary.LittleEndian
	if v := le.Uint32(b[4:]); v != 1 {
		return nil, fmt.Errorf("unsupported firmware header version %d", v)
	}
	if vid, pid := le.Uint16(b[8:]), le.Uint16(b[0x0a:]); vid != VendorID || pid != ProductID {
		return nil, fmt.Errorf("firmware is for %04x:%04x, not the Elite Series 2 (045e:0b00)", vid, pid)
	}
	img := &Image{
		Version: Version{Major: le.Uint16(b[0x0c:]), Minor: le.Uint16(b[0x0e:]), Build: le.Uint32(b[0x10:])},
		Regions: map[uint32]*Region{},
		SHA256:  sha256.Sum256(b),
	}
	n := int(le.Uint32(b[0x1c:]))
	if n < 1 || n > maxRegions {
		return nil, fmt.Errorf("firmware has %d regions", n)
	}
	for i := 0; i < n; i++ {
		d := b[0x40+i*0x40:]
		r := &Region{
			Type:    le.Uint32(d),
			Addr:    le.Uint32(d[0x10:]),
			Version: Version{Major: le.Uint16(d[0x18:]), Minor: le.Uint16(d[0x1a:]), Build: le.Uint32(d[0x1c:])},
		}
		off, size := uint64(le.Uint32(d[0x08:])), uint64(le.Uint32(d[0x0c:]))
		if off < headerSize || off+size > uint64(len(b)) {
			return nil, fmt.Errorf("firmware region %d lies outside the file", r.Type)
		}
		r.Data = b[off : off+size]
		img.Regions[r.Type] = r
	}
	for _, t := range []uint32{RegionStaging, RegionApp} {
		r := img.Regions[t]
		switch {
		case r == nil:
			return nil, fmt.Errorf("firmware is missing region %d", t)
		case r.Addr != AppBase:
			return nil, fmt.Errorf("firmware region %d has load address %#x, expected %#x", t, r.Addr, AppBase)
		case len(r.Data) == 0 || len(r.Data)%PageSize != 0:
			return nil, fmt.Errorf("firmware region %d size %#x is not a multiple of 4096", t, len(r.Data))
		}
	}
	return img, nil
}

// Build writes an rdhm image, laying regions out in order after the header.
// The demo and tests use it; real images come from Microsoft.
func Build(v Version, regions ...Region) []byte {
	le := binary.LittleEndian
	h := make([]byte, headerSize)
	copy(h, "rdhm")
	le.PutUint32(h[4:], 1)
	le.PutUint16(h[8:], VendorID)
	le.PutUint16(h[0x0a:], ProductID)
	le.PutUint16(h[0x0c:], v.Major)
	le.PutUint16(h[0x0e:], v.Minor)
	le.PutUint32(h[0x10:], v.Build)
	le.PutUint32(h[0x1c:], uint32(len(regions)))
	off := uint32(headerSize)
	for i, r := range regions {
		d := h[0x40+i*0x40:]
		le.PutUint32(d, r.Type)
		le.PutUint32(d[0x08:], off)
		le.PutUint32(d[0x0c:], uint32(len(r.Data)))
		le.PutUint32(d[0x10:], r.Addr)
		le.PutUint16(d[0x18:], r.Version.Major)
		le.PutUint16(d[0x1a:], r.Version.Minor)
		le.PutUint32(d[0x1c:], r.Version.Build)
		off += uint32(len(r.Data))
	}
	out := h
	for _, r := range regions {
		out = append(out, r.Data...)
	}
	return out
}

var nameRE = regexp.MustCompile(`^DELP_045E_0B00_v_(\d+)_(\d+)_(\d+)_(\d+)_h_([A-Za-z0-9_=-]+)\.bin$`)

// ParseFilename reads the version and SHA-256 from an official name such as
// DELP_045E_0B00_v_5_23_6_0_h_<base64url SHA-256>.bin.
func ParseFilename(name string) (Version, [32]byte, bool) {
	var sum [32]byte
	m := nameRE.FindStringSubmatch(filepath.Base(name))
	if m == nil {
		return Version{}, sum, false
	}
	var n [4]uint64
	for i := range n {
		v, err := strconv.ParseUint(m[i+1], 10, 32)
		if err != nil || (i != 2 && v > 0xffff) {
			return Version{}, sum, false
		}
		n[i] = v
	}
	h, err := base64.URLEncoding.DecodeString(m[5])
	if err != nil || len(h) != len(sum) {
		return Version{}, sum, false
	}
	copy(sum[:], h)
	return Version{uint16(n[0]), uint16(n[1]), uint32(n[2]), uint16(n[3])}, sum, true
}

// Filename is the official name for an image with this version and hash.
func Filename(v Version, sum [32]byte) string {
	return fmt.Sprintf("DELP_045E_0B00_v_%d_%d_%d_%d_h_%s.bin",
		v.Major, v.Minor, v.Build, v.Rev, base64.URLEncoding.EncodeToString(sum[:]))
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/firmware/ && XBE2_FW_BIN="$(ls .tmp/DELP_*5_23_6_0*.bin)" go test ./internal/firmware/ -run TestParseRealImage -v`
Expected: `ok`. Then `TestParseRealImage` PASS (not SKIP).

- [ ] **Step 5: Commit**

```bash
git add internal/firmware/image.go internal/firmware/image_test.go
git commit -m "Parse Elite Series 2 firmware images"
```

---

### Task 3: Firmware wire format

**Files:**
- Create: `internal/firmware/wire.go`
- Test: `internal/firmware/wire_test.go`

**Interfaces:**
- Consumes: `Region`, `Version`, `PageSize` (Task 2); `protocol.Frame`, `protocol.Packet`, `protocol.CmdPower`, `protocol.CmdAck`, `protocol.FlagSystem` (existing).
- Produces:
  - `const CmdFirmware = 0x0c` and `CmdLEDMode = 0x0a`.
  - `const OpInfo, OpRegion, OpStart, OpHash, OpWrite, OpFinish = 0..5` and `StatusOK = 0x00`, `StatusReset = 0x03`.
  - Request builders, each returning `[]byte`: `InfoRequest(seq byte)`, `RegionRequest(seq byte, typ uint32)`, `StartRequest(seq byte, r *Region)`, `HashRequest(seq byte, addr uint32)`, `FinishRequest(seq byte)`, `ResetRequest(seq byte)`, `LEDRequest(seq byte)`, `EnterUpdateRequest(seq byte)`.
  - `type PageWrite struct{ Seq byte; Windows [][][]byte; AckAt []int; Terminator []byte }` and `ChunkPage(seq byte, addr uint32, page []byte) PageWrite`.
  - `type Ack struct{ Seq byte; Received, Window int }` and `ParseAck(p protocol.Packet) (Ack, bool)`.
  - `readVarint(b []byte) (v, n int)`, `parseRegionVersion(body []byte) (uint32, Version, error)`, `type info struct{ VID, PID uint16; PageSize uint32 }`, and `parseInfo(body []byte) (info, error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/firmware/wire_test.go`. The golden hex strings are copied from the capture.

```go
package firmware

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"xbelite2-configurator/internal/protocol"
)

func eqHex(t *testing.T, got []byte, want string) {
	t.Helper()
	if hex.EncodeToString(got) != want {
		t.Errorf("got  %x\nwant %s", got, want)
	}
}

func TestRequestsMatchCapture(t *testing.T) {
	staging := &Region{Type: RegionStaging, Addr: AppBase, Version: Version{5, 23, 330, 0}, Data: make([]byte, 0x7e000)}
	eqHex(t, InfoRequest(0x34), "0c30340100")
	eqHex(t, RegionRequest(0x35, RegionStaging), "0c3035050104000000")
	eqHex(t, StartRequest(0x3a, staging), "0c303a1502040000000070021000e00700050017004a010000")
	eqHex(t, HashRequest(0x40, 0x10027000), "0c304009030070021000100000")
	eqHex(t, FinishRequest(0x30), "0c30300105")
	eqHex(t, ResetRequest(0x3b), "05203b0107")
	eqHex(t, LEDRequest(0x3d), "0a203d03000114")
	eqHex(t, EnterUpdateRequest(0x33), "0521330101")
}

func TestReadVarint(t *testing.T) {
	for in, want := range map[string][2]int{"3a": {58, 1}, "ba00": {58, 2}, "8820": {4104, 2}, "8a04": {522, 2}} {
		b, _ := hex.DecodeString(in)
		if v, n := readVarint(b); v != want[0] || n != want[1] {
			t.Errorf("%s: got %d,%d want %v", in, v, n, want)
		}
	}
}

func TestChunkPageMatchesCapture(t *testing.T) {
	page := make([]byte, PageSize)
	for i := range page {
		page[i] = byte(i * 7)
	}
	w := ChunkPage(0x41, 0x10027000, page)
	if len(w.Windows) != 9 {
		t.Fatalf("%d windows, want 9", len(w.Windows))
	}
	wantAcks := []int{58, 570, 1082, 1594, 2106, 2618, 3130, 3642, 4104}
	for i, a := range wantAcks {
		if w.AckAt[i] != a {
			t.Errorf("ack %d at %d, want %d", i, w.AckAt[i], a)
		}
	}
	first := w.Windows[0][0]
	eqHex(t, first[:14], "0cf0413a88200400000000700210")
	eqHex(t, w.Windows[1][0][:6], "0ca041ba003a")
	eqHex(t, w.Windows[1][1][:6], "0ca041ba0074")
	eqHex(t, w.Windows[1][2][:6], "0ca0413aae01")
	eqHex(t, w.Windows[1][8][:6], "0cb041308a04")
	last := w.Windows[8][len(w.Windows[8])-1]
	eqHex(t, last[:6], "0cb04138d01f")
	eqHex(t, w.Terminator, "0ca041008820")

	// Reassemble by the offsets in the headers; every frame fits a 64-byte report.
	msg := make([]byte, 4104)
	for wi, win := range w.Windows {
		for _, c := range win {
			if len(c) > 64 {
				t.Fatalf("chunk of %d bytes", len(c))
			}
			ln, n := readVarint(c[3:])
			off, m := readVarint(c[3+n:])
			if wi == 0 {
				off = 0
			}
			copy(msg[off:], c[3+n+m:3+n+m+ln])
		}
	}
	if msg[0] != OpWrite || binary.LittleEndian.Uint32(msg[4:]) != 0x10027000 || !bytes.Equal(msg[8:], page) {
		t.Error("reassembled message differs")
	}
}

func TestParseAck(t *testing.T) {
	b, _ := hex.DecodeString("01204109000c203a0200000002")
	p, _ := protocol.ParsePacket(b)
	a, ok := ParseAck(p)
	if !ok || a != (Ack{Seq: 0x41, Received: 570, Window: 512}) {
		t.Errorf("got %+v %v", a, ok)
	}
	b, _ = hex.DecodeString("01200109004d00020000000000")
	p, _ = protocol.ParsePacket(b)
	if _, ok := ParseAck(p); ok {
		t.Error("ACK of a 0x4D frame parsed as firmware ACK")
	}
}

func TestParseReplies(t *testing.T) {
	body, _ := hex.DecodeString("04000000050018004a010000453a251a0070021000e00700")
	typ, v, err := parseRegionVersion(body)
	if err != nil || typ != RegionStaging || v != (Version{5, 24, 330, 0}) {
		t.Errorf("region: %d %s %v", typ, v, err)
	}
	body, _ = hex.DecodeString("02005e04000b0010000000100000f049020003000000")
	in, err := parseInfo(body)
	if err != nil || in != (info{VID: 0x045e, PID: 0x0b00, PageSize: 0x1000}) {
		t.Errorf("info: %+v %v", in, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/firmware/`
Expected: FAIL to compile with `undefined: InfoRequest`.

- [ ] **Step 3: Implement**

Create `internal/firmware/wire.go`:

```go
package firmware

import (
	"encoding/binary"
	"errors"

	"xbelite2-configurator/internal/protocol"
)

// GIP firmware-update command and the frames around it, as in the capture.
//
//	op 00 info    host: 00                         device: 00 <st> 0200 <vid> <pid> <page LE32> ...
//	op 01 region  host: 01 <type LE32>             device: 01 <st> <type> <maj> <min> <build LE32> ...
//	op 02 start   host: 02 <type> <addr> <size> <maj> <min> <build>   device: 02 <st> (03 = reset first)
//	op 03 hash    host: 03 <addr LE32> <0x1000>    device: 03 <st> <SHA-1 of the 4 KB page>
//	op 04 write   chunked: 04 00 00 00 <addr LE32> <4096 bytes>       device: 04 <st>
//	op 05 finish  host: 05                         device: 05 <st> (03 = reset)
const (
	CmdFirmware = 0x0c
	CmdLEDMode  = 0x0a

	OpInfo   = 0x00
	OpRegion = 0x01
	OpStart  = 0x02
	OpHash   = 0x03
	OpWrite  = 0x04
	OpFinish = 0x05

	StatusOK    = 0x00
	StatusReset = 0x03

	flagsControl = 0x30 // system | ack-request
	chunkFirst   = 0xf0 // chunked | chunk start | system | ack-request
	chunkMore    = 0xa0 // chunked | system
	chunkAck     = 0xb0 // chunked | system | ack-request

	chunkData  = 58  // data bytes per 64-byte report after the 6-byte header
	windowSize = 512 // bytes between device ACKs
)

var le = binary.LittleEndian

func le32(v uint32) []byte { return le.AppendUint32(nil, v) }

func control(seq byte, payload ...byte) []byte {
	return protocol.Frame(CmdFirmware, flagsControl, seq, payload)
}

func InfoRequest(seq byte) []byte { return control(seq, OpInfo) }

func RegionRequest(seq byte, typ uint32) []byte {
	return control(seq, append([]byte{OpRegion}, le32(typ)...)...)
}

// StartRequest copies the region's descriptor: type, address, size, version.
func StartRequest(seq byte, r *Region) []byte {
	p := []byte{OpStart}
	p = le.AppendUint32(p, r.Type)
	p = le.AppendUint32(p, r.Addr)
	p = le.AppendUint32(p, uint32(len(r.Data)))
	p = le.AppendUint16(p, r.Version.Major)
	p = le.AppendUint16(p, r.Version.Minor)
	p = le.AppendUint32(p, r.Version.Build)
	return control(seq, p...)
}

func HashRequest(seq byte, addr uint32) []byte {
	return control(seq, append(append([]byte{OpHash}, le32(addr)...), le32(PageSize)...)...)
}

func FinishRequest(seq byte) []byte { return control(seq, OpFinish) }

// ResetRequest is GIP power "reset": the controller reboots and re-enumerates.
func ResetRequest(seq byte) []byte {
	return protocol.Frame(protocol.CmdPower, protocol.FlagSystem, seq, []byte{0x07})
}

// LEDRequest turns the guide-button LED on at the brightness the app uses.
func LEDRequest(seq byte) []byte {
	return protocol.Frame(CmdLEDMode, protocol.FlagSystem, seq, []byte{0x00, 0x01, 0x14})
}

// EnterUpdateRequest is sent once before op 00 in normal mode. Its meaning is
// unknown; the Windows app sends it, so we do too.
func EnterUpdateRequest(seq byte) []byte {
	return protocol.Frame(protocol.CmdPower, protocol.FlagSystem|0x01, seq, []byte{0x01})
}

// varint encodes v as 7-bit little-endian groups padded to width bytes.
func varint(v, width int) []byte {
	b := make([]byte, width)
	for i := range b {
		b[i] = byte(v & 0x7f)
		v >>= 7
		if i < width-1 {
			b[i] |= 0x80
		}
	}
	if v != 0 {
		panic("varint overflow")
	}
	return b
}

func readVarint(b []byte) (v, n int) {
	for i, c := range b {
		v |= int(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

// PageWrite is one op 04 message split the way the Windows GIP driver does:
// send each window, wait for the ACK reporting AckAt[i] bytes, then send the
// terminator and wait for the op 04 reply.
type PageWrite struct {
	Seq        byte
	Windows    [][][]byte
	AckAt      []int
	Terminator []byte
}

// ChunkPage builds the chunked message for one 4 KB page. Every chunk header
// is 6 bytes: if the offset fits one varint byte the length takes two.
func ChunkPage(seq byte, addr uint32, page []byte) PageWrite {
	msg := append([]byte{OpWrite, 0, 0, 0}, le32(addr)...)
	msg = append(msg, page...)
	total := len(msg)
	w := PageWrite{Seq: seq}

	first := append([]byte{CmdFirmware, chunkFirst, seq}, varint(chunkData, 1)...)
	first = append(first, varint(total, 2)...)
	w.Windows = append(w.Windows, [][]byte{append(first, msg[:chunkData]...)})
	w.AckAt = append(w.AckAt, chunkData)

	for off := chunkData; off < total; {
		end := min(off+windowSize, total)
		var win [][]byte
		for off < end {
			n := min(chunkData, end-off)
			flags := byte(chunkMore)
			if off+n == end {
				flags = chunkAck
			}
			c := []byte{CmdFirmware, flags, seq}
			if off < 0x80 {
				c = append(append(c, varint(n, 2)...), varint(off, 1)...)
			} else {
				c = append(append(c, varint(n, 1)...), varint(off, 2)...)
			}
			win = append(win, append(c, msg[off:off+n]...))
			off += n
		}
		w.Windows = append(w.Windows, win)
		w.AckAt = append(w.AckAt, end)
	}
	w.Terminator = append(append([]byte{CmdFirmware, chunkMore, seq}, varint(0, 1)...), varint(total, 2)...)
	return w
}

// Ack is the device's ACK of a 0x0c frame: `01 20 <seq> 09 00 0c 20
// <received LE16> 00 00 <window LE16>`, window being what it will take next.
type Ack struct {
	Seq      byte
	Received int
	Window   int
}

func ParseAck(p protocol.Packet) (Ack, bool) {
	if p.Cmd != protocol.CmdAck || len(p.Payload) < 9 || p.Payload[1] != CmdFirmware {
		return Ack{}, false
	}
	return Ack{Seq: p.Seq, Received: int(le.Uint16(p.Payload[3:])), Window: int(le.Uint16(p.Payload[7:]))}, true
}

func parseRegionVersion(body []byte) (uint32, Version, error) {
	if len(body) < 12 {
		return 0, Version{}, errors.New("short region reply")
	}
	return le.Uint32(body), Version{Major: le.Uint16(body[4:]), Minor: le.Uint16(body[6:]), Build: le.Uint32(body[8:])}, nil
}

type info struct {
	VID, PID uint16
	PageSize uint32
}

func parseInfo(body []byte) (info, error) {
	if len(body) < 10 {
		return info{}, errors.New("short info reply")
	}
	return info{VID: le.Uint16(body[2:]), PID: le.Uint16(body[4:]), PageSize: le.Uint32(body[6:])}, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/firmware/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/firmware/wire.go internal/firmware/wire_test.go
git commit -m "Encode the GIP firmware-update protocol and chunked page writes"
```

---

### Task 4: Fake bootloader and demo images

**Files:**
- Create: `internal/firmware/fake.go`
- Test: `internal/firmware/fake_test.go`

**Interfaces:**
- Consumes: everything from Tasks 2–3.
- Produces:
  - `type FakeBootloader struct`, which implements the `Link` interface from Task 5: `Send`, `Recv`, `Reset`, `NextSeq`, `Close`.
    - Exported fields: `InBootloader bool`, `Pages map[uint32][]byte`, `Versions map[uint32]Version`, `Sent [][]byte`, `Resets int`, `Delay`, `ResetDelay time.Duration`.
    - Fault fields: `FailWrite`, `CorruptWrite int`, `StartStatus byte`, `ForgetVersion`, `Silent bool`, `ResetErr error`.
  - `NewFakeBootloader(installed *Image) *FakeBootloader`.
  - `DemoImage(v Version) []byte`.
  - `type DemoSource struct{}` with `List(ctx context.Context) ([]Entry, string)` and `Fetch(ctx context.Context, e Entry, progress func(done, total int64)) ([]byte, error)`.
  - `var ErrTimeout = errors.New("controller did not answer")`. It's defined here and used by the Link contract.
  - `type Entry struct{ Version Version; File, Source string }`. It's defined here so `DemoSource` compiles; Task 7's catalog reuses it.

- [ ] **Step 1: Write the failing tests**

Create `internal/firmware/fake_test.go`:

```go
package firmware

import (
	"crypto/sha1"
	"errors"
	"testing"
	"time"

	"xbelite2-configurator/internal/protocol"
)

var (
	v5244 = Version{5, 24, 4, 0}
	v5236 = Version{5, 23, 6, 0}
)

func demo(t *testing.T, v Version) *Image {
	t.Helper()
	img, err := Parse(DemoImage(v))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// recvOp returns the next 0x0c reply, skipping ACKs.
func recvOp(t *testing.T, l *FakeBootloader) protocol.Packet {
	t.Helper()
	for {
		p, err := l.Recv(time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if p.Cmd == CmdFirmware {
			return p
		}
	}
}

func TestFakeAsksForResetThenEntersBootloader(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	img := demo(t, v5236)
	f.Send(StartRequest(f.NextSeq(), img.Regions[RegionStaging]))
	if p := recvOp(t, f); p.Payload[1] != StatusReset {
		t.Fatalf("start in normal mode: %x", p.Payload)
	}
	if err := f.Reset(); err != nil || !f.InBootloader {
		t.Fatalf("reset: %v, bootloader %v", err, f.InBootloader)
	}
	f.Send(StartRequest(f.NextSeq(), img.Regions[RegionStaging]))
	if p := recvOp(t, f); p.Payload[1] != StatusOK {
		t.Fatalf("start in bootloader: %x", p.Payload)
	}
}

func TestFakeHashAndChunkedWrite(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.InBootloader = true
	page := demo(t, v5236).Regions[RegionApp].Data[:PageSize]
	w := ChunkPage(f.NextSeq(), AppBase, page)
	for i, win := range w.Windows {
		for _, c := range win {
			f.Send(c)
		}
		p, err := f.Recv(time.Second)
		a, ok := ParseAck(p)
		if err != nil || !ok || a.Received != w.AckAt[i] || a.Seq != w.Seq {
			t.Fatalf("window %d: %+v %v %v", i, a, ok, err)
		}
	}
	f.Send(w.Terminator)
	if p := recvOp(t, f); p.Payload[0] != OpWrite || p.Payload[1] != StatusOK {
		t.Fatalf("write reply %x", p.Payload)
	}
	f.Send(HashRequest(f.NextSeq(), AppBase))
	sum := sha1.Sum(page)
	if p := recvOp(t, f); string(p.Payload[2:]) != string(sum[:]) {
		t.Error("hash after write")
	}
}

func TestFakeSilentTimesOut(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.Silent = true
	f.Send(InfoRequest(f.NextSeq()))
	if _, err := f.Recv(time.Second); !errors.Is(err, ErrTimeout) {
		t.Errorf("got %v", err)
	}
}

func TestDemoImagesDiffer(t *testing.T) {
	a, b := demo(t, v5244), demo(t, v5236)
	if string(a.Regions[RegionApp].Data) == string(b.Regions[RegionApp].Data) {
		t.Error("demo images for different versions are identical")
	}
	if a.Regions[RegionApp].Pages() != 126 || a.Regions[RegionBoot] == nil {
		t.Error("demo image layout")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/firmware/`
Expected: FAIL to compile with `undefined: NewFakeBootloader`.

- [ ] **Step 3: Implement**

Create `internal/firmware/fake.go`:

```go
package firmware

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"xbelite2-configurator/internal/protocol"
)

// ErrTimeout is what a Link's Recv returns when nothing arrives in time.
var ErrTimeout = errors.New("controller did not answer")

// Entry is one firmware image the user can pick.
type Entry struct {
	Version Version
	File    string // official filename, which carries the SHA-256
	Source  string // "Microsoft manifest", "built-in list", "demo"
}

// FakeBootloader is an in-memory controller that answers the firmware
// protocol the way the capture shows: op 02 asks for a reset unless it is
// already in the bootloader, op 05 asks for one after every pass, and the
// controller boots normally once region 3 has been finished.
type FakeBootloader struct {
	InBootloader bool
	Pages        map[uint32][]byte // app-slot contents; a missing page is erased (0xFF)
	Versions     map[uint32]Version
	Sent         [][]byte // every frame the host sent, including during Reset
	Resets       int
	Delay        time.Duration // per page write, so -demo shows progress
	ResetDelay   time.Duration

	FailWrite     int   // reject the Nth page write (1-based) with status 0x01
	CorruptWrite  int   // store the Nth page write wrongly, so the verify fails
	StartStatus   byte  // if nonzero, op 02 always answers this
	ForgetVersion bool  // op 05 doesn't record the new version
	Silent        bool  // never reply
	ResetErr      error // Reset fails (controller never comes back)

	seq, devSeq byte
	queue       []protocol.Packet
	writes      int
	msg         []byte
	msgTotal    int
	msgSeq      byte
	wantBoot    bool
	started     uint32
	pending     Version
	finished    uint32
}

// NewFakeBootloader returns a controller in normal mode running installed.
func NewFakeBootloader(installed *Image) *FakeBootloader {
	f := &FakeBootloader{Pages: map[uint32][]byte{}, Versions: map[uint32]Version{}}
	for t, r := range installed.Regions {
		f.Versions[t] = r.Version
	}
	app := installed.Regions[RegionApp]
	for i := 0; i < app.Pages(); i++ {
		f.Pages[app.Addr+uint32(i*PageSize)] = bytes.Clone(app.Data[i*PageSize : (i+1)*PageSize])
	}
	return f
}

func (f *FakeBootloader) NextSeq() byte { f.seq = f.seq%0xFF + 1; return f.seq }

func (f *FakeBootloader) Close() {}

func (f *FakeBootloader) Recv(time.Duration) (protocol.Packet, error) {
	if len(f.queue) == 0 {
		return protocol.Packet{}, ErrTimeout
	}
	p := f.queue[0]
	f.queue = f.queue[1:]
	return p, nil
}

func (f *FakeBootloader) Reset() error {
	f.Sent = append(f.Sent, ResetRequest(f.NextSeq()))
	f.Resets++
	f.queue = nil
	if f.ResetErr != nil {
		return f.ResetErr
	}
	time.Sleep(f.ResetDelay)
	switch {
	case f.wantBoot:
		f.InBootloader = true
	case f.InBootloader && f.finished == RegionApp:
		f.InBootloader = false
	}
	f.wantBoot, f.finished = false, 0
	f.Sent = append(f.Sent, protocol.PowerOnRequest(f.NextSeq()), LEDRequest(f.NextSeq()))
	return nil
}

func (f *FakeBootloader) Send(frame []byte) error {
	f.Sent = append(f.Sent, bytes.Clone(frame))
	if len(frame) < 4 || frame[0] != CmdFirmware {
		return nil
	}
	if frame[1]&0x80 != 0 {
		return f.chunk(frame)
	}
	if frame[1]&protocol.FlagNeedsAck != 0 {
		f.push(protocol.Packet{Cmd: protocol.CmdAck, Flags: protocol.FlagSystem, Seq: frame[2],
			Payload: []byte{0x00, CmdFirmware, protocol.FlagSystem, frame[3], 0, 0, 0, 0, 0}})
	}
	if len(frame) > 4 {
		f.control(frame[4:])
	}
	return nil
}

func (f *FakeBootloader) push(p protocol.Packet) {
	if !f.Silent {
		f.queue = append(f.queue, p)
	}
}

func (f *FakeBootloader) reply(op, status byte, body []byte) {
	f.devSeq = f.devSeq%0xFF + 1
	f.push(protocol.Packet{Cmd: CmdFirmware, Flags: flagsControl, Seq: f.devSeq, Payload: append([]byte{op, status}, body...)})
}

func (f *FakeBootloader) page(addr uint32) []byte {
	if p, ok := f.Pages[addr]; ok {
		return p
	}
	return bytes.Repeat([]byte{0xff}, PageSize)
}

func (f *FakeBootloader) control(p []byte) {
	switch p[0] {
	case OpInfo:
		body := []byte{0x02, 0x00}
		body = le.AppendUint16(body, VendorID)
		body = le.AppendUint16(body, ProductID)
		body = le.AppendUint32(body, PageSize)
		f.reply(OpInfo, StatusOK, body)
	case OpRegion:
		t := le.Uint32(p[1:])
		v := f.Versions[t]
		body := le32(t)
		body = le.AppendUint16(body, v.Major)
		body = le.AppendUint16(body, v.Minor)
		body = le.AppendUint32(body, v.Build)
		f.reply(OpRegion, StatusOK, append(body, 0x45, 0x3a, 0x25, 0x1a))
	case OpStart:
		switch {
		case f.StartStatus != 0:
			f.reply(OpStart, f.StartStatus, nil)
		case !f.InBootloader:
			f.wantBoot = true
			f.reply(OpStart, StatusReset, nil)
		default:
			f.started = le.Uint32(p[1:])
			f.pending = Version{Major: le.Uint16(p[13:]), Minor: le.Uint16(p[15:]), Build: le.Uint32(p[17:])}
			f.Versions[RegionApp] = Version{} // the slot is about to be overwritten
			f.reply(OpStart, StatusOK, nil)
		}
	case OpHash:
		sum := sha1.Sum(f.page(le.Uint32(p[1:])))
		f.reply(OpHash, StatusOK, sum[:])
	case OpFinish:
		if !f.ForgetVersion {
			f.Versions[f.started] = f.pending
		}
		f.finished = f.started
		f.reply(OpFinish, StatusReset, nil)
	}
}

func (f *FakeBootloader) chunk(c []byte) error {
	ln, n := readVarint(c[3:])
	off, m := readVarint(c[3+n:])
	data := c[3+n+m:]
	if len(data) != ln {
		return fmt.Errorf("fake: chunk says %d bytes, carries %d", ln, len(data))
	}
	switch {
	case c[1]&0x40 != 0: // first chunk: the offset field is the total length
		f.msg, f.msgTotal, f.msgSeq = bytes.Clone(data), off, c[2]
	case ln == 0:
		return f.finishWrite()
	case off != len(f.msg):
		return fmt.Errorf("fake: chunk at %d, expected %d", off, len(f.msg))
	default:
		f.msg = append(f.msg, data...)
	}
	if c[1]&protocol.FlagNeedsAck != 0 {
		window := min(windowSize, f.msgTotal-len(f.msg))
		payload := []byte{0x00, CmdFirmware, protocol.FlagSystem}
		payload = le.AppendUint16(payload, uint16(len(f.msg)))
		payload = append(payload, 0, 0)
		payload = le.AppendUint16(payload, uint16(window))
		f.push(protocol.Packet{Cmd: protocol.CmdAck, Flags: protocol.FlagSystem, Seq: f.msgSeq, Payload: payload})
	}
	return nil
}

func (f *FakeBootloader) finishWrite() error {
	if len(f.msg) != f.msgTotal || len(f.msg) != 8+PageSize || f.msg[0] != OpWrite {
		return fmt.Errorf("fake: bad page message of %d bytes", len(f.msg))
	}
	time.Sleep(f.Delay)
	f.writes++
	if f.writes == f.FailWrite {
		f.reply(OpWrite, 0x01, nil)
		return nil
	}
	data := bytes.Clone(f.msg[8:])
	if f.writes == f.CorruptWrite {
		data[0] ^= 0xff
	}
	f.Pages[le.Uint32(f.msg[4:])] = data
	f.reply(OpWrite, StatusOK, nil)
	return nil
}

// DemoImage builds a synthetic image shaped like the real ones: regions 4
// and 3 of 126 pages each (with 39 and 13 blank pages at the end), and a
// region 2. Contents are seeded by version so different versions differ.
func DemoImage(v Version) []byte {
	rng := rand.New(rand.NewPCG(uint64(v.Major)<<48|uint64(v.Minor)<<32|uint64(v.Build), 1))
	region := func(t, addr uint32, pages, blank int) Region {
		d := bytes.Repeat([]byte{0xff}, pages*PageSize)
		for i := 0; i < (pages-blank)*PageSize; i += 8 {
			le.PutUint64(d[i:], rng.Uint64())
		}
		return Region{Type: t, Addr: addr, Version: v, Data: d}
	}
	return Build(v, region(RegionStaging, AppBase, 126, 39), region(RegionApp, AppBase, 126, 13), region(RegionBoot, 0x10005000, 34, 0))
}

// DemoSource offers synthetic images for -demo; nothing is downloaded.
type DemoSource struct{}

var demoVersions = []Version{{5, 23, 6, 0}, {5, 21, 3242, 0}}

func (DemoSource) List(context.Context) ([]Entry, string) {
	var out []Entry
	for _, v := range demoVersions {
		out = append(out, Entry{Version: v, File: Filename(v, sha256.Sum256(DemoImage(v))), Source: "demo"})
	}
	return out, "Demo mode: synthetic images, nothing is downloaded or flashed."
}

func (DemoSource) Fetch(_ context.Context, e Entry, progress func(done, total int64)) ([]byte, error) {
	b := DemoImage(e.Version)
	if progress != nil {
		progress(int64(len(b)), int64(len(b)))
	}
	return b, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/firmware/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/firmware/fake.go internal/firmware/fake_test.go
git commit -m "Add an in-memory firmware bootloader for tests and the demo"
```

---

### Task 5: The flasher

**Files:**
- Create: `internal/firmware/flash.go`
- Test: `internal/firmware/flash_test.go`

**Interfaces:**
- Consumes: Tasks 2–4.
- Produces:

```go
type Link interface {
	Send(frame []byte) error
	Recv(timeout time.Duration) (protocol.Packet, error) // cmd 0x0c frames and their ACKs; ErrTimeout when none
	Reset() error                                        // send 05 20 s 01 07, wait for re-enumeration, re-claim, power on + LED
	NextSeq() byte
	Close()
}
type Event struct{ Pass, Done, Total, Written, Skipped int; Rebooting bool }
type Options struct{ FromBootloader bool }
type State struct{ Staging, App, Boot Version }
func Probe(link Link) (State, error)
func Flash(link Link, img *Image, opts Options, progress func(Event)) error
```

- [ ] **Step 1: Write the failing tests**

Create `internal/firmware/flash_test.go`:

```go
package firmware

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"xbelite2-configurator/internal/protocol"
)

func lastPass(evs []Event, pass int) Event {
	var last Event
	for _, e := range evs {
		if e.Pass == pass && !e.Rebooting {
			last = e
		}
	}
	return last
}

func flash(t *testing.T, f *FakeBootloader, img *Image, opts Options) ([]Event, error) {
	t.Helper()
	var evs []Event
	err := Flash(f, img, opts, func(e Event) { evs = append(evs, e) })
	return evs, err
}

func TestFlashFromNormalMode(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	img := demo(t, v5236)
	evs, err := flash(t, f, img, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.InBootloader || f.Versions[RegionApp] != v5236 {
		t.Errorf("bootloader %v, app %s", f.InBootloader, f.Versions[RegionApp])
	}
	app := img.Regions[RegionApp]
	for i := 0; i < app.Pages(); i++ {
		if !bytes.Equal(f.page(AppBase+uint32(i*PageSize)), app.Data[i*PageSize:(i+1)*PageSize]) {
			t.Fatalf("page %d differs after flashing", i)
		}
	}
	if f.Resets != 4 {
		t.Errorf("%d resets, want 4 (enter bootloader, after each pass, final)", f.Resets)
	}
	for pass := 1; pass <= 2; pass++ {
		if e := lastPass(evs, pass); e.Done != 126 || e.Written != 113 || e.Skipped != 13 {
			t.Errorf("pass %d: %+v", pass, e)
		}
	}
	if !evs[len(evs)-1].Rebooting {
		t.Error("last event isn't the final reboot")
	}
	// The normal-mode preamble, in capture order.
	if got := f.Sent[0]; got[0] != protocol.CmdPower || got[1] != 0x21 {
		t.Errorf("first frame %x", got)
	}
	if got := f.Sent[1]; got[0] != CmdFirmware || got[4] != OpInfo {
		t.Errorf("second frame %x", got)
	}
}

func TestFlashNeverWritesBootRegion(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	if _, err := flash(t, f, demo(t, v5236), Options{}); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.Sent {
		if s[0] != CmdFirmware {
			continue
		}
		if s[1]&0x80 == 0 && s[4] == OpStart && binary.LittleEndian.Uint32(s[5:]) == RegionBoot {
			t.Fatal("op 02 for the bootloader region")
		}
		if s[1] == 0xf0 {
			addr := binary.LittleEndian.Uint32(s[10:])
			if addr < AppBase || addr >= AppBase+126*PageSize {
				t.Fatalf("write outside the app slot: %#x", addr)
			}
		}
	}
}

func TestFlashResumesAfterFailure(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	img := demo(t, v5236)
	f.FailWrite = 50
	_, err := flash(t, f, img, Options{})
	if err == nil || !strings.Contains(err.Error(), "pass 1 of 2") || !strings.Contains(err.Error(), "page 50 of 126") {
		t.Fatalf("got %v", err)
	}
	if !f.InBootloader {
		t.Fatal("fake should be left in the bootloader")
	}
	f.FailWrite = 0
	evs, err := flash(t, f, img, Options{FromBootloader: true})
	if err != nil {
		t.Fatal(err)
	}
	if e := lastPass(evs, 1); e.Skipped != 62 || e.Written != 64 {
		t.Errorf("resumed pass 1: %+v (want 49 done + 13 blank skipped)", e)
	}
	if f.Versions[RegionApp] != v5236 || f.InBootloader {
		t.Error("resume didn't finish")
	}
}

func TestFlashAbortsOnUnexpectedStartStatus(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.StartStatus = 0x01
	_, err := flash(t, f, demo(t, v5236), Options{})
	if err == nil || !strings.Contains(err.Error(), "status 0x01") {
		t.Fatalf("got %v", err)
	}
	for _, s := range f.Sent {
		if s[0] == CmdFirmware && s[1]&0x80 != 0 {
			t.Fatal("wrote a page after a refused start")
		}
	}
}

func TestFlashVerifyMismatchAborts(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.CorruptWrite = 1
	_, err := flash(t, f, demo(t, v5236), Options{})
	if err == nil || !strings.Contains(err.Error(), "verify failed") {
		t.Fatalf("got %v", err)
	}
}

func TestFlashChecksFinalVersion(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.ForgetVersion = true
	_, err := flash(t, f, demo(t, v5236), Options{})
	if err == nil || !strings.Contains(err.Error(), "expected 5.23.6.0") {
		t.Fatalf("got %v", err)
	}
}

func TestFlashTimesOut(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.Silent = true
	if _, err := flash(t, f, demo(t, v5236), Options{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
}

func TestFlashResetFailureIsReported(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.ResetErr = errors.New("controller did not come back within 30s after rebooting")
	_, err := flash(t, f, demo(t, v5236), Options{})
	if err == nil || !strings.Contains(err.Error(), "pass 1 of 2") || !strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("got %v", err)
	}
}

func TestAwaitSkipsStaleReplies(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	// A late answer to a region-2 query must not be taken for region 4's.
	f.reply(OpRegion, StatusOK, append(le32(RegionBoot), 5, 0, 23, 0, 6, 0, 0, 0))
	s := session{link: f}
	v, err := s.regionVersion(RegionStaging)
	if err != nil || v != v5244 {
		t.Fatalf("got %s %v", v, err)
	}
}

func TestProbe(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	st, err := Probe(f)
	if err != nil || st.App != v5244 || st.Boot != v5244 {
		t.Fatalf("got %+v %v", st, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/firmware/`
Expected: FAIL to compile with `undefined: Flash`.

- [ ] **Step 3: Implement**

Create `internal/firmware/flash.go`:

```go
package firmware

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"time"

	"xbelite2-configurator/internal/protocol"
)

// Link carries firmware frames to a controller. device.FirmwareLink is the
// USB one; FakeBootloader is the in-memory one.
type Link interface {
	Send(frame []byte) error
	// Recv returns the next cmd 0x0c frame or ACK of one, or ErrTimeout.
	Recv(timeout time.Duration) (protocol.Packet, error)
	// Reset reboots the controller, waits for it to re-enumerate, re-claims
	// it, and sends power-on and LED-on.
	Reset() error
	NextSeq() byte
	Close()
}

const (
	replyTimeout = 2 * time.Second
	ackTimeout   = 2 * time.Second
	writeTimeout = 5 * time.Second
)

// Event reports progress. Pass is 1 (region 4) or 2 (region 3), 0 outside.
type Event struct {
	Pass, Done, Total, Written, Skipped int
	Rebooting                           bool
}

type Options struct {
	// FromBootloader skips the normal-mode preamble because the controller is
	// already in its bootloader, e.g. after an interrupted flash.
	FromBootloader bool
}

// State is what the controller reports for each region. Zero means unknown.
type State struct {
	Staging, App, Boot Version
}

// Probe reads region versions with op 01, which both normal mode and the
// bootloader answer. Region 2 is best-effort.
func Probe(link Link) (State, error) {
	s := session{link: link}
	var st State
	var err error
	if st.Staging, err = s.regionVersion(RegionStaging); err != nil {
		return st, err
	}
	if st.App, err = s.regionVersion(RegionApp); err != nil {
		return st, err
	}
	st.Boot, _ = s.regionVersion(RegionBoot)
	return st, nil
}

// Flash writes region 4 then region 3 of img, the way Xbox Accessories does:
// pages whose SHA-1 already matches are skipped, each written page is
// re-hashed, and the controller is rebooted between passes. Region 2 (the
// bootloader) is never written, so re-running after a failure resumes.
func Flash(link Link, img *Image, opts Options, progress func(Event)) error {
	if progress == nil {
		progress = func(Event) {}
	}
	s := &session{link: link, progress: progress}
	if !opts.FromBootloader {
		if err := link.Send(EnterUpdateRequest(link.NextSeq())); err != nil {
			return err
		}
		if err := s.checkInfo(); err != nil {
			return err
		}
	}
	for i, typ := range []uint32{RegionStaging, RegionApp} {
		if err := s.pass(i+1, img.Regions[typ]); err != nil {
			return fmt.Errorf("pass %d of 2: %w", i+1, err)
		}
	}
	got, err := s.regionVersion(RegionApp)
	if err != nil {
		return fmt.Errorf("checking the new firmware: %w", err)
	}
	if want := img.Regions[RegionApp].Version; got != want {
		return fmt.Errorf("controller reports firmware %s after flashing, expected %s", got, want)
	}
	progress(Event{Rebooting: true})
	return link.Reset()
}

type session struct {
	link     Link
	progress func(Event)
}

func (s *session) pass(n int, r *Region) error {
	if err := s.start(n, r); err != nil {
		return err
	}
	ev := Event{Pass: n, Total: r.Pages()}
	s.progress(ev)
	for i := 0; i < ev.Total; i++ {
		addr := r.Addr + uint32(i*PageSize)
		page := r.Data[i*PageSize : (i+1)*PageSize]
		want := sha1.Sum(page)
		have, err := s.hash(addr)
		if err != nil {
			return fmt.Errorf("page %d of %d: %w", i+1, ev.Total, err)
		}
		if have == want {
			ev.Skipped++
		} else {
			if err := s.writePage(addr, page); err != nil {
				return fmt.Errorf("page %d of %d: %w", i+1, ev.Total, err)
			}
			if have, err = s.hash(addr); err != nil {
				return fmt.Errorf("page %d of %d: %w", i+1, ev.Total, err)
			}
			if have != want {
				return fmt.Errorf("page %d of %d: verify failed, the controller holds different data", i+1, ev.Total)
			}
			ev.Written++
		}
		ev.Done = i + 1
		s.progress(ev)
	}
	status, _, err := s.call(FinishRequest(s.link.NextSeq()), OpFinish, nil, replyTimeout)
	switch {
	case err != nil:
		return err
	case status == StatusReset:
		ev.Rebooting = true
		s.progress(ev)
		return s.link.Reset()
	case status != StatusOK:
		return fmt.Errorf("controller rejected finishing region %d (status 0x%02x)", r.Type, status)
	}
	return nil
}

// start sends op 02. In normal mode the controller answers 03: reboot into
// the bootloader, then ask again. Like the capture, region versions are
// re-read before each start.
func (s *session) start(n int, r *Region) error {
	for attempt := 1; ; attempt++ {
		for _, t := range []uint32{RegionStaging, RegionApp} {
			if _, err := s.regionVersion(t); err != nil {
				return err
			}
			if t == r.Type {
				break
			}
		}
		status, _, err := s.call(StartRequest(s.link.NextSeq(), r), OpStart, nil, replyTimeout)
		switch {
		case err != nil:
			return err
		case status == StatusOK:
			return nil
		case status == StatusReset && attempt == 1:
			s.progress(Event{Pass: n, Rebooting: true})
			if err := s.link.Reset(); err != nil {
				return err
			}
		case status == StatusReset:
			return errors.New("controller still asks for a reboot after rebooting")
		default:
			return fmt.Errorf("controller refused to start region %d (status 0x%02x)", r.Type, status)
		}
	}
}

func (s *session) checkInfo() error {
	status, body, err := s.call(InfoRequest(s.link.NextSeq()), OpInfo, nil, replyTimeout)
	if err != nil {
		return err
	}
	in, err := parseInfo(body)
	if err != nil || status != StatusOK {
		return fmt.Errorf("unexpected device info reply (status 0x%02x)", status)
	}
	if in.VID != VendorID || in.PID != ProductID || in.PageSize != PageSize {
		return fmt.Errorf("controller reports %04x:%04x with %d-byte pages, expected 045e:0b00 with 4096",
			in.VID, in.PID, in.PageSize)
	}
	return nil
}

func (s *session) regionVersion(t uint32) (Version, error) {
	status, body, err := s.call(RegionRequest(s.link.NextSeq(), t), OpRegion, le32(t), replyTimeout)
	if err != nil {
		return Version{}, err
	}
	if status != StatusOK {
		return Version{}, fmt.Errorf("controller rejected the region %d query (status 0x%02x)", t, status)
	}
	_, v, err := parseRegionVersion(body)
	return v, err
}

func (s *session) hash(addr uint32) ([20]byte, error) {
	var h [20]byte
	status, body, err := s.call(HashRequest(s.link.NextSeq(), addr), OpHash, nil, replyTimeout)
	if err != nil {
		return h, err
	}
	if status != StatusOK || len(body) < len(h) {
		return h, fmt.Errorf("controller rejected the hash query for %#x (status 0x%02x)", addr, status)
	}
	copy(h[:], body)
	return h, nil
}

func (s *session) writePage(addr uint32, page []byte) error {
	w := ChunkPage(s.link.NextSeq(), addr, page)
	for i, win := range w.Windows {
		for _, c := range win {
			if err := s.link.Send(c); err != nil {
				return err
			}
		}
		if err := s.awaitAck(w.Seq, w.AckAt[i]); err != nil {
			return err
		}
	}
	if err := s.link.Send(w.Terminator); err != nil {
		return err
	}
	status, _, err := s.await(OpWrite, nil, writeTimeout)
	if err != nil {
		return err
	}
	if status != StatusOK {
		return fmt.Errorf("controller rejected the write (status 0x%02x)", status)
	}
	return nil
}

func (s *session) call(req []byte, op byte, key []byte, timeout time.Duration) (byte, []byte, error) {
	if err := s.link.Send(req); err != nil {
		return 0, nil, err
	}
	return s.await(op, key, timeout)
}

// await returns the reply to op whose body starts with key, skipping ACKs
// and stale replies.
func (s *session) await(op byte, key []byte, timeout time.Duration) (byte, []byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		p, err := s.recv(deadline)
		if err != nil {
			return 0, nil, fmt.Errorf("%w (op 0x%02x)", err, op)
		}
		if p.Cmd != CmdFirmware || len(p.Payload) < 2 || p.Payload[0] != op || !bytes.HasPrefix(p.Payload[2:], key) {
			continue
		}
		return p.Payload[1], p.Payload[2:], nil
	}
}

func (s *session) awaitAck(seq byte, received int) error {
	deadline := time.Now().Add(ackTimeout)
	for {
		p, err := s.recv(deadline)
		if err != nil {
			return fmt.Errorf("%w (ACK at byte %d)", err, received)
		}
		if a, ok := ParseAck(p); ok && a.Seq == seq && a.Received == received {
			return nil
		}
	}
}

func (s *session) recv(deadline time.Time) (protocol.Packet, error) {
	left := time.Until(deadline)
	if left <= 0 {
		return protocol.Packet{}, ErrTimeout
	}
	return s.link.Recv(left)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/firmware/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/firmware/flash.go internal/firmware/flash_test.go
git commit -m "Flash firmware regions with hash-skipping, verify, and reboots"
```

---

### Task 6: Classic pcap support and the capture replay test

**Files:**
- Modify: `internal/pcap/pcap.go`. `Read` gains classic-pcap support, `Transfer` gains `Device uint16`, and `parseUSBPcap` fills it in.
- Test: `internal/pcap/pcap_test.go`
- Create: `internal/firmware/replay_test.go`

**Interfaces:**
- Consumes: `Flash`, `Parse`, `ParseAck`, `readVarint`, the request builders, and `ErrTimeout`.
- Produces: `pcap.Transfer.Device uint16` (the USB device address). `pcap.Read` now also reads classic `.pcap` files.

- [ ] **Step 1: Write the failing pcap test**

Append to `internal/pcap/pcap_test.go`. Add `"encoding/binary"` and `"path/filepath"` to the imports.

```go
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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/pcap/ -run TestReadClassicPcap`
Expected: FAIL to compile with `got[0].Device undefined`.

- [ ] **Step 3: Implement classic pcap and `Device`**

In `internal/pcap/pcap.go`, change the package comment's first line to `// Package pcap reads USBPcap captures (pcapng or classic pcap, no tshark needed)`. Add `Device` to `Transfer`:

```go
type Transfer struct {
	Time     float64 // seconds since first packet
	FromHost bool
	Device   uint16 // USB device address; changes when the controller re-enumerates
	Data     []byte
}
```

At the top of `Read`, right after `os.ReadFile`, add:

```go
	if len(raw) >= 24 {
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			if m := order.Uint32(raw); m == 0xa1b2c3d4 || m == 0xa1b23c4d {
				return readClassic(path, raw, order)
			}
		}
	}
```

Add the function:

```go
// readClassic reads a libpcap file: a 24-byte header, then records of
// ts_sec, ts_frac, incl_len, orig_len and the packet.
func readClassic(path string, raw []byte, order binary.ByteOrder) ([]Transfer, error) {
	res := 1e-6
	if order.Uint32(raw) == 0xa1b23c4d {
		res = 1e-9
	}
	if order.Uint32(raw[20:])&0x0fffffff != linkTypeUSBPcap {
		return nil, fmt.Errorf("%s is not a USBPcap capture", path)
	}
	var out []Transfer
	first := -1.0
	for off := 24; off+16 <= len(raw); {
		ts := float64(order.Uint32(raw[off:])) + float64(order.Uint32(raw[off+4:]))*res
		n := int(order.Uint32(raw[off+8:]))
		if off+16+n > len(raw) {
			return nil, fmt.Errorf("%s: corrupt record at offset %d", path, off)
		}
		if first < 0 {
			first = ts
		}
		if t, ok := parseUSBPcap(raw[off+16 : off+16+n]); ok {
			t.Time = ts - first
			out = append(out, t)
		}
		off += 16 + n
	}
	if out == nil {
		return nil, errors.New("no USBPcap interrupt/bulk data found in " + path)
	}
	return out, nil
}
```

In `parseUSBPcap`, set the device in the returned value:

```go
	return Transfer{FromHost: pkt[16]&1 == 0, Device: binary.LittleEndian.Uint16(pkt[19:]), Data: pkt[hdrLen:]}, true
```

- [ ] **Step 4: Run pcap tests**

Run: `go test ./internal/pcap/`
Expected: `ok`.

- [ ] **Step 5: Write the replay test**

Create `internal/firmware/replay_test.go`:

```go
package firmware

import (
	"bytes"
	"crypto/sha1"
	"os"
	"testing"
	"time"

	"xbelite2-configurator/internal/pcap"
	"xbelite2-configurator/internal/protocol"
)

// The flash capture and the image aren't committed: the capture holds account
// IDs and serials, the image is Microsoft's. To run:
//
//	XBE2_FW_CAPTURE=.tmp/usbpcap2-flash.pcap \
//	XBE2_FW_BIN=.tmp/DELP_045E_0B00_v_5_23_6_0_h_....bin go test ./internal/firmware/ -run Replay
//
// The controller's recorded replies are served back to Flash, and every frame
// on the write path must match the Windows app's byte for byte, ignoring
// sequence numbers and the verify queries Flash adds after each write.
func TestReplayMatchesCapture(t *testing.T) {
	capPath, binPath := os.Getenv("XBE2_FW_CAPTURE"), os.Getenv("XBE2_FW_BIN")
	if capPath == "" || binPath == "" {
		t.Skip("set XBE2_FW_CAPTURE and XBE2_FW_BIN to run")
	}
	transfers, err := pcap.Read(capPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	img, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	r := newReplay(t, transfers)
	if err := Flash(r, img, Options{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.expect) < 1000 {
		t.Fatalf("only %d expected frames; wrong capture?", len(r.expect))
	}
	for i := range min(len(r.got), len(r.expect)) {
		if !bytes.Equal(r.got[i], r.expect[i]) {
			t.Fatalf("frame %d: sent %x, capture has %x", i, r.got[i], r.expect[i])
		}
	}
	if len(r.got) != len(r.expect) {
		t.Fatalf("sent %d compared frames, capture has %d", len(r.got), len(r.expect))
	}
}

// compared reports whether a host frame is on the write path: op 02/03/05,
// page chunks, resets, power-on and LED.
func compared(f []byte) bool {
	if len(f) < 5 {
		return false
	}
	switch f[0] {
	case protocol.CmdPower:
		return f[1] == protocol.FlagSystem && (f[4] == 0x07 || f[4] == 0x00)
	case CmdLEDMode:
		return true
	case CmdFirmware:
		return f[1]&0x80 != 0 || f[4] == OpStart || f[4] == OpHash || f[4] == OpFinish
	}
	return false
}

func zeroSeq(f []byte) []byte {
	f = bytes.Clone(f)
	f[2] = 0
	return f
}

type replayKey struct {
	dev  uint16
	addr uint32
}

// replay is a Link that answers from the capture. Device addresses change at
// each re-enumeration; Reset moves to the next one.
type replay struct {
	t       *testing.T
	stages  []uint16
	stage   int
	replies map[uint16][]protocol.Packet // op 00/01/02/05 replies per device address
	used    map[uint16][]bool
	hashes  map[replayKey]protocol.Packet
	acks    map[replayKey][]protocol.Packet
	written map[replayKey]protocol.Packet
	expect  [][]byte
	got     [][]byte

	queue     []protocol.Packet
	seq       byte
	msg       []byte
	writeAddr uint32
	ackIdx    int
	lastAddr  uint32
	lastData  []byte
	justWrote bool
	done      bool
}

func newReplay(t *testing.T, ts []pcap.Transfer) *replay {
	r := &replay{t: t, replies: map[uint16][]protocol.Packet{}, used: map[uint16][]bool{},
		hashes: map[replayKey]protocol.Packet{}, acks: map[replayKey][]protocol.Packet{}, written: map[replayKey]protocol.Packet{}}
	var hashAddr, writeAddr uint32
	var writeSeq byte
	started, resets := false, 0
	for _, tr := range ts {
		if n := len(r.stages); n == 0 || r.stages[n-1] != tr.Device {
			r.stages = append(r.stages, tr.Device)
		}
		d := tr.Data
		if len(d) < 4 {
			continue
		}
		if tr.FromHost {
			if d[0] == protocol.CmdPower && d[1] == 0x21 {
				started = true
			}
			// Flash from normal mode resets four times; the capture after the
			// fourth is the controller's ordinary startup.
			if started && resets < 4 && compared(d) {
				r.expect = append(r.expect, zeroSeq(d))
				if d[0] == protocol.CmdPower && d[4] == 0x07 {
					resets++
				}
			}
			if d[0] == CmdFirmware && d[1]&0x80 == 0 && len(d) >= 9 && d[4] == OpHash {
				hashAddr = le.Uint32(d[5:])
			}
			if d[0] == CmdFirmware && d[1] == chunkFirst {
				_, n := readVarint(d[3:])
				_, m := readVarint(d[3+n:])
				writeAddr, writeSeq = le.Uint32(d[3+n+m+4:]), d[2]
			}
			continue
		}
		p, err := protocol.ParsePacket(d)
		if err != nil {
			continue
		}
		switch {
		case p.Cmd == CmdFirmware && len(p.Payload) > 0 && p.Payload[0] == OpHash:
			r.hashes[replayKey{tr.Device, hashAddr}] = p
		case p.Cmd == CmdFirmware && len(p.Payload) > 0 && p.Payload[0] == OpWrite:
			r.written[replayKey{tr.Device, writeAddr}] = p
		case p.Cmd == CmdFirmware && len(p.Payload) > 0:
			r.replies[tr.Device] = append(r.replies[tr.Device], p)
			r.used[tr.Device] = append(r.used[tr.Device], false)
		default:
			if a, ok := ParseAck(p); ok && a.Seq == writeSeq {
				k := replayKey{tr.Device, writeAddr}
				r.acks[k] = append(r.acks[k], p)
			}
		}
	}
	return r
}

func (r *replay) dev() uint16   { return r.stages[r.stage] }
func (r *replay) NextSeq() byte { r.seq = r.seq%0xFF + 1; return r.seq }
func (r *replay) Close()        {}

func (r *replay) Recv(time.Duration) (protocol.Packet, error) {
	if len(r.queue) == 0 {
		return protocol.Packet{}, ErrTimeout
	}
	p := r.queue[0]
	r.queue = r.queue[1:]
	return p, nil
}

func (r *replay) Reset() error {
	r.Send(ResetRequest(r.NextSeq()))
	r.stage++
	r.queue = nil
	if r.stage >= len(r.stages)-1 {
		r.done = true // the last address is the controller's ordinary startup
	}
	r.Send(protocol.PowerOnRequest(r.NextSeq()))
	return r.Send(LEDRequest(r.NextSeq()))
}

func (r *replay) Send(f []byte) error {
	if r.done {
		return nil
	}
	f = bytes.Clone(f)
	verify := f[0] == CmdFirmware && f[1]&0x80 == 0 && f[4] == OpHash && r.justWrote && le.Uint32(f[5:]) == r.lastAddr
	if compared(f) && !verify {
		r.got = append(r.got, zeroSeq(f))
	}
	switch {
	case f[0] == CmdFirmware && f[1]&0x80 != 0:
		r.chunk(f)
	case f[0] == CmdFirmware:
		r.control(f, verify)
	}
	return nil
}

func (r *replay) control(f []byte, verify bool) {
	op := f[4]
	if op == OpHash {
		r.justWrote = false
		if verify {
			sum := sha1.Sum(r.lastData)
			r.queue = append(r.queue, protocol.Packet{Cmd: CmdFirmware, Payload: append([]byte{OpHash, StatusOK}, sum[:]...)})
			return
		}
		p, ok := r.hashes[replayKey{r.dev(), le.Uint32(f[5:])}]
		if !ok {
			r.t.Fatalf("capture has no hash reply for %#x on device %d", le.Uint32(f[5:]), r.dev())
		}
		r.queue = append(r.queue, p)
		return
	}
	var key []byte
	if op == OpRegion {
		key = f[5:9]
	}
	list, used := r.replies[r.dev()], r.used[r.dev()]
	last := -1
	for i, p := range list {
		if p.Payload[0] != op || !bytes.HasPrefix(p.Payload[2:], key) {
			continue
		}
		last = i
		if !used[i] {
			used[i] = true
			r.queue = append(r.queue, p)
			return
		}
	}
	if last < 0 {
		r.t.Fatalf("capture has no op %#02x reply on device %d", op, r.dev())
	}
	r.queue = append(r.queue, list[last])
}

func (r *replay) chunk(f []byte) {
	ln, n := readVarint(f[3:])
	_, m := readVarint(f[3+n:])
	data := f[3+n+m:]
	k := replayKey{r.dev(), r.writeAddr}
	switch {
	case f[1] == chunkFirst:
		r.msg = bytes.Clone(data)
		r.writeAddr = le.Uint32(data[4:])
		r.ackIdx = 0
		k = replayKey{r.dev(), r.writeAddr}
	case ln == 0:
		p, ok := r.written[k]
		if !ok {
			r.t.Fatalf("capture has no write reply for %#x on device %d", r.writeAddr, r.dev())
		}
		r.queue = append(r.queue, p)
		r.lastAddr, r.lastData, r.justWrote = r.writeAddr, r.msg[8:], true
		return
	default:
		r.msg = append(r.msg, data...)
	}
	if f[1]&protocol.FlagNeedsAck != 0 {
		acks := r.acks[k]
		if r.ackIdx >= len(acks) {
			r.t.Fatalf("capture has only %d ACKs for %#x", len(acks), r.writeAddr)
		}
		a := acks[r.ackIdx]
		a.Seq = f[2] // the device echoes the host's seq; ours differs from the capture's
		r.queue = append(r.queue, a)
		r.ackIdx++
	}
}
```

- [ ] **Step 6: Run the replay test against the real capture**

Run:
```bash
XBE2_FW_CAPTURE=.tmp/usbpcap2-flash.pcap XBE2_FW_BIN="$(ls .tmp/DELP_*5_23_6_0*.bin)" \
  go test ./internal/firmware/ -run TestReplayMatchesCapture -v
```
Expected: PASS, not SKIP.

If it fails, the message names the first frame that differs. Fix `wire.go` or `flash.go`, not the test's `compared` filter. The filter is what the spec defines as the write path. Then run `go test ./...` and expect every package to pass, with the replay test skipped.

- [ ] **Step 7: Commit**

```bash
git add internal/pcap/pcap.go internal/pcap/pcap_test.go internal/firmware/replay_test.go
git commit -m "Replay the Windows flash capture against the flasher"
```

---

### Task 7: Catalog: manifest, built-in list, cache, local files

**Files:**
- Create: `internal/firmware/catalog.go`
- Test: `internal/firmware/catalog_test.go`

**Interfaces:**
- Consumes: `Entry` (Task 4), `ParseFilename`, `Filename`, and `Version` (Task 2).
- Produces:
  - `const ManifestURL`, `DefaultBase`.
  - `var BuiltIn []string`.
  - `type Catalog struct{ ManifestURL string; BuiltIn []string; CacheDir string; Client *http.Client }`.
  - `NewCatalog(cacheDir string) *Catalog`.
  - `(*Catalog).List(ctx context.Context) ([]Entry, string)`.
  - `(*Catalog).Fetch(ctx context.Context, e Entry, progress func(done, total int64)) ([]byte, error)`.
  - `LoadLocal(path string) ([]byte, bool, error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/firmware/catalog_test.go`:

```go
package firmware

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cdn serves a manifest behind a redirect (as aka.ms does) and image files.
func cdn(t *testing.T, manifest string, files map[string][]byte) *httptest.Server {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/aka", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/M/PC_GA_x.json", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/M/PC_GA_x.json", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.ReplaceAll(manifest, "BASE", srv.URL+"/FW/"))
	})
	mux.HandleFunc("/FW/", func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/FW/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testCatalog(t *testing.T, srv *httptest.Server) *Catalog {
	c := NewCatalog(t.TempDir())
	c.ManifestURL = srv.URL + "/aka"
	c.Client = srv.Client()
	c.BuiltIn = []string{Filename(Version{5, 21, 3242, 0}, sha256.Sum256(DemoImage(Version{5, 21, 3242, 0})))}
	return c
}

func manifestFor(name string) string {
	return `{"g":"pc_ga","v":"1.0.0.23","p":"BASE","d":[
	  {"__n":"merl","id":"045E_0B12","f":[{"__t":"beta","p":"MERL_045E_0B12_v_5_23_6_0_h_jC-y7nRWRtQCHQCyi2rv6xtQpgikM6JG0r6lYE3A5pM=.bin"}]},
	  {"__n":"delp","id":"045E_0B00","f":[{"__t":"beta","p":"` + name + `"}],"req":[{"v":"5.21.3242.0"}]}]}`
}

func TestCatalogListMergesManifestAndBuiltIn(t *testing.T) {
	img := DemoImage(v5236)
	name := Filename(v5236, sha256.Sum256(img))
	c := testCatalog(t, cdn(t, manifestFor(name), map[string][]byte{name: img}))
	entries, note := c.List(context.Background())
	if note != "" || len(entries) != 2 {
		t.Fatalf("entries %+v note %q", entries, note)
	}
	if entries[0].Version != v5236 || entries[0].Source != "Microsoft manifest" ||
		entries[1].Version != (Version{5, 21, 3242, 0}) || entries[1].Source != "built-in list" {
		t.Errorf("entries %+v", entries)
	}
}

func TestCatalogManifestUnreachable(t *testing.T) {
	srv := cdn(t, "", nil)
	c := testCatalog(t, srv)
	srv.Close()
	entries, note := c.List(context.Background())
	if len(entries) != 1 || entries[0].Source != "built-in list" || !strings.Contains(note, "manifest") {
		t.Fatalf("entries %+v note %q", entries, note)
	}
}

func TestCatalogManifestGarbage(t *testing.T) {
	c := testCatalog(t, cdn(t, "<html>not json</html>", nil))
	entries, note := c.List(context.Background())
	if len(entries) != 1 || note == "" {
		t.Fatalf("entries %+v note %q", entries, note)
	}
}

func TestCatalogFetchVerifiesAndCaches(t *testing.T) {
	img := DemoImage(v5236)
	name := Filename(v5236, sha256.Sum256(img))
	srv := cdn(t, manifestFor(name), map[string][]byte{name: img})
	c := testCatalog(t, srv)
	entries, _ := c.List(context.Background())
	var last int64
	b, err := c.Fetch(context.Background(), entries[0], func(done, total int64) { last = done })
	if err != nil || len(b) != len(img) || last != int64(len(img)) {
		t.Fatalf("fetch: %v, %d bytes, progress %d", err, len(b), last)
	}
	srv.Close()
	if _, err := c.Fetch(context.Background(), entries[0], nil); err != nil {
		t.Fatalf("cached fetch: %v", err)
	}
}

func TestCatalogFetchRejectsCorruptDownload(t *testing.T) {
	img := DemoImage(v5236)
	name := Filename(v5236, sha256.Sum256(img))
	bad := append([]byte(nil), img...)
	bad[1000] ^= 1
	c := testCatalog(t, cdn(t, manifestFor(name), map[string][]byte{name: bad}))
	entries, _ := c.List(context.Background())
	if _, err := c.Fetch(context.Background(), entries[0], nil); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.CacheDir, name)); !os.IsNotExist(err) {
		t.Error("corrupt download was cached")
	}
}

func TestCatalogFetchIgnoresCorruptCache(t *testing.T) {
	img := DemoImage(v5236)
	name := Filename(v5236, sha256.Sum256(img))
	c := testCatalog(t, cdn(t, manifestFor(name), map[string][]byte{name: img}))
	if err := os.WriteFile(filepath.Join(c.CacheDir, name), img[:5000], 0o644); err != nil {
		t.Fatal(err)
	}
	entries, _ := c.List(context.Background())
	b, err := c.Fetch(context.Background(), entries[0], nil)
	if err != nil || len(b) != len(img) {
		t.Fatalf("got %d bytes, %v", len(b), err)
	}
}

func TestLoadLocal(t *testing.T) {
	dir := t.TempDir()
	img := DemoImage(v5236)
	official := filepath.Join(dir, Filename(v5236, sha256.Sum256(img)))
	os.WriteFile(official, img, 0o644)
	if _, verified, err := LoadLocal(official); err != nil || !verified {
		t.Errorf("official: %v %v", verified, err)
	}
	plain := filepath.Join(dir, "firmware.bin")
	os.WriteFile(plain, img, 0o644)
	if _, verified, err := LoadLocal(plain); err != nil || verified {
		t.Errorf("plain: %v %v", verified, err)
	}
	wrong := filepath.Join(dir, Filename(v5236, [32]byte{1}))
	os.WriteFile(wrong, img, 0o644)
	if _, _, err := LoadLocal(wrong); err == nil {
		t.Error("hash mismatch accepted")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/firmware/`
Expected: FAIL to compile with `undefined: NewCatalog`.

- [ ] **Step 3: Implement**

Create `internal/firmware/catalog.go`:

```go
package firmware

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// ManifestURL is what Xbox Accessories resolves; it redirects to a JSON
	// manifest listing the current image per device. No account needed.
	ManifestURL = "https://aka.ms/xboxaccessoriesfwcdnpcga"
	// DefaultBase is the manifest's firmware base URL, for when it's unreachable.
	DefaultBase = "https://xbaccessories-bhf7bsbfgxdpbrg4.b02.azurefd.net/accessories/FW/"

	deviceID     = "045E_0B00"
	maxImageSize = 4 << 20
)

// BuiltIn lists official images the manifest no longer offers. The manifest
// only names the latest; these stay downloadable by exact name.
var BuiltIn = []string{
	"DELP_045E_0B00_v_5_23_6_0_h_JxWi1z5Qk2QjWtZUAC3puP7TebjR789X3GyCFqNKjtg=.bin",
	"DELP_045E_0B00_v_5_21_3242_0_h_8APxwmp_Ea7mJ3kLqvXD9wJLh7aX1HWOi3h4Lbek2KI=.bin",
}

// Catalog finds official images and downloads them into CacheDir.
type Catalog struct {
	ManifestURL string
	BuiltIn     []string
	CacheDir    string
	Client      *http.Client
	base        string
}

func NewCatalog(cacheDir string) *Catalog {
	return &Catalog{ManifestURL: ManifestURL, BuiltIn: BuiltIn, CacheDir: cacheDir,
		Client: &http.Client{Timeout: 2 * time.Minute}}
}

type manifest struct {
	P string `json:"p"`
	D []struct {
		ID string `json:"id"`
		F []struct {
			P string `json:"p"`
		} `json:"f"`
	} `json:"d"`
}

// List returns images newest first. note explains a manifest failure; the
// built-in list is still returned then.
func (c *Catalog) List(ctx context.Context) ([]Entry, string) {
	byVersion := map[Version]Entry{}
	add := func(name, source string) {
		if v, _, ok := ParseFilename(name); ok {
			if _, seen := byVersion[v]; !seen {
				byVersion[v] = Entry{Version: v, File: name, Source: source}
			}
		}
	}
	var note string
	if m, err := c.manifest(ctx); err != nil {
		note = "Couldn't read Microsoft's firmware manifest (" + err.Error() + "); showing the built-in list."
	} else {
		c.base = m.P
		for _, d := range m.D {
			if strings.EqualFold(d.ID, deviceID) {
				for _, f := range d.F {
					add(f.P, "Microsoft manifest")
				}
			}
		}
	}
	for _, name := range c.BuiltIn {
		add(name, "built-in list")
	}
	out := make([]Entry, 0, len(byVersion))
	for _, e := range byVersion {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version.Compare(out[j].Version) > 0 })
	return out, note
}

func (c *Catalog) manifest(ctx context.Context) (*manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ManifestURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var m manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("bad manifest: %w", err)
	}
	if !strings.HasPrefix(m.P, "https://") && !strings.HasPrefix(m.P, "http://") {
		return nil, fmt.Errorf("manifest has no firmware base URL")
	}
	return &m, nil
}

// Fetch returns the image for e, from the cache if a cached copy still
// matches the SHA-256 in its name, otherwise downloaded and cached.
func (c *Catalog) Fetch(ctx context.Context, e Entry, progress func(done, total int64)) ([]byte, error) {
	_, want, ok := ParseFilename(e.File)
	if !ok {
		return nil, fmt.Errorf("%s is not an official firmware name", e.File)
	}
	path := filepath.Join(c.CacheDir, e.File)
	if b, err := os.ReadFile(path); err == nil && sha256.Sum256(b) == want {
		if progress != nil {
			progress(int64(len(b)), int64(len(b)))
		}
		return b, nil
	}
	base := c.base
	if base == "" {
		base = DefaultBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+e.File, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: HTTP %d", e.Version, resp.StatusCode)
	}
	b, err := io.ReadAll(&progressReader{r: io.LimitReader(resp.Body, maxImageSize), total: resp.ContentLength, fn: progress})
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(b) != want {
		return nil, fmt.Errorf("download of %s is corrupt (SHA-256 mismatch)", e.Version)
	}
	if err := os.MkdirAll(c.CacheDir, 0o755); err == nil {
		tmp := path + ".part"
		if os.WriteFile(tmp, b, 0o644) == nil {
			os.Rename(tmp, path)
		}
	}
	return b, nil
}

type progressReader struct {
	r     io.Reader
	done  int64
	total int64
	fn    func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.fn != nil && n > 0 {
		p.fn(p.done, p.total)
	}
	return n, err
}

// LoadLocal reads a .bin from disk. verified is true when its name carries a
// SHA-256 and the file matches it; a name with a hash that doesn't match is
// an error.
func LoadLocal(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	if _, want, ok := ParseFilename(path); ok {
		if sha256.Sum256(b) != want {
			return nil, false, fmt.Errorf("%s doesn't match the SHA-256 in its name", filepath.Base(path))
		}
		return b, true, nil
	}
	return b, false, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/firmware/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/firmware/catalog.go internal/firmware/catalog_test.go
git commit -m "Find official firmware from the manifest and a built-in list"
```

---

### Task 8: USB firmware link

**Files:**
- Modify: `internal/device/device.go`
- Create: `internal/device/firmware.go`

**Interfaces:**
- Consumes: the `firmware.Link` contract, `firmware.ErrTimeout`, `firmware.ResetRequest`, `firmware.LEDRequest`, `firmware.CmdFirmware` (Tasks 3–5); `protocol.PowerOnRequest`.
- Produces:
  - `device.OpenFirmwareLink(pid gousb.ID, debug io.Writer) (*FirmwareLink, error)`. `*FirmwareLink` implements `firmware.Link`.
  - `(*USB).Close()` is now safe to call twice.

There are no unit tests here, matching the existing `device.go`, which talks to libusb directly. Its logic is exercised through the fake in Tasks 5 and 9 and checked by hand in Task 10.

- [ ] **Step 1: Split `open` and forward firmware frames**

In `internal/device/device.go`:

Add fields to `USB`:

```go
	fw        chan protocol.Packet // firmware frames and their ACKs, for FirmwareLink
	closeOnce sync.Once
```

Replace `Open` and `open` with:

```go
// Open finds and claims the controller. pid 0 tries the known product IDs.
// debug, if non-nil, receives a log of every USB step and packet.
func Open(pid gousb.ID, debug io.Writer) (*USB, error) {
	lg := newLogger(debug)
	return withRetries(lg, func() (*USB, error) {
		u, err := openRaw(pid, lg, nil)
		if err != nil {
			return nil, err
		}
		if err := u.handshake(); err != nil {
			u.Close()
			return nil, err
		}
		return u, nil
	})
}

func newLogger(debug io.Writer) *log.Logger {
	if debug == nil {
		debug = io.Discard
	}
	return log.New(debug, "", log.Lmicroseconds)
}

func withRetries(lg *log.Logger, try func() (*USB, error)) (*USB, error) {
	var lastErr error
	for attempt := 1; attempt <= openAttempts; attempt++ {
		u, err := try()
		if err == nil {
			return u, nil
		}
		lg.Printf("open attempt %d failed: %v", attempt, err)
		lastErr = err
		if errors.Is(err, errNotFound) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, lastErr
}
```

Rename the old `open` to `openRaw(pid gousb.ID, lg *log.Logger, fw chan protocol.Packet) (*USB, error)`. Set `fw: fw` in the `&USB{...}` literal. Delete its last lines, which do the handshake, so that it ends:

```go
	rctx, cancel := context.WithCancel(context.Background())
	u.cancel = cancel
	go u.readLoop(rctx)
	return u, nil
}
```

In `readLoop`'s `switch pkt.Cmd`, add a case before `case protocol.CmdConfig:`:

```go
		case protocol.CmdAck, firmware.CmdFirmware:
			if u.fw != nil && (pkt.Cmd == firmware.CmdFirmware || len(pkt.Payload) > 1 && pkt.Payload[1] == firmware.CmdFirmware) {
				select {
				case u.fw <- pkt:
				default:
					u.log.Printf("firmware frame dropped: %x", data)
				}
			}
```

Make `Close` idempotent:

```go
func (u *USB) Close() {
	u.closeOnce.Do(func() {
		if u.cancel != nil {
			u.cancel()
			<-u.done
		}
		u.closeUSB()
		u.log.Printf("closed; controller handed back to the OS driver")
	})
}
```

Add `"xbelite2-configurator/internal/firmware"` to the imports.

- [ ] **Step 2: Create `internal/device/firmware.go`**

```go
package device

import (
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"time"

	"github.com/google/gousb"

	"xbelite2-configurator/internal/firmware"
	"xbelite2-configurator/internal/protocol"
)

const (
	reacquireFor  = 30 * time.Second
	reacquirePoll = 250 * time.Millisecond
	dropFor       = 3 * time.Second
)

// FirmwareLink is a firmware.Link over USB. It skips the 0x4D handshake, so it
// also works with a controller sitting in its bootloader, and Reset survives
// the controller re-enumerating.
type FirmwareLink struct {
	pid    gousb.ID
	log    *log.Logger
	u      *USB
	frames chan protocol.Packet
	seq    byte
}

func OpenFirmwareLink(pid gousb.ID, debug io.Writer) (*FirmwareLink, error) {
	f := &FirmwareLink{pid: pid, log: newLogger(debug), frames: make(chan protocol.Packet, 64)}
	u, err := withRetries(f.log, func() (*USB, error) { return openRaw(pid, f.log, f.frames) })
	if err != nil {
		return nil, err
	}
	f.u = u
	if err := f.Send(protocol.PowerOnRequest(f.NextSeq())); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (f *FirmwareLink) NextSeq() byte { f.seq = f.seq%0xFF + 1; return f.seq }

func (f *FirmwareLink) Send(frame []byte) error {
	if f.u == nil {
		return errors.New("controller is not connected")
	}
	return f.u.send(frame)
}

func (f *FirmwareLink) Recv(timeout time.Duration) (protocol.Packet, error) {
	select {
	case p := <-f.frames:
		return p, nil
	case <-time.After(timeout):
		return protocol.Packet{}, firmware.ErrTimeout
	}
}

// Reset reboots the controller and re-claims it once it re-enumerates. It
// waits for the old device to drop off the bus first, so it doesn't reopen
// the instance that is about to vanish.
func (f *FirmwareLink) Reset() error {
	if err := f.Send(firmware.ResetRequest(f.NextSeq())); err != nil {
		f.log.Printf("reset send: %v (controller may already be rebooting)", err)
	}
	f.Close()
	f.waitGone()
	deadline := time.Now().Add(reacquireFor)
	var lastErr error
	for time.Now().Before(deadline) {
		time.Sleep(reacquirePoll)
		u, err := openRaw(f.pid, f.log, f.frames)
		if err != nil {
			lastErr = err
			continue
		}
		f.u = u
		for len(f.frames) > 0 {
			<-f.frames
		}
		f.log.Printf("controller re-acquired after reset")
		if err := f.Send(protocol.PowerOnRequest(f.NextSeq())); err != nil {
			return err
		}
		return f.Send(firmware.LEDRequest(f.NextSeq()))
	}
	return fmt.Errorf("controller did not come back within %s after rebooting (%v)", reacquireFor, lastErr)
}

func (f *FirmwareLink) waitGone() {
	ctx := gousb.NewContext()
	defer ctx.Close()
	deadline := time.Now().Add(dropFor)
	for time.Now().Before(deadline) {
		if !f.present(ctx) {
			f.log.Printf("controller dropped off the bus")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.log.Printf("controller still listed %s after reset", dropFor)
}

// present lists devices without opening any.
func (f *FirmwareLink) present(ctx *gousb.Context) bool {
	found := false
	devs, _ := ctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
		if d.Vendor == VendorID && (d.Product == f.pid || f.pid == 0 && slices.Contains(ProductIDs, d.Product)) {
			found = true
		}
		return false
	})
	for _, d := range devs {
		d.Close()
	}
	return found
}

func (f *FirmwareLink) Close() {
	if f.u != nil {
		f.u.Close()
		f.u = nil
	}
}
```

- [ ] **Step 3: Build and vet**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: no errors, and every package `ok`. The `-demo` and USB paths still compile because `main.go` is unchanged so far.

- [ ] **Step 4: Commit**

```bash
git add internal/device/device.go internal/device/firmware.go
git commit -m "Add a USB firmware link that survives re-enumeration"
```

---

### Task 9: TUI firmware screen and recovery mode

**Files:**
- Create: `internal/tui/firmware.go`
- Modify: `internal/tui/tui.go`
- Test: `internal/tui/firmware_test.go`

**Interfaces:**
- Consumes: `firmware.{Link, Entry, Image, State, Event, Options, Flash, Probe, Parse, LoadLocal, RegionApp, RegionBoot}` and `device.Controller`.
- Produces:
  - `type FirmwareEnv struct{ Source Source; OpenLink func() (firmware.Link, error); Reopen func(firmware.Link) (device.Controller, error) }`.
  - `type Source interface{ List(context.Context) ([]firmware.Entry, string); Fetch(context.Context, firmware.Entry, func(done, total int64)) ([]byte, error) }`.
  - `Run(dev device.Controller, env FirmwareEnv) error` and `RunRecovery(link firmware.Link, env FirmwareEnv) error`.
  - `NewRecovery(env FirmwareEnv, link firmware.Link) Model`.
  - `(*Store).Reconnect(dev device.Controller) error`.

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/firmware_test.go`:

```go
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

func key(k string) tea.KeyMsg {
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

func press(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		next, cmd := m.Update(key(k))
		m = drive(t, next.(Model), cmd)
	}
	return m
}

func TestFirmwareUpdateEndToEnd(t *testing.T) {
	env, bl := fwEnv(t)
	m := press(t, newFwModel(t, env), "f")
	if m.fw == nil || m.fw.stage != fwList {
		t.Fatalf("firmware screen not open: %+v", m.fw)
	}
	if v := m.View(); !strings.Contains(v, "5.24.4.0") || !strings.Contains(v, "5.23.6.0") || !strings.Contains(v, "5.21.3242.0") {
		t.Fatalf("list view:\n%s", v)
	}
	m = press(t, m, "enter")
	if m.fw.stage != fwConfirm || !strings.Contains(m.View(), "DOWNGRADE") {
		t.Fatalf("confirm view:\n%s", m.View())
	}
	m = press(t, m, "y")
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
	m = press(t, m, "f")
	if m.fw != nil || !strings.Contains(m.status, "unsaved") {
		t.Fatalf("fw %v status %q", m.fw, m.status)
	}
}

func TestFirmwareFailureOffersRetry(t *testing.T) {
	env, bl := fwEnv(t)
	bl.FailWrite = 10
	m := press(t, newFwModel(t, env), "f", "enter", "y")
	if m.fw == nil || m.fw.stage != fwFailed || !strings.Contains(m.View(), "Retrying is safe") {
		t.Fatalf("view:\n%s", m.View())
	}
	bl.FailWrite = 0
	m = press(t, m, "r")
	if m.fw != nil {
		t.Fatalf("retry didn't finish: %s", m.fw.errMsg)
	}
}

func TestFirmwareQuitIgnoredWhileFlashing(t *testing.T) {
	env, _ := fwEnv(t)
	m := newFwModel(t, env)
	m.fw = &fwScreen{stage: fwFlashing}
	for _, k := range []string{"ctrl+c", "q", "esc"} {
		if _, cmd := m.Update(key(k)); cmd != nil {
			t.Errorf("%s returned a command while flashing", k)
		}
	}
}

func TestFirmwareLocalFileNotAnImage(t *testing.T) {
	env, bl := fwEnv(t)
	path := filepath.Join(t.TempDir(), "notes.bin")
	os.WriteFile(path, []byte("hello"), 0o644)
	m := press(t, newFwModel(t, env), "f", "down", "down", "enter", path, "enter")
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
	m = press(t, m, "esc") // can't leave recovery without a controller
	if m.fw == nil {
		t.Fatal("esc left recovery")
	}
	m = press(t, m, "enter", "y")
	if m.fw != nil || m.store == nil {
		t.Fatalf("recovery didn't finish: %+v", m.fw)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tui/`
Expected: FAIL to compile with `m.fwEnv undefined`.

- [ ] **Step 3: Create `internal/tui/firmware.go`**

```go
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
	f.stage, f.ev, f.errMsg = fwFlashing, firmware.Event{}, ""
	ch := make(chan tea.Msg, 64)
	f.events = ch
	env, link, img := m.fwEnv, f.link, f.img
	opts := firmware.Options{FromBootloader: retry || f.recovery}
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
		if f.recovery {
			return "r retry   q quit"
		}
		return "r retry   esc back"
	}
	return ""
}
```

- [ ] **Step 4: Wire the screen into `internal/tui/tui.go`**

Add fields to `Model`:

```go
	fwEnv   FirmwareEnv
	fw      *fwScreen
	initCmd tea.Cmd
```

Replace `Init`:

```go
func (m Model) Init() tea.Cmd {
	if m.initCmd != nil {
		return tea.Batch(tick(), m.initCmd)
	}
	return tick()
}
```

Add `Reconnect` after `Store.Load`:

```go
// Reconnect switches to a reopened controller and reloads every profile.
func (s *Store) Reconnect(dev device.Controller) error {
	s.dev = dev
	for _, p := range protocol.Profiles {
		if err := s.Load(p); err != nil {
			return err
		}
	}
	return nil
}
```

In `Update`, add the firmware messages before `case tea.KeyMsg:`:

```go
	case fwOpenedMsg, fwDownloadMsg, fwImageMsg, fwLinkMsg, fwEventMsg, fwDoneMsg, fwReopenedMsg:
		if m.fw != nil {
			return m.updateFirmwareMsg(msg)
		}
```

Replace the start of the `case tea.KeyMsg:` body with:

```go
	case tea.KeyMsg:
		if m.fw != nil && m.fw.stage == fwFlashing {
			return m, nil // never leave a controller half-flashed
		}
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.fw != nil {
			return m.updateFirmwareKey(msg)
		}
		if m.busy {
			return m, nil
		}
```

Keep the rest of that case (`m.color`, `m.picking`, `updateMain`) unchanged.

In `updateMain`'s switch, add:

```go
	case "f":
		return m.openFirmware()
```

Update `help`:

```go
const help = "↑/↓ select   ←/→ change output   enter pick from list   0 disable\n" +
	"1/2/3 profile   tab standard/shift   c color   d reset profile   r reload   w write   f firmware   q quit"
```

At the top of `View`:

```go
	if m.fw != nil {
		return m.fw.view(m.status)
	}
```

Replace `Run` with:

```go
// Run loads all profiles and starts the UI. It owns dev and closes it.
func Run(dev device.Controller, env FirmwareEnv) error {
	store, err := NewStore(dev)
	if err != nil {
		dev.Close()
		return err
	}
	m := New(store)
	m.fwEnv = env
	return runProgram(m)
}

// RunRecovery starts on the firmware screen for a controller in its bootloader.
func RunRecovery(link firmware.Link, env FirmwareEnv) error {
	return runProgram(NewRecovery(env, link))
}

func runProgram(m Model) error {
	useTrueColorUnderSudo()
	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	fm, _ := final.(Model)
	if fm.fw != nil && fm.fw.link != nil {
		fm.fw.link.Close()
	}
	if fm.store != nil {
		if fm.fw == nil {
			if perr := fm.store.EndPreview(); err == nil && perr != nil {
				err = fmt.Errorf("could not end LED preview: %w", perr)
			}
		}
		fm.store.dev.Close()
	}
	return err
}
```

Add `"xbelite2-configurator/internal/firmware"` to `tui.go`'s imports.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/tui/`
Expected: FAIL to compile, because `main.go` still calls `tui.Run(dev)`. That's expected; it's fixed in Task 10. Run the package alone to check it compiles and passes:

Run: `go vet ./internal/tui/ && go test ./internal/tui/`
Expected: `ok`. (`go test ./internal/tui/` builds only that package and its dependencies, not `main`.)

- [ ] **Step 6: Commit**

```bash
git add internal/tui/firmware.go internal/tui/firmware_test.go internal/tui/tui.go
git commit -m "Add the firmware screen and bootloader recovery to the TUI"
```

---

### Task 10: Wire `main.go`, docs, and the hardware check

**Files:**
- Modify: `main.go`
- Modify: `FIRMWARE.md` (currently untracked; rewrite it, then add it)
- Modify: `README.md`

**Interfaces:**
- Consumes: `tui.Run`, `tui.RunRecovery`, `tui.FirmwareEnv`, `device.OpenFirmwareLink`, `firmware.NewCatalog`, `firmware.Probe`, `firmware.DemoImage`, `firmware.DemoSource`, `firmware.NewFakeBootloader`.

- [ ] **Step 1: Wire `main.go`**

Replace `run` in `main.go` and add the helpers. Add `"path/filepath"`, `"time"`, and `"xbelite2-configurator/internal/firmware"` to the imports.

```go
func run(demo bool, capture, decode, pidStr, debug string) error {
	if decode != "" {
		return pcap.Dump(os.Stdout, decode)
	}

	if demo {
		pages := fixture.Pages()
		if capture != "" {
			var err error
			if pages, err = pcap.LastPages(capture); err != nil {
				return err
			}
		}
		dev := device.NewFake(pages)
		return tui.Run(dev, demoFirmware(dev))
	}

	var pid gousb.ID
	if pidStr != "" {
		v, err := strconv.ParseUint(pidStr, 0, 16)
		if err != nil {
			return fmt.Errorf("bad -pid: %v", err)
		}
		pid = gousb.ID(v)
	}
	var logw io.Writer
	if debug != "" {
		f, err := os.OpenFile(debug, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		logw = f
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	env := tui.FirmwareEnv{
		Source:   firmware.NewCatalog(filepath.Join(cache, "xbelite2-configurator", "firmware")),
		OpenLink: func() (firmware.Link, error) { return device.OpenFirmwareLink(pid, logw) },
		Reopen: func(l firmware.Link) (device.Controller, error) {
			if l != nil {
				l.Close()
			}
			return device.Open(pid, logw)
		},
	}
	fmt.Fprintln(os.Stderr, "Connecting to controller...")
	u, err := device.Open(pid, logw)
	if err != nil {
		if link, ok := bootloaderLink(pid, logw); ok {
			fmt.Fprintln(os.Stderr, "The controller is in firmware-update mode; opening recovery.")
			return tui.RunRecovery(link, env)
		}
		return err
	}
	return tui.Run(u, env)
}

// bootloaderLink checks whether a controller that failed the normal handshake
// answers the firmware protocol, as it does when stuck in its bootloader.
func bootloaderLink(pid gousb.ID, logw io.Writer) (firmware.Link, bool) {
	link, err := device.OpenFirmwareLink(pid, logw)
	if err != nil {
		return nil, false
	}
	if _, err := firmware.Probe(link); err != nil {
		link.Close()
		return nil, false
	}
	return link, true
}

// demoFirmware pretends the controller runs 5.24.4.0 and flashes synthetic
// images slowly enough to watch.
func demoFirmware(dev device.Controller) tui.FirmwareEnv {
	installed, err := firmware.Parse(firmware.DemoImage(firmware.Version{Major: 5, Minor: 24, Build: 4}))
	if err != nil {
		panic(err)
	}
	bl := firmware.NewFakeBootloader(installed)
	bl.Delay = 10 * time.Millisecond
	bl.ResetDelay = time.Second
	return tui.FirmwareEnv{
		Source:   firmware.DemoSource{},
		OpenLink: func() (firmware.Link, error) { return bl, nil },
		Reopen:   func(firmware.Link) (device.Controller, error) { return dev, nil },
	}
}
```

Delete the old `defer dev.Close()`, since `tui.Run` now owns the controller, and the old `var dev device.Controller` block.

- [ ] **Step 2: Build, test, and try the demo**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: every package `ok`.

Run: `go build && ./xbelite2-configurator -demo`, then press `f`, `enter`, `y`, and watch both passes complete and return to the main screen. Press `q` to quit.
Expected: the screen shows "DOWNGRADE", both pass labels, the progress bar, and "Firmware 5.23.6.0 installed."

- [ ] **Step 3: Rewrite `FIRMWARE.md`**

Replace the whole file:

````markdown
# Firmware update protocol (Elite Series 2)

Decoded from a USBPcap capture of the Windows Xbox Accessories app downgrading a wired
Elite Series 2 from 5.24.4.0 to 5.23.6.0, cross-checked against the official 5.23.6.0
and 5.21.3242.0 images. Same conventions as [PROTOCOL.md](PROTOCOL.md). Anything marked
*(inferred)* is interpretation; everything else matched the capture byte for byte.
`internal/firmware` implements it, and `TestReplayMatchesCapture` replays the capture
against it.

> ⚠️ A failed flash leaves the controller in its bootloader. That's recoverable by
> flashing again, as long as the bootloader itself (region 2) is never written. This
> configurator never writes it.

## Finding images

`https://aka.ms/xboxaccessoriesfwcdnpcga` redirects to an unauthenticated JSON manifest.
Keys: `p` is the firmware base URL; `d[]` lists devices with `id` (`045E_0B00` for the
Elite 2), `f[].p` (filename), and `req[].v` (a minimum version, advisory only). It lists
only the latest image. Older ones stay downloadable at `<p><filename>` if you know the
exact name. The `_h_<base64url>` part of a filename is the SHA-256 of the file.

## Image format (`rdhm`)

Little-endian. A `0x100`-byte header, not flashed:

| off | size | field |
|---|---|---|
| `0x00` | 4 | magic `rdhm` |
| `0x04` | 4 | header version `1` |
| `0x08` | 2 | VID `0x045e` |
| `0x0a` | 2 | PID `0x0b00` |
| `0x0c` | 2 | version major |
| `0x0e` | 2 | version minor |
| `0x10` | 4 | version build |
| `0x1c` | 4 | region count (3) |

Region descriptors are `0x40` bytes each, at `0x40`, `0x80`, and `0xC0`:

| off | size | field |
|---|---|---|
| `0x00` | 4 | type |
| `0x08` | 4 | file offset |
| `0x0c` | 4 | size |
| `0x10` | 4 | load address |
| `0x14` | 4 | unknown, not sent |
| `0x18` | 2 | version major |
| `0x1a` | 2 | version minor |
| `0x1c` | 4 | version build |

| type | size | load | contents | flashed |
|---|---|---|---|---|
| 4 | `0x7E000` | `0x10027000` | small NV/calibration/"patch image" programmer; the same code in every release apart from its signature *(role inferred)* | pass 1 |
| 3 | `0x7E000` | `0x10027000` | the controller firmware | pass 2 |
| 2 | `0x22000` | `0x10005000` | bootloader (GIP and device-auth stack) *(inferred)*; changes between releases | never seen written |

There is one app slot. When pass 2 starts, the controller's page hashes equal region 4's,
so pass 2 overwrites what pass 1 wrote. Regions 4 and 3 are different programs, not A/B
copies.

## Transport

Interface 0, interrupt EP `0x02` OUT / `0x82` IN, GIP framing. The controller keeps PID
`0x0b00` in its bootloader, where it announces firmware `0.0.0.0`. It re-enumerates with
a new USB address on every reset, so the host has to reopen it.

Firmware messages are cmd `0x0c` with flags `0x30` (system + ACK-request). The host ACKs
device frames as `01 20 s 09 00 0c 20 <len LE16> 00 00 00 00`. Note the `20`, which
echoes the system flag.

| op | host | device |
|---|---|---|
| `00` | `00` | `00 <st> 0200 <vid> <pid> <page 0x1000> <0x1000> <0x249f0> <3>` |
| `01` | `01 <type LE32>` | `01 <st> <type> <maj u16> <min u16> <build LE32> 453a251a <addr> <size>` |
| `02` | `02 <type> <addr> <size> <maj> <min> <build>` (the descriptor) | `02 <st>`: `00` go, `03` reboot into bootloader first |
| `03` | `03 <addr LE32> <0x1000 LE32>` | `03 <st> <SHA-1 of that 4 KB page>` |
| `04` | chunked, see below | `04 <st>` |
| `05` | `05` | `05 <st>`: `03` reboot |

Other host frames: `05 20 s 01 07` resets; `05 20 s 01 00` powers on; `0a 20 s 03 00 01 14`
turns the LED on; `05 21 s 01 01` is sent once before op `00` *(meaning unknown)*.

## Page writes: GIP chunked transfer

One message per page: `04 00 00 00 <addr LE32>` followed by 4,096 data bytes, 4,104
bytes in total, all under one seq.

1. First chunk `0c f0 s 3a 88 20`, then message bytes 0–57. Wait for the ACK.
2. Windows of 512 bytes, in chunks of up to 58 bytes. Each chunk header is
   `0c <flag> s <len><offset>`, with the varints padded to 3 bytes: when the offset is
   below 128 the length takes 2 bytes (`ba 00`), otherwise the offset does. The window's
   last chunk has flag `b0` (ACK-request); the others have `a0`. The device ACKs with
   `01 20 s 09 00 0c 20 <received LE16> 00 00 <next window LE16>`.
3. Terminator `0c a0 s 00 88 20`. The device replies op `04`.

Varints are 7-bit little-endian groups with a `0x80` continuation bit. This rule
re-encodes all 226 page writes in the capture byte for byte.

## Sequence

| stage | host |
|---|---|
| normal firmware | `05 21 01`; op `00`; op `01` for the regions; op `02` (region 4) → `03`; reset |
| bootloader | power on; LED; op `01`; op `02` (4) → `00`; for each of 126 pages: op `03`, and op `04` if the hash differs; op `05` → `03`; reset |
| bootloader | power on; LED; op `01` (4, 3); op `02` (3) → `00`; the pages; op `05` → `03`; reset |
| new firmware | power on; LED; op `01` (3) reports the new version; reset |

The capture wrote 113 of the 126 pages in each pass; the 13 skipped pages are blank
(`0xFF`). The configurator also re-queries op `03` after each write to verify it. Gaps
between re-enumerations ran 2.5–5.3 s.

## Not known

- What `05 21 s 01 01` does, and what op `01`'s constant `453a251a` means.
- Whether, and how, the Windows app writes region 2 when the image's bootloader differs
  from the controller's.
- Whether the bootloader answers op `00` (the configurator doesn't rely on it).
````

- [ ] **Step 4: Update `README.md`**

After the paragraph that starts "Nothing changes on the controller until you press `w`.", add:

```markdown
## Firmware updates

Press `f` to open the firmware screen. It shows the installed firmware and the images you
can flash: the latest one from Microsoft's firmware manifest, older official ones the app
knows by name (so you can downgrade), or a local `.bin`. Downloads are checked against the
SHA-256 in their official filename and cached in your user cache directory.

Flashing takes about 4 minutes over a USB cable, and the controller reboots several times.
**Don't unplug it while it's flashing.** If a flash is interrupted, the controller stays in
firmware-update mode. Run the app again and it opens the firmware screen to recover it;
flashing again is safe and skips the pages already written. The bootloader is never
written. See [FIRMWARE.md](FIRMWARE.md) for the protocol.
```

In the keys line, add `` `f` firmware `` before `` `q` quit ``.

- [ ] **Step 5: Run everything once more**

Run:
```bash
go vet ./... && go test ./... && \
XBE2_FW_CAPTURE=.tmp/usbpcap2-flash.pcap XBE2_FW_BIN="$(ls .tmp/DELP_*5_23_6_0*.bin)" \
  go test ./internal/firmware/ -run 'Replay|RealImage' -v
```
Expected: every package `ok`. `TestReplayMatchesCapture` and `TestParseRealImage` both PASS, not SKIP.

- [ ] **Step 6: Commit**

```bash
git add main.go FIRMWARE.md README.md
git commit -m "Wire firmware updates into the app and document the protocol"
```

- [ ] **Step 7: Hardware check (manual, by the user, before a release)**

Run on macOS and on Linux, with `-debug fw.log`, and keep the log:

1. Reflash 5.23.6.0 over 5.23.6.0. Every page should be skipped, which exercises only the resets and re-acquire.
2. Downgrade to 5.21.3242.0. Check the controller works, then upgrade back to 5.23.6.0.
3. Unplug the controller mid-pass-1, plug it back in, and restart the app. It should open recovery, and the reflash should complete.

Report any failure with its `fw.log` before tagging a release.
