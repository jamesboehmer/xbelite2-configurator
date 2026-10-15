package calibrate

import (
	"math"
	"testing"
	"time"

	"xbelite2-configurator/internal/protocol"
)

func stick(k Kind, x, y float64) protocol.Axes {
	ix, iy := int16(x*32767), int16(y*32767)
	if k == LeftStick {
		return protocol.Axes{LX: ix, LY: iy}
	}
	return protocol.Axes{RX: ix, RY: iy}
}

// circle feeds turns full turns (negative = clockwise) at radius r, 10° per sample.
func circle(d *Detector, k Kind, turns, r float64) int {
	events := 0
	steps := int(math.Abs(turns) * 36)
	for i := 0; i <= steps; i++ {
		a := math.Copysign(float64(i)*10, turns) * math.Pi / 180
		if d.Feed(stick(k, r*math.Cos(a), r*math.Sin(a))) {
			events++
		}
	}
	return events
}

func push(d *Detector, k Kind, x, y float64) int {
	events := 0
	for _, s := range [][2]float64{{0, 0}, {x * 0.5, y * 0.5}, {x, y}, {x * 0.5, y * 0.5}, {0.05, 0.05}} {
		if d.Feed(stick(k, s[0], s[1])) {
			events++
		}
	}
	return events
}

func TestStickFullSequence(t *testing.T) {
	for _, first := range []float64{6.2, -6.2} {
		d := New(RightStick)
		if n := circle(d, RightStick, first, 1); n != 1 || d.Step() != 1 {
			t.Fatalf("first direction %v: %d events, step %d", first, n, d.Step())
		}
		if n := circle(d, RightStick, -first, 1); n != 1 || d.Step() != 2 {
			t.Fatalf("other direction: %d events, step %d", n, d.Step())
		}
		for i, p := range [][2]float64{{0, 1}, {1, 0}, {0, -1}, {-1, 0}} {
			if n := push(d, RightStick, p[0], p[1]); n != 1 {
				t.Fatalf("push %d: %d events", i, n)
			}
		}
		if !d.Done() || d.Progress() != "" {
			t.Fatalf("not done: step %d", d.Step())
		}
	}
}

func TestStickTurnsAwayFromTheRimDontCount(t *testing.T) {
	d := New(LeftStick)
	if n := circle(d, LeftStick, 7, 0.7); n != 0 || d.Step() != 0 {
		t.Fatalf("turns at 70%% counted: %d events", n)
	}
}

func TestStickSameDirectionTwiceDoesNotFinishStepTwo(t *testing.T) {
	d := New(LeftStick)
	circle(d, LeftStick, 6.2, 1)
	circle(d, LeftStick, 6.2, 1)
	if d.Step() != 1 {
		t.Fatalf("step %d, want 1", d.Step())
	}
}

func TestStickPushesMustBeInOrder(t *testing.T) {
	d := New(LeftStick)
	circle(d, LeftStick, 6.2, 1)
	circle(d, LeftStick, -6.2, 1)
	if n := push(d, LeftStick, 1, 0); n != 0 {
		t.Fatal("a right push counted before up")
	}
	if n := push(d, LeftStick, 0, 1); n != 1 {
		t.Fatal("up push not counted")
	}
}

func pull(d *Detector, k Kind, peak float64) int {
	events := 0
	for _, v := range []float64{0, peak / 2, peak, peak / 2, 0} {
		a := protocol.Axes{LT: uint16(v * 1023)}
		if k == RightTrigger {
			a = protocol.Axes{RT: uint16(v * 1023)}
		}
		if d.Feed(a) {
			events++
		}
	}
	return events
}

func TestTriggerPulls(t *testing.T) {
	d := New(LeftTrigger)
	for round := 0; round < 3; round++ {
		if pull(d, LeftTrigger, 0.5) != 0 {
			t.Fatal("a half pull counted")
		}
		events := 0
		for i := 0; i < 12; i++ {
			events += pull(d, LeftTrigger, 1)
		}
		if events != 1 || d.Step() != round+1 {
			t.Fatalf("round %d: %d events, step %d", round, events, d.Step())
		}
	}
	if !d.Done() {
		t.Fatal("not done after 36 pulls")
	}
}

