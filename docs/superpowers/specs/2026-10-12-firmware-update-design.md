# Firmware updates — design

Date: 2026-10-12 · Branch: `firmware`

## Goal

A user on macOS or Linux can update, downgrade, or recover the firmware of a wired
Xbox Elite Series 2 (`045e:0b00`) from the configurator's TUI, without Windows.

**Success criteria**

- The host's write path is byte-exact with the Windows Xbox Accessories capture
  (`usbpcap2-flash.pcap`), proven by a replay test, before the flasher touches hardware.
- Only official, signed images are offered; downloads are SHA-256-verified.
- The bootloader region is never written, so a failed or interrupted flash is always
  recoverable by re-running.
- A controller found in bootloader mode at startup can be recovered from the app.

**Out of scope:** Bluetooth or wireless-adapter flashing; writing region type 2; modified
or unsigned images; a CLI flashing interface.

## Protocol facts (decoded from the capture)

Everything here was verified against the capture and the 5.23.6.0 / 5.21.3242.0 `.bin`
files. Items marked *(inferred)* are interpretation, not needed for byte-exact behavior.

### `.bin` container (`rdhm`)

Little-endian. Header `0x100` bytes, not flashed.

| off | size | field |
|---|---|---|
| `0x00` | 4 | magic `rdhm` |
| `0x04` | 4 | header version `1` |
| `0x08` | 2 | VID `0x045e` |
| `0x0a` | 2 | PID `0x0b00` |
| `0x0c` | 2 | version major |
| `0x0e` | 2 | version minor |
| `0x10` | 4 | version build (rev = 0) |
| `0x1c` | 4 | region count (`3`) |

Region descriptors, `0x40` bytes each, at `0x40`, `0x80`, `0xC0`:

| off | size | field |
|---|---|---|
| `0x00` | 4 | type |
| `0x08` | 4 | file offset of region data |
| `0x0c` | 4 | region size |
| `0x10` | 4 | load address |
| `0x14` | 4 | unknown (`0x7de80` / `0x100`) — not sent |
| `0x18` | 2 | version major |
| `0x1a` | 2 | version minor |
| `0x1c` | 4 | version build |

| type | file off | size | load addr | contents | written |
|---|---|---|---|---|---|
| 4 | `0x100` | `0x7E000` | `0x10027000` | NV/calibration/"patch image" programmer; identical code across releases (only its 256-byte signature differs) *(role inferred)* | pass 1 |
| 3 | `0x7E100` | `0x7E000` | `0x10027000` | main controller firmware (Bluetooth stack etc.) | pass 2 |
| 2 | `0xFC100` | `0x22000` | `0x10005000` | bootloader (GIP + device auth stack) *(inferred)*; differs between releases | **never** |

There is **one** app slot at `0x10027000`: at the start of pass 2 the controller's page
hashes equal region 4's pages (all but the last page), so pass 2 overwrites what pass 1 wrote.
Filename `_h_<base64url>` segment = SHA-256 of the whole file.

### Transport and GIP

Interface 0, interrupt EP `0x02` OUT / `0x82` IN. Frames `[cmd][flags][seq][len…][payload]`.
Host seq is one counter shared by all host commands (skips 0, as `nextSeq` already does).
All firmware messages are cmd `0x0c` with flags `0x30` (system | ack-request).

**ACK.** Device frames with flag `0x10` are ACKed as
`01 20 <seq> 09 00 <cmd> <flags&0x20> <len LE16> 00 00 00 00`. The Windows app echoes
`0x20` for system frames; current `AckFor` sends `00` — fix it.
Device ACKs of host data: `01 20 <seq> 09 00 0c 20 <received LE16> 00 00 <window LE16>`,
where `window` is how many bytes the host may send next (`512`, or the remainder, or `0`).

### Commands (host `0c 30 s <len> <payload>`; device replies `0c 30 <dseq> <len> <payload>`)

| op | host payload | device reply payload |
|---|---|---|
| `00` info | `00` | `00 00 02 00 5e04 000b <page 0x1000 LE32> <0x1000 LE32> <0x249f0 LE32> <regions 3 LE32> …zeros` (60 bytes) |
| `01` region query | `01 <type LE32>` | `01 <st> <type LE32> <maj u16> <min u16> <build LE32> 453a251a <addr LE32> <size LE32>` |
| `02` region start | `02 <type LE32> <addr LE32> <size LE32> <maj u16> <min u16> <build LE32>` (from descriptor) | `02 <st>`: `00` go, `03` reset to bootloader first |
| `03` page hash | `03 <addr LE32> <0x1000 LE32>` | `03 <st 00> <SHA-1 of the 4 KB page at addr, 20 bytes>` |
| `04` page write | chunked message, see below | `04 <st 00>` after the terminator |
| `05` region finish | `05` | `05 <st>`: `03` = reset required |

In bootloader mode the region-3 query reports version `0.0.0` after pass 1, and the GIP
announce (cmd `0x02`) reports firmware `0.0.0.0`; PID stays `0x0b00`.

Other host frames used:

