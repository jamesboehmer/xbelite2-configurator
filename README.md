# xbelite2-configurator

A terminal UI for configuring the Xbox Elite Series 2 controller on macOS and Linux,
without the Windows-only Xbox Accessories app. It remaps the paddles and buttons in each
of the controller's 3 profiles, in both standard and shift mode, and sets each profile's
color. Settings are stored on the controller itself, so they apply everywhere afterwards.

The protocol was decoded from USB captures of Xbox Accessories. See [PROTOCOL.md](PROTOCOL.md).

## Install

Download the archive for your platform from [Releases](../../releases), then extract it:

| Platform | Archive |
|---|---|
| macOS (Apple Silicon) | `xbelite2-configurator-<version>-darwin-arm64.tar.gz` |
| Linux x64 | `xbelite2-configurator-<version>-linux-amd64.tar.gz` |
| Linux arm64 | `xbelite2-configurator-<version>-linux-arm64.tar.gz` |

libusb is built into the binaries, so there's nothing else to install. The Linux builds
need glibc 2.35 or newer and libudev, which systemd distros have. On macOS, clear the
download quarantine before the first run, because the binary isn't notarized:

```sh
xattr -d com.apple.quarantine xbelite2-configurator
```

## Build

Needs Go 1.22+, libusb, and a C compiler, because gousb uses cgo.

```sh
brew install libusb                  # macOS (plus Xcode command line tools)
sudo apt install libusb-1.0-0-dev    # Debian/Ubuntu
go build
```

## Run

Connect the controller with a **USB data cable**. Bluetooth and the wireless adapter are not supported.

```sh
sudo ./xbelite2-configurator                         # real controller
./xbelite2-configurator -demo                        # try the UI on built-in sample data
./xbelite2-configurator -decode capture.pcapng       # dump decoded config traffic from a USBPcap capture
sudo ./xbelite2-configurator -debug xbe2.log         # log every USB step and packet
sudo ./xbelite2-configurator -pid 0x0b22             # if your controller reports a different product ID
```

Keys: `↑/↓` select · `←/→` cycle output · `enter` pick from list · `0` disable ·
`1/2/3` profile · `tab` standard/shift · `c` profile color · `d` reset · `r` reload · `w` write · `q` quit.

Nothing changes on the controller until you press `w`. It writes all four pages of the
current profile, the same way Xbox Accessories does, and reads them back to verify.

The "Held:" line shows what the controller is reporting live. Press a paddle to see
which P number it is.

### Profile color

`c` opens a color picker:
- hue, saturation and brightness sliders
- hex entry (`#`)
- presets
- `x` for the default color

The controller shows the color live while you pick. `enter` applies it, and `w` stores
it on the controller. Canceling or quitting returns the light to the stored color.

### Why sudo

- **macOS:** Apple's built-in game controller driver owns the device. libusb can only
  detach it as root, and does so by making the controller drop off and reconnect. The
  first claim after that can fail with "bad access", so the tool retries with a fresh
  handle. The controller goes back to macOS when you quit.
- **Linux:** the `xpad` driver owns the device. The tool detaches it and reattaches it on
  exit. To run without sudo, add a udev rule:

  ```sh
  echo 'SUBSYSTEM=="usb", ATTRS{idVendor}=="045e", ATTRS{idProduct}=="0b00", MODE="0660", TAG+="uaccess"' \
    | sudo tee /etc/udev/rules.d/70-xbox-elite2.rules && sudo udevadm control --reload && sudo udevadm trigger
  ```

## Releasing

Push a semantic version tag. The [release workflow](.github/workflows/release.yml) builds
each platform on a native runner, runs the tests, and publishes a GitHub release with the
archives and their SHA-256 checksums. A tag with a pre-release suffix such as `v1.2.0-rc.1`
is published as a pre-release.

```sh
git tag -s v0.1.0 -m v0.1.0 && git push origin v0.1.0
```

To build a release archive locally for the current platform, run
`scripts/build-release.sh v0.1.0 dist`.

## Tests

```sh
go test ./...
XBE2_CAPTURE=/path/to/buttonremap.pcapng go test ./internal/pcap   # also check the decoder against a capture
```

Capture files are not committed. The tests use profile pages taken from a real controller
in `internal/fixture`.

## Not supported yet

These never appeared in the captures, so they aren't decoded:
- choosing which button acts as the shift button
- mapping a paddle to a trigger
- keyboard mappings
- stick curves and trigger dead zones

The tool writes those bytes back unchanged.

## License

[MIT](LICENSE). Not affiliated with or endorsed by Microsoft. Xbox is a trademark of Microsoft.
