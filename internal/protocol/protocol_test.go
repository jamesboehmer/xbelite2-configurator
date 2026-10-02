package protocol

import (
	"encoding/hex"
	"reflect"
	"testing"
)

// Expected bytes are copied from buttonremap.pcapng.
var profile1After, _ = hex.DecodeString("11070506040405060708090a0b0c0d0e0f000000000000000000000064646464" +
	"ff00ff000000ff00ff000000640070ff5d30300000000000")

func eqHex(t *testing.T, got []byte, want string) {
	t.Helper()
	if hex.EncodeToString(got) != want {
		t.Errorf("got %x, want %s", got, want)
	}
}

func TestReadRequestMatchesCapture(t *testing.T) {
	eqHex(t, ReadRequest(0x03, 0x20), "4d100303022038")
	eqHex(t, ReadRequest(0x05, 0x21), "4d10050302212b")
}

func TestWriteRequestMatchesCapture(t *testing.T) {
	req, err := WriteRequest(0x1d, 0x20, profile1After)
	if err != nil {
		t.Fatal(err)
	}
	eqHex(t, req, "4d101d3b012038"+hex.EncodeToString(profile1After))
	if _, err := WriteRequest(1, 0x20, profile1After[:10]); err == nil {
		t.Error("short page accepted")
	}
}

func TestInitCommit(t *testing.T) {
	eqHex(t, InitRequest(0x01), "4d1001020700")
	eqHex(t, CommitRequest(0x0f), "4d100f0103")
}

func TestAckMatchesCapture(t *testing.T) {
	dev, _ := hex.DecodeString("4d10033c028020")
	eqHex(t, AckFor(append(dev, make([]byte, 56)...)), "01200309004d003c0000000000")
}

func TestPageIDs(t *testing.T) {
	if got := PageIDs(1); got != (Pages{0x20, 0x21, 0x26, 0x27}) {
		t.Errorf("profile 1: %+v", got)
	}
	if PageIDs(3).MapShift != 0x2A {
		t.Error("profile 3 shift map")
	}
	for _, p := range Profiles {
		for _, page := range PageIDs(p).WriteOrder() {
			if ProfileForPage(page) != p {
				t.Errorf("page %#x -> profile %d, want %d", page, ProfileForPage(page), p)
			}
		}
	}
}

func TestCommitReplyNamesActivePage(t *testing.T) {
	if page, ok := ActivePageFromCommit([]byte{0x03, 0x80, 0x20}); !ok || page != 0x20 {
		t.Errorf("got %#x %v", page, ok)
	}
}

func TestPaddleByte(t *testing.T) {
	payload := make([]byte, 47)
	payload[0], payload[14] = 0x20, 0x01
	if got := PressedFromInput(payload); !reflect.DeepEqual(got, []string{"B", "P1"}) {
		t.Errorf("got %v", got)
	}
}

func TestReplyMatchesIgnoresSeq(t *testing.T) {
	// From a real session: INIT sent as seq 02, answered as seq 01.
	reply, _ := ParsePacket([]byte{0x4d, 0x00, 0x01, 0x02, 0x07, 0x00})
	if !ReplyMatches(InitRequest(0x02), reply) {
		t.Error("INIT reply with controller's own seq not matched")
	}
	if ReplyMatches(CommitRequest(0x02), reply) {
		t.Error("INIT reply matched a COMMIT request")
	}
	read, _ := hex.DecodeString("4d10123c028020381107")
	pkt, _ := ParsePacket(read)
	if !ReplyMatches(ReadRequest(0x05, 0x20), pkt) || ReplyMatches(ReadRequest(0x05, 0x26), pkt) {
		t.Error("read reply page matching wrong")
	}
	write, _ := ParsePacket([]byte{0x4d, 0x00, 0x09, 0x03, 0x01, 0x00, 0x26})
	req, _ := WriteRequest(0x30, 0x26, make([]byte, MapSize))
	if !ReplyMatches(req, write) {
		t.Error("write reply not matched")
	}
}

func TestLEDPreviewMatchesCapture(t *testing.T) {
	eqHex(t, LEDPreview(0x01, [3]byte{0x70, 0xff, 0x5d}), "0e0001050000"+"70ff5d")
	eqHex(t, LEDPreviewEnd(0x05), "0e0005050100000000")
}

func TestColor(t *testing.T) {
	m, _ := ParseMappingPage(profile1After)
	if rgb, custom := m.Color(); !custom || rgb != [3]byte{0x70, 0xff, 0x5d} {
		t.Errorf("profile 1 color = %x custom=%v", rgb, custom)
	}
	m.SetColor([3]byte{1, 2, 3}, false)
	if rgb, custom := m.Color(); custom || rgb != [3]byte{} || m[45] != 0xff {
		t.Errorf("default color: %x %v flag %x", rgb, custom, m[45])
	}
	m.SetColor([3]byte{1, 2, 3}, true)
	eqHex(t, m[44:49], "6400010203")
}
