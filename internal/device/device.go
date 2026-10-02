// Package device talks to the controller over USB (libusb via gousb) and
// provides an in-memory fake for demos and tests.
package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime"
	"sync"
	"time"

	"github.com/google/gousb"

	"xbelite2-configurator/internal/protocol"
)

// Controller is what the TUI needs from a controller.
type Controller interface {
	ActivePage() byte
	ReadPage(page byte) ([]byte, error)
	WriteProfile(profile int, pages map[byte][]byte) error
	Pressed() []string
	// PreviewLED shows a color live without storing it; EndPreview reverts.
	PreviewLED(rgb [3]byte) error
	EndPreview() error
	Close()
}

const (
	VendorID = 0x045E
	// 0x0b00 = Elite Series 2 (wired). 0x0b22 shows up on later firmware.
	iface = 0
	epNum = 2 // interrupt 0x02 OUT / 0x82 IN

	// On macOS the first claim after detaching Apple's driver can fail with
	// "bad access"; retrying on the same handle never helps, but reopening
	// the device does.
	openAttempts  = 4
	claimAttempts = 2
	claimRetry    = 250 * time.Millisecond
	// A freshly re-enumerated controller ignores power-on/init until it has booted.
	handshakeFor     = 6 * time.Second
	handshakeTimeout = 700 * time.Millisecond
)

var ProductIDs = []gousb.ID{0x0B00, 0x0B22}

// USB is a real controller on interface 0's interrupt endpoints.
type USB struct {
	ctx     *gousb.Context
	dev     *gousb.Device
	cfg     *gousb.Config
	intf    *gousb.Interface
	in      *gousb.InEndpoint
	out     *gousb.OutEndpoint
	timeout time.Duration
	log     *log.Logger

	mu      sync.Mutex // serializes config requests
	seq     byte
	sendMu  sync.Mutex // serializes endpoint writes
	ledMu   sync.Mutex
	ledSeq  byte // 0x0E frames have their own counter in the capture
	replies chan protocol.Packet
	seen    chan struct{} // closed when the first packet of any kind arrives

	stateMu   sync.Mutex
	pressed   []string
	lastInput string
	active    byte

	cancel context.CancelFunc
	done   chan struct{}
}

func permHint(err error) error {
	if runtime.GOOS == "darwin" {
		return fmt.Errorf("%w\nOn macOS, run with sudo so libusb can detach Apple's game controller driver", err)
	}
	return fmt.Errorf("%w\nOn Linux, run with sudo or install the udev rule from README.md", err)
}

