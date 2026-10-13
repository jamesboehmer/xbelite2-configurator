package protocol

import (
	"encoding/hex"
	"testing"

	"xbelite2-configurator/internal/fixture"
)

func TestRumbleMatchesCapture(t *testing.T) {
	// 4vibes.pcapng: test buzz on at 38.71 s, off at 39.22 s.
	eqHex(t, RumbleRequest(0x07, Motors{0x32, 0x32, 0x32, 0x32}), "09000709000f32323232ff00eb")
	eqHex(t, RumbleRequest(0x08, Motors{}), "09000809000f00000000ff00eb")
	// Field order is left trigger, right trigger, left grip, right grip.
	eqHex(t, RumbleRequest(0x01, Motors{LeftTrigger: 1, RightTrigger: 2, LeftGrip: 3, RightGrip: 4}), "09000109000f01020304ff00eb")
}

func TestCalibrationRequestsMatchCapture(t *testing.T) {
	eqHex(t, CalibrationRead(0x1c, CalLeftStick), "1e101c020f01")
	eqHex(t, CalibrationStart(0x1d, CalLeftStick), "1e101d021001")
	eqHex(t, CalibrationRead(0x34, CalRightTrigger), "1e1034020f08")
}

func TestCalibrationAck(t *testing.T) {
	dev, _ := hex.DecodeString("1e10010c0f00000153038b0c2303c60c")
	eqHex(t, AckFor(dev), "01200109001e000c0000000000")
}

func TestParseCalibrationReplies(t *testing.T) {
	cases := []struct {
		frame      string
		target     byte
		calibrated bool
		extra      byte
		values     [4]uint16
	}{
		{"1e10010c0f00000153038b0c2303c60c", CalLeftStick, false, 0, [4]uint16{851, 3211, 803, 3270}},
		{"1e10030c0f0010013b034d0c4502c80d", CalLeftStick, true, 0, [4]uint16{827, 3149, 581, 3528}},
		{"1e10040c0f0000026f03460c6103f10c", CalRightStick, false, 0, [4]uint16{879, 3142, 865, 3313}},
		{"1e10060c0f0010024e038b0c54039b0b", CalRightStick, true, 0, [4]uint16{846, 3211, 852, 2971}},
		{"1e10070d0f00000404480487055207ee0d", CalLeftTrigger, false, 4, [4]uint16{1096, 1415, 1874, 3566}},
		{"1e10090d0f00100404bd0331058407430d", CalLeftTrigger, true, 4, [4]uint16{957, 1329, 1924, 3395}},
		{"1e100a0d0f000008041d041f051907690d", CalRightTrigger, false, 4, [4]uint16{1053, 1311, 1817, 3433}},
		{"1e100c0d0f00100804d4033c058d07170e", CalRightTrigger, true, 4, [4]uint16{980, 1340, 1933, 3607}},
	}
	for _, c := range cases {
		b, _ := hex.DecodeString(c.frame)
		p, _ := ParsePacket(b)
		got, err := ParseCalibration(p.Payload)
		want := Calibration{Target: c.target, Calibrated: c.calibrated, Extra: c.extra, Values: c.values}
		if err != nil || got != want {
			t.Errorf("%s: got %+v %v, want %+v", c.frame, got, err, want)
		}
	}
	if _, err := ParseCalibration([]byte{0x0f, 0x00, 0x00, 0x01, 0x53}); err == nil {
		t.Error("short reply accepted")
	}
	if _, err := ParseCalibration([]byte{0x10, 0x00, 0x01}); err == nil {
		t.Error("start reply parsed as a read reply")
	}
}

func TestCalibrationReplyMatching(t *testing.T) {
	b, _ := hex.DecodeString("1e10040c0f0000026f03460c6103f10c") // right stick read reply
	read, _ := ParsePacket(b)
	if !ReplyMatches(CalibrationRead(0x05, CalRightStick), read) || ReplyMatches(CalibrationRead(0x05, CalLeftStick), read) {
		t.Error("read reply must match by target")
	}
	b, _ = hex.DecodeString("1e100203100001") // left stick start reply
	start, _ := ParsePacket(b)
	if !ReplyMatches(CalibrationStart(0x09, CalLeftStick), start) || ReplyMatches(CalibrationStart(0x09, CalRightStick), start) {
		t.Error("start reply must match by target")
	}
	if ReplyMatches(InitRequest(0x05), read) {
		t.Error("a 0x1e reply matched a 0x4d request")
	}
}

func TestVibrationBytes(t *testing.T) {
	m, err := ParseMappingPage(fixture.Pages()[0x20])
	if err != nil {
		t.Fatal(err)
	}
	if v := m.Vibration(); v != (Motors{100, 100, 100, 100}) {
		t.Fatalf("fixture vibration %+v", v)
	}
	m.SetVibration(Motors{LeftTrigger: 80, RightTrigger: 81, LeftGrip: 90, RightGrip: 91})
	if got := hex.EncodeToString(m[28:32]); got != "5b5a5150" {
		t.Errorf("bytes 28-31 = %s, want right grip, left grip, right trigger, left trigger", got)
	}
	if v := m.Vibration(); v != (Motors{80, 81, 90, 91}) {
		t.Errorf("round trip %+v", v)
	}
}

func TestAxesFromInput(t *testing.T) {
	p := make([]byte, 18)
	copy(p[2:], []byte{0xff, 0x03, 0x00, 0x02, 0x00, 0x80, 0xff, 0x7f, 0x01, 0x00, 0xfe, 0xff})
	want := Axes{LT: 1023, RT: 512, LX: -32768, LY: 32767, RX: 1, RY: -2}
	if got := AxesFromInput(p); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if AxesFromInput(p[:10]) != (Axes{}) {
		t.Error("short report should give zero axes")
	}
}
