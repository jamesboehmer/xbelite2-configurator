package protocol

import (
	"encoding/binary"
	"errors"
)

// Rumble (cmd 0x09) and stick/trigger calibration (cmd 0x1e), decoded from
// captures of Xbox Accessories. See ../../PROTOCOL.md.
//
//	rumble    host: 09 00 s 09 00 0f <LT> <RT> <Lgrip> <Rgrip> ff 00 eb
//	cal read  host: 1e 10 s 02 0f <t>   device: 0f 00 <state> <t> [<extra>] <4 x u16>
//	cal start host: 1e 10 s 02 10 <t>   device: 10 00 <t>
const (
	CmdRumble      = 0x09
	CmdCalibration = 0x1E

	OpCalRead  = 0x0F
	OpCalStart = 0x10

	CalLeftStick    byte = 0x01
	CalRightStick   byte = 0x02
	CalLeftTrigger  byte = 0x04
	CalRightTrigger byte = 0x08

	calCalibrated = 0x10 // reply state after a user calibration
)

// Motors are rumble levels, 0-100, in the order the rumble command uses.
type Motors struct {
	LeftTrigger, RightTrigger, LeftGrip, RightGrip byte
}

// RumbleRequest runs the motors at these levels until the next rumble frame.
func RumbleRequest(seq byte, m Motors) []byte {
	return Frame(CmdRumble, 0x00, seq, []byte{0x00, 0x0f, m.LeftTrigger, m.RightTrigger, m.LeftGrip, m.RightGrip, 0xff, 0x00, 0xeb})
}

func CalibrationRead(seq, target byte) []byte {
	return Frame(CmdCalibration, FlagNeedsAck, seq, []byte{OpCalRead, target})
}

func CalibrationStart(seq, target byte) []byte {
	return Frame(CmdCalibration, FlagNeedsAck, seq, []byte{OpCalStart, target})
}

// Calibration is what a calibration read reports. Sticks carry X min/max and
// Y min/max; triggers carry an extra byte and four values whose meaning is unknown.
type Calibration struct {
	Target     byte
	Calibrated bool // a user calibration is stored
	Extra      byte
	Values     [4]uint16
}

func ParseCalibration(p []byte) (Calibration, error) {
	if len(p) < 4 || p[0] != OpCalRead {
		return Calibration{}, errors.New("not a calibration read reply")
	}
	c := Calibration{Target: p[3], Calibrated: p[2] == calCalibrated}
	v := p[4:]
	if c.Target == CalLeftTrigger || c.Target == CalRightTrigger {
		if len(v) < 1 {
			return Calibration{}, errors.New("short calibration reply")
		}
		c.Extra, v = v[0], v[1:]
	}
	if len(v) < 8 {
		return Calibration{}, errors.New("short calibration reply")
	}
	for i := range c.Values {
		c.Values[i] = binary.LittleEndian.Uint16(v[2*i:])
	}
	return c, nil
}

// Axes are the sticks and triggers from an input report (cmd 0x20).
type Axes struct {
	LT, RT         uint16 // 0-1023
	LX, LY, RX, RY int16  // up and right are positive
}

func AxesFromInput(p []byte) Axes {
	if len(p) < 14 {
		return Axes{}
	}
	le := binary.LittleEndian
	return Axes{
		LT: le.Uint16(p[2:]), RT: le.Uint16(p[4:]),
		LX: int16(le.Uint16(p[6:])), LY: int16(le.Uint16(p[8:])),
		RX: int16(le.Uint16(p[10:])), RY: int16(le.Uint16(p[12:])),
	}
}

// Vibration strength: bytes 28-31 of a mapping page, 0-100, stored the same in
// a profile's standard and shift pages. Order confirmed on a controller.
const vibrationOffset = 28 // right grip, left grip, right trigger, left trigger

func (m *MappingPage) Vibration() Motors {
	return Motors{LeftTrigger: m[vibrationOffset+3], RightTrigger: m[vibrationOffset+2],
		LeftGrip: m[vibrationOffset+1], RightGrip: m[vibrationOffset]}
}

func (m *MappingPage) SetVibration(v Motors) {
	m[vibrationOffset], m[vibrationOffset+1], m[vibrationOffset+2], m[vibrationOffset+3] =
		v.RightGrip, v.LeftGrip, v.RightTrigger, v.LeftTrigger
}