// Open finds and claims the controller. pid 0 tries the known product IDs.
// debug, if non-nil, receives a log of every USB step and packet.
func Open(pid gousb.ID, debug io.Writer) (*USB, error) {
	if debug == nil {
		debug = io.Discard
	}
	lg := log.New(debug, "", log.Lmicroseconds)
	var lastErr error
	for attempt := 1; attempt <= openAttempts; attempt++ {
		u, err := open(pid, lg)
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

var errNotFound = errors.New("no Elite Series 2 found over USB. Plug it in with a data cable " +
	"(Bluetooth and the wireless adapter are not supported)")

func open(pid gousb.ID, lg *log.Logger) (*USB, error) {
	ctx := gousb.NewContext()
	ids := ProductIDs
	if pid != 0 {
		ids = []gousb.ID{pid}
	}
	var dev *gousb.Device
	var lastErr error
	for _, id := range ids {
		d, err := ctx.OpenDeviceWithVIDPID(VendorID, id)
		if d != nil {
			lg.Printf("opened %s", d)
			dev = d
			break
		}
		if err != nil {
			lastErr = err
		}
	}
	if dev == nil {
		ctx.Close()
		if lastErr != nil {
			return nil, permHint(fmt.Errorf("could not open the controller: %v", lastErr))
		}
		return nil, errNotFound
	}
	u := &USB{ctx: ctx, dev: dev, timeout: time.Second, log: lg,
		replies: make(chan protocol.Packet, 16), seen: make(chan struct{}), done: make(chan struct{})}
	if err := u.claim(); err != nil {
		u.closeUSB()
		return nil, err
	}
	rctx, cancel := context.WithCancel(context.Background())
	u.cancel = cancel
	go u.readLoop(rctx)
	if err := u.handshake(); err != nil {
		u.Close()
		return nil, err
	}
	return u, nil
}

func (u *USB) claim() error {
	// Detaches the OS driver (xpad / Apple's) and reattaches it on close.
	if err := u.dev.SetAutoDetach(true); err != nil {
		return permHint(fmt.Errorf("could not enable driver auto-detach: %v", err))
	}
	cfgNum, err := u.dev.ActiveConfigNum()
	if err != nil || cfgNum == 0 {
		cfgNum = 1
	}
	if u.cfg, err = u.dev.Config(cfgNum); err != nil {
		return permHint(fmt.Errorf("could not take over the controller: %v", err))
	}
	u.log.Printf("config %d selected, driver detached", cfgNum)
	for attempt := 1; ; attempt++ {
		if u.intf, err = u.cfg.Interface(iface, 0); err == nil {
			u.log.Printf("claimed interface %d on attempt %d", iface, attempt)
			break
		}
		u.log.Printf("claim attempt %d: %v", attempt, err)
		if attempt == claimAttempts {
			return permHint(fmt.Errorf("could not claim the controller: %v", err))
		}
		time.Sleep(claimRetry)
	}
	if u.in, err = u.intf.InEndpoint(epNum); err != nil {
		return err
	}
	u.out, err = u.intf.OutEndpoint(epNum)
	return err
}

// handshake brings the controller up: power on (harmless if already on), then
// INIT, retrying until the controller answers. A controller that has just
// re-enumerated ignores both until it has finished booting.
func (u *USB) handshake() error {
	powered := false
	select {
	case <-u.seen:
		u.log.Printf("controller already talking")
		powered = true
	case <-time.After(1500 * time.Millisecond):
		u.log.Printf("controller silent after claim")
	}
	deadline := time.Now().Add(handshakeFor)
	var lastErr error
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		if !powered || attempt > 1 {
			u.mu.Lock()
			err := u.send(protocol.PowerOnRequest(u.nextSeq()))
			u.mu.Unlock()
			if err != nil {
				u.log.Printf("power-on send failed: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if _, lastErr = u.configWithin(handshakeTimeout, protocol.InitRequest); lastErr == nil {
			u.log.Printf("handshake done on attempt %d", attempt)
			return u.refreshActive()
		}
		u.log.Printf("handshake attempt %d: %v", attempt, lastErr)
	}
	return fmt.Errorf("controller did not respond to initialization after %s (%v). "+
		"Try unplugging it, waiting a few seconds, and plugging it back in", handshakeFor, lastErr)
}

func (u *USB) nextSeq() byte {
	u.seq = u.seq%0xFF + 1
	return u.seq
}

func (u *USB) send(data []byte) error {
	u.sendMu.Lock()
	defer u.sendMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), u.timeout)
	defer cancel()
	_, err := u.out.WriteContext(ctx, data)
	u.log.Printf("-> %x (err=%v)", data, err)
	return err
}

func (u *USB) readLoop(ctx context.Context) {
	defer close(u.done)
	buf := make([]byte, 64)
	first := true
	for ctx.Err() == nil {
		rctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		n, err := u.in.ReadContext(rctx, buf)
		cancel()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, gousb.TransferCancelled) {
				u.log.Printf("read error: %v", err)
				time.Sleep(50 * time.Millisecond)
			}
			continue
		}
		data := append([]byte(nil), buf[:n]...)
		pkt, err := protocol.ParsePacket(data)
		if err != nil {
			continue
		}
		if first {
			first = false
			close(u.seen)
		}
		if pkt.Cmd != protocol.CmdInput {
			u.log.Printf("<- %x", data)
		}
		if pkt.NeedsAck() {
			_ = u.send(protocol.AckFor(data))
		}
		switch pkt.Cmd {
		case protocol.CmdInput:
			p := protocol.PressedFromInput(pkt.Payload)
			u.stateMu.Lock()
			u.pressed = p
			if s := fmt.Sprint(p); s != u.lastInput {
				u.lastInput = s
				u.log.Printf("input %s", s)
			}
			u.stateMu.Unlock()
		case protocol.CmdConfig:
			select {
			case u.replies <- pkt:
			default:
			}
		}
	}
}

// config sends a 0x4D request built by build(seq) and waits for its reply.
func (u *USB) config(build func(seq byte) []byte) (protocol.Packet, error) {
	return u.configWithin(u.timeout, build)
}

func (u *USB) configWithin(timeout time.Duration, build func(seq byte) []byte) (protocol.Packet, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	seq := u.nextSeq()
	for len(u.replies) > 0 {
		<-u.replies
	}
	req := build(seq)
	if err := u.send(req); err != nil {
		return protocol.Packet{}, fmt.Errorf("send failed: %w", err)
	}
	deadline := time.After(timeout)
	for {
		select {
		case pkt := <-u.replies:
			if protocol.ReplyMatches(req, pkt) {
				return pkt, nil
			}
			u.log.Printf("ignoring unrelated config reply %x", pkt.Payload)
		case <-deadline:
			return protocol.Packet{}, fmt.Errorf("controller did not answer request %x", req[4:min(len(req), 7)])
		}
	}
}

