# Elite Series 2 remap protocol (decoded from the captures)

Device: `045e:0b00` Xbox Elite Series 2, interface 0, interrupt EP `0x02` OUT / `0x82` IN.
All traffic is GIP: `[cmd] [flags] [seq] [len] [payload]`. Flag `0x10` means "ACK me";
the host ACKs device frames as `01 20 <seq> 09 00 <cmd> 00 <len> 00 00 00 00 00`.

Sequence numbers are per sender. The controller ACKs a request with the host's seq,
but its 0x4D *reply* carries the controller's own counter, which keeps running across
host sessions. In the Windows capture the two counters happened to be in step. Match
replies by op code (and page), not by seq.

Run `./xbelite2-configurator -decode capture.pcapng` to see the annotated conversation in a USBPcap capture.

Sources: two USBPcap captures of Xbox Accessories on Windows (one pressing every button, one
remapping profile 1's paddles), cross-checked against the protocol notes in
[lemonxah/xbelite2](https://github.com/lemonxah/xbelite2), cited below as *lemonxah*.
Where the two disagree, the capture wins.

## Config command 0x4D

| op | host sends | device replies | meaning |
|----|-----------|----------------|---------|
| `07` | `4d 10 s 02 07 00` | `4d 00 s 02 07 00` | init / reload profiles |
| `05` | `4d 10 s 01 05` | 33 zero bytes | unknown (app sends once at startup) |
| `02` | `4d 10 s 03 02 <page> <size>` | `4d 10 s <len> 02 <st> <page> <size> <data>` | read page |
| `01` | `4d 10 s <3+size> 01 <page> <size> <data>` | `4d 00 s 03 01 <st> <page>` | write page |
| `03` | `4d 10 s 01 03` | `4d 00 s 03 03 80 <page>` | commit/unlock; reply names the active profile's mapping page |

`<st>` is `0x80` for pages of the currently active profile, `0x00` otherwise.

## Pages

| Profile | Mapping (standard) | Curves (standard) | Mapping (shift) | Curves (shift) |
|---|---|---|---|---|
| 1 | `0x20` | `0x21` | `0x26` | `0x27` |
| 2 | `0x22` | `0x23` | `0x28` | `0x29` |
| 3 | `0x24` | `0x25` | `0x2a` | `0x2b` |

Mapping pages are 56 bytes, curve pages are 43. Saving a profile in Xbox Accessories wrote
all four of its pages (`20, 26, 21, 27`), then sent INIT and COMMIT, then read them back.

## Mapping page (56 bytes)

| bytes | meaning | evidence |
|---|---|---|
| 0 | flags: `0x10` factory, app writes `0x11` on save | capture |
| 1-4 | output for paddles P1..P4 | capture: changed `05 04 07 06` → `07 05 06 04` |
| 5-16 | output for A B X Y, D-pad U D L R, LB RB, LS RS (identity `04..0f` by default) | capture + lemonxah |
| 17-27 | zero; lemonxah says keyboard-remap metadata | lemonxah |
| 28-31 | `64 64 64 64` (motor intensity, 100%); the app's first save wrote `ff` here | capture + lemonxah |
| 32-43 | `ff 00 ff 00 00 00 ff 00 ff 00 00 00` (per-axis saturation) | lemonxah |
| 44 | `64` (LED brightness) | lemonxah |
| 45-48 | `00 70 ff 5d` on profile 1 = custom color flag + RGB. `ff 00 00 00` on default profiles | capture (also sent via cmd `0x0e` as a live LED preview) |
| 49-50 | `30 30` | lemonxah: vibration |

### Output codes

`00` disabled, `04` A, `05` B, `06` X, `07` Y, `08` D-pad Up, `09` Down, `0a` Left,
`0b` Right, `0c` LB, `0d` RB, `0e` LS click, `0f` RS click.

The pairs A/B/X/Y = `04..07` and paddle bit order are confirmed by the button-press capture.
The paddles were pressed in order bit `0x08`, `0x02`, `0x04`, `0x01` and the controller
reported X, A, Y, B. That matches profile 1's paddle bytes `05 04 07 06` read in position order.

## Profile color preview (cmd 0x0E)

Sent by the app while picking a color, with its own sequence counter and no ACK:

| host sends | meaning |
|---|---|
| `0e 00 s 05 00 00 <r> <g> <b>` | show this color now (not stored) |
| `0e 00 s 05 01 00 00 00 00` | end preview, back to the stored color |

The stored color is mapping-page bytes 45-48 and is written to both the standard and shift pages.

## Input report (cmd 0x20)

Byte 4: `0x04` Menu, `0x08` View, `0x10` A, `0x20` B, `0x40` X, `0x80` Y.
Byte 5: D-pad U/D/L/R `0x01..0x08`, LB `0x10`, RB `0x20`, LS `0x40`, RS `0x80`.
Bytes 6-9: LT, RT (u16 LE, 0-1023). Bytes 10-17: sticks. Byte 18: raw paddle bits P1-P4 = `0x01..0x08`.

## Not decoded

Shift-button assignment, trigger outputs (LT/RT as a paddle target), and keyboard remaps
never appear in these captures. If you capture the app doing one of those, the decoder will
show you which bytes changed.
