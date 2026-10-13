package firmware

import (
	"encoding/binary"
	"errors"

	"xbelite2-configurator/internal/protocol"
)

// GIP firmware-update command and the frames around it, as in the capture.
//
//	op 00 info    host: 00                         device: 00 <st> 0200 <vid> <pid> <page LE32> ...
//	op 01 region  host: 01 <type LE32>             device: 01 <st> <type> <maj> <min> <build LE32> ...
//	op 02 start   host: 02 <type> <addr> <size> <maj> <min> <build>   device: 02 <st> (03 = reset first)
//	op 03 hash    host: 03 <addr LE32> <0x1000>    device: 03 <st> <SHA-1 of the 4 KB page>
//	op 04 write   chunked: 04 00 00 00 <addr LE32> <4096 bytes>       device: 04 <st>
//	op 05 finish  host: 05                         device: 05 <st> (03 = reset)
const (
	CmdFirmware = 0x0c
	CmdLEDMode  = 0x0a

	OpInfo   = 0x00
	OpRegion = 0x01
	OpStart  = 0x02
	OpHash   = 0x03
	OpWrite  = 0x04
	OpFinish = 0x05

	StatusOK    = 0x00
	StatusReset = 0x03

	flagsControl = 0x30 // system | ack-request
	chunkFirst   = 0xf0 // chunked | chunk start | system | ack-request
	chunkMore    = 0xa0 // chunked | system
	chunkAck     = 0xb0 // chunked | system | ack-request

	chunkData  = 58  // data bytes per 64-byte report after the 6-byte header
	windowSize = 512 // bytes between device ACKs
)

var le = binary.LittleEndian

func le32(v uint32) []byte { return le.AppendUint32(nil, v) }

func control(seq byte, payload ...byte) []byte {
	return protocol.Frame(CmdFirmware, flagsControl, seq, payload)
}

func InfoRequest(seq byte) []byte { return control(seq, OpInfo) }

func RegionRequest(seq byte, typ uint32) []byte {
	return control(seq, append([]byte{OpRegion}, le32(typ)...)...)
}

// StartRequest copies the region's descriptor: type, address, size, version.
func StartRequest(seq byte, r *Region) []byte {
	p := []byte{OpStart}
	p = le.AppendUint32(p, r.Type)
	p = le.AppendUint32(p, r.Addr)
	p = le.AppendUint32(p, uint32(len(r.Data)))
	p = le.AppendUint16(p, r.Version.Major)
	p = le.AppendUint16(p, r.Version.Minor)
	p = le.AppendUint32(p, r.Version.Build)
	return control(seq, p...)
}

func HashRequest(seq byte, addr uint32) []byte {
	return control(seq, append(append([]byte{OpHash}, le32(addr)...), le32(PageSize)...)...)
}

func FinishRequest(seq byte) []byte { return control(seq, OpFinish) }

// ResetRequest is GIP power "reset": the controller reboots and re-enumerates.
func ResetRequest(seq byte) []byte {
	return protocol.Frame(protocol.CmdPower, protocol.FlagSystem, seq, []byte{0x07})
}

// LEDRequest turns the guide-button LED on at the brightness the app uses.
func LEDRequest(seq byte) []byte {
	return protocol.Frame(CmdLEDMode, protocol.FlagSystem, seq, []byte{0x00, 0x01, 0x14})
}

// EnterUpdateRequest is sent once before op 00 in normal mode. Its meaning is
// unknown; the Windows app sends it, so we do too.
func EnterUpdateRequest(seq byte) []byte {
	return protocol.Frame(protocol.CmdPower, protocol.FlagSystem|0x01, seq, []byte{0x01})
}

// varint encodes v as 7-bit little-endian groups padded to width bytes.
func varint(v, width int) []byte {
	b := make([]byte, width)
	for i := range b {
		b[i] = byte(v & 0x7f)
		v >>= 7
		if i < width-1 {
			b[i] |= 0x80
		}
	}
	if v != 0 {
		panic("varint overflow")
	}
	return b
}

func readVarint(b []byte) (v, n int) {
	for i, c := range b {
		v |= int(c&0x7f) << (7 * i)
		if c&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

// PageWrite is one op 04 message split the way the Windows GIP driver does:
// send each window, wait for the ACK reporting AckAt[i] bytes, then send the
// terminator and wait for the op 04 reply.
type PageWrite struct {
	Seq        byte
	Windows    [][][]byte
	AckAt      []int
	Terminator []byte
}

// ChunkPage builds the chunked message for one 4 KB page. Every chunk header
// is 6 bytes: if the offset fits one varint byte the length takes two.
func ChunkPage(seq byte, addr uint32, page []byte) PageWrite {
	msg := append([]byte{OpWrite, 0, 0, 0}, le32(addr)...)
	msg = append(msg, page...)
	total := len(msg)
	w := PageWrite{Seq: seq}

	first := append([]byte{CmdFirmware, chunkFirst, seq}, varint(chunkData, 1)...)
	first = append(first, varint(total, 2)...)
	w.Windows = append(w.Windows, [][]byte{append(first, msg[:chunkData]...)})
	w.AckAt = append(w.AckAt, chunkData)

	for off := chunkData; off < total; {
		end := min(off+windowSize, total)
		var win [][]byte
		for off < end {
			n := min(chunkData, end-off)
			flags := byte(chunkMore)
			if off+n == end {
				flags = chunkAck
			}
			c := []byte{CmdFirmware, flags, seq}
			if off < 0x80 {
				c = append(append(c, varint(n, 2)...), varint(off, 1)...)
			} else {
				c = append(append(c, varint(n, 1)...), varint(off, 2)...)
			}
			win = append(win, append(c, msg[off:off+n]...))
			off += n
		}
		w.Windows = append(w.Windows, win)
		w.AckAt = append(w.AckAt, end)
	}
	w.Terminator = append(append([]byte{CmdFirmware, chunkMore, seq}, varint(0, 1)...), varint(total, 2)...)
	return w
}

// Ack is the device's ACK of a 0x0c frame: `01 20 <seq> 09 00 0c 20
// <received LE16> 00 00 <window LE16>`, window being what it will take next.
type Ack struct {
	Seq      byte
	Received int
	Window   int
}

func ParseAck(p protocol.Packet) (Ack, bool) {
	if p.Cmd != protocol.CmdAck || len(p.Payload) < 9 || p.Payload[1] != CmdFirmware {
		return Ack{}, false
	}
	return Ack{Seq: p.Seq, Received: int(le.Uint16(p.Payload[3:])), Window: int(le.Uint16(p.Payload[7:]))}, true
}

func parseRegionVersion(body []byte) (uint32, Version, error) {
	if len(body) < 12 {
		return 0, Version{}, errors.New("short region reply")
	}
	return le.Uint32(body), Version{Major: le.Uint16(body[4:]), Minor: le.Uint16(body[6:]), Build: le.Uint32(body[8:])}, nil
}

type info struct {
	VID, PID uint16
	PageSize uint32
}

func parseInfo(body []byte) (info, error) {
	if len(body) < 10 {
		return info{}, errors.New("short info reply")
	}
	return info{VID: le.Uint16(body[2:]), PID: le.Uint16(body[4:]), PageSize: le.Uint32(body[6:])}, nil
}
