package firmware

import (
	"crypto/sha1"
	"errors"
	"testing"
	"time"

	"xbelite2-configurator/internal/protocol"
)

var (
	v5244 = Version{5, 24, 4, 0}
	v5236 = Version{5, 23, 6, 0}
)

func demo(t *testing.T, v Version) *Image {
	t.Helper()
	img, err := Parse(DemoImage(v))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// recvOp returns the next 0x0c reply, skipping ACKs.
func recvOp(t *testing.T, l *FakeBootloader) protocol.Packet {
	t.Helper()
	for {
		p, err := l.Recv(time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if p.Cmd == CmdFirmware {
			return p
		}
	}
}

func TestFakeAsksForResetThenEntersBootloader(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	img := demo(t, v5236)
	f.Send(StartRequest(f.NextSeq(), img.Regions[RegionStaging]))
	if p := recvOp(t, f); p.Payload[1] != StatusReset {
		t.Fatalf("start in normal mode: %x", p.Payload)
	}
	if err := f.Reset(); err != nil || !f.InBootloader {
		t.Fatalf("reset: %v, bootloader %v", err, f.InBootloader)
	}
	f.Send(StartRequest(f.NextSeq(), img.Regions[RegionStaging]))
	if p := recvOp(t, f); p.Payload[1] != StatusOK {
		t.Fatalf("start in bootloader: %x", p.Payload)
	}
}

func TestFakeHashAndChunkedWrite(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.InBootloader = true
	page := demo(t, v5236).Regions[RegionApp].Data[:PageSize]
	w := ChunkPage(f.NextSeq(), AppBase, page)
	for i, win := range w.Windows {
		for _, c := range win {
			f.Send(c)
		}
		p, err := f.Recv(time.Second)
		a, ok := ParseAck(p)
		if err != nil || !ok || a.Received != w.AckAt[i] || a.Seq != w.Seq {
			t.Fatalf("window %d: %+v %v %v", i, a, ok, err)
		}
	}
	f.Send(w.Terminator)
	if p := recvOp(t, f); p.Payload[0] != OpWrite || p.Payload[1] != StatusOK {
		t.Fatalf("write reply %x", p.Payload)
	}
	f.Send(HashRequest(f.NextSeq(), AppBase))
	sum := sha1.Sum(page)
	if p := recvOp(t, f); string(p.Payload[2:]) != string(sum[:]) {
		t.Error("hash after write")
	}
}

func TestFakeSilentTimesOut(t *testing.T) {
	f := NewFakeBootloader(demo(t, v5244))
	f.Silent = true
	f.Send(InfoRequest(f.NextSeq()))
	if _, err := f.Recv(time.Second); !errors.Is(err, ErrTimeout) {
		t.Errorf("got %v", err)
	}
}

func TestDemoImagesDiffer(t *testing.T) {
	a, b := demo(t, v5244), demo(t, v5236)
	if string(a.Regions[RegionApp].Data) == string(b.Regions[RegionApp].Data) {
		t.Error("demo images for different versions are identical")
	}
	if a.Regions[RegionApp].Pages() != 126 || a.Regions[RegionBoot] == nil {
		t.Error("demo image layout")
	}
}