func (u *USB) refreshActive() error {
	reply, err := u.config(protocol.CommitRequest)
	if err != nil {
		return err
	}
	if page, ok := protocol.ActivePageFromCommit(reply.Payload); ok {
		u.stateMu.Lock()
		u.active = page
		u.stateMu.Unlock()
	}
	return nil
}

func (u *USB) sendLED(build func(seq byte) []byte) error {
	u.ledMu.Lock()
	defer u.ledMu.Unlock()
	u.ledSeq = u.ledSeq%0xFF + 1
	return u.send(build(u.ledSeq))
}

func (u *USB) PreviewLED(rgb [3]byte) error {
	return u.sendLED(func(s byte) []byte { return protocol.LEDPreview(s, rgb) })
}

func (u *USB) EndPreview() error { return u.sendLED(protocol.LEDPreviewEnd) }

func (u *USB) ActivePage() byte {
	u.stateMu.Lock()
	defer u.stateMu.Unlock()
	return u.active
}

func (u *USB) Pressed() []string {
	u.stateMu.Lock()
	defer u.stateMu.Unlock()
	return u.pressed
}

func (u *USB) ReadPage(page byte) ([]byte, error) {
	reply, err := u.config(func(s byte) []byte { return protocol.ReadRequest(s, page) })
	if err != nil {
		return nil, err
	}
	got, data, err := protocol.ParseReadResponse(reply.Payload)
	if err != nil {
		return nil, err
	}
	if got != page {
		return nil, fmt.Errorf("asked for page 0x%02x, got 0x%02x", page, got)
	}
	return data, nil
}

// WriteProfile writes a profile the way Xbox Accessories does: all four pages,
// then INIT + COMMIT so the controller reloads them, then reads them back.
func (u *USB) WriteProfile(profile int, pages map[byte][]byte) error {
	order := protocol.PageIDs(profile).WriteOrder()
	if err := u.refreshActive(); err != nil { // COMMIT doubles as "unlock" before writes
		return err
	}
	for _, page := range order {
		req, err := protocol.WriteRequest(0, page, protocol.MarkCustom(pages[page]))
		if err != nil {
			return err
		}
		reply, err := u.config(func(s byte) []byte { req[2] = s; return req })
		if err != nil {
			return err
		}
		if p := reply.Payload; len(p) < 3 || p[0] != protocol.OpWrite || p[2] != page {
			return fmt.Errorf("unexpected write reply for page 0x%02x: %x", page, p)
		}
	}
	if _, err := u.config(protocol.InitRequest); err != nil {
		return err
	}
	if err := u.refreshActive(); err != nil {
		return err
	}
	for _, page := range order {
		got, err := u.ReadPage(page)
		if err != nil {
			return err
		}
		if string(got[1:]) != string(pages[page][1:]) {
			return fmt.Errorf("verify failed: page 0x%02x did not stick", page)
		}
	}
	return nil
}

func (u *USB) Close() {
	if u.cancel != nil {
		u.cancel()
		<-u.done
	}
	u.closeUSB()
	u.log.Printf("closed; controller handed back to the OS driver")
}

func (u *USB) closeUSB() {
	if u.intf != nil {
		u.intf.Close()
	}
	if u.cfg != nil {
		u.cfg.Close()
	}
	u.dev.Close()
	u.ctx.Close()
}

// Fake is an in-memory controller seeded from a capture.
type Fake struct {
	Pages    map[byte][]byte
	Writes   [][2][]byte // (page, data) in write order
	Previews [][3]byte
	Ended    int
}

func NewFake(pages map[byte][]byte) *Fake {
	f := &Fake{Pages: map[byte][]byte{}}
	for k, v := range pages {
		f.Pages[k] = append([]byte(nil), v...)
	}
	return f
}

func (f *Fake) ActivePage() byte  { return 0x20 }
func (f *Fake) Pressed() []string { return nil }
func (f *Fake) Close()            {}

func (f *Fake) PreviewLED(rgb [3]byte) error { f.Previews = append(f.Previews, rgb); return nil }
func (f *Fake) EndPreview() error            { f.Ended++; return nil }

func (f *Fake) ReadPage(page byte) ([]byte, error) {
	d, ok := f.Pages[page]
	if !ok {
		return nil, fmt.Errorf("demo data has no page 0x%02x", page)
	}
	return append([]byte(nil), d...), nil
}

func (f *Fake) WriteProfile(profile int, pages map[byte][]byte) error {
	for _, page := range protocol.PageIDs(profile).WriteOrder() {
		d := protocol.MarkCustom(pages[page])
		f.Writes = append(f.Writes, [2][]byte{{page}, d})
		f.Pages[page] = d
	}
	return nil
}
