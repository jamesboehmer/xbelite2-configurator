package firmware

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"xbelite2-configurator/internal/protocol"
)

func eqHex(t *testing.T, got []byte, want string) {
	t.Helper()
	if hex.EncodeToString(got) != want {
		t.Errorf("got  %x\nwant %s", got, want)
	}
}

func TestRequestsMatchCapture(t *testing.T) {
	staging := &Region{Type: RegionStaging, Addr: AppBase, Version: Version{5, 23, 330, 0}, Data: make([]byte, 0x7e000)}
	eqHex(t, InfoRequest(0x34), "0c30340100")
	eqHex(t, RegionRequest(0x35, RegionStaging), "0c3035050104000000")
	eqHex(t, StartRequest(0x3a, staging), "0c303a1502040000000070021000e00700050017004a010000")
	eqHex(t, HashRequest(0x40, 0x10027000), "0c304009030070021000100000")
	eqHex(t, FinishRequest(0x30), "0c30300105")
	eqHex(t, ResetRequest(0x3b), "05203b0107")
	eqHex(t, LEDRequest(0x3d), "0a203d03000114")
	eqHex(t, EnterUpdateRequest(0x33), "0521330101")
}

func TestReadVarint(t *testing.T) {
	for in, want := range map[string][2]int{"3a": {58, 1}, "ba00": {58, 2}, "8820": {4104, 2}, "8a04": {522, 2}} {
		b, _ := hex.DecodeString(in)
		if v, n := readVarint(b); v != want[0] || n != want[1] {
			t.Errorf("%s: got %d,%d want %v", in, v, n, want)
		}
	}
}

func TestChunkPageMatchesCapture(t *testing.T) {
	page := make([]byte, PageSize)
	for i := range page {
		page[i] = byte(i * 7)
	}
	w := ChunkPage(0x41, 0x10027000, page)
	if len(w.Windows) != 9 {
		t.Fatalf("%d windows, want 9", len(w.Windows))
	}
	wantAcks := []int{58, 570, 1082, 1594, 2106, 2618, 3130, 3642, 4104}
	for i, a := range wantAcks {
		if w.AckAt[i] != a {
			t.Errorf("ack %d at %d, want %d", i, w.AckAt[i], a)
		}
	}
	first := w.Windows[0][0]
	eqHex(t, first[:14], "0cf0413a88200400000000700210")
	eqHex(t, w.Windows[1][0][:6], "0ca041ba003a")
	eqHex(t, w.Windows[1][1][:6], "0ca041ba0074")
	eqHex(t, w.Windows[1][2][:6], "0ca0413aae01")
	eqHex(t, w.Windows[1][8][:6], "0cb041308a04")
	last := w.Windows[8][len(w.Windows[8])-1]
	eqHex(t, last[:6], "0cb04138d01f")
	eqHex(t, w.Terminator, "0ca041008820")

	// Reassemble by the offsets in the headers; every frame fits a 64-byte report.
	msg := make([]byte, 4104)
	for wi, win := range w.Windows {
		for _, c := range win {
			if len(c) > 64 {
				t.Fatalf("chunk of %d bytes", len(c))
			}
			ln, n := readVarint(c[3:])
			off, m := readVarint(c[3+n:])
			if wi == 0 {
				off = 0
			}
			copy(msg[off:], c[3+n+m:3+n+m+ln])
		}
	}
	if msg[0] != OpWrite || binary.LittleEndian.Uint32(msg[4:]) != 0x10027000 || !bytes.Equal(msg[8:], page) {
		t.Error("reassembled message differs")
	}
}

func TestParseAck(t *testing.T) {
	b, _ := hex.DecodeString("01204109000c203a0200000002")
	p, _ := protocol.ParsePacket(b)
	a, ok := ParseAck(p)
	if !ok || a != (Ack{Seq: 0x41, Received: 570, Window: 512}) {
		t.Errorf("got %+v %v", a, ok)
	}
	b, _ = hex.DecodeString("01200109004d00020000000000")
	p, _ = protocol.ParsePacket(b)
	if _, ok := ParseAck(p); ok {
		t.Error("ACK of a 0x4D frame parsed as firmware ACK")
	}
}

func TestParseReplies(t *testing.T) {
	body, _ := hex.DecodeString("04000000050018004a010000453a251a0070021000e00700")
	typ, v, err := parseRegionVersion(body)
	if err != nil || typ != RegionStaging || v != (Version{5, 24, 330, 0}) {
		t.Errorf("region: %d %s %v", typ, v, err)
	}
	body, _ = hex.DecodeString("02005e04000b0010000000100000f049020003000000")
	in, err := parseInfo(body)
	if err != nil || in != (info{VID: 0x045e, PID: 0x0b00, PageSize: 0x1000}) {
		t.Errorf("info: %+v %v", in, err)
	}
}
