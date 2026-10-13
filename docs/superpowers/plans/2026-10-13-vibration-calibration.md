# Vibration and Calibration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add per-profile vibration strength editing, a vibration test buzz, and guided stick/trigger calibration to the TUI.

**Architecture:** Wire formats go in `internal/protocol` (rumble `0x09`, calibration `0x1e`, vibration bytes 28–31, stick/trigger axes from the input report). `internal/device` exposes them on `Controller` (USB + Fake). A new pure package `internal/calibrate` decides when each calibration step is done and when a control is resting, from input samples alone, so it is unit- and replay-testable. The TUI gets a vibration side panel (`v`) and a calibration screen (`s`).

**Tech Stack:** Go 1.27, gousb, Bubble Tea, lipgloss. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-10-13-vibration-calibration-design.md`

## Global Constraints

- Branch `calibration` (stacked on `firmware`). Commit only the files each task names; `xbox-elite2-fw-re.md` stays untracked.
- Commit messages: plain imperative summaries; **no `Co-Authored-By` or any AI attribution** (user's global instruction). Run `gofmt -w` on touched Go files.
- Host frames must match the captures byte for byte: rumble `09 00 s 09 00 0f <LT> <RT> <Lgrip> <Rgrip> ff 00 eb` (own seq counter, flags `0x00`); calibration read `1e 10 s 02 0f <t>`, start `1e 10 s 02 10 <t>` (share the `0x4D` seq counter, ACK-requested).
- Vibration bytes: 28 right grip, 29 left grip, 30 right trigger, 31 left trigger; 0–100; written to both mapping pages of a profile.
- Calibration targets: `01` left stick, `02` right stick, `04` left trigger, `08` right trigger.
- Per target, mirror the capture: read → start → steps (soft buzz `0x0a` 300 ms per completed step) → strong buzz `0x1e` → read → INIT (`4d 10 s 02 07 00`) → two more strong buzzes (300 ms on, 200 ms gaps). After the user leaves the calibration screen having calibrated anything: one COMMIT (`4d 10 s 01 03`).
- Do not send start or the final read until the control has rested (stick within 15% of centre, trigger below 5%) for 1 s.
- Stick turns count only while the stick is past 85% of full scale.
- **Refinement of the spec's interface list:** the spec lists `Reload() error // INIT then COMMIT`, but its captured sequence sends INIT after each target and COMMIT once at the end. This plan therefore exposes `Init()` and `Commit()` separately; the sequence (binding) wins.
- Real-capture tests read env vars (`XBE2_CAL_CAPTURE`) and skip when unset; captures and logs are never committed.
- Match surrounding code: sparse comments, small helpers, plain `t.Fatal`/`t.Errorf` tests.

## Review Focus

1. **The user moves the stick during a "let go" wait** — the rest timer must restart, not carry over. Test: `TestRestRestartsWhenMoved` (Task 2).
2. **The controller stops answering mid-calibration** (unplugged) — start/read failures must land on the menu with the error, not hang or crash. Test: `TestCalibrationStartFailureReturnsToMenu` (Task 6).
3. **A profile page holds an out-of-range vibration byte** (e.g. `0xff` from an older app) — the panel must not panic drawing bars and edits must clamp to 0–100. Test: `TestVibrationOutOfRangeValueIsClamped` (Task 5).
4. **Esc while waiting for rest before the start command** — nothing has been sent yet, so it must return to the menu without sending start or finishing reads. Test: `TestCalibrationEscBeforeStartSendsNothing` (Task 6).
5. **A stale tick arrives after the screen moved on or closed** — must be ignored (no double sampling, no nil dereference). Test: `TestCalibrationStaleTickIgnored` (Task 6).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/protocol/calibration.go` (new) | rumble frame, `Motors`, calibration frames + `ParseCalibration`, `Axes` + `AxesFromInput`, vibration accessors |
| `internal/protocol/protocol.go` | `ReplyMatches` handles cmd `0x1e` |
| `internal/protocol/calibration_test.go` (new) | golden tests from the captures |
| `internal/calibrate/calibrate.go` (new) | `Kind`, step `Detector`, `Rest` |
| `internal/calibrate/calibrate_test.go` (new) | synthetic-input tests |
| `internal/calibrate/replay_test.go` (new) | detector vs. Xbox Accessories buzz times on the real capture |
| `internal/device/device.go` | `Controller` methods; USB rumble/calibration/axes; Fake support |
| `internal/device/fake_test.go` (new) | Fake calibration/rumble behaviour |
| `internal/tui/vibration.go` (new) | vibration panel, `rumble` helper |
| `internal/tui/calibration.go` (new) | calibration screen |
| `internal/tui/tui.go` | keys, routing, `Store` vibration methods, `Run(..., demo)` |
| `internal/tui/vibration_test.go`, `internal/tui/calibration_test.go` (new) | screen tests |
| `main.go` | pass `demo` to `tui.Run` |
| `PROTOCOL.md`, `README.md` | docs |

---

### Task 1: Protocol — rumble, vibration bytes, calibration frames, axes

**Files:**
- Create: `internal/protocol/calibration.go`, `internal/protocol/calibration_test.go`
- Modify: `internal/protocol/protocol.go` (`ReplyMatches`)

**Interfaces:**
- Produces: `CmdRumble = 0x09`, `CmdCalibration = 0x1E`, `OpCalRead = 0x0F`, `OpCalStart = 0x10`, `CalLeftStick/CalRightStick/CalLeftTrigger/CalRightTrigger byte = 0x01/0x02/0x04/0x08`; `type Motors struct{ LeftTrigger, RightTrigger, LeftGrip, RightGrip byte }`; `RumbleRequest(seq byte, m Motors) []byte`; `CalibrationRead(seq, target byte) []byte`; `CalibrationStart(seq, target byte) []byte`; `type Calibration struct{ Target byte; Calibrated bool; Extra byte; Values [4]uint16 }`; `ParseCalibration(payload []byte) (Calibration, error)`; `type Axes struct{ LT, RT uint16; LX, LY, RX, RY int16 }`; `AxesFromInput(payload []byte) Axes`; `(*MappingPage).Vibration() Motors`, `(*MappingPage).SetVibration(Motors)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/protocol/calibration_test.go`:

```go
package protocol

import (
	"encoding/hex"
	"testing"

	"xbelite2-configurator/internal/fixture"
)

func TestRumbleMatchesCapture(t *testing.T) {
	// 4vibes.pcapng: test buzz on at 38.71 s, off at 39.22 s.
	eqHex(t, RumbleRequest(0x07, Motors{0x32, 0x32, 0x32, 0x32}), "09000709000f32323232ff00eb")
	eqHex(t, RumbleRequest(0x08, Motors{}), "09000809000f00000000ff00eb")
	// Field order is left trigger, right trigger, left grip, right grip.
	eqHex(t, RumbleRequest(0x01, Motors{LeftTrigger: 1, RightTrigger: 2, LeftGrip: 3, RightGrip: 4}), "09000109000f01020304ff00eb")
}

func TestCalibrationRequestsMatchCapture(t *testing.T) {
	eqHex(t, CalibrationRead(0x1c, CalLeftStick), "1e101c020f01")
	eqHex(t, CalibrationStart(0x1d, CalLeftStick), "1e101d021001")
	eqHex(t, CalibrationRead(0x34, CalRightTrigger), "1e1034020f08")
}

func TestCalibrationAck(t *testing.T) {
	dev, _ := hex.DecodeString("1e10010c0f00000153038b0c2303c60c")
	eqHex(t, AckFor(dev), "01200109001e000c0000000000")
}

func TestParseCalibrationReplies(t *testing.T) {
	cases := []struct {
		frame      string
		target     byte
		calibrated bool
		extra      byte
		values     [4]uint16
	}{
		{"1e10010c0f00000153038b0c2303c60c", CalLeftStick, false, 0, [4]uint16{851, 3211, 803, 3270}},
		{"1e10030c0f0010013b034d0c4502c80d", CalLeftStick, true, 0, [4]uint16{827, 3149, 581, 3528}},
		{"1e10040c0f0000026f03460c6103f10c", CalRightStick, false, 0, [4]uint16{879, 3142, 865, 3313}},
		{"1e10060c0f0010024e038b0c54039b0c", CalRightStick, true, 0, [4]uint16{846, 3211, 852, 3227}},
		{"1e10070d0f00000404480487055207ee0d", CalLeftTrigger, false, 4, [4]uint16{1096, 1415, 1874, 3566}},
		{"1e10090d0f00100404bd0331058407430d", CalLeftTrigger, true, 4, [4]uint16{957, 1329, 1924, 3395}},
		{"1e100a0d0f000008041d041f051907690d", CalRightTrigger, false, 4, [4]uint16{1053, 1311, 1817, 3433}},
		{"1e100c0d0f00100804d4033c058d07170e", CalRightTrigger, true, 4, [4]uint16{980, 1340, 1933, 3607}},
	}
	for _, c := range cases {
		b, _ := hex.DecodeString(c.frame)
		p, _ := ParsePacket(b)
		got, err := ParseCalibration(p.Payload)
		want := Calibration{Target: c.target, Calibrated: c.calibrated, Extra: c.extra, Values: c.values}
		if err != nil || got != want {
			t.Errorf("%s: got %+v %v, want %+v", c.frame, got, err, want)
		}
	}
	if _, err := ParseCalibration([]byte{0x0f, 0x00, 0x00, 0x01, 0x53}); err == nil {
		t.Error("short reply accepted")
	}
	if _, err := ParseCalibration([]byte{0x10, 0x00, 0x01}); err == nil {
		t.Error("start reply parsed as a read reply")
	}
}

