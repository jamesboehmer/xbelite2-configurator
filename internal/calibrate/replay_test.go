package calibrate

import (
	"os"
	"testing"

	"xbelite2-configurator/internal/pcap"
	"xbelite2-configurator/internal/protocol"
)

// The capture isn't committed. To run:
//
//	XBE2_CAL_CAPTURE=$PWD/.tmp/leftstick_rightstick_lefttrigger_righttrigger.pcapng \
//	  go test ./internal/calibrate/ -run Replay -v
//
// Xbox Accessories gave a soft buzz (all motors 0x0a) after each step it saw
// completed. Feeding the same input reports through the detector must produce
// one step event per buzz, none more than half a second after it.
func TestReplayMatchesXboxAccessories(t *testing.T) {
	path := os.Getenv("XBE2_CAL_CAPTURE")
	if path == "" {
		t.Skip("set XBE2_CAL_CAPTURE to the calibration capture to run")
	}
	ts, err := pcap.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	type window struct {
		kind       Kind
		start, end float64
		buzzes     []float64
	}
	var wins []*window
	var cur *window
	for _, tr := range ts {
		d := tr.Data
		if !tr.FromHost || len(d) < 6 {
			continue
		}
		switch {
		case d[0] == protocol.CmdCalibration && d[4] == protocol.OpCalStart:
			k, ok := KindForTarget(d[5])
			if !ok {
				t.Fatalf("unknown target %#x", d[5])
			}
			cur = &window{kind: k, start: tr.Time}
			wins = append(wins, cur)
		case d[0] == protocol.CmdCalibration && d[4] == protocol.OpCalRead && cur != nil && cur.end == 0:
			cur.end = tr.Time
		case d[0] == protocol.CmdRumble && len(d) >= 10 && d[6] == 0x0a && cur != nil && cur.end == 0:
			cur.buzzes = append(cur.buzzes, tr.Time)
		}
	}
	if len(wins) != 4 {
		t.Fatalf("found %d calibrations, want 4", len(wins))
	}
	for _, w := range wins {
		det := New(w.kind)
		var events []float64
		for _, tr := range ts {
			if tr.FromHost || tr.Time < w.start || tr.Time > w.end || len(tr.Data) < 4 || tr.Data[0] != protocol.CmdInput {
				continue
			}
			p, err := protocol.ParsePacket(tr.Data)
			if err != nil {
				continue
			}
			if det.Feed(protocol.AxesFromInput(p.Payload)) {
				events = append(events, tr.Time)
			}
		}
		t.Logf("%s: app buzzed at %.2f, detector stepped at %.2f", w.kind, w.buzzes, events)
		if len(events) != len(w.buzzes) {
			t.Errorf("%s: %d step events, app gave %d buzzes", w.kind, len(events), len(w.buzzes))
			continue
		}
		for i := range events {
			dt := events[i] - w.buzzes[i]
			// The app's step-0 buzz lags the true 6.0-turn point by 0.28-0.51 s,
			// so the detector may lead a turn or trigger buzz by up to 0.75 s.
			// Push steps (stick steps 2-5) may lead by any amount: in the capture
			// the app ignored a full, straight right-stick push made about 2 s
			// after the turn steps.
			early := dt < -0.75
			if w.kind.Stick() && i >= 2 {
				early = false
			}
			if early || dt > 0.5 {
				t.Errorf("%s step %d: detector at %.2f s, app buzzed at %.2f s", w.kind, i+1, events[i], w.buzzes[i])
			}
		}
	}
}
