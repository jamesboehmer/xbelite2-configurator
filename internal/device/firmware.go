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
	dropFor       = 6 * time.Second // macOS can list the old device for over 3 s after a reset
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
	u, err := withRetries(f.log, f.open)
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

// bootloaderPID is what the bootloader enumerates as, whatever -pid says the
// controller uses in normal mode.
const bootloaderPID gousb.ID = 0x0B00

// ids returns the product ids the link accepts: the override plus the
// bootloader's, or every known id when there is no override.
func (f *FirmwareLink) ids() []gousb.ID {
	if f.pid == 0 || f.pid == bootloaderPID {
		return ProductIDs
	}
	return []gousb.ID{f.pid, bootloaderPID}
}

// open tries the override pid first, then the bootloader's.
func (f *FirmwareLink) open() (*USB, error) {
	if f.pid == 0 {
		return openRaw(0, f.log, f.frames)
	}
	u, err := openRaw(f.pid, f.log, f.frames)
	if errors.Is(err, errNotFound) && f.pid != bootloaderPID {
		return openRaw(bootloaderPID, f.log, f.frames)
	}
	return u, err
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
	var oldBus, oldAddr int
	if f.u != nil {
		oldBus, oldAddr = f.u.dev.Desc.Bus, f.u.dev.Desc.Address
	}
	f.Close()
	f.waitGone(oldBus, oldAddr)
	deadline := time.Now().Add(reacquireFor)
	var lastErr error
	for time.Now().Before(deadline) {
		time.Sleep(reacquirePoll)
		u, err := f.open()
		if err != nil {
			lastErr = err
			continue
		}
		// No same-address check here: macOS hands the re-enumerated
		// controller the address it had before the reset.
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

func (f *FirmwareLink) waitGone(bus, addr int) {
	ctx := gousb.NewContext()
	defer ctx.Close()
	deadline := time.Now().Add(dropFor)
	for time.Now().Before(deadline) {
		if !f.present(ctx, bus, addr) {
			f.log.Printf("controller dropped off the bus")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.log.Printf("controller still listed %s after reset", dropFor)
}

// present lists devices without opening any, and reports whether the device
// at bus/addr is still there. With no known address (0) any matching device
// counts.
func (f *FirmwareLink) present(ctx *gousb.Context, bus, addr int) bool {
	found := false
	devs, _ := ctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
		if d.Vendor == VendorID && slices.Contains(f.ids(), d.Product) && (addr == 0 || d.Bus == bus && d.Address == addr) {
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