func TestCalibrationReplyMatching(t *testing.T) {
	b, _ := hex.DecodeString("1e10040c0f0000026f03460c6103f10c") // right stick read reply
	read, _ := ParsePacket(b)
	if !ReplyMatches(CalibrationRead(0x05, CalRightStick), read) || ReplyMatches(CalibrationRead(0x05, CalLeftStick), read) {
		t.Error("read reply must match by target")
	}
	b, _ = hex.DecodeString("1e100203100001") // left stick start reply
	start, _ := ParsePacket(b)
	if !ReplyMatches(CalibrationStart(0x09, CalLeftStick), start) || ReplyMatches(CalibrationStart(0x09, CalRightStick), start) {
		t.Error("start reply must match by target")
	}
	if ReplyMatches(InitRequest(0x05), read) {
		t.Error("a 0x1e reply matched a 0x4d request")
	}
}

func TestVibrationBytes(t *testing.T) {
	m, err := ParseMappingPage(fixture.Pages()[0x20])
	if err != nil {
		t.Fatal(err)
	}
	if v := m.Vibration(); v != (Motors{100, 100, 100, 100}) {
		t.Fatalf("fixture vibration %+v", v)
	}
	m.SetVibration(Motors{LeftTrigger: 80, RightTrigger: 81, LeftGrip: 90, RightGrip: 91})
	if got := hex.EncodeToString(m[28:32]); got != "5b5a5150" {
		t.Errorf("bytes 28-31 = %s, want right grip, left grip, right trigger, left trigger", got)
	}
	if v := m.Vibration(); v != (Motors{80, 81, 90, 91}) {
		t.Errorf("round trip %+v", v)
	}
}

