// Package protocol encodes and decodes the Xbox Elite Series 2 configuration
// protocol (GIP vendor command 0x4D). It does no I/O, so it can be tested
// directly against the USBPcap captures. See ../../PROTOCOL.md.
//
// GIP frame: [cmd] [flags] [seq] [len] [payload...]
// flags 0x20 = system message, 0x10 = receiver must ACK.
//
// Config command 0x4D payload: [op] ...
//
//	op 0x01 WRITE   host: 01 <page> <size> <data>   device: 01 <status> <page>
//	op 0x02 READ    host: 02 <page> <size>          device: 02 <status> <page> <size> <data>
//	op 0x03 COMMIT  host: 03 (also "unlock")        device: 03 <status> <page>
//	op 0x07 INIT    host: 07 00                     device: 07 00
package protocol

import (
	"errors"
	"fmt"
)

const (
	CmdAck    = 0x01
	CmdPower  = 0x05
	CmdInput  = 0x20
	CmdConfig = 0x4D
	CmdLED    = 0x0E // live profile-color preview

	FlagSystem   = 0x20
	FlagNeedsAck = 0x10

	OpWrite  = 0x01
	OpRead   = 0x02
	OpCommit = 0x03
	OpInit   = 0x07

	MapSize   = 0x38 // 56-byte button mapping page
	CurveSize = 0x2B // 43-byte stick curve page

	// FlagCustom is set on byte 0 of every page Xbox Accessories saves (0x10 -> 0x11).
	FlagCustom = 0x01
)

var Profiles = []int{1, 2, 3}

type Mode int

const (
	Standard Mode = iota
	Shift
)

func (m Mode) String() string {
	if m == Shift {
		return "Shift"
	}
	return "Standard"
}

// Pages holds the four page IDs that make up one profile.
type Pages struct {
	MapStandard, CurvesStandard, MapShift, CurvesShift byte
}

// Map returns the mapping page for a mode.
func (p Pages) Map(m Mode) byte {
	if m == Shift {
		return p.MapShift
	}
	return p.MapStandard
}

// WriteOrder is the order Xbox Accessories writes a profile in (20, 26, 21, 27).
func (p Pages) WriteOrder() []byte {
	return []byte{p.MapStandard, p.MapShift, p.CurvesStandard, p.CurvesShift}
}

// PageIDs returns the page IDs for profile 1-3.
func PageIDs(profile int) Pages {
	if profile < 1 || profile > 3 {
		panic(fmt.Sprintf("profile must be 1-3, got %d", profile))
	}
	base := byte(0x20 + 2*(profile-1))
	return Pages{base, base + 1, base + 6, base + 7}
}

func PageSize(page byte) int {
	if (page-0x20)%2 == 0 {
		return MapSize
	}
	return CurveSize
}

// ProfileForPage maps a page ID back to its profile, or 0 if unknown.
func ProfileForPage(page byte) int {
	idx := int(page) - 0x20
	if idx < 0 || idx >= 12 {
		return 0
	}
	return (idx%6)/2 + 1
}

// Output codes a physical control can be mapped to, in cycling order.
var OutputOrder = []byte{0x00, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F}

var outputNames = map[byte]string{
	0x00: "Disabled", 0x04: "A", 0x05: "B", 0x06: "X", 0x07: "Y",
	0x08: "D-pad Up", 0x09: "D-pad Down", 0x0A: "D-pad Left", 0x0B: "D-pad Right",
	0x0C: "LB", 0x0D: "RB", 0x0E: "LS click", 0x0F: "RS click",
}

func OutputName(code byte) string {
	if n, ok := outputNames[code]; ok {
		return n
	}
	return fmt.Sprintf("unknown 0x%02x", code)
}

// Input is a remappable control: its byte offset in the mapping page and factory default.
type Input struct {
	Label   string
	Offset  int
	Default byte
}