| frame | meaning |
|---|---|
| `05 20 s 01 07` | GIP power: reset (controller re-enumerates) |
| `05 20 s 01 00` | GIP power: on (sent after each re-acquire) |
| `0a 20 s 03 00 01 14` | LED on, brightness `0x14` (sent after each re-acquire) |
| `05 21 s 01 01` | sent once before op `00` in normal mode *(meaning unknown; replicated)* |

### Page write (op `04`) — GIP chunked transfer

Message = `04 00 00 00 <addr LE32>` + 4096 data bytes = 4104 bytes, sent with one seq:

1. First chunk, flag `0xf0`: `0c f0 s <58:1B varint> <4104:2B varint>` + bytes `0..58`.
   Wait for ACK.
2. Then windows of up to 512 bytes. Each window is chunks of ≤ 58 bytes; the window's last
   chunk has flag `0xb0` (ack-request), others `0xa0`. Chunk header after `0c <flag> s` is
   `<len varint><offset varint>` padded to 3 bytes total: if `offset < 128`,
   len is 2 bytes (`ba 00`) and offset 1 byte; else len 1 byte and offset 2 bytes.
   Wait for each window's ACK.
3. Terminator: `0c a0 s 00 <4104:2B varint>` (`88 20`).
4. Device replies op `04` status `00`.

Varints are 7-bit little-endian groups, continuation bit `0x80`, padded with `0x80`
groups to the given width. This rule re-encodes all 226 page writes in the capture
byte-exact.

### Captured sequence

| USB addr | mode | host does |
|---|---|---|
| 15 | normal | `05 21 01`; op `00`; op `01` ×5 (types 4,3,2,2,4); op `02` type 4 → `03`; reset |
| 16 | bootloader | power on; LED; op `01` (4); op `02` (4) → `00`; per page: op `03`, op `04` if hash differs (113 of 126 written; the 13 skipped are all `0xFF`); op `05` → `03`; reset |
| 17 | bootloader | power on; LED; op `01` (4), (3); op `02` (3) → `00`; pages as above; op `05` → `03`; reset |
| 18 | normal 5.23.6 | power on; LED; op `01` (3) → 5.23.6; reset |
| 19 | normal | ordinary startup |

Re-enumeration gaps: 3.3 s, 5.3 s, 2.5 s, 4 s.

## Architecture

```
TUI firmware screen ──► firmware.Catalog ──► manifest + built-in list ──► cache (sha-verified)
        │
        ▼
firmware.Flash(link, image, progress) ──► device.FirmwareLink (gousb)
        ▼
progress events ──► TUI progress view
```

### `internal/firmware` (new)

- `image.go` — `Parse([]byte) (*Image, error)`: validates magic, header version, VID/PID,
  region count; requires regions 4 and 3 with in-bounds offset/size, size a multiple of
  `0x1000`, load address `0x10027000`. Exposes version, per-region descriptor and data.
  `HashFromFilename(name) ([]byte, bool)`.
- `wire.go` — frame builders for ops `00`–`05`, power/LED frames, `ChunkPage(seq, addr, data) [][]byte`
  split into windows, reply/ACK parsers.
- `catalog.go` — manifest fetch (`https://aka.ms/xboxaccessoriesfwcdnpcga`, follow redirects,
  10 s timeout, entries with `id == 045E_0B00`), built-in list:
  - `DELP_045E_0B00_v_5_23_6_0_h_JxWi1z5Qk2QjWtZUAC3puP7TebjR789X3GyCFqNKjtg=.bin`
  - `DELP_045E_0B00_v_5_21_3242_0_h_8APxwmp_Ea7mJ3kLqvXD9wJLh7aX1HWOi3h4Lbek2KI=.bin`

  Merged and de-duplicated by version, newest first, each tagged with its source. Download
  base URL is the manifest's `p`, or the last known base if the manifest is unreachable.
  Cache in `os.UserCacheDir()/xbelite2-configurator/firmware/`; a cached file is used only
  if its SHA-256 matches its filename. Version is parsed from the filename (`_v_5_23_6_0_`).
- `flash.go` — `Flash(ctx, link Link, img *Image, progress func(Event)) error` and
  `Probe(link) (State, error)` (device info + region versions, used by the confirm screen
  and startup). `Link` interface:

  ```go
  type Link interface {
      Send(frame []byte) error
      Recv(timeout time.Duration) (Frame, error) // device 0x0c replies and ACKs of 0x0c
      Reset() error                              // send reset, wait for re-enumeration, re-claim
      NextSeq() byte
  }
  ```
- `fake.go` — `FakeBootloader`, an in-memory `Link` implementing ops `00`–`05`, page
  hashes, reset/mode transitions, and fault injection (fail at page N, bad status,
  hash mismatch after write, drop on reset). Used by tests and `-demo`.

### `internal/device` changes

- `FirmwareLink` (gousb): opens/claims like `USB` but without the `0x4D` handshake; its
  read loop auto-ACKs with the fixed flags and delivers cmd `0x0c` frames and cmd `0x01`
  ACKs for `0x0c` to `Recv`. `Reset()` sends `05 20 s 01 07`, closes all handles, polls
  `OpenDeviceWithVIDPID(045e, 0b00)` every 250 ms up to 30 s, re-claims (existing
  auto-detach and claim retries), then sends power-on and LED.