func TestAxesFromInput(t *testing.T) {
	p := make([]byte, 18)
	copy(p[2:], []byte{0xff, 0x03, 0x00, 0x02, 0x00, 0x80, 0xff, 0x7f, 0x01, 0x00, 0xfe, 0xff})
	want := Axes{LT: 1023, RT: 512, LX: -32768, LY: 32767, RX: 1, RY: -2}
	if got := AxesFromInput(p); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if AxesFromInput(p[:10]) != (Axes{}) {
		t.Error("short report should give zero axes")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/protocol/`
Expected: FAIL to compile (`undefined: RumbleRequest`).

- [ ] **Step 3: Implement**

Create `internal/protocol/calibration.go`:

```go
package protocol

import (
	"encoding/binary"
	"errors"
)

// Rumble (cmd 0x09) and stick/trigger calibration (cmd 0x1e), decoded from
// captures of Xbox Accessories. See ../../PROTOCOL.md.
//
//	rumble    host: 09 00 s 09 00 0f <LT> <RT> <Lgrip> <Rgrip> ff 00 eb
//	cal read  host: 1e 10 s 02 0f <t>   device: 0f 00 <state> <t> [<extra>] <4 x u16>
//	cal start host: 1e 10 s 02 10 <t>   device: 10 00 <t>
const (
	CmdRumble      = 0x09
	CmdCalibration = 0x1E

	OpCalRead  = 0x0F
	OpCalStart = 0x10

	CalLeftStick    byte = 0x01
	CalRightStick   byte = 0x02
	CalLeftTrigger  byte = 0x04
	CalRightTrigger byte = 0x08

	calCalibrated = 0x10 // reply state after a user calibration
)

// Motors are rumble levels, 0-100, in the order the rumble command uses.
type Motors struct {
	LeftTrigger, RightTrigger, LeftGrip, RightGrip byte
}

// RumbleRequest runs the motors at these levels until the next rumble frame.
func RumbleRequest(seq byte, m Motors) []byte {
	return Frame(CmdRumble, 0x00, seq, []byte{0x00, 0x0f, m.LeftTrigger, m.RightTrigger, m.LeftGrip, m.RightGrip, 0xff, 0x00, 0xeb})
}

func CalibrationRead(seq, target byte) []byte {
	return Frame(CmdCalibration, FlagNeedsAck, seq, []byte{OpCalRead, target})
}

func CalibrationStart(seq, target byte) []byte {
	return Frame(CmdCalibration, FlagNeedsAck, seq, []byte{OpCalStart, target})
}

// Calibration is what a calibration read reports. Sticks carry X min/max and
// Y min/max; triggers carry an extra byte and four values whose meaning is unknown.
type Calibration struct {
	Target     byte
	Calibrated bool // a user calibration is stored
	Extra      byte
	Values     [4]uint16
}

func ParseCalibration(p []byte) (Calibration, error) {
	if len(p) < 4 || p[0] != OpCalRead {
		return Calibration{}, errors.New("not a calibration read reply")
	}
	c := Calibration{Target: p[3], Calibrated: p[2] == calCalibrated}
	v := p[4:]
	if c.Target == CalLeftTrigger || c.Target == CalRightTrigger {
		if len(v) < 1 {
			return Calibration{}, errors.New("short calibration reply")
		}
		c.Extra, v = v[0], v[1:]
	}
	if len(v) < 8 {
		return Calibration{}, errors.New("short calibration reply")
	}
	for i := range c.Values {
		c.Values[i] = binary.LittleEndian.Uint16(v[2*i:])
	}
	return c, nil
}

// Axes are the sticks and triggers from an input report (cmd 0x20).
type Axes struct {
	LT, RT         uint16 // 0-1023
	LX, LY, RX, RY int16  // up and right are positive
}

func AxesFromInput(p []byte) Axes {
	if len(p) < 14 {
		return Axes{}
	}
	le := binary.LittleEndian
	return Axes{
		LT: le.Uint16(p[2:]), RT: le.Uint16(p[4:]),
		LX: int16(le.Uint16(p[6:])), LY: int16(le.Uint16(p[8:])),
		RX: int16(le.Uint16(p[10:])), RY: int16(le.Uint16(p[12:])),
	}
}

// Vibration strength: bytes 28-31 of a mapping page, 0-100, stored the same in
// a profile's standard and shift pages. Order confirmed on a controller.
const vibrationOffset = 28 // right grip, left grip, right trigger, left trigger

func (m *MappingPage) Vibration() Motors {
	return Motors{LeftTrigger: m[vibrationOffset+3], RightTrigger: m[vibrationOffset+2],
		LeftGrip: m[vibrationOffset+1], RightGrip: m[vibrationOffset]}
}

func (m *MappingPage) SetVibration(v Motors) {
	m[vibrationOffset], m[vibrationOffset+1], m[vibrationOffset+2], m[vibrationOffset+3] =
		v.RightGrip, v.LeftGrip, v.RightTrigger, v.LeftTrigger
}
```

Replace `ReplyMatches` in `internal/protocol/protocol.go`:

```go
// ReplyMatches reports whether a device frame answers request req (0x4D or 0x1e).
// Replies carry the controller's own sequence counter, not the request's, so
// they are matched by command, op code, and page or target instead.
func ReplyMatches(req []byte, reply Packet) bool {
	if len(req) < 5 || reply.Cmd != req[0] || len(reply.Payload) == 0 || reply.Payload[0] != req[4] {
		return false
	}
	switch {
	case req[0] == CmdConfig && (req[4] == OpRead || req[4] == OpWrite):
		return len(req) >= 6 && len(reply.Payload) >= 3 && reply.Payload[2] == req[5]
	case req[0] == CmdCalibration && req[4] == OpCalRead:
		return len(req) >= 6 && len(reply.Payload) >= 4 && reply.Payload[3] == req[5]
	case req[0] == CmdCalibration && req[4] == OpCalStart:
		return len(req) >= 6 && len(reply.Payload) >= 3 && reply.Payload[2] == req[5]
	}
	return true
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/protocol/`
Expected: `ok` (existing `TestReplyMatchesIgnoresSeq` still passes).

- [ ] **Step 5: Commit**

```bash
git add internal/protocol/calibration.go internal/protocol/calibration_test.go internal/protocol/protocol.go
git commit -m "Encode rumble, calibration, and vibration strength"
```

---

### Task 2: Calibration step detector

**Files:**
- Create: `internal/calibrate/calibrate.go`, `internal/calibrate/calibrate_test.go`

**Interfaces:**
- Consumes: `protocol.Axes`, `protocol.Cal*` targets (Task 1).
- Produces: `type Kind int` with `LeftStick, RightStick, LeftTrigger, RightTrigger`, `var Kinds []Kind`, `(Kind).Target() byte`, `(Kind).String() string`, `(Kind).Stick() bool`, `KindForTarget(t byte) (Kind, bool)`; `New(k Kind) *Detector` with `Kind() Kind`, `Steps() []string`, `Step() int`, `Done() bool`, `Progress() string`, `Feed(a protocol.Axes) bool`, `Skip()`; `const RestFor = time.Second`; `NewRest(k Kind, d time.Duration) *Rest` with `Feed(t time.Time, a protocol.Axes) bool`.

- [ ] **Step 1: Write the failing tests**

Create `internal/calibrate/calibrate_test.go`:

```go
package calibrate

import (
	"math"
	"testing"
	"time"

	"xbelite2-configurator/internal/protocol"
)

func stick(k Kind, x, y float64) protocol.Axes {
	ix, iy := int16(x*32767), int16(y*32767)
	if k == LeftStick {
		return protocol.Axes{LX: ix, LY: iy}
	}
	return protocol.Axes{RX: ix, RY: iy}
}

// circle feeds turns full turns (negative = clockwise) at radius r, 10° per sample.
func circle(d *Detector, k Kind, turns, r float64) int {
	events := 0
	steps := int(math.Abs(turns) * 36)
	for i := 0; i <= steps; i++ {
		a := math.Copysign(float64(i)*10, turns) * math.Pi / 180
		if d.Feed(stick(k, r*math.Cos(a), r*math.Sin(a))) {
			events++
		}
	}
	return events
}

func push(d *Detector, k Kind, x, y float64) int {
	events := 0
	for _, s := range [][2]float64{{0, 0}, {x * 0.5, y * 0.5}, {x, y}, {x * 0.5, y * 0.5}, {0.05, 0.05}} {
		if d.Feed(stick(k, s[0], s[1])) {
			events++
		}
	}
	return events
}

func TestStickFullSequence(t *testing.T) {
	for _, first := range []float64{6.2, -6.2} {
		d := New(RightStick)
		if n := circle(d, RightStick, first, 1); n != 1 || d.Step() != 1 {
			t.Fatalf("first direction %v: %d events, step %d", first, n, d.Step())
		}
		if n := circle(d, RightStick, -first, 1); n != 1 || d.Step() != 2 {
			t.Fatalf("other direction: %d events, step %d", n, d.Step())
		}
		for i, p := range [][2]float64{{0, 1}, {1, 0}, {0, -1}, {-1, 0}} {
			if n := push(d, RightStick, p[0], p[1]); n != 1 {
				t.Fatalf("push %d: %d events", i, n)
			}
		}
		if !d.Done() || d.Progress() != "" {
			t.Fatalf("not done: step %d", d.Step())
		}
	}
}

func TestStickTurnsAwayFromTheRimDontCount(t *testing.T) {
	d := New(LeftStick)
	if n := circle(d, LeftStick, 7, 0.7); n != 0 || d.Step() != 0 {
		t.Fatalf("turns at 70%% counted: %d events", n)
	}
}

func TestStickSameDirectionTwiceDoesNotFinishStepTwo(t *testing.T) {
	d := New(LeftStick)
	circle(d, LeftStick, 6.2, 1)
	circle(d, LeftStick, 6.2, 1)
	if d.Step() != 1 {
		t.Fatalf("step %d, want 1", d.Step())
	}
}

func TestStickPushesMustBeInOrder(t *testing.T) {
	d := New(LeftStick)
	circle(d, LeftStick, 6.2, 1)
	circle(d, LeftStick, -6.2, 1)
	if n := push(d, LeftStick, 1, 0); n != 0 {
		t.Fatal("a right push counted before up")
	}
	if n := push(d, LeftStick, 0, 1); n != 1 {
		t.Fatal("up push not counted")
	}
}

func pull(d *Detector, k Kind, peak float64) int {
	events := 0
	for _, v := range []float64{0, peak / 2, peak, peak / 2, 0} {
		a := protocol.Axes{LT: uint16(v * 1023)}
		if k == RightTrigger {
			a = protocol.Axes{RT: uint16(v * 1023)}
		}
		if d.Feed(a) {
			events++
		}
	}
	return events
}

func TestTriggerPulls(t *testing.T) {
	d := New(LeftTrigger)
	for round := 0; round < 3; round++ {
		if pull(d, LeftTrigger, 0.5) != 0 {
			t.Fatal("a half pull counted")
		}
		events := 0
		for i := 0; i < 12; i++ {
			events += pull(d, LeftTrigger, 1)
		}
		if events != 1 || d.Step() != round+1 {
			t.Fatalf("round %d: %d events, step %d", round, events, d.Step())
		}
	}
	if !d.Done() {
		t.Fatal("not done after 36 pulls")
	}
}

func TestProgress(t *testing.T) {
	d := New(LeftStick)
	circle(d, LeftStick, 3.2, 1)
	if d.Progress() != "3 of 6 turns" {
		t.Errorf("got %q", d.Progress())
	}
	tr := New(RightTrigger)
	pull(tr, RightTrigger, 1)
	if tr.Progress() != "1 of 12 pulls" {
		t.Errorf("got %q", tr.Progress())
	}
}

func TestSkip(t *testing.T) {
	d := New(LeftTrigger)
	for range d.Steps() {
		d.Skip()
	}
	if !d.Done() {
		t.Fatal("skip didn't finish")
	}
	d.Skip() // no panic past the end
}

func TestRestRestartsWhenMoved(t *testing.T) {
	r := NewRest(LeftStick, time.Second)
	t0 := time.Unix(0, 0)
	if r.Feed(t0, stick(LeftStick, 0.05, 0)) {
		t.Fatal("rested immediately")
	}
	if r.Feed(t0.Add(900*time.Millisecond), stick(LeftStick, 0.5, 0)) {
		t.Fatal("rested while pushed")
	}
	if r.Feed(t0.Add(1500*time.Millisecond), stick(LeftStick, 0, 0.1)) {
		t.Fatal("rest didn't restart after moving")
	}
	if !r.Feed(t0.Add(2600*time.Millisecond), stick(LeftStick, 0, 0.1)) {
		t.Fatal("not rested after a full second")
	}
	tr := NewRest(RightTrigger, 0)
	if tr.Feed(t0, protocol.Axes{RT: 200}) || !tr.Feed(t0, protocol.Axes{RT: 10}) {
		t.Fatal("trigger rest")
	}
}

func TestKindForTarget(t *testing.T) {
	for _, k := range Kinds {
		if got, ok := KindForTarget(k.Target()); !ok || got != k {
			t.Errorf("%s", k)
		}
	}
	if _, ok := KindForTarget(0x03); ok {
		t.Error("0x03 accepted")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/calibrate/`
Expected: FAIL to compile (`undefined: New`).

- [ ] **Step 3: Implement**

Create `internal/calibrate/calibrate.go`:

```go
// Package calibrate follows a user through stick and trigger calibration the
// way Xbox Accessories does, from input reports alone: it says when each step
// is done (so the app can buzz) and when a control is resting. The controller
// measures the calibration itself; this only decides when to move on.
package calibrate

import (
	"fmt"
	"math"
	"time"

	"xbelite2-configurator/internal/protocol"
)

type Kind int

const (
	LeftStick Kind = iota
	RightStick
	LeftTrigger
	RightTrigger
)

var Kinds = []Kind{LeftStick, RightStick, LeftTrigger, RightTrigger}

var targets = [...]byte{protocol.CalLeftStick, protocol.CalRightStick, protocol.CalLeftTrigger, protocol.CalRightTrigger}

func (k Kind) Target() byte   { return targets[k] }
func (k Kind) String() string { return [...]string{"Left stick", "Right stick", "Left trigger", "Right trigger"}[k] }
func (k Kind) Stick() bool    { return k == LeftStick || k == RightStick }

func KindForTarget(t byte) (Kind, bool) {
	for _, k := range Kinds {
		if k.Target() == t {
			return k, true
		}
	}
	return 0, false
}

// Thresholds, as fractions of full scale; checked against the capture by the replay test.
const (
	rimFrac      = 0.85 // turns count only this far out: the controller records extremes
	pushFrac     = 0.80 // a push reaches this far in its direction...
	centerFrac   = 0.25 // ...then comes back within this of centre
	pullFrac     = 0.60 // a pull goes past this...
	releaseFrac  = 0.10 // ...then back below this
	turnsPerStep = 6
	pullsPerStep = 12

	stickRestFrac   = 0.15
	triggerRestFrac = 0.05
	stickFull       = 32767.0
	triggerFull     = 1023.0

	// RestFor is how long a control must stay at rest before start and the final read.
	RestFor = time.Second
)

var stickSteps = []string{
	"Turn the stick around its edge 6 times, either direction",
	"Now turn it around the edge 6 times the other way",
	"Push the stick all the way up, then let go",
	"Push it all the way right, then let go",
	"Push it all the way down, then let go",
	"Push it all the way left, then let go",
}

var triggerSteps = []string{
	"Set the trigger lock to long throw, then pull the trigger fully 12 times",
	"Set the trigger lock to medium throw, then pull fully 12 times",
	"Set the trigger lock to short throw, then pull fully 12 times",
}

var pushDirs = [4][2]float64{{0, 1}, {1, 0}, {0, -1}, {-1, 0}} // up, right, down, left

// Detector tracks the steps for one stick or trigger.
type Detector struct {
	kind     Kind
	step     int
	turns    float64 // net revolutions this step, signed
	prev     float64
	havePrev bool
	dir      float64 // sign of the first direction
	pushed   bool
	pulls    int
	down     bool
}

func New(k Kind) *Detector { return &Detector{kind: k} }

func (d *Detector) Kind() Kind { return d.kind }

func (d *Detector) Steps() []string {
	if d.kind.Stick() {
		return stickSteps
	}
	return triggerSteps
}

func (d *Detector) Step() int  { return d.step }
func (d *Detector) Done() bool { return d.step >= len(d.Steps()) }

// Progress describes the current step's count, e.g. "3 of 6 turns".
func (d *Detector) Progress() string {
	switch {
	case d.Done():
		return ""
	case !d.kind.Stick():
		return fmt.Sprintf("%d of %d pulls", d.pulls, pullsPerStep)
	case d.step == 0:
		return fmt.Sprintf("%d of %d turns", int(math.Abs(d.turns)), turnsPerStep)
	case d.step == 1:
		return fmt.Sprintf("%d of %d turns", int(math.Max(0, -d.turns*d.dir)), turnsPerStep)
	}
	return ""
}

// Skip completes the current step (demo mode).
func (d *Detector) Skip() {
	if !d.Done() {
		d.next()
	}
}

func (d *Detector) next() {
	d.step++
	d.turns, d.havePrev, d.pushed, d.pulls, d.down = 0, false, false, 0, false
}

// Feed takes one input sample and reports whether it completed a step.
func (d *Detector) Feed(a protocol.Axes) bool {
	if d.Done() {
		return false
	}
	before := d.step
	if d.kind.Stick() {
		d.feedStick(stickXY(d.kind, a))
	} else {
		d.feedTrigger(triggerValue(d.kind, a))
	}
	return d.step != before
}

func (d *Detector) feedStick(x, y float64) {
	r := math.Hypot(x, y)
	if d.step < 2 {
		if r < rimFrac {
			d.havePrev = false
			return
		}
		ang := math.Atan2(y, x)
		if d.havePrev {
			delta := math.Mod(ang-d.prev+3*math.Pi, 2*math.Pi) - math.Pi
			d.turns += delta / (2 * math.Pi)
		}
		d.prev, d.havePrev = ang, true
		switch {
		case d.step == 0 && math.Abs(d.turns) >= turnsPerStep:
			d.dir = math.Copysign(1, d.turns)
			d.next()
		case d.step == 1 && d.turns*d.dir <= -turnsPerStep:
			d.next()
		}
		return
	}
	dir := pushDirs[d.step-2]
	if x*dir[0]+y*dir[1] >= pushFrac {
		d.pushed = true
	}
	if d.pushed && r <= centerFrac {
		d.next()
	}
}

func (d *Detector) feedTrigger(v float64) {
	if v >= pullFrac {
		d.down = true
	}
	if d.down && v <= releaseFrac {
		d.down = false
		d.pulls++
		if d.pulls >= pullsPerStep {
			d.next()
		}
	}
}

func stickXY(k Kind, a protocol.Axes) (float64, float64) {
	if k == LeftStick {
		return float64(a.LX) / stickFull, float64(a.LY) / stickFull
	}
	return float64(a.RX) / stickFull, float64(a.RY) / stickFull
}

func triggerValue(k Kind, a protocol.Axes) float64 {
	if k == LeftTrigger {
		return float64(a.LT) / triggerFull
	}
	return float64(a.RT) / triggerFull
}

// Rest reports when a control has stayed at rest for a while.
type Rest struct {
	kind  Kind
	dur   time.Duration
	since time.Time
	in    bool
}

func NewRest(k Kind, d time.Duration) *Rest { return &Rest{kind: k, dur: d} }

func (r *Rest) Feed(t time.Time, a protocol.Axes) bool {
	var resting bool
	if r.kind.Stick() {
		x, y := stickXY(r.kind, a)
		resting = math.Hypot(x, y) <= stickRestFrac
	} else {
		resting = triggerValue(r.kind, a) <= triggerRestFrac
	}
	if !resting {
		r.in = false
		return false
	}
	if !r.in {
		r.in, r.since = true, t
	}
	return t.Sub(r.since) >= r.dur
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/calibrate/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/calibrate/calibrate.go internal/calibrate/calibrate_test.go
git commit -m "Track calibration steps and rest from input reports"
```

---

### Task 3: Replay the calibration capture through the detector

**Files:**
- Create: `internal/calibrate/replay_test.go`
- Modify (only if Step 3 requires tuning): `internal/calibrate/calibrate.go` threshold constants

**Interfaces:**
- Consumes: `New`, `KindForTarget`, `Feed` (Task 2); `protocol.AxesFromInput`, `ParsePacket`, `CmdCalibration`, `OpCalStart`, `OpCalRead`, `CmdRumble`, `CmdInput` (Task 1); `pcap.Read` (existing, reads pcapng).

- [ ] **Step 1: Write the replay test**

Create `internal/calibrate/replay_test.go`:

```go
package calibrate

import (
	"math"
	"os"
	"testing"

	"xbelite2-configurator/internal/pcap"
	"xbelite2-configurator/internal/protocol"
)

// The capture isn't committed. To run:
//
//	XBE2_CAL_CAPTURE=$PWD/.tmp/leftstick_rightstick_lefttrigger_righttrigger.pcapng \
//	  go test ./internal/calibrate/ -run Replay -v
//
// Xbox Accessories gave a soft buzz (all motors 0x0a) after each step it saw
// completed. Feeding the same input reports through the detector must produce
// one step event per buzz, each within half a second of it.
func TestReplayMatchesXboxAccessories(t *testing.T) {
	path := os.Getenv("XBE2_CAL_CAPTURE")
	if path == "" {
		t.Skip("set XBE2_CAL_CAPTURE to the calibration capture to run")
	}
	ts, err := pcap.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	type window struct {
		kind       Kind
		start, end float64
		buzzes     []float64
	}
	var wins []*window
	var cur *window
	for _, tr := range ts {
		d := tr.Data
		if !tr.FromHost || len(d) < 6 {
			continue
		}
		switch {
		case d[0] == protocol.CmdCalibration && d[4] == protocol.OpCalStart:
			k, ok := KindForTarget(d[5])
			if !ok {
				t.Fatalf("unknown target %#x", d[5])
			}
			cur = &window{kind: k, start: tr.Time}
			wins = append(wins, cur)
		case d[0] == protocol.CmdCalibration && d[4] == protocol.OpCalRead && cur != nil && cur.end == 0:
			cur.end = tr.Time
		case d[0] == protocol.CmdRumble && len(d) >= 10 && d[6] == 0x0a && cur != nil && cur.end == 0:
			cur.buzzes = append(cur.buzzes, tr.Time)
		}
	}
	if len(wins) != 4 {
		t.Fatalf("found %d calibrations, want 4", len(wins))
	}
	for _, w := range wins {
		det := New(w.kind)
		var events []float64
		for _, tr := range ts {
			if tr.FromHost || tr.Time < w.start || tr.Time > w.end || len(tr.Data) < 4 || tr.Data[0] != protocol.CmdInput {
				continue
			}
			p, err := protocol.ParsePacket(tr.Data)
			if err != nil {
				continue
			}
			if det.Feed(protocol.AxesFromInput(p.Payload)) {
				events = append(events, tr.Time)
			}
		}
		t.Logf("%s: app buzzed at %.2f, detector stepped at %.2f", w.kind, w.buzzes, events)
		if len(events) != len(w.buzzes) {
			t.Errorf("%s: %d step events, app gave %d buzzes", w.kind, len(events), len(w.buzzes))
			continue
		}
		for i := range events {
			if math.Abs(events[i]-w.buzzes[i]) > 0.5 {
				t.Errorf("%s step %d: detector at %.2f s, app buzzed at %.2f s", w.kind, i+1, events[i], w.buzzes[i])
			}
		}
	}
}
```

- [ ] **Step 2: Run it against the real capture**

Run:
```bash
XBE2_CAL_CAPTURE=$PWD/.tmp/leftstick_rightstick_lefttrigger_righttrigger.pcapng \
  go test ./internal/calibrate/ -run Replay -v -count=1
```
Expected: PASS (not SKIP), with logged times: 6 events for each stick and 3 for each trigger, each within 0.5 s of a buzz.

- [ ] **Step 3: If it fails, tune thresholds (only these constants)**

Read the logged times. Adjust only these threshold constants in `calibrate.go`, within these ranges: `pushFrac` 0.60–0.90, `centerFrac` 0.15–0.35, `pullFrac` 0.40–0.80, `releaseFrac` 0.05–0.25. `rimFrac` stays at 0.85 (the spec fixes it). Re-run Task 2's tests and this replay after each change. If no combination passes — including turn steps that miss at `rimFrac` 0.85 — stop and report the logged times instead of widening the ±0.5 s tolerance, the ranges, or `rimFrac`. Record any tuned values and the reason in the commit message.

- [ ] **Step 4: Run the package tests (replay skips without the env var)**

Run: `go test ./internal/calibrate/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/calibrate/replay_test.go internal/calibrate/calibrate.go
git commit -m "Replay the Xbox Accessories calibration capture through the step detector"
```

---

### Task 4: Device — rumble, calibration, axes on `Controller`

**Files:**
- Modify: `internal/device/device.go`
- Create: `internal/device/fake_test.go`

**Interfaces:**
- Consumes: Task 1 protocol API.
- Produces on `device.Controller` (implemented by `*USB` and `*Fake`):

```go
Rumble(m protocol.Motors) error
ReadCalibration(target byte) (protocol.Calibration, error)
StartCalibration(target byte) error
Init() error   // 0x4D op 07: the controller reloads its settings
Commit() error // 0x4D op 03
Axes() protocol.Axes
```

  Plus on `*Fake`: `Rumbles []protocol.Motors`, `Cal map[byte]protocol.Calibration`, `CalStarts []byte`, `Inits`, `Commits int`, `CalErr error` (returned by `ReadCalibration`/`StartCalibration` when set), `SetAxes(protocol.Axes)`. Fake methods that the TUI calls from background commands lock `f.mu`.

- [ ] **Step 1: Write the failing Fake test**

Create `internal/device/fake_test.go`:

```go
package device

import (
	"errors"
	"testing"

	"xbelite2-configurator/internal/fixture"
	"xbelite2-configurator/internal/protocol"
)

func TestFakeCalibration(t *testing.T) {
	f := NewFake(fixture.Pages())
	before, err := f.ReadCalibration(protocol.CalLeftStick)
	if err != nil || before.Calibrated || before.Values != [4]uint16{851, 3211, 803, 3270} {
		t.Fatalf("default left stick %+v %v", before, err)
	}
	if err := f.StartCalibration(protocol.CalLeftStick); err != nil {
		t.Fatal(err)
	}
	after, _ := f.ReadCalibration(protocol.CalLeftStick)
	if !after.Calibrated || after.Values == before.Values {
		t.Fatalf("after %+v", after)
	}
	if trig, _ := f.ReadCalibration(protocol.CalRightTrigger); trig.Extra != 4 {
		t.Errorf("trigger extra %d", trig.Extra)
	}
	if _, err := f.ReadCalibration(0x03); err == nil {
		t.Error("unknown target accepted")
	}
	f.CalErr = errors.New("unplugged")
	if err := f.StartCalibration(protocol.CalLeftStick); err == nil {
		t.Error("CalErr ignored")
	}
}

func TestFakeRumbleInitCommitAxes(t *testing.T) {
	f := NewFake(fixture.Pages())
	f.Rumble(protocol.Motors{LeftGrip: 50})
	f.Init()
	f.Commit()
	f.SetAxes(protocol.Axes{LX: 100})
	if len(f.Rumbles) != 1 || f.Rumbles[0].LeftGrip != 50 || f.Inits != 1 || f.Commits != 1 || f.Axes().LX != 100 {
		t.Fatalf("%+v", f)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/device/`
Expected: FAIL to compile (`f.ReadCalibration undefined`).

- [ ] **Step 3: Implement**

In `internal/device/device.go`:

Add to the `Controller` interface, after `EndPreview() error`:

```go
	// Rumble runs the four motors at these levels (0-100) until called again.
	Rumble(m protocol.Motors) error
	ReadCalibration(target byte) (protocol.Calibration, error)
	StartCalibration(target byte) error
	// Init makes the controller reload its settings; Commit is what Xbox
	// Accessories sends once calibration is finished.
	Init() error
	Commit() error
	// Axes is the latest stick and trigger state.
	Axes() protocol.Axes
```

Add fields to `USB` (next to `ledMu`/`ledSeq`, and in the `stateMu` group):

```go
	rumbleMu  sync.Mutex
	rumbleSeq byte // 0x09 frames have their own counter in the capture
```
```go
	axes      protocol.Axes
```

In `readLoop`, in `case protocol.CmdInput:` after `u.pressed = p`, add `u.axes = protocol.AxesFromInput(pkt.Payload)`. Change `case protocol.CmdConfig:` to `case protocol.CmdConfig, protocol.CmdCalibration:` so `0x1e` replies reach `u.replies` (they are matched by `ReplyMatches`).

Add methods (after `EndPreview`):

```go
func (u *USB) Rumble(m protocol.Motors) error {
	u.rumbleMu.Lock()
	defer u.rumbleMu.Unlock()
	u.rumbleSeq = u.rumbleSeq%0xFF + 1
	return u.send(protocol.RumbleRequest(u.rumbleSeq, m))
}

func (u *USB) ReadCalibration(target byte) (protocol.Calibration, error) {
	reply, err := u.config(func(s byte) []byte { return protocol.CalibrationRead(s, target) })
	if err != nil {
		return protocol.Calibration{}, err
	}
	return protocol.ParseCalibration(reply.Payload)
}

func (u *USB) StartCalibration(target byte) error {
	reply, err := u.config(func(s byte) []byte { return protocol.CalibrationStart(s, target) })
	if err != nil {
		return err
	}
	if len(reply.Payload) < 2 || reply.Payload[1] != 0x00 {
		return fmt.Errorf("controller refused to start calibration (%x)", reply.Payload)
	}
	return nil
}

func (u *USB) Init() error {
	_, err := u.config(protocol.InitRequest)
	return err
}

func (u *USB) Commit() error { return u.refreshActive() }

func (u *USB) Axes() protocol.Axes {
	u.stateMu.Lock()
	defer u.stateMu.Unlock()
	return u.axes
}
```

Extend `Fake`:

```go
type Fake struct {
	Pages    map[byte][]byte
	Writes   [][2][]byte // (page, data) in write order
	Previews [][3]byte
	Ended    int

	mu        sync.Mutex
	Rumbles   []protocol.Motors
	Cal       map[byte]protocol.Calibration
	CalStarts []byte
	Inits     int
	Commits   int
	CalErr    error // returned by ReadCalibration and StartCalibration when set
	axes      protocol.Axes
}

// defaultCalibration is what the controller in the capture reported before calibrating.
func defaultCalibration() map[byte]protocol.Calibration {
	return map[byte]protocol.Calibration{
		protocol.CalLeftStick:    {Target: protocol.CalLeftStick, Values: [4]uint16{851, 3211, 803, 3270}},
		protocol.CalRightStick:   {Target: protocol.CalRightStick, Values: [4]uint16{879, 3142, 865, 3313}},
		protocol.CalLeftTrigger:  {Target: protocol.CalLeftTrigger, Extra: 4, Values: [4]uint16{1096, 1415, 1874, 3566}},
		protocol.CalRightTrigger: {Target: protocol.CalRightTrigger, Extra: 4, Values: [4]uint16{1053, 1311, 1817, 3433}},
	}
}
```

In `NewFake`, initialise `Cal: defaultCalibration()` in the struct literal. Add methods:

```go
func (f *Fake) Rumble(m protocol.Motors) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Rumbles = append(f.Rumbles, m)
	return nil
}

func (f *Fake) ReadCalibration(target byte) (protocol.Calibration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CalErr != nil {
		return protocol.Calibration{}, f.CalErr
	}
	c, ok := f.Cal[target]
	if !ok {
		return protocol.Calibration{}, fmt.Errorf("no calibration target 0x%02x", target)
	}
	return c, nil
}

// StartCalibration pretends the user widened the range a little.
func (f *Fake) StartCalibration(target byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CalErr != nil {
		return f.CalErr
	}
	c, ok := f.Cal[target]
	if !ok {
		return fmt.Errorf("no calibration target 0x%02x", target)
	}
	f.CalStarts = append(f.CalStarts, target)
	c.Calibrated = true
	for i := range c.Values {
		if i%2 == 0 {
			c.Values[i] -= 10
		} else {
			c.Values[i] += 10
		}
	}
	f.Cal[target] = c
	return nil
}

func (f *Fake) Init() error   { f.mu.Lock(); f.Inits++; f.mu.Unlock(); return nil }
func (f *Fake) Commit() error { f.mu.Lock(); f.Commits++; f.mu.Unlock(); return nil }

func (f *Fake) Axes() protocol.Axes {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.axes
}

func (f *Fake) SetAxes(a protocol.Axes) {
	f.mu.Lock()
	f.axes = a
	f.mu.Unlock()
}
```

- [ ] **Step 4: Run tests and build**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all `ok` (`firmware.FakeBootloader` is a `firmware.Link`, not a `Controller`, so it needs no change).

- [ ] **Step 5: Commit**

```bash
git add internal/device/device.go internal/device/fake_test.go
git commit -m "Expose rumble, calibration, and axes on the controller"
```

---

### Task 5: Vibration panel and test buzz

**Files:**
- Create: `internal/tui/vibration.go`, `internal/tui/vibration_test.go`
- Modify: `internal/tui/tui.go`

**Interfaces:**
- Consumes: `Store` (existing), `Controller.Rumble`, `protocol.Motors`, `MappingPage.Vibration/SetVibration`.
- Produces: `(*Store).Vibration(profile int) protocol.Motors`, `(*Store).SetVibration(profile int, v protocol.Motors)`; `rumble(dev device.Controller, level byte, d time.Duration) tea.Cmd`; `type rumbleErrMsg struct{ err error }`; `Model.vib *vibPanel`.

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/vibration_test.go`:

```go
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
	_ = m.View()                                       // must not panic
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
	want := []protocol.Motors{{50, 50, 50, 50}, {}}
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tui/ -run Vibration`
Expected: FAIL to compile (`m.vib undefined`).

- [ ] **Step 3: Implement**

Create `internal/tui/vibration.go`:

```go
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/protocol"
)

const (
	vibStep    = 5
	vibMax     = 100
	testLevel  = 0x32 // Xbox Accessories' test buzz
	testLength = 500 * time.Millisecond
	vibBarLen  = 20
)

var motorRows = []struct {
	label string
	get   func(protocol.Motors) byte
	set   func(*protocol.Motors, byte)
}{
	{"Left grip", func(m protocol.Motors) byte { return m.LeftGrip }, func(m *protocol.Motors, v byte) { m.LeftGrip = v }},
	{"Right grip", func(m protocol.Motors) byte { return m.RightGrip }, func(m *protocol.Motors, v byte) { m.RightGrip = v }},
	{"Left trigger", func(m protocol.Motors) byte { return m.LeftTrigger }, func(m *protocol.Motors, v byte) { m.LeftTrigger = v }},
	{"Right trigger", func(m protocol.Motors) byte { return m.RightTrigger }, func(m *protocol.Motors, v byte) { m.RightTrigger = v }},
}

type vibPanel struct{ row int }

type rumbleErrMsg struct{ err error }

// rumble runs all four motors at level for d, then stops them, off the UI goroutine.
func rumble(dev device.Controller, level byte, d time.Duration) tea.Cmd {
	return func() tea.Msg {
		if err := dev.Rumble(protocol.Motors{LeftTrigger: level, RightTrigger: level, LeftGrip: level, RightGrip: level}); err != nil {
			return rumbleErrMsg{err}
		}
		time.Sleep(d)
		if err := dev.Rumble(protocol.Motors{}); err != nil {
			return rumbleErrMsg{err}
		}
		return nil
	}
}

func clampVib(v int) byte { return byte(max(0, min(vibMax, v))) }

func (m Model) updateVibration(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	v := m.store.Vibration(m.profile)
	r := motorRows[m.vib.row]
	set := func(val int) {
		r.set(&v, clampVib(val))
		m.store.SetVibration(m.profile, v)
		m.status = fmt.Sprintf("%s vibration %d%% (not written yet, press w)", r.label, r.get(v))
	}
	n := len(motorRows)
	switch msg.String() {
	case "up", "k":
		m.vib.row = (m.vib.row - 1 + n) % n
	case "down", "j":
		m.vib.row = (m.vib.row + 1) % n
	case "left", "h":
		set(int(clampVib(int(r.get(v)))) - vibStep)
	case "right", "l":
		set(int(clampVib(int(r.get(v)))) + vibStep)
	case "0":
		set(0)
	case "t":
		return m, rumble(m.store.dev, testLevel, testLength)
	case "esc", "enter", "v":
		m.vib = nil
	}
	return m, nil
}

func (p *vibPanel) view(title string, v protocol.Motors, note string) string {
	var b strings.Builder
	b.WriteString(boldStyle.Render(title) + "\n")
	for i, r := range motorRows {
		val := int(clampVib(int(r.get(v))))
		n := val * vibBarLen / vibMax
		line := fmt.Sprintf("%-14s %s%s %3d%%", r.label, strings.Repeat("█", n), strings.Repeat("░", vibBarLen-n), val)
		if i == p.row {
			line = selStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	if note != "" {
		b.WriteString(dimStyle.Render(note) + "\n")
	}
	b.WriteString(dimStyle.Render("↑/↓ motor  ←/→ ±5%  0 off  t test  esc done"))
	return boxStyle.Render(b.String())
}
```

In `internal/tui/tui.go`:

Add to `Store` (after `SetColor`):

```go
// Vibration returns a profile's motor strengths (from the standard page).
func (s *Store) Vibration(profile int) protocol.Motors {
	return s.Mapping(profile, protocol.Standard).Vibration()
}

// SetVibration sets the strengths on both pages of a profile, as Xbox Accessories stores them.
func (s *Store) SetVibration(profile int, v protocol.Motors) {
	for _, mode := range []protocol.Mode{protocol.Standard, protocol.Shift} {
		m := s.Mapping(profile, mode)
		before := *m
		m.SetVibration(v)
		if *m != before {
			s.Dirty[profile] = true
		}
	}
}
```

Add `vib *vibPanel` to `Model`. In `Update`, add a case before `case tea.KeyMsg:`:

```go
	case rumbleErrMsg:
		m.status = "Vibration failed: " + msg.err.Error()
```

In the `tea.KeyMsg` branch, after the `m.color != nil` check:

```go
		if m.vib != nil {
			return m.updateVibration(msg)
		}
```

In `updateMain`'s switch:

```go
	case "v":
		m.vib = &vibPanel{}
		m.status = fmt.Sprintf("Vibration strength for profile %d", m.profile)
```

In `View`, extend the side-panel chain (`if m.color != nil { … } else if m.picking { … }`) with:

```go
	} else if m.vib != nil {
		note := ""
		if protocol.ProfileForPage(m.store.dev.ActivePage()) != m.profile || m.store.Dirty[m.profile] {
			note = "t tests the saved settings of the active profile"
		}
		body = lipgloss.JoinHorizontal(lipgloss.Top, body, "  ",
			m.vib.view(fmt.Sprintf("Profile %d vibration", m.profile), m.store.Vibration(m.profile), note))
```

Update `help`:

```go
const help = "↑/↓ select   ←/→ change output   enter pick from list   0 disable\n" +
	"1/2/3 profile   tab standard/shift   c color   v vibration   s calibrate   d reset profile\n" +
	"r reload   w write   f firmware   q quit"
```

(`s` is wired in Task 6; until then it does nothing.)

- [ ] **Step 4: Run tests**

Run: `go vet ./internal/tui/ && go test ./internal/tui/`
Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/vibration.go internal/tui/vibration_test.go internal/tui/tui.go
git commit -m "Add a vibration strength panel and test buzz"
```

---

### Task 6: Calibration screen

**Files:**
- Create: `internal/tui/calibration.go`, `internal/tui/calibration_test.go`
- Modify: `internal/tui/tui.go`, `internal/tui/firmware_test.go` (`drive` stops on `calTickMsg`), `main.go`

**Interfaces:**
- Consumes: `calibrate.*` (Task 2), `Controller.ReadCalibration/StartCalibration/Init/Commit/Axes/Rumble` (Task 4), `rumble` (Task 5).
- Produces: `Model.cal *calScreen`, `Model.demo bool`; `Run(dev device.Controller, env FirmwareEnv, demo bool) error`.

- [ ] **Step 1: Write the failing tests**

In `internal/tui/firmware_test.go`, change `drive`'s tick check to also stop on calibration ticks:

```go
		switch msg.(type) {
		case tickMsg, calTickMsg:
			return m
		}
```

(replacing `if _, ok := msg.(tickMsg); ok { return m }`).

Create `internal/tui/calibration_test.go`:

```go
package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/fixture"
	"xbelite2-configurator/internal/protocol"
)

func calModel(t *testing.T) (Model, *device.Fake) {
	t.Helper()
	dev := device.NewFake(fixture.Pages())
	store, err := NewStore(dev)
	if err != nil {
		t.Fatal(err)
	}
	m := New(store)
	m.demo = true
	return m, dev
}

// tickCal delivers one calibration tick of the current generation.
func tickCal(t *testing.T, m Model) Model {
	t.Helper()
	next, cmd := m.Update(calTickMsg{gen: m.cal.gen})
	return drive(t, next.(Model), cmd)
}

func openCal(t *testing.T, m Model) Model {
	t.Helper()
	m = fwPress(t, m, "s")
	if m.cal == nil || m.cal.stage != calMenu {
		t.Fatalf("calibration menu not open: %+v", m.cal)
	}
	m.cal.restFor = 0
	return m
}

func countLevel(rs []protocol.Motors, level byte) int {
	n := 0
	for _, r := range rs {
		if r.LeftGrip == level {
			n++
		}
	}
	return n
}

func TestCalibrationEndToEnd(t *testing.T) {
	m, dev := calModel(t)
	m = openCal(t, m)
	if !strings.Contains(m.View(), "X 851–3211") {
		t.Fatalf("menu:\n%s", m.View())
	}
	m = fwPress(t, m, "enter") // left stick, waiting for rest
	if m.cal.stage != calRestStart {
		t.Fatalf("stage %d", m.cal.stage)
	}
	m = tickCal(t, m) // resting -> start -> running
	if m.cal.stage != calRunning || len(dev.CalStarts) != 1 || dev.CalStarts[0] != protocol.CalLeftStick {
		t.Fatalf("stage %d, starts %v", m.cal.stage, dev.CalStarts)
	}
	for i := 0; i < 6; i++ {
		m = fwPress(t, m, "n") // demo: complete a step
	}
	if m.cal.stage != calRestEnd {
		t.Fatalf("stage %d after all steps", m.cal.stage)
	}
	m = tickCal(t, m) // resting -> finish
	if m.cal.stage != calResult || dev.Inits != 1 || !m.cal.after.Calibrated {
		t.Fatalf("stage %d, inits %d, after %+v", m.cal.stage, dev.Inits, m.cal.after)
	}
	if countLevel(dev.Rumbles, 0x0a) != 6 || countLevel(dev.Rumbles, 0x1e) != 3 {
		t.Errorf("buzzes: %v", dev.Rumbles)
	}
	m = fwPress(t, m, "enter", "esc") // back to menu, leave
	if m.cal != nil || dev.Commits != 1 || !strings.Contains(m.status, "Calibration finished") {
		t.Fatalf("cal %+v commits %d status %q", m.cal, dev.Commits, m.status)
	}
}

func TestCalibrationCancelFinishesLikeTheApp(t *testing.T) {
	m, dev := calModel(t)
	m = openCal(t, m)
	m = tickCal(t, fwPress(t, m, "enter"))
	m = fwPress(t, m, "esc")
	if m.cal.stage != calConfirm {
		t.Fatalf("stage %d", m.cal.stage)
	}
	m = fwPress(t, m, "y")
	if m.cal.stage != calResult || !m.cal.cancelled || dev.Inits != 1 || countLevel(dev.Rumbles, 0x1e) != 0 {
		t.Fatalf("stage %d cancelled %v inits %d rumbles %v", m.cal.stage, m.cal.cancelled, dev.Inits, dev.Rumbles)
	}
	if !strings.Contains(m.View(), "run it again") {
		t.Fatalf("no re-run advice:\n%s", m.View())
	}
}

func TestCalibrationEscBeforeStartSendsNothing(t *testing.T) {
	m, dev := calModel(t)
	m = openCal(t, m)
	m.cal.restFor = 1 << 62 // never rests
	m = fwPress(t, m, "enter", "esc")
	if m.cal.stage != calMenu || len(dev.CalStarts) != 0 || dev.Inits != 0 {
		t.Fatalf("stage %d starts %v inits %d", m.cal.stage, dev.CalStarts, dev.Inits)
	}
}

func TestCalibrationQuitIgnoredWhileRunning(t *testing.T) {
	m, _ := calModel(t)
	m = tickCal(t, fwPress(t, openCal(t, m), "enter"))
	for _, k := range []string{"ctrl+c", "q"} {
		if _, cmd := m.Update(fwKey(k)); cmd != nil {
			if _, quit := cmd().(tea.QuitMsg); quit {
				t.Errorf("%s quit mid-calibration", k)
			}
		}
	}
}

func TestCalibrationStartFailureReturnsToMenu(t *testing.T) {
	m, dev := calModel(t)
	m = openCal(t, m)
	dev.CalErr = errors.New("unplugged")
	m = tickCal(t, fwPress(t, m, "enter"))
	if m.cal.stage != calMenu || !strings.Contains(m.View(), "unplugged") {
		t.Fatalf("stage %d:\n%s", m.cal.stage, m.View())
	}
}

func TestCalibrationRefusedWithUnsavedChanges(t *testing.T) {
	m, _ := calModel(t)
	m.store.Set(1, protocol.Standard, 1, 0x0C)
	m = fwPress(t, m, "s")
	if m.cal != nil || !strings.Contains(m.status, "unsaved") {
		t.Fatalf("cal %+v status %q", m.cal, m.status)
	}
}

func TestCalibrationStaleTickIgnored(t *testing.T) {
	m, dev := calModel(t)
	m = openCal(t, m)
	m = fwPress(t, m, "enter")
	stale := m.cal.gen - 1
	next, cmd := m.Update(calTickMsg{gen: stale})
	if cmd != nil || len(dev.CalStarts) != 0 {
		t.Fatal("stale tick acted")
	}
	m = fwPress(t, next.(Model), "esc", "esc") // leave
	if _, cmd := m.Update(calTickMsg{gen: 99}); cmd != nil {
		t.Fatal("tick after closing acted")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/tui/ -run Calibration`
Expected: FAIL to compile (`undefined: calTickMsg`).

- [ ] **Step 3: Implement**

Create `internal/tui/calibration.go`:

```go
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"xbelite2-configurator/internal/calibrate"
	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/protocol"
)

const (
	softBuzz     = 0x0a // after each step, as Xbox Accessories does
	strongBuzz   = 0x1e // at the end
	buzzFor      = 300 * time.Millisecond
	buzzGap      = 200 * time.Millisecond
	calTickEvery = 20 * time.Millisecond
)

type calStage int

const (
	calBusy calStage = iota
	calMenu
	calRestStart
	calRunning
	calRestEnd
	calConfirm
	calResult
)

type calScreen struct {
	stage       calStage
	sel         int
	cals        [4]protocol.Calibration
	known       bool
	det         *calibrate.Detector
	rest        *calibrate.Rest
	restFor     time.Duration
	before      protocol.Calibration
	after       protocol.Calibration
	cancelled   bool
	ran         bool
	gen         int
	confirmFrom calStage
	busyMsg     string
	err         string
}

type calReadMsg struct {
	cals [4]protocol.Calibration
	err  error
}
type calStartedMsg struct{ err error }
type calDoneMsg struct {
	after     protocol.Calibration
	cancelled bool
	err       error
}
type calClosedMsg struct{ err error }
type calTickMsg struct{ gen int }

func calTick(gen int) tea.Cmd {
	return tea.Tick(calTickEvery, func(time.Time) tea.Msg { return calTickMsg{gen} })
}

// newTicks starts a fresh tick chain; ticks from older chains are ignored.
func (c *calScreen) newTicks() tea.Cmd {
	c.gen++
	return calTick(c.gen)
}

// locked reports whether quitting now could leave a calibration half done.
func (c *calScreen) locked() bool {
	switch c.stage {
	case calRunning, calRestEnd, calConfirm, calBusy:
		return true
	}
	return false
}

func (m Model) openCalibration() (tea.Model, tea.Cmd) {
	if len(m.store.Dirty) > 0 {
		m.status = "Write or reload your unsaved changes before calibrating."
		return m, nil
	}
	m.cal = &calScreen{stage: calBusy, busyMsg: "Reading calibration...", restFor: calibrate.RestFor}
	m.status = ""
	dev := m.store.dev
	return m, func() tea.Msg {
		var msg calReadMsg
		for i, k := range calibrate.Kinds {
			if msg.cals[i], msg.err = dev.ReadCalibration(k.Target()); msg.err != nil {
				break
			}
		}
		return msg
	}
}

// finishCalibration ends a calibration the way the capture does: strong buzz,
// read, INIT, then two more strong buzzes. A cancelled one only reads and INITs.
func finishCalibration(dev device.Controller, target byte, cancelled bool) tea.Cmd {
	return func() tea.Msg {
		on := func() { dev.Rumble(protocol.Motors{LeftTrigger: strongBuzz, RightTrigger: strongBuzz, LeftGrip: strongBuzz, RightGrip: strongBuzz}) }
		off := func() { dev.Rumble(protocol.Motors{}) }
		if !cancelled {
			on()
		}
		after, err := dev.ReadCalibration(target)
		if err == nil {
			err = dev.Init()
		}
		if !cancelled {
			time.Sleep(buzzFor)
			off()
			for i := 0; i < 2; i++ {
				time.Sleep(buzzGap)
				on()
				time.Sleep(buzzFor)
				off()
			}
		}
		return calDoneMsg{after: after, cancelled: cancelled, err: err}
	}
}

func (m Model) updateCalibrationMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	c := m.cal
	switch msg := msg.(type) {
	case calReadMsg:
		c.stage = calMenu
		if msg.err != nil {
			c.err = "Couldn't read calibration: " + msg.err.Error()
		} else {
			c.cals, c.known = msg.cals, true
		}
	case calTickMsg:
		if msg.gen != c.gen {
			return m, nil
		}
		return m.calTickUpdate()
	case calStartedMsg:
		if msg.err != nil {
			c.stage, c.err = calMenu, "Couldn't start calibration: "+msg.err.Error()
			return m, nil
		}
		c.stage = calRunning
		return m, c.newTicks()
	case calDoneMsg:
		c.stage, c.ran, c.after, c.cancelled = calResult, true, msg.after, msg.cancelled
		if msg.err != nil {
			c.err = msg.err.Error()
		} else {
			c.cals[c.sel] = msg.after
		}
	case calClosedMsg:
		m.cal = nil
		if msg.err != nil {
			m.status = "Calibration finished, but the final commit failed: " + msg.err.Error()
		} else {
			m.status = "Calibration finished."
		}
	}
	return m, nil
}

func (m Model) calTickUpdate() (tea.Model, tea.Cmd) {
	c := m.cal
	a := m.store.dev.Axes()
	switch c.stage {
	case calRestStart:
		if c.rest.Feed(time.Now(), a) {
			c.stage, c.busyMsg = calBusy, "Starting calibration..."
			dev, target := m.store.dev, c.det.Kind().Target()
			return m, func() tea.Msg { return calStartedMsg{dev.StartCalibration(target)} }
		}
	case calRunning:
		var buzz tea.Cmd
		if c.det.Feed(a) {
			buzz = rumble(m.store.dev, softBuzz, buzzFor)
		}
		if c.det.Done() {
			c.stage, c.rest = calRestEnd, calibrate.NewRest(c.det.Kind(), c.restFor)
		}
		return m, tea.Batch(buzz, calTick(c.gen))
	case calRestEnd:
		if c.rest.Feed(time.Now(), a) {
			c.stage, c.busyMsg = calBusy, "Saving calibration..."
			return m, finishCalibration(m.store.dev, c.det.Kind().Target(), false)
		}
	default:
		return m, nil
	}
	return m, calTick(c.gen)
}

func (m Model) updateCalibrationKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	c := m.cal
	k := msg.String()
	switch c.stage {
	case calMenu:
		switch k {
		case "up", "k":
			c.sel = (c.sel + len(calibrate.Kinds) - 1) % len(calibrate.Kinds)
		case "down", "j":
			c.sel = (c.sel + 1) % len(calibrate.Kinds)
		case "enter":
			if !c.known {
				return m, nil
			}
			kind := calibrate.Kinds[c.sel]
			c.det, c.rest = calibrate.New(kind), calibrate.NewRest(kind, c.restFor)
			c.before, c.err, c.cancelled, c.stage = c.cals[c.sel], "", false, calRestStart
			return m, c.newTicks()
		case "esc", "q":
			if !c.ran {
				m.cal = nil
				return m, nil
			}
			c.stage, c.busyMsg = calBusy, "Saving..."
			dev := m.store.dev
			return m, func() tea.Msg { return calClosedMsg{dev.Commit()} }
		}
	case calRestStart:
		if k == "esc" { // nothing sent yet
			c.stage = calMenu
			c.gen++
		}
	case calRunning, calRestEnd:
		switch k {
		case "esc":
			c.confirmFrom, c.stage = c.stage, calConfirm
			c.gen++
		case "n":
			if m.demo && c.stage == calRunning {
				c.det.Skip()
				if c.det.Done() {
					c.stage, c.rest = calRestEnd, calibrate.NewRest(c.det.Kind(), c.restFor)
				}
				return m, rumble(m.store.dev, softBuzz, buzzFor)
			}
		}
	case calConfirm:
		switch k {
		case "y":
			c.stage, c.busyMsg = calBusy, "Stopping calibration..."
			return m, finishCalibration(m.store.dev, c.det.Kind().Target(), true)
		case "n", "esc":
			c.stage = c.confirmFrom
			return m, c.newTicks()
		}
	case calResult:
		if k == "enter" || k == "esc" {
			c.stage = calMenu
		}
	}
	return m, nil
}

func calValues(k calibrate.Kind, c protocol.Calibration) string {
	state := "default"
	if c.Calibrated {
		state = "user calibrated"
	}
	v := c.Values
	if k.Stick() {
		return fmt.Sprintf("X %d–%d  Y %d–%d  (%s)", v[0], v[1], v[2], v[3], state)
	}
	return fmt.Sprintf("%d %d %d %d  (%s)", v[0], v[1], v[2], v[3], state)
}

func liveView(k calibrate.Kind, a protocol.Axes) string {
	if !k.Stick() {
		v := a.LT
		if k == calibrate.RightTrigger {
			v = a.RT
		}
		n := min(int(v), 1023) * 30 / 1023
		return fmt.Sprintf(" [%s%s] %4d\n", strings.Repeat("█", n), strings.Repeat("░", 30-n), v)
	}
	x, y := a.LX, a.LY
	if k == calibrate.RightStick {
		x, y = a.RX, a.RY
	}
	const size = 11
	col := (int(x) + 32768) * (size - 1) / 65535
	row := (32767 - int(y)) * (size - 1) / 65535
	var b strings.Builder
	for r := 0; r < size; r++ {
		b.WriteString(" ")
		for c := 0; c < size; c++ {
			if r == row && c == col {
				b.WriteString("● ")
			} else {
				b.WriteString("· ")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (c *calScreen) view(a protocol.Axes, demo bool, status string) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Stick and trigger calibration") + "\n\n")
	switch c.stage {
	case calBusy:
		b.WriteString(" " + c.busyMsg + "\n")
	case calMenu:
		for i, k := range calibrate.Kinds {
			vals := "unknown"
			if c.known {
				vals = calValues(k, c.cals[i])
			}
			line := fmt.Sprintf("%-14s %s", k, vals)
			if i == c.sel {
				line = selStyle.Render(line)
			}
			b.WriteString("   " + line + "\n")
		}
		if c.err != "" {
			b.WriteString("\n " + boldStyle.Render(c.err) + "\n")
		}
	case calRestStart, calRunning, calRestEnd, calConfirm:
		kind := c.det.Kind()
		what := "stick"
		if !kind.Stick() {
			what = "trigger"
		}
		b.WriteString(" " + boldStyle.Render(kind.String()) + "\n\n")
		for i, s := range c.det.Steps() {
			mark := "  "
			switch {
			case i < c.det.Step():
				mark = "✓ "
			case i == c.det.Step() && c.stage == calRunning:
				mark = "› "
			}
			b.WriteString(" " + mark + s + "\n")
		}
		b.WriteString("\n")
		switch c.stage {
		case calRestStart:
			b.WriteString(boldStyle.Render(fmt.Sprintf(" Let go of the %s and leave it at rest...", what)) + "\n")
		case calRunning:
			b.WriteString(" " + c.det.Progress() + "\n")
			if demo {
				b.WriteString(dimStyle.Render(" (demo: press n to complete this step)") + "\n")
			}
		case calRestEnd:
			b.WriteString(boldStyle.Render(fmt.Sprintf(" Done. Let go of the %s...", what)) + "\n")
		case calConfirm:
			b.WriteString(boldStyle.Render(" Stop calibrating? The result may be partial; run it again afterwards. y stop  n keep going") + "\n")
		}
		b.WriteString("\n" + liveView(kind, a))
	case calResult:
		kind := calibrate.Kinds[c.sel]
		b.WriteString(" " + boldStyle.Render(kind.String()) + "\n\n")
		b.WriteString(" Before: " + calValues(kind, c.before) + "\n")
		b.WriteString(" After:  " + calValues(kind, c.after) + "\n")
		if c.err != "" {
			b.WriteString("\n " + boldStyle.Render("Error: "+c.err) + "\n")
		}
		if c.cancelled {
			b.WriteString("\n " + boldStyle.Render("Calibration was stopped early and may be partial; run it again.") + "\n")
		}
	}
	b.WriteString("\n " + boldStyle.Render(status) + "\n\n")
	b.WriteString(dimStyle.Render(c.help()))
	return b.String()
}

func (c *calScreen) help() string {
	switch c.stage {
	case calMenu:
		return "↑/↓ select   enter calibrate   esc done"
	case calRestStart:
		return "esc back"
	case calRunning, calRestEnd:
		return "esc stop"
	case calResult:
		return "enter back"
	}
	return ""
}
```

In `internal/tui/tui.go`:
- Add `cal *calScreen` and `demo bool` to `Model`.
- In `Update`, add before `case tea.KeyMsg:`:

```go
	case calReadMsg, calStartedMsg, calDoneMsg, calClosedMsg, calTickMsg:
		if m.cal != nil {
			return m.updateCalibrationMsg(msg)
		}
```

- In the `tea.KeyMsg` branch, after the firmware-flashing guard and before the `ctrl+c` check:

```go
		if m.cal != nil && m.cal.locked() && (msg.String() == "ctrl+c" || msg.String() == "q") {
			return m, nil // don't leave a calibration half done
		}
```

  and after the `m.fw != nil` dispatch:

```go
		if m.cal != nil {
			return m.updateCalibrationKey(msg)
		}
```

- In `updateMain`'s switch: `case "s": return m.openCalibration()`.
- At the top of `View`, after the firmware check:

```go
	if m.cal != nil {
		return m.cal.view(m.store.dev.Axes(), m.demo, m.status)
	}
```

- Change `Run` to `func Run(dev device.Controller, env FirmwareEnv, demo bool) error` and set `m.demo = demo` after `m.fwEnv = env`.

In `main.go`: `return tui.Run(dev, demoFirmware(dev), true)` and `return tui.Run(u, env, false)`.

- [ ] **Step 4: Run tests**

Run: `go build ./... && go vet ./... && go test ./... && go test -race ./internal/tui/`
Expected: all `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/calibration.go internal/tui/calibration_test.go internal/tui/tui.go internal/tui/firmware_test.go main.go
git commit -m "Add guided stick and trigger calibration"
```

---

### Task 7: Docs and hardware check

**Files:**
- Modify: `PROTOCOL.md`, `README.md`

- [ ] **Step 1: Add to `PROTOCOL.md`** (after the "Profile color preview" section):

````markdown
## Vibration strength (mapping page bytes 28-31)

| byte | motor |
|---|---|
| 28 | right grip |
| 29 | left grip |
| 30 | right trigger |
| 31 | left trigger |

0-100, stored the same in a profile's standard and shift pages. The controller scales
rumble from the host by the active profile's saved strengths. Byte order confirmed on a
controller by setting one byte to 100 and the others to 0.

## Rumble (cmd 0x09)

`09 00 s 09 00 0f <LT> <RT> <left grip> <right grip> ff 00 eb`, own sequence counter, no ACK.
Levels 0-100; the same frame with zeros stops. Xbox Accessories' test buzz is all four at
`0x32` for about half a second.

## Calibration (cmd 0x1e)

Same sequence counter as 0x4D, ACK requested. Targets: `01` left stick, `02` right stick,
`04` left trigger, `08` right trigger.

| op | host | device |
|---|---|---|
| `0f` read | `1e 10 s 02 0f <t>` | sticks `0f 00 <state> <t> <xmin> <xmax> <ymin> <ymax>`; triggers `0f 00 <state> <t> <b> <v0> <v1> <v2> <v3>` (u16 LE) |
| `10` start | `1e 10 s 02 10 <t>` | `10 00 <t>` |

`<state>` is `00` until a user calibration, then `10`. The controller measures the
calibration itself. Xbox Accessories: read, start, guide the user while watching the input
report (sticks: 6 turns each way, then push up, right, down, left; triggers: 12 full pulls
at long, medium, and short throw), soft buzz (`0x0a`) after each step, then strong buzz
(`0x1e`), read, INIT, two more strong buzzes. One COMMIT when done. No factory-reset command
is known.

## Raw input (cmd 0x0c)

While Xbox Accessories is connected the controller also streams cmd `0x0c` frames (flags
`0x00`) laid out like the input report, apparently before profile remapping.
````

- [ ] **Step 2: Update `README.md`**

In the keys line, add `` `v` vibration `` and `` `s` calibrate `` before `` `d` reset ``. After the Firmware updates section add:

```markdown
## Vibration and calibration

Press `v` to set the selected profile's vibration strength for each motor (left and right
grip, left and right trigger); `t` in that panel buzzes the controller. Strengths are saved
with `w` like the other profile settings.

Press `s` to calibrate a thumbstick or trigger. Follow the steps on screen; the controller
buzzes after each one. Let go of the stick or trigger when asked: the app waits until it is
at rest before starting and before saving. If you stop part way, run the calibration again.
```

- [ ] **Step 3: Verify and commit**

Run: `go test ./...`
Expected: all `ok`.

```bash
git add PROTOCOL.md README.md
git commit -m "Document rumble, vibration strength, and calibration"
```

- [ ] **Step 4: Hardware check (manual, by the user)**

`sudo ./xbelite2-configurator -debug cal.log`:
1. `v`, change a motor, `t`, `w`; re-open and confirm it stuck; set it back.
2. `s`, calibrate the left stick end to end; check the after values are plausible; repeat for a trigger.
3. Start a calibration and stop it with esc → y; re-run it fully.
