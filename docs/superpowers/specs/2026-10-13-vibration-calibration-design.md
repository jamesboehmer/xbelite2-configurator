# Vibration settings, vibration test, and stick/trigger calibration — design

Date: 2026-10-13 · Branch: `calibration` (stacked on `firmware`)

## Goal

From the TUI, without Windows, a user can:

1. set each profile's vibration strength per motor (left/right grip, left/right trigger);
2. feel a test buzz;
3. calibrate each thumbstick and trigger the way Xbox Accessories does.

**Success criteria**

- Every new host frame matches the Xbox Accessories captures byte for byte (golden tests).
- The calibration step detector, fed the real input stream from the capture, signals each
  step within ±0.5 s of the buzz Xbox Accessories gave (replay test).
- Vibration strengths survive a write/read round trip and keep the rest of the page intact.

**Out of scope:** factory-reset of calibration (no command known); per-trigger dead zones or
other settings not in the captures; wireless.

## Protocol facts

Sources: `4vibes.pcapng` (four test buzzes) and
`leftstick_rightstick_lefttrigger_righttrigger.pcapng` (all four calibrations), both from
Xbox Accessories on Windows, plus an on-controller probe (2026-10-13) for motor order.

### Rumble (GIP cmd `0x09`)

`09 00 <seq> 09 | 00 0f <LT> <RT> <Lgrip> <Rgrip> ff 00 eb`

- Own sequence counter (the app's counter for `0x09` is separate from `0x4D`'s), flags `0x00`,
  no ACK.
- Byte order of the four levels confirmed by feel: left trigger, right trigger, left grip,
  right grip. Levels 0–100.
- Stop: the same frame with all four levels `00`.
- The app's test buzz: all four at `0x32` for ~0.5 s, then stop (`4vibes.pcapng`, e.g.
  `09 00 07 09 00 0f 32 32 32 32 ff 00 eb` at 38.71 s, stop at 39.22 s).
- The controller scales host rumble by the **active profile's stored** vibration strengths
  (probe: each strength byte set to 100 with the others 0 left exactly one motor running).

### Vibration strength (mapping page bytes 28–31)

| byte | motor |
|---|---|
| 28 | right grip |
| 29 | left grip |
| 30 | right trigger |
| 31 | left trigger |

