package firmware

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"xbelite2-configurator/internal/protocol"
)

func lastPass(evs []Event, pass int) Event {
	var last Event
	for _, e := range evs {
		if e.Pass == pass && !e.Rebooting {
			last = e
		}
	}
	return last
}

func flash(t *testing.T, f *FakeBootloader, img *Image, opts Options) ([]Event, error) {
	t.Helper()
	var evs []Event
	err := Flash(f, img, opts, func(e Event) { evs = append(evs, e) })
	return evs, err
}

func TestFlashFromNormalMode(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	img := demo(t, v5236)
	evs, err := flash(t, f, img, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.InBootloader || f.Versions[RegionApp] != v5236 {
		t.Errorf("bootloader %v, app %s", f.InBootloader, f.Versions[RegionApp])
	}
	app := img.Regions[RegionApp]
	for i := 0; i < app.Pages(); i++ {
		if !bytes.Equal(f.page(AppBase+uint32(i*PageSize)), app.Data[i*PageSize:(i+1)*PageSize]) {
			t.Fatalf("page %d differs after flashing", i)
		}
	}
	if f.Resets != 4 {
		t.Errorf("%d resets, want 4 (enter bootloader, after each pass, final)", f.Resets)
	}
	for pass := 1; pass <= 2; pass++ {
		if e := lastPass(evs, pass); e.Done != 126 || e.Written != 113 || e.Skipped != 13 {
			t.Errorf("pass %d: %+v", pass, e)
		}
	}
	if !evs[len(evs)-1].Rebooting {
		t.Error("last event isn't the final reboot")
	}
	// The normal-mode preamble, in capture order.
	if got := f.Sent[0]; got[0] != protocol.CmdPower || got[1] != 0x21 {
		t.Errorf("first frame %x", got)
	}
	if got := f.Sent[1]; got[0] != CmdFirmware || got[4] != OpInfo {
		t.Errorf("second frame %x", got)
	}
}

func TestFlashNeverWritesBootRegion(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	if _, err := flash(t, f, demo(t, v5236), Options{}); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.Sent {
		if s[0] != CmdFirmware {
			continue
		}
		if s[1]&0x80 == 0 && s[4] == OpStart && binary.LittleEndian.Uint32(s[5:]) == RegionBoot {
			t.Fatal("op 02 for the bootloader region")
		}
		if s[1] == 0xf0 {
			addr := binary.LittleEndian.Uint32(s[10:])
			if addr < AppBase || addr >= AppBase+126*PageSize {
				t.Fatalf("write outside the app slot: %#x", addr)
			}
		}
	}
}

func TestFlashResumesAfterFailure(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	img := demo(t, v5236)
	f.FailWrite = 50
	_, err := flash(t, f, img, Options{})
	if err == nil || !strings.Contains(err.Error(), "pass 1 of 2") || !strings.Contains(err.Error(), "page 50 of 126") {
		t.Fatalf("got %v", err)
	}
	if !f.InBootloader {
		t.Fatal("fake should be left in the bootloader")
	}
	f.FailWrite = 0
	evs, err := flash(t, f, img, Options{FromBootloader: true})
	if err != nil {
		t.Fatal(err)
	}
	if e := lastPass(evs, 1); e.Skipped != 62 || e.Written != 64 {
		t.Errorf("resumed pass 1: %+v (want 49 done + 13 blank skipped)", e)
	}
	if f.Versions[RegionApp] != v5236 || f.InBootloader {
		t.Error("resume didn't finish")
	}
}

func TestFlashAbortsOnUnexpectedStartStatus(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.StartStatus = 0x01
	_, err := flash(t, f, demo(t, v5236), Options{})
	if err == nil || !strings.Contains(err.Error(), "status 0x01") {
		t.Fatalf("got %v", err)
	}
	for _, s := range f.Sent {
		if s[0] == CmdFirmware && s[1]&0x80 != 0 {
			t.Fatal("wrote a page after a refused start")
		}
	}
}

func TestFlashVerifyMismatchAborts(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.CorruptWrite = 1
	_, err := flash(t, f, demo(t, v5236), Options{})
	if err == nil || !strings.Contains(err.Error(), "verify failed") {
		t.Fatalf("got %v", err)
	}
}

func TestFlashChecksFinalVersion(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.ForgetVersion = true
	_, err := flash(t, f, demo(t, v5236), Options{})
	if err == nil || !strings.Contains(err.Error(), "expected 5.23.6.0") {
		t.Fatalf("got %v", err)
	}
}

func TestFlashTimesOut(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.Silent = true
	if _, err := flash(t, f, demo(t, v5236), Options{}); !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
}

func TestFlashResetFailureIsReported(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.ResetErr = errors.New("controller did not come back within 30s after rebooting")
	_, err := flash(t, f, demo(t, v5236), Options{})
	if err == nil || !strings.Contains(err.Error(), "pass 1 of 2") || !strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("got %v", err)
	}
}

