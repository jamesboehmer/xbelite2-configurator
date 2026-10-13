package device

import (
	"errors"
	"testing"

	"xbelite2-configurator/internal/fixture"
	"xbelite2-configurator/internal/protocol"
)

func TestFakeCalibration(t *testing.T) {
	f := NewFake(fixture.Pages())
	before, err := f.ReadCalibration(protocol.CalLeftStick)
	if err != nil || before.Calibrated || before.Values != [4]uint16{851, 3211, 803, 3270} {
		t.Fatalf("default left stick %+v %v", before, err)
	}
	if err := f.StartCalibration(protocol.CalLeftStick); err != nil {
		t.Fatal(err)
	}
	after, _ := f.ReadCalibration(protocol.CalLeftStick)
	if !after.Calibrated || after.Values == before.Values {
		t.Fatalf("after %+v", after)
	}
	if trig, _ := f.ReadCalibration(protocol.CalRightTrigger); trig.Extra != 4 {
		t.Errorf("trigger extra %d", trig.Extra)
	}
	if _, err := f.ReadCalibration(0x03); err == nil {
		t.Error("unknown target accepted")
	}
	f.CalErr = errors.New("unplugged")
	if err := f.StartCalibration(protocol.CalLeftStick); err == nil {
		t.Error("CalErr ignored")
	}
}

func TestFakeRumbleInitCommitAxes(t *testing.T) {
	f := NewFake(fixture.Pages())
	f.Rumble(protocol.Motors{LeftGrip: 50})
	f.Init()
	f.Commit()
	f.SetAxes(protocol.Axes{LX: 100})
	if len(f.Rumbles) != 1 || f.Rumbles[0].LeftGrip != 50 || f.Inits != 1 || f.Commits != 1 || f.Axes().LX != 100 {
		t.Fatalf("%+v", f)
	}
}