// Inputs lists the remappable controls. Paddle order matches the paddle bitfield
// in input reports (byte 18, bit0..bit3), which is how the capture tied page
// bytes 1-4 to paddles. Positions follow the Linux xpad driver's naming.
// Paddle defaults (B, Y, A, X) are what the untouched profiles 2 and 3 contain.
var Inputs = []Input{
	{"P1 (upper right)", 1, 0x05},
	{"P2 (lower right)", 2, 0x07},
	{"P3 (upper left)", 3, 0x04},
	{"P4 (lower left)", 4, 0x06},
	{"A", 5, 0x04},
	{"B", 6, 0x05},
	{"X", 7, 0x06},
	{"Y", 8, 0x07},
	{"D-pad Up", 9, 0x08},
	{"D-pad Down", 10, 0x09},
	{"D-pad Left", 11, 0x0A},
	{"D-pad Right", 12, 0x0B},
	{"LB", 13, 0x0C},
	{"RB", 14, 0x0D},
	{"LS click", 15, 0x0E},
	{"RS click", 16, 0x0F},
}

const PaddleCount = 4

// MappingPage is a 56-byte mapping page. Unknown bytes are carried through untouched.
type MappingPage [MapSize]byte

func ParseMappingPage(data []byte) (MappingPage, error) {
	var m MappingPage
	if len(data) != MapSize {
		return m, fmt.Errorf("mapping page must be %d bytes, got %d", MapSize, len(data))
	}
	copy(m[:], data)
	return m, nil
}

func (m *MappingPage) Flags() byte { return m[0] }

// Profile color: byte 45 is 0xFF for the default color or 0x00 for custom,
// bytes 46-48 are R, G, B. Xbox Accessories writes the same color to the
// standard and shift pages of a profile.
const (
	colorFlagOffset = 45
	colorOffset     = 46
	colorDefault    = 0xFF
	colorCustom     = 0x00
)

// Color returns the profile color and whether it is a custom one.
func (m *MappingPage) Color() (rgb [3]byte, custom bool) {
	copy(rgb[:], m[colorOffset:colorOffset+3])
	return rgb, m[colorFlagOffset] != colorDefault
}

// SetColor stores a custom color, or the default one when custom is false.
func (m *MappingPage) SetColor(rgb [3]byte, custom bool) {
	if !custom {
		m[colorFlagOffset] = colorDefault
		rgb = [3]byte{}
	} else {
		m[colorFlagOffset] = colorCustom
	}
	copy(m[colorOffset:], rgb[:])
}

func (m *MappingPage) ResetButtons() {
	for _, in := range Inputs {
		m[in.Offset] = in.Default
	}
}

// MarkCustom returns a copy of a page with the "saved by app" flag set.
func MarkCustom(data []byte) []byte {
	out := append([]byte(nil), data...)
	out[0] |= FlagCustom
	return out
}

// --- GIP framing ---------------------------------------------------------

func Frame(cmd, flags, seq byte, payload []byte) []byte {
	if len(payload) > 0x7F {
		panic("payload too long for single-byte GIP length")
	}
	return append([]byte{cmd, flags, seq, byte(len(payload))}, payload...)
}

func configFrame(seq byte, payload ...byte) []byte {
	return Frame(CmdConfig, FlagNeedsAck, seq, payload)
}

func ReadRequest(seq, page byte) []byte {
	return configFrame(seq, OpRead, page, byte(PageSize(page)))
}

func WriteRequest(seq, page byte, data []byte) ([]byte, error) {
	if len(data) != PageSize(page) {
		return nil, fmt.Errorf("page 0x%02x needs %d bytes, got %d", page, PageSize(page), len(data))
	}
	return configFrame(seq, append([]byte{OpWrite, page, byte(len(data))}, data...)...), nil
}

// LEDPreview shows a color on the controller without storing it. Matches
// capture: `0e 00 01 05 00 00 70 ff 5d` while picking #70ff5d.
func LEDPreview(seq byte, rgb [3]byte) []byte {
	return Frame(CmdLED, 0x00, seq, []byte{0x00, 0x00, rgb[0], rgb[1], rgb[2]})
}

// LEDPreviewEnd returns the light to the stored color (`0e 00 05 05 01 00 00 00 00`).
func LEDPreviewEnd(seq byte) []byte {
	return Frame(CmdLED, 0x00, seq, []byte{0x01, 0x00, 0x00, 0x00, 0x00})
}

func CommitRequest(seq byte) []byte { return configFrame(seq, OpCommit) }

