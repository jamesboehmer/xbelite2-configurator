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
