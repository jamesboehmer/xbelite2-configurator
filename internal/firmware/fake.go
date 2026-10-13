package firmware

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"xbelite2-configurator/internal/protocol"
)

// ErrTimeout is what a Link's Recv returns when nothing arrives in time.
var ErrTimeout = errors.New("controller did not answer")

// Entry is one firmware image the user can pick.
type Entry struct {
	Version Version
	File    string // official filename, which carries the SHA-256
	Source  string // "Microsoft manifest", "built-in list", "demo"
}

// FakeBootloader is an in-memory controller that answers the firmware
// protocol the way the capture shows: op 02 asks for a reset unless it is
// already in the bootloader, op 05 asks for one after every pass, and the
// controller boots normally once region 3 has been finished.
type FakeBootloader struct {
	InBootloader bool
	Pages        map[uint32][]byte // app-slot contents; a missing page is erased (0xFF)
	Versions     map[uint32]Version
	Sent         [][]byte // every frame the host sent, including during Reset
	Resets       int
	Delay        time.Duration // per page write, so -demo shows progress
	ResetDelay   time.Duration

	FailWrite     int   // reject the Nth page write (1-based) with status 0x01
	CorruptWrite  int   // store the Nth page write wrongly, so the verify fails
	StartStatus   byte  // if nonzero, op 02 always answers this
	ForgetVersion bool  // op 05 doesn't record the new version
	Silent        bool  // never reply
	ResetErr      error // Reset fails (controller never comes back)

	seq, devSeq byte
	queue       []protocol.Packet
	writes      int
	msg         []byte
	msgTotal    int
	msgSeq      byte
	wantBoot    bool
	started     uint32
	pending     Version
	finished    uint32
}

// NewFakeBootloader returns a controller in normal mode running installed.
func NewFakeBootloader(installed *Image) *FakeBootloader {
	f := &FakeBootloader{Pages: map[uint32][]byte{}, Versions: map[uint32]Version{}}
	for t, r := range installed.Regions {
		f.Versions[t] = r.Version
	}
	app := installed.Regions[RegionApp]
	for i := 0; i < app.Pages(); i++ {
		f.Pages[app.Addr+uint32(i*PageSize)] = bytes.Clone(app.Data[i*PageSize : (i+1)*PageSize])
	}
	return f
}

func (f *FakeBootloader) NextSeq() byte { f.seq = f.seq%0xFF + 1; return f.seq }

func (f *FakeBootloader) Close() {}

func (f *FakeBootloader) Recv(time.Duration) (protocol.Packet, error) {
	if len(f.queue) == 0 {
		return protocol.Packet{}, ErrTimeout
	}
	p := f.queue[0]
	f.queue = f.queue[1:]
	return p, nil
}

func (f *FakeBootloader) Reset() error {
	f.Sent = append(f.Sent, ResetRequest(f.NextSeq()))
	f.Resets++
	f.queue = nil
	if f.ResetErr != nil {
		return f.ResetErr
	}
	time.Sleep(f.ResetDelay)
	switch {
	case f.wantBoot:
		f.InBootloader = true
	case f.InBootloader && f.finished == RegionApp:
		f.InBootloader = false
	}
	f.wantBoot, f.finished = false, 0
	f.Sent = append(f.Sent, protocol.PowerOnRequest(f.NextSeq()), LEDRequest(f.NextSeq()))
	return nil
}

func (f *FakeBootloader) Send(frame []byte) error {
	f.Sent = append(f.Sent, bytes.Clone(frame))
	if len(frame) < 4 || frame[0] != CmdFirmware {
		return nil
	}
	if frame[1]&0x80 != 0 {
		return f.chunk(frame)
	}
	if frame[1]&protocol.FlagNeedsAck != 0 {
		f.push(protocol.Packet{Cmd: protocol.CmdAck, Flags: protocol.FlagSystem, Seq: frame[2],
			Payload: []byte{0x00, CmdFirmware, protocol.FlagSystem, frame[3], 0, 0, 0, 0, 0}})
	}
	if len(frame) > 4 {
		f.control(frame[4:])
	}
	return nil
}

func (f *FakeBootloader) push(p protocol.Packet) {
	if !f.Silent {
		f.queue = append(f.queue, p)
	}
}

func (f *FakeBootloader) reply(op, status byte, body []byte) {
	f.devSeq = f.devSeq%0xFF + 1
	f.push(protocol.Packet{Cmd: CmdFirmware, Flags: flagsControl, Seq: f.devSeq, Payload: append([]byte{op, status}, body...)})
}

func (f *FakeBootloader) page(addr uint32) []byte {
	if p, ok := f.Pages[addr]; ok {
		return p
	}
	return bytes.Repeat([]byte{0xff}, PageSize)
}

