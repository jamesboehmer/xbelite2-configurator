// Command elite2 remaps Xbox Elite Series 2 paddles and buttons on macOS and Linux.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/gousb"

	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/firmware"
	"xbelite2-configurator/internal/fixture"
	"xbelite2-configurator/internal/pcap"
	"xbelite2-configurator/internal/tui"
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	demo := flag.Bool("demo", false, "run the TUI against built-in sample data (no controller)")
	capture := flag.String("capture", "", "with -demo, seed the profiles from a USBPcap `capture` instead")
	decode := flag.String("decode", "", "print the decoded 0x4D config traffic in a `capture` and exit")
	pidStr := flag.String("pid", "", "USB product id override (default: try 0x0b00, 0x0b22)")
	debug := flag.String("debug", "", "append a log of every USB step and packet to `file`")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("xbelite2-configurator", version)
		return
	}

	if err := run(*demo, *capture, *decode, *pidStr, *debug); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

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
// OpenFirmwareLink also tries 0x0b00, which is what the bootloader enumerates
// as even when -pid names another normal-mode id.
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