- `AckFor` echoes `flags & 0x20` (shared by config and firmware paths).
- Startup: if the normal handshake fails, open a `FirmwareLink` and send op `00`. A valid
  reply means bootloader mode; return that state so the TUI opens in recovery.
- The TUI `Store` can swap its `Controller`: before flashing it closes the normal `USB`
  handle and opens a `FirmwareLink`; afterwards it reopens a normal `USB`.

### Flash algorithm

1. Preflight (refuse on any failure, before sending anything that changes state):
   - catalog downloads: SHA-256 equals filename hash; local files named in the official
     pattern get the same check, otherwise shown as **unverified**;
   - `firmware.Parse` succeeds;
   - op `00` reports `045e:0b00`, page size `0x1000`.
2. In normal mode send `05 21 s 01 01`; query op `01` for types 4, 3, 2 (feeds the confirm
   screen).
3. For region 4 then region 3:
   1. Op `02` with the descriptor. `03` → `Reset()`, then repeat op `01` + op `02`; `00` → go;
      anything else → abort (nothing written yet).
   2. For each page `addr = 0x10027000 + i*0x1000`, `i in 0..125`: op `03`; if the hash
      equals SHA-1 of the image page, skip; else op `04` chunked write, require status `00`,
      then op `03` again and require the new hash (verify; not in the Windows capture,
      read-only).
   3. Op `05`; on `03` → `Reset()`.
4. Op `01` type 3 must report the image's region-3 version. Then `Reset()` (as captured).
5. Region type 2 is never sent in op `02`/`04`.

**Timeouts:** 2 s per control reply and per window ACK; 5 s for the op `04` status;
30 s per re-acquire. **Errors:** any timeout or unexpected status aborts immediately with
pass, page, and cause; no blind retries of writes. Re-running is the recovery path: done
pages are skipped by hash.

## TUI

- `f` on the main screen opens **Firmware**:
  - header: controller version and bootloader (region 2) version;
  - list: catalog entries (version, source, "installed" marker, bootloader-in-image note if
    it differs) and **Local file…** (one-line path input);
  - `enter` → download with progress → preflight → confirm screen.
- Confirm screen: from → to versions, **downgrade** label when older, **unverified**
  label, bootloader-mismatch note ("not written"), "Type y to flash".
- Progress screen: "Flashing — don't unplug", "Pass 1 of 2: preparing storage (region 4)"
  / "Pass 2 of 2: firmware (region 3)", pages written / skipped / 126, "rebooting
  controller…" during re-acquire, a progress bar. `q` and `ctrl+c` are ignored.
- Success: reopen the controller in normal mode, reload the store, return to main screen
  with a status line. Failure: error with "the controller is probably in bootloader mode;
  retrying is safe and resumes".
- Recovery mode: started when the controller is in bootloader mode; the firmware screen
  opens with a banner, latest image preselected, main screen unavailable until success.
- `-demo` uses `FakeBootloader` so the screen can be exercised without hardware.

## Testing

- **Unit:** `Parse` (good images built in-test; truncated, bad magic, wrong PID, missing
  region, misaligned size), varint and `ChunkPage` (golden headers `0c f0 s 3a 88 20`,
  `0c a0 s ba 00 3a`, `0c b0 s 30 8a 04`, terminator `0c a0 s 00 88 20`, window sizes),
  ACK parsing, filename hash/version parsing, catalog merge (trimmed manifest JSON
  fixture, unreachable manifest), `AckFor` flag echo.
- **Flasher against `FakeBootloader`:** full run; all pages skipped; resume after a fault
  in pass 1; op `02` → `03` triggers reset; unexpected status aborts with nothing written;
  verify mismatch aborts; region 2 is never written; final version check.
- **Replay test:** with `XBE2_FW_CAPTURE` and `XBE2_FW_BIN` set, a `Link` serves the
  capture's device replies in order and the host's op `02`/`03`/`04`/`05`, reset, power,
  LED and every chunk frame must match the capture byte-exact, ignoring seq bytes and the
  added verify queries. Skips when unset.
- **Committed fixtures:** no serials, account data, or Microsoft firmware bytes. `.tmp/`
  is added to `.gitignore`.
- **Hardware (manual, before release), macOS and Linux:** reflash 5.23.6 over 5.23.6
  (all pages skipped; exercises the resets), downgrade to 5.21.3242.0, upgrade back to
  5.23.6.0.

## Docs

- Rewrite `FIRMWARE.md` from this decode: drop the dual-bank claim, the chunk arithmetic,
  and the wrong descriptor offsets; add chunked transfer, op `00`–`05`, region roles.
- README: firmware screen, warning, and recovery instructions.
- `xbox-elite2-fw-re.md` stays out of the repo.

## Open items (do not block the design)

- Meaning of `05 21 s 01 01` and of op `01`'s constant `453a251a`.
- What the Windows app does when the image's region 2 differs from the controller's.
- Whether the bootloader answers `0x4D` (the startup probe doesn't depend on it).
