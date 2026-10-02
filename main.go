// Command elite2 remaps Xbox Elite Series 2 paddles and buttons on macOS and Linux.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/google/gousb"

	"xbelite2-configurator/internal/device"
	"xbelite2-configurator/internal/fixture"
	"xbelite2-configurator/internal/pcap"
	"xbelite2-configurator/internal/tui"
)

func main() {
	demo := flag.Bool("demo", false, "run the TUI against built-in sample data (no controller)")
	capture := flag.String("capture", "", "with -demo, seed the profiles from a USBPcap `capture` instead")
	decode := flag.String("decode", "", "print the decoded 0x4D config traffic in a `capture` and exit")
	pidStr := flag.String("pid", "", "USB product id override (default: try 0x0b00, 0x0b22)")
	debug := flag.String("debug", "", "append a log of every USB step and packet to `file`")
	flag.Parse()

	if err := run(*demo, *capture, *decode, *pidStr, *debug); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(demo bool, capture, decode, pidStr, debug string) error {
	if decode != "" {
		return pcap.Dump(os.Stdout, decode)
	}

	var dev device.Controller
	if demo {
		pages := fixture.Pages()
		if capture != "" {
			var err error
			if pages, err = pcap.LastPages(capture); err != nil {
				return err
			}
		}
		dev = device.NewFake(pages)
	} else {
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
		fmt.Fprintln(os.Stderr, "Connecting to controller...")
		u, err := device.Open(pid, logw)
		if err != nil {
			return err
		}
		dev = u
	}
	defer dev.Close()
	return tui.Run(dev)
}