func (f *FakeBootloader) control(p []byte) {
	switch p[0] {
	case OpInfo:
		body := []byte{0x02, 0x00}
		body = le.AppendUint16(body, VendorID)
		body = le.AppendUint16(body, ProductID)
		body = le.AppendUint32(body, PageSize)
		f.reply(OpInfo, StatusOK, body)
	case OpRegion:
		t := le.Uint32(p[1:])
		v := f.Versions[t]
		body := le32(t)
		body = le.AppendUint16(body, v.Major)
		body = le.AppendUint16(body, v.Minor)
		body = le.AppendUint32(body, v.Build)
		f.reply(OpRegion, StatusOK, append(body, 0x45, 0x3a, 0x25, 0x1a))
	case OpStart:
		switch {
		case f.StartStatus != 0:
			f.reply(OpStart, f.StartStatus, nil)
		case !f.InBootloader:
			f.wantBoot = true
			f.reply(OpStart, StatusReset, nil)
		default:
			f.started = le.Uint32(p[1:])
			f.pending = Version{Major: le.Uint16(p[13:]), Minor: le.Uint16(p[15:]), Build: le.Uint32(p[17:])}
			f.Versions[RegionApp] = Version{} // the slot is about to be overwritten
			f.reply(OpStart, StatusOK, nil)
		}
	case OpHash:
		sum := sha1.Sum(f.page(le.Uint32(p[1:])))
		f.reply(OpHash, StatusOK, sum[:])
	case OpFinish:
		if !f.ForgetVersion {
			f.Versions[f.started] = f.pending
		}
		f.finished = f.started
		f.reply(OpFinish, StatusReset, nil)
	}
}

func (f *FakeBootloader) chunk(c []byte) error {
	ln, n := readVarint(c[3:])
	off, m := readVarint(c[3+n:])
	data := c[3+n+m:]
	if len(data) != ln {
		return fmt.Errorf("fake: chunk says %d bytes, carries %d", ln, len(data))
	}
	switch {
	case c[1]&0x40 != 0: // first chunk: the offset field is the total length
		f.msg, f.msgTotal, f.msgSeq = bytes.Clone(data), off, c[2]
	case ln == 0:
		return f.finishWrite()
	case off != len(f.msg):
		return fmt.Errorf("fake: chunk at %d, expected %d", off, len(f.msg))
	default:
		f.msg = append(f.msg, data...)
	}
	if c[1]&protocol.FlagNeedsAck != 0 {
		window := min(windowSize, f.msgTotal-len(f.msg))
		payload := []byte{0x00, CmdFirmware, protocol.FlagSystem}
		payload = le.AppendUint16(payload, uint16(len(f.msg)))
		payload = append(payload, 0, 0)
		payload = le.AppendUint16(payload, uint16(window))
		f.push(protocol.Packet{Cmd: protocol.CmdAck, Flags: protocol.FlagSystem, Seq: f.msgSeq, Payload: payload})
	}
	return nil
}

func (f *FakeBootloader) finishWrite() error {
	if len(f.msg) != f.msgTotal || len(f.msg) != 8+PageSize || f.msg[0] != OpWrite {
		return fmt.Errorf("fake: bad page message of %d bytes", len(f.msg))
	}
	time.Sleep(f.Delay)
	f.writes++
	if f.writes == f.FailWrite {
		f.reply(OpWrite, 0x01, nil)
		return nil
	}
	data := bytes.Clone(f.msg[8:])
	if f.writes == f.CorruptWrite {
		data[0] ^= 0xff
	}
	f.Pages[le.Uint32(f.msg[4:])] = data
	f.reply(OpWrite, StatusOK, nil)
	return nil
}

// DemoImage builds a synthetic image shaped like the real ones: regions 4
// and 3 of 126 pages each (with 39 and 13 blank pages at the end), and a
// region 2. Contents are seeded by version so different versions differ.
func DemoImage(v Version) []byte {
	rng := rand.New(rand.NewPCG(uint64(v.Major)<<48|uint64(v.Minor)<<32|uint64(v.Build), 1))
	region := func(t, addr uint32, pages, blank int) Region {
		d := bytes.Repeat([]byte{0xff}, pages*PageSize)
		for i := 0; i < (pages-blank)*PageSize; i += 8 {
			le.PutUint64(d[i:], rng.Uint64())
		}
		return Region{Type: t, Addr: addr, Version: v, Data: d}
	}
	return Build(v, region(RegionStaging, AppBase, 126, 39), region(RegionApp, AppBase, 126, 13), region(RegionBoot, 0x10005000, 34, 0))
}

// DemoSource offers synthetic images for -demo; nothing is downloaded.
type DemoSource struct{}

var demoVersions = []Version{{5, 23, 6, 0}, {5, 21, 3242, 0}}

func (DemoSource) List(context.Context) ([]Entry, string) {
	var out []Entry
	for _, v := range demoVersions {
		out = append(out, Entry{Version: v, File: Filename(v, sha256.Sum256(DemoImage(v))), Source: "demo"})
	}
	return out, "Demo mode: synthetic images, nothing is downloaded or flashed."
}

func (DemoSource) Fetch(_ context.Context, e Entry, progress func(done, total int64)) ([]byte, error) {
	b := DemoImage(e.Version)
	if progress != nil {
		progress(int64(len(b)), int64(len(b)))
	}
	return b, nil
}