func TestProgress(t *testing.T) {
	d := New(LeftStick)
	circle(d, LeftStick, 3.2, 1)
	if d.Progress() != "3 of 6 turns" {
		t.Errorf("got %q", d.Progress())
	}
	tr := New(RightTrigger)
	pull(tr, RightTrigger, 1)
	if tr.Progress() != "1 of 12 pulls" {
		t.Errorf("got %q", tr.Progress())
	}
}

func TestSkip(t *testing.T) {
	d := New(LeftTrigger)
	for range d.Steps() {
		d.Skip()
	}
	if !d.Done() {
		t.Fatal("skip didn't finish")
	}
	d.Skip() // no panic past the end
}

func TestRestRestartsWhenMoved(t *testing.T) {
	r := NewRest(LeftStick, time.Second)
	t0 := time.Unix(0, 0)
	if r.Feed(t0, stick(LeftStick, 0.05, 0)) {
		t.Fatal("rested immediately")
	}
	if r.Feed(t0.Add(900*time.Millisecond), stick(LeftStick, 0.5, 0)) {
		t.Fatal("rested while pushed")
	}
	if r.Feed(t0.Add(1500*time.Millisecond), stick(LeftStick, 0, 0.1)) {
		t.Fatal("rest didn't restart after moving")
	}
	if !r.Feed(t0.Add(2600*time.Millisecond), stick(LeftStick, 0, 0.1)) {
		t.Fatal("not rested after a full second")
	}
	tr := NewRest(RightTrigger, 0)
	if tr.Feed(t0, protocol.Axes{RT: 200}) || !tr.Feed(t0, protocol.Axes{RT: 10}) {
		t.Fatal("trigger rest")
	}
}

func TestKindForTarget(t *testing.T) {
	for _, k := range Kinds {
		if got, ok := KindForTarget(k.Target()); !ok || got != k {
			t.Errorf("%s", k)
		}
	}
	if _, ok := KindForTarget(0x03); ok {
		t.Error("0x03 accepted")
	}
}

func TestStickOvershootDoesNotCountAgainstStepTwo(t *testing.T) {
	d := New(LeftStick)
	events := circle(d, LeftStick, 6.6, 1)
	// Carry on from where the circle ended, back the other way.
	start := 237.0 * 10
	for i := 1; i <= 217; i++ {
		a := (start - float64(i)*10) * math.Pi / 180
		if d.Feed(stick(LeftStick, math.Cos(a), math.Sin(a))) {
			events++
		}
	}
	if d.Step() != 2 || events != 2 {
		t.Fatalf("step %d, %d events; want step 2, 2 events", d.Step(), events)
	}
}

func TestStickOvershootThroughTheTopDoesNotCompletePushUp(t *testing.T) {
	d := New(RightStick)
	circle(d, RightStick, 6.2, 1)
	circle(d, RightStick, -6.2, 1) // step 1 completes, still on the rim
	if d.Step() != 2 {
		t.Fatalf("step %d, want 2", d.Step())
	}
	for deg := -80.0; deg >= -300; deg -= 10 { // on through the top
		a := deg * math.Pi / 180
		d.Feed(stick(RightStick, math.Cos(a), math.Sin(a)))
	}
	d.Feed(stick(RightStick, 0, 0))
	if d.Step() != 2 {
		t.Fatalf("overshoot completed push up: step %d", d.Step())
	}
	if n := push(d, RightStick, 0, 1); n != 1 || d.Step() != 3 {
		t.Fatalf("real push: %d events, step %d", n, d.Step())
	}
}

func TestResumeClearsTurnContinuity(t *testing.T) {
	d := New(LeftStick)
	circle(d, LeftStick, 2.5, 1)
	step, turns := d.Step(), d.turns
	d.Resume()
	d.Feed(stick(LeftStick, -1, 0)) // opposite side: no delta against the old angle
	if d.Step() != step || d.turns != turns {
		t.Fatalf("step %d turns %v, want %d %v", d.Step(), d.turns, step, turns)
	}
}