func InitRequest(seq byte) []byte { return configFrame(seq, OpInit, 0x00) }

// PowerOnRequest is the standard GIP "power on" drivers send at connect.
func PowerOnRequest(seq byte) []byte { return Frame(CmdPower, FlagSystem, seq, []byte{0x00}) }

// AckFor ACKs a device frame that had FlagNeedsAck. Matches capture:
// device `4d 10 03 3c ...` -> host `01 20 03 09 00 4d 00 3c 00 00 00 00 00`.
func AckFor(pkt []byte) []byte {
	return Frame(CmdAck, FlagSystem, pkt[2], []byte{0x00, pkt[0], 0x00, pkt[3], 0, 0, 0, 0, 0})
}

type Packet struct {
	Cmd, Flags, Seq byte
	Payload         []byte
}

func ParsePacket(data []byte) (Packet, error) {
	if len(data) < 4 {
		return Packet{}, errors.New("short GIP packet")
	}
	end := 4 + int(data[3])
	if end > len(data) {
		end = len(data)
	}
	return Packet{data[0], data[1], data[2], data[4:end]}, nil
}

func (p Packet) NeedsAck() bool { return p.Flags&FlagNeedsAck != 0 }

// ReplyMatches reports whether a device 0x4D frame answers request req.
// Replies carry the controller's own sequence counter, not the request's, so
// they are matched by op code (and page, for reads and writes) instead.
func ReplyMatches(req []byte, reply Packet) bool {
	if len(req) < 5 || reply.Cmd != CmdConfig || len(reply.Payload) == 0 || reply.Payload[0] != req[4] {
		return false
	}
	switch req[4] {
	case OpRead, OpWrite:
		return len(req) >= 6 && len(reply.Payload) >= 3 && reply.Payload[2] == req[5]
	}
	return true
}

// ActivePageFromCommit reads the active profile's mapping page from a COMMIT reply `03 <status> <page>`.
func ActivePageFromCommit(payload []byte) (byte, bool) {
	if len(payload) >= 3 && payload[0] == OpCommit {
		return payload[2], true
	}
	return 0, false
}

// ParseReadResponse returns (page, data) from a 0x4D op-0x02 response payload.
func ParseReadResponse(payload []byte) (byte, []byte, error) {
	if len(payload) < 4 || payload[0] != OpRead {
		return 0, nil, fmt.Errorf("not a read response: %x", payload)
	}
	page, size := payload[2], int(payload[3])
	data := payload[4:]
	if len(data) < size {
		return 0, nil, fmt.Errorf("truncated page 0x%02x: %d/%d", page, len(data), size)
	}
	return page, data[:size], nil
}

// --- input reports ---------------------------------------------------------

type bitName struct {
	bit  byte
	name string
}

var (
	faceBits   = []bitName{{0x10, "A"}, {0x20, "B"}, {0x40, "X"}, {0x80, "Y"}, {0x04, "Menu"}, {0x08, "View"}}
	padBits    = []bitName{{0x01, "Up"}, {0x02, "Down"}, {0x04, "Left"}, {0x08, "Right"}, {0x10, "LB"}, {0x20, "RB"}, {0x40, "LS"}, {0x80, "RS"}}
	paddleBits = []bitName{{0x01, "P1"}, {0x02, "P2"}, {0x04, "P3"}, {0x08, "P4"}}
)

// PressedFromInput lists held controls from a 0x20 input report payload. Face and
// d-pad bits already reflect the active profile's remap; paddle bits
// (payload byte 14 = packet byte 18) are the raw paddles.
func PressedFromInput(payload []byte) []string {
	var out []string
	add := func(b byte, names []bitName) {
		for _, bn := range names {
			if b&bn.bit != 0 {
				out = append(out, bn.name)
			}
		}
	}
	if len(payload) >= 2 {
		add(payload[0], faceBits)
		add(payload[1], padBits)
	}
	if len(payload) >= 6 {
		if int(payload[2])|int(payload[3])<<8 > 100 {
			out = append(out, "LT")
		}
		if int(payload[4])|int(payload[5])<<8 > 100 {
			out = append(out, "RT")
		}
	}
	if len(payload) >= 15 {
		add(payload[14], paddleBits)
	}
	return out
}
