package firmware

import (
	"bytes"
	"crypto/sha1"
	"os"
	"testing"
	"time"

	"xbelite2-configurator/internal/pcap"
	"xbelite2-configurator/internal/protocol"
)

// The flash capture and the image aren't committed: the capture holds account
// IDs and serials, the image is Microsoft's. To run:
//
//	XBE2_FW_CAPTURE=$PWD/.tmp/usbpcap2-flash.pcap \
//	XBE2_FW_BIN=$PWD/.tmp/DELP_045E_0B00_v_5_23_6_0_h_....bin go test ./internal/firmware/ -run Replay
//
// The controller's recorded replies are served back to Flash, and every frame
// on the write path must match the Windows app's byte for byte, ignoring
// sequence numbers and the verify queries Flash adds after each write.
func TestReplayMatchesCapture(t *testing.T) {
	capPath, binPath := os.Getenv("XBE2_FW_CAPTURE"), os.Getenv("XBE2_FW_BIN")
	if capPath == "" || binPath == "" {
		t.Skip("set XBE2_FW_CAPTURE and XBE2_FW_BIN to run")
	}
	transfers, err := pcap.Read(capPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	img, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	r := newReplay(t, transfers)
	if err := Flash(r, img, Options{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.expect) < 1000 {
		t.Fatalf("only %d expected frames; wrong capture?", len(r.expect))
	}
	for i := range min(len(r.got), len(r.expect)) {
		if !bytes.Equal(r.got[i], r.expect[i]) {
			t.Fatalf("frame %d: sent %x, capture has %x", i, r.got[i], r.expect[i])
		}
	}
	if len(r.got) != len(r.expect) {
		t.Fatalf("sent %d compared frames, capture has %d", len(r.got), len(r.expect))
	}
	t.Logf("%d host frames match the capture", len(r.got))
}

// compared reports whether a host frame is on the write path: op 02/03/05,
// page chunks, resets, power-on and LED.
func compared(f []byte) bool {
	if len(f) < 5 {
		return false
	}
	switch f[0] {
	case protocol.CmdPower:
		return f[1] == protocol.FlagSystem && (f[4] == 0x07 || f[4] == 0x00)
	case CmdLEDMode:
		return true
	case CmdFirmware:
		return f[1]&0x80 != 0 || f[4] == OpStart || f[4] == OpHash || f[4] == OpFinish
	}
	return false
}

func zeroSeq(f []byte) []byte {
	f = bytes.Clone(f)
	f[2] = 0
	return f
}

type replayKey struct {
	dev  uint16
	addr uint32
}

// replay is a Link that answers from the capture. Device addresses change at
// each re-enumeration; Reset moves to the next one.
type replay struct {
	t       *testing.T
	stages  []uint16
	stage   int
	replies map[uint16][]protocol.Packet // op 00/01/02/05 replies per device address
	used    map[uint16][]bool
	hashes  map[replayKey]protocol.Packet
	acks    map[replayKey][]protocol.Packet
	written map[replayKey]protocol.Packet
	expect  [][]byte
	got     [][]byte

	queue     []protocol.Packet
	seq       byte
	msg       []byte
	writeAddr uint32
	ackIdx    int
	lastAddr  uint32
	lastData  []byte
	justWrote bool
	done      bool
}

func newReplay(t *testing.T, ts []pcap.Transfer) *replay {
	r := &replay{t: t, replies: map[uint16][]protocol.Packet{}, used: map[uint16][]bool{},
		hashes: map[replayKey]protocol.Packet{}, acks: map[replayKey][]protocol.Packet{}, written: map[replayKey]protocol.Packet{}}
	var hashAddr, writeAddr uint32
	var writeSeq byte
	started, resets := false, 0
	for _, tr := range ts {
		if n := len(r.stages); n == 0 || r.stages[n-1] != tr.Device {
			r.stages = append(r.stages, tr.Device)
		}
		d := tr.Data
		if len(d) < 4 {
			continue
		}
		if tr.FromHost {
			if d[0] == protocol.CmdPower && d[1] == 0x21 {
				started = true
			}
			// Flash from normal mode resets four times; the capture after the
			// fourth is the controller's ordinary startup.
			if started && resets < 4 && compared(d) {
				r.expect = append(r.expect, zeroSeq(d))
				if d[0] == protocol.CmdPower && d[4] == 0x07 {
					resets++
				}
			}
			if d[0] == CmdFirmware && d[1]&0x80 == 0 && len(d) >= 9 && d[4] == OpHash {
				hashAddr = le.Uint32(d[5:])
			}
			if d[0] == CmdFirmware && d[1] == chunkFirst {
				_, n := readVarint(d[3:])
				_, m := readVarint(d[3+n:])
				writeAddr, writeSeq = le.Uint32(d[3+n+m+4:]), d[2]
			}
			continue
		}
		p, err := protocol.ParsePacket(d)
		if err != nil {
			continue
		}
		switch {
		case p.Cmd == CmdFirmware && len(p.Payload) > 0 && p.Payload[0] == OpHash:
			r.hashes[replayKey{tr.Device, hashAddr}] = p
		case p.Cmd == CmdFirmware && len(p.Payload) > 0 && p.Payload[0] == OpWrite:
			r.written[replayKey{tr.Device, writeAddr}] = p
		case p.Cmd == CmdFirmware && len(p.Payload) > 0:
			r.replies[tr.Device] = append(r.replies[tr.Device], p)
			r.used[tr.Device] = append(r.used[tr.Device], false)
		default:
			if a, ok := ParseAck(p); ok && a.Seq == writeSeq {
				k := replayKey{tr.Device, writeAddr}
				r.acks[k] = append(r.acks[k], p)
			}
		}
	}
	return r
}

func (r *replay) dev() uint16   { return r.stages[r.stage] }
func (r *replay) NextSeq() byte { r.seq = r.seq%0xFF + 1; return r.seq }
func (r *replay) Close()        {}

func (r *replay) Recv(time.Duration) (protocol.Packet, error) {
	if len(r.queue) == 0 {
		return protocol.Packet{}, ErrTimeout
	}
	p := r.queue[0]
	r.queue = r.queue[1:]
	return p, nil
}

func (r *replay) Reset() error {
	r.Send(ResetRequest(r.NextSeq()))
	r.stage++
	r.queue = nil
	if r.stage >= len(r.stages)-1 {
		r.done = true // the last address is the controller's ordinary startup
	}
	r.Send(protocol.PowerOnRequest(r.NextSeq()))
	return r.Send(LEDRequest(r.NextSeq()))
}

func (r *replay) Send(f []byte) error {
	if r.done {
		return nil
	}
	f = bytes.Clone(f)
	verify := f[0] == CmdFirmware && f[1]&0x80 == 0 && f[4] == OpHash && r.justWrote && le.Uint32(f[5:]) == r.lastAddr
	if compared(f) && !verify {
		r.got = append(r.got, zeroSeq(f))
	}
	switch {
	case f[0] == CmdFirmware && f[1]&0x80 != 0:
		r.chunk(f)
	case f[0] == CmdFirmware:
		r.control(f, verify)
	}
	return nil
}

func (r *replay) control(f []byte, verify bool) {
	op := f[4]
	if op == OpHash {
		r.justWrote = false
		if verify {
			sum := sha1.Sum(r.lastData)
			r.queue = append(r.queue, protocol.Packet{Cmd: CmdFirmware, Payload: append([]byte{OpHash, StatusOK}, sum[:]...)})
			return
		}
		p, ok := r.hashes[replayKey{r.dev(), le.Uint32(f[5:])}]
		if !ok {
			r.t.Fatalf("capture has no hash reply for %#x on device %d", le.Uint32(f[5:]), r.dev())
		}
		r.queue = append(r.queue, p)
		return
	}
	var key []byte
	if op == OpRegion {
		key = f[5:9]
	}
	list, used := r.replies[r.dev()], r.used[r.dev()]
	last := -1
	for i, p := range list {
		if p.Payload[0] != op || !bytes.HasPrefix(p.Payload[2:], key) {
			continue
		}
		last = i
		if !used[i] {
			used[i] = true
			r.queue = append(r.queue, p)
			return
		}
	}
	if last < 0 {
		r.t.Fatalf("capture has no op %#02x reply on device %d", op, r.dev())
	}
	r.queue = append(r.queue, list[last])
}

func (r *replay) chunk(f []byte) {
	ln, n := readVarint(f[3:])
	_, m := readVarint(f[3+n:])
	data := f[3+n+m:]
	k := replayKey{r.dev(), r.writeAddr}
	switch {
	case f[1] == chunkFirst:
		r.msg = bytes.Clone(data)
		r.writeAddr = le.Uint32(data[4:])
		r.ackIdx = 0
		k = replayKey{r.dev(), r.writeAddr}
	case ln == 0:
		p, ok := r.written[k]
		if !ok {
			r.t.Fatalf("capture has no write reply for %#x on device %d", r.writeAddr, r.dev())
		}
		r.queue = append(r.queue, p)
		r.lastAddr, r.lastData, r.justWrote = r.writeAddr, r.msg[8:], true
		return
	default:
		r.msg = append(r.msg, data...)
	}
	if f[1]&protocol.FlagNeedsAck != 0 {
		acks := r.acks[k]
		if r.ackIdx >= len(acks) {
			r.t.Fatalf("capture has only %d ACKs for %#x", len(acks), r.writeAddr)
		}
		a := acks[r.ackIdx]
		a.Seq = f[2] // the device echoes the host's seq; ours differs from the capture's
		r.queue = append(r.queue, a)
		r.ackIdx++
	}
}