func TestAwaitSkipsStaleReplies(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	// A late answer to a region-2 query must not be taken for region 4's.
	f.reply(OpRegion, StatusOK, append(le32(RegionBoot), 5, 0, 23, 0, 6, 0, 0, 0))
	s := session{link: f}
	v, err := s.regionVersion(RegionStaging)
	if err != nil || v != v5244 {
		t.Fatalf("got %s %v", v, err)
	}
}

func TestProbe(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	st, err := Probe(f)
	if err != nil || st.App != v5244 || st.Boot != v5244 {
		t.Fatalf("got %+v %v", st, err)
	}
}

// tamperLink wraps the fake to alter what the host hears or how Reset behaves.
type tamperLink struct {
	*FakeBootloader
	onWindowAck func(p protocol.Packet) protocol.Packet // replaces the first window ACK
	failResetAt int                                     // fail the Nth Reset call (1-based)
	chunkSeq    byte
	done        bool
	resets      int
}

func (l *tamperLink) Send(frame []byte) error {
	if l.chunkSeq == 0 && len(frame) > 2 && frame[0] == CmdFirmware && frame[1]&0x80 != 0 {
		l.chunkSeq = frame[2]
	}
	return l.FakeBootloader.Send(frame)
}

func (l *tamperLink) Recv(d time.Duration) (protocol.Packet, error) {
	p, err := l.FakeBootloader.Recv(d)
	if err == nil && !l.done && l.onWindowAck != nil && l.chunkSeq != 0 && p.Cmd == protocol.CmdAck && p.Seq == l.chunkSeq {
		l.done = true
		return l.onWindowAck(p), nil
	}
	return p, err
}

func (l *tamperLink) Reset() error {
	l.resets++
	if l.resets == l.failResetAt {
		return errors.New("controller did not come back within 30s after rebooting")
	}
	return l.FakeBootloader.Reset()
}

func flashFast(t *testing.T, l Link, opts Options) error {
	t.Helper()
	start := time.Now()
	err := Flash(l, demo(t, v5236), opts, nil)
	if el := time.Since(start); el > time.Second {
		t.Errorf("took %v, want a fast failure", el)
	}
	return err
}

func TestFlashAbortsOnWrongAckCount(t *testing.T) {
	l := &tamperLink{FakeBootloader: NewFakeBootloader(demo(t, v5244))}
	l.onWindowAck = func(p protocol.Packet) protocol.Packet {
		p.Payload = bytes.Clone(p.Payload)
		p.Payload[3] ^= 0x01 // lost chunk: wrong Received
		return p
	}
	err := flashFast(t, l, Options{})
	if err == nil || !strings.Contains(err.Error(), "pass 1 of 2: page 1 of 126") || !strings.Contains(err.Error(), "controller acknowledged") {
		t.Fatalf("got %v", err)
	}
}

func TestFlashAbortsOnWriteStatusDuringWindow(t *testing.T) {
	l := &tamperLink{FakeBootloader: NewFakeBootloader(demo(t, v5244))}
	l.onWindowAck = func(protocol.Packet) protocol.Packet {
		return protocol.Packet{Cmd: CmdFirmware, Flags: flagsControl, Seq: 0x42, Payload: []byte{OpWrite, 0x01}}
	}
	err := flashFast(t, l, Options{})
	if err == nil || !strings.Contains(err.Error(), "pass 1 of 2: page 1 of 126") || !strings.Contains(err.Error(), "rejected the write (status 0x01)") {
		t.Fatalf("got %v", err)
	}
}

func TestFlashFinalResetFailureSaysInstalled(t *testing.T) {
	l := &tamperLink{FakeBootloader: NewFakeBootloader(demo(t, v5244)), failResetAt: 4}
	err := flashFast(t, l, Options{})
	if err == nil || !strings.Contains(err.Error(), "installed") || !strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("got %v", err)
	}
}

func TestFlashFromBootloaderSkipsPreamble(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.InBootloader = true
	if _, err := flash(t, f, demo(t, v5236), Options{FromBootloader: true}); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.Sent {
		if s[0] == protocol.CmdPower && s[1] == 0x21 {
			t.Errorf("sent enter-update frame %x", s)
		}
		if s[0] == CmdFirmware && s[1]&0x80 == 0 && len(s) > 4 && s[4] == OpInfo {
			t.Errorf("sent op 00: %x", s)
		}
	}
}