Values 0–100 (`0x64`). Stored identically in the profile's standard and shift mapping pages
(observed `5a 5a 50 50` in both on the user's profile 1). Written with the existing profile
write (all four pages, INIT, COMMIT, read-back).

### Calibration (GIP cmd `0x1e`, flags `0x10`, ACKed like `0x4D`)

Targets: `01` left stick, `02` right stick, `04` left trigger, `08` right trigger. Host uses
the same sequence counter as `0x4D`.

| op | host | device reply payload |
|---|---|---|
| `0f` read | `1e 10 s 02 0f <t>` | sticks: `0f 00 <state> <t> <xmin> <xmax> <ymin> <ymax>` (u16 LE each, len `0c`); triggers: `0f 00 <state> <t> <b> <v0> <v1> <v2> <v3>` (len `0d`, `<b>` = `04` meaning unknown) |
| `10` start | `1e 10 s 02 10 <t>` | `10 00 <t>` |

`<state>` is `00` before a user calibration and `10` after one. Captured values (decimal):

| target | before | after |
|---|---|---|
| left stick | 851 3211 803 3270 | 827 3149 581 3528 |
| right stick | 879 3142 865 3313 | 846 3211 852 2971 |
| left trigger | 04 · 1096 1415 1874 3566 | 04 · 957 1329 1924 3395 |
| right trigger | 04 · 1053 1311 1817 3433 | 04 · 980 1340 1933 3607 |

(Stick values are raw ADC extremes; trigger values' meaning is unknown and shown raw.)

### Captured calibration sequence (per target)

1. `0f` read → state `00`, current values.
2. `10` start.
3. The user performs the steps; the app watches the normal input report (cmd `0x20`) and, after
   each completed step, sends a soft buzz: all four motors `0x0a` for ~0.3 s, then stop.
   - Sticks (6 buzzes): 6 full turns in one direction; 6 full turns the other way; then push
     **up, right, down, left**, each followed by release to centre.
   - Triggers (3 buzzes): 12 full pulls with the trigger lock at long throw, 12 at medium,
     12 at short.
4. Strong buzz (`0x1e`, ~0.3 s), `0f` read → state `10`, new values; then `4d 10 s 02 07 00`
   (INIT); then two more strong buzzes ~0.5 s apart.
5. After all targets: `4d 10 s 01 03` (COMMIT).

### Raw input stream (cmd `0x0c`, flags `0x00`)

While Xbox Accessories is connected the controller also streams cmd `0x0c` frames with the
same layout as the input report (sticks, triggers, buttons), apparently pre-remapping input.
Not needed: the step detector uses the normal input report. Documented only.

### Input report fields used (cmd `0x20`, payload offsets)

`0–1` buttons, `2` LT u16 (0–1023), `4` RT u16, `6` LX i16, `8` LY i16, `10` RX i16, `12` RY i16.

## Architecture

```
tui/vibration.go ──► Store (mapping page bytes 28-31) ──► device.Controller.WriteProfile
       └─ test ───► device.Controller.Rumble
tui/calibrate.go ──► internal/calibrate (step detector, pure) ◄── device.Controller.Axes
       └────────────► device.Controller.ReadCalibration / StartCalibration / Reload / Rumble
```

### `internal/protocol`

- `RumbleRequest(seq byte, m Motors) []byte`; `type Motors struct{ LeftTrigger, RightTrigger, LeftGrip, RightGrip byte }`.
- `MappingPage.Vibration() Motors` / `SetVibration(Motors)` over bytes 28–31 (order above).
- `CalibrationRead(seq, t)`, `CalibrationStart(seq, t)`; `ParseCalibration(payload) (Calibration, error)`
  with `Calibration{Target byte; Calibrated bool; Extra byte; Values [4]uint16}`;
  target constants `CalLeftStick=0x01, CalRightStick=0x02, CalLeftTrigger=0x04, CalRightTrigger=0x08`.
- `AxesFromInput(payload) Axes` with `Axes{LX, LY, RX, RY int16; LT, RT uint16}`.

### `internal/device`

`Controller` gains:

```go
Rumble(m protocol.Motors) error            // own 0x09 seq counter, no ACK
ReadCalibration(t byte) (protocol.Calibration, error)
StartCalibration(t byte) error
Reload() error                              // INIT then COMMIT, as the app sends
Axes() protocol.Axes                        // latest input report
```

`USB`: the read loop records axes from each input report and forwards `0x1e` replies to a
reply channel matched by op and target (like `0x4D`); requests share `u.mu`/`nextSeq` with
config requests. `Fake`: stores calibration per target (read returns stored values; start then
read marks `Calibrated` and nudges values), records rumble frames, and lets tests and the
demo set axes.

### `internal/calibrate`

Pure step detector: `New(kind Kind)`; `Feed(t time.Time, a protocol.Axes) []Event`;
`Step() Step`; `Done() bool`. Kinds: left/right stick, left/right trigger.

- Rest: before the start command and before the final read, the stick must have stayed
  within 15% of centre for 1 s (triggers: below 5%). The read returns only min/max, so how
  the controller sets centre is unknown; in the capture both sticks were resting at centre at
  both moments, so we require it.
- Stick turns: accumulate signed angle change only while radius > 85% of full scale (the
  controller records extremes, so turns that miss the rim would shrink the range); a step
  completes at |net turns| ≥ 6 in the first direction (either sign), then at 6 turns back
  in the opposite direction (net returns to ~0).
- Stick pushes, in order up, right, down, left: the axis passes 80% of full scale in that
  direction, then the stick returns within 25% of centre.
- Trigger: a pull is a rise above 60% of full scale followed by a fall below 10%; a step is
  12 pulls; three steps (long, medium, short throw).
- Thresholds are constants, tuned by the replay test.

## TUI

- `v` on the main screen opens a **vibration panel** beside the mapping table (like the colour
  picker) for the selected profile: four rows (left grip, right grip, left trigger, right
  trigger) with 0–100% bars; `↑/↓` motor, `←/→` ±5%, `0` off, `t` test buzz, `esc` close.
  Edits set both mapping pages and mark the profile dirty; `w` writes as today. When the
  selected profile isn't the active one or has unsaved changes, the panel notes "test uses the
  saved settings of the active profile".
- `s` on the main screen opens **calibration** (refused with unsaved changes):
  - menu: the four targets with their current values (read on entry);
  - running: step list with the current step highlighted and a count (turns, pulls), a live
    view (a dot in a circle for sticks, a bar for triggers), trigger-lock reminder between
    trigger rounds; "let go of the stick" while waiting for rest before start and before the
    final read; soft buzz per completed step; on completion strong buzz, read, INIT,
    two strong buzzes, then before/after values and back to the menu;
  - leaving the menu sends COMMIT if any calibration ran;
  - `esc` while running asks to confirm; on confirm the app finishes as the capture does
    (read, INIT) and warns the calibration may be partial and should be re-run;
  - `q`/`ctrl+c` ignored while a calibration runs.
- `-demo`: the fake controller supports both features; in calibration a key press completes
  the current step.

## Testing

- Golden: rumble on/off frames and the `0x1e` read/start frames from the captures; parsing of all
  eight captured replies; vibration byte mapping on a fixture page.
- `internal/calibrate`: synthetic circles (both directions first), pushes in order, pulls,
  noise near thresholds, wrong push order not counted.
- Replay: with `XBE2_CAL_CAPTURE` set, feed the capture's input reports per calibration window
  into the detector; each step event must fall within ±0.5 s of the app's soft buzz (and the
  last step before the strong buzz). Skips when unset.
- TUI via `device.Fake`: vibration edit → dirty → write writes bytes 28–31 in both mapping pages;
  test sends one on and one off frame; calibration end to end (demo step keys); cancel path
  sends the finishing read and INIT; quit ignored while running.

## Docs

`PROTOCOL.md`: rumble, vibration bytes, `0x1e` calibration, `0x0c` raw input. README: `v`, `s`.

## Open items

- **Accepted risk:** what an aborted calibration leaves on the controller, and whether a
  factory reset exists, are unknown (no capture). The cancel path mirrors the normal finish
  and tells the user to re-run; re-calibrating is the only known remedy.
- Meaning of the trigger reply's extra byte (`04`) and of the four trigger values.
- Whether the final `0f` read ends calibration or only reads it (we mirror the app either way).
