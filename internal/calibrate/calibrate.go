// Package calibrate follows a user through stick and trigger calibration the
// way Xbox Accessories does, from input reports alone: it says when each step
// is done (so the app can buzz) and when a control is resting. The controller
// measures the calibration itself; this only decides when to move on.
package calibrate

import (
	"fmt"
	"math"
	"time"

	"xbelite2-configurator/internal/protocol"
)

type Kind int

const (
	LeftStick Kind = iota
	RightStick
	LeftTrigger
	RightTrigger
)

var Kinds = []Kind{LeftStick, RightStick, LeftTrigger, RightTrigger}

var targets = [...]byte{protocol.CalLeftStick, protocol.CalRightStick, protocol.CalLeftTrigger, protocol.CalRightTrigger}

func (k Kind) Target() byte { return targets[k] }
func (k Kind) String() string {
	return [...]string{"Left stick", "Right stick", "Left trigger", "Right trigger"}[k]
}
func (k Kind) Stick() bool { return k == LeftStick || k == RightStick }

func KindForTarget(t byte) (Kind, bool) {
	for _, k := range Kinds {
		if k.Target() == t {
			return k, true
		}
	}
	return 0, false
}

// Thresholds, as fractions of full scale; checked against the capture by the replay test.
const (
	rimFrac      = 0.85 // turns count only this far out: the controller records extremes
	pushFrac     = 0.80 // a push reaches this far in its direction...
	centerFrac   = 0.25 // ...then comes back within this of centre
	pullFrac     = 0.60 // a pull goes past this...
	releaseFrac  = 0.10 // ...then back below this
	turnsPerStep = 6
	pullsPerStep = 12

	stickRestFrac   = 0.15
	triggerRestFrac = 0.05
	stickFull       = 32767.0
	triggerFull     = 1023.0

	// RestFor is how long a control must stay at rest before start and the final read.
	RestFor = time.Second
)

var stickSteps = []string{
	"Turn the stick around its edge 6 times, either direction",
	"Now turn it around the edge 6 times the other way",
	"Push the stick all the way up, then let go",
	"Push it all the way right, then let go",
	"Push it all the way down, then let go",
	"Push it all the way left, then let go",
}

var triggerSteps = []string{
	"Set the trigger lock to long throw, then pull the trigger fully 12 times",
	"Set the trigger lock to medium throw, then pull fully 12 times",
	"Set the trigger lock to short throw, then pull fully 12 times",
}

var pushDirs = [4][2]float64{{0, 1}, {1, 0}, {0, -1}, {-1, 0}} // up, right, down, left

// Detector tracks the steps for one stick or trigger.
type Detector struct {
	kind     Kind
	step     int
	turns    float64 // net revolutions this step, signed
	prev     float64
	havePrev bool
	dir      float64 // sign of the first direction
	pushed   bool
	pulls    int
	down     bool
}

func New(k Kind) *Detector { return &Detector{kind: k} }

func (d *Detector) Kind() Kind { return d.kind }

func (d *Detector) Steps() []string {
	if d.kind.Stick() {
		return stickSteps
	}
	return triggerSteps
}

func (d *Detector) Step() int  { return d.step }
func (d *Detector) Done() bool { return d.step >= len(d.Steps()) }

// Progress describes the current step's count, e.g. "3 of 6 turns".
func (d *Detector) Progress() string {
	switch {
	case d.Done():
		return ""
	case !d.kind.Stick():
		return fmt.Sprintf("%d of %d pulls", d.pulls, pullsPerStep)
	case d.step == 0:
		return fmt.Sprintf("%d of %d turns", int(math.Abs(d.turns)), turnsPerStep)
	case d.step == 1:
		return fmt.Sprintf("%d of %d turns", int(math.Max(0, -d.turns*d.dir)), turnsPerStep)
	}
	return ""
}

// Skip completes the current step (demo mode).
func (d *Detector) Skip() {
	if !d.Done() {
		d.next()
	}
}

func (d *Detector) next() {
	d.step++
	d.turns, d.havePrev, d.pushed, d.pulls, d.down = 0, false, false, 0, false
}

// Feed takes one input sample and reports whether it completed a step.
func (d *Detector) Feed(a protocol.Axes) bool {
	if d.Done() {
		return false
	}
	before := d.step
	if d.kind.Stick() {
		d.feedStick(stickXY(d.kind, a))
	} else {
		d.feedTrigger(triggerValue(d.kind, a))
	}
	return d.step != before
}

func (d *Detector) feedStick(x, y float64) {
	r := math.Hypot(x, y)
	if d.step < 2 {
		if r < rimFrac {
			d.havePrev = false
			return
		}
		ang := math.Atan2(y, x)
		if d.havePrev {
			delta := math.Mod(ang-d.prev+3*math.Pi, 2*math.Pi) - math.Pi
			d.turns += delta / (2 * math.Pi)
		}
		d.prev, d.havePrev = ang, true
		switch {
		case d.step == 0 && math.Abs(d.turns) >= turnsPerStep:
			d.dir = math.Copysign(1, d.turns)
			d.next()
		case d.step == 1 && d.turns*d.dir <= -turnsPerStep:
			d.next()
		}
		return
	}
	dir := pushDirs[d.step-2]
	if x*dir[0]+y*dir[1] >= pushFrac {
		d.pushed = true
	}
	if d.pushed && r <= centerFrac {
		d.next()
	}
}

func (d *Detector) feedTrigger(v float64) {
	if v >= pullFrac {
		d.down = true
	}
	if d.down && v <= releaseFrac {
		d.down = false
		d.pulls++
		if d.pulls >= pullsPerStep {
			d.next()
		}
	}
}

func stickXY(k Kind, a protocol.Axes) (float64, float64) {
	if k == LeftStick {
		return float64(a.LX) / stickFull, float64(a.LY) / stickFull
	}
	return float64(a.RX) / stickFull, float64(a.RY) / stickFull
}

func triggerValue(k Kind, a protocol.Axes) float64 {
	if k == LeftTrigger {
		return float64(a.LT) / triggerFull
	}
	return float64(a.RT) / triggerFull
}

// Rest reports when a control has stayed at rest for a while.
type Rest struct {
	kind  Kind
	dur   time.Duration
	since time.Time
	in    bool
}

func NewRest(k Kind, d time.Duration) *Rest { return &Rest{kind: k, dur: d} }

func (r *Rest) Feed(t time.Time, a protocol.Axes) bool {
	var resting bool
	if r.kind.Stick() {
		x, y := stickXY(r.kind, a)
		resting = math.Hypot(x, y) <= stickRestFrac
	} else {
		resting = triggerValue(r.kind, a) <= triggerRestFrac
	}
	if !resting {
		r.in = false
		return false
	}
	if !r.in {
		r.in, r.since = true, t
	}
	return t.Sub(r.since) >= r.dur
}
