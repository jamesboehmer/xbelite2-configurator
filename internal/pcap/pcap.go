// Package pcap reads USBPcap pcapng captures (no tshark needed) and decodes
// the Elite Series 2 config traffic in them.
package pcap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"xbelite2-configurator/internal/protocol"
)

const (
	blockSHB = 0x0A0D0D0A
	blockIDB = 0x00000001
	blockEPB = 0x00000006

	linkTypeUSBPcap = 249
	transferIntr    = 1 // GIP uses interrupt endpoints
	transferBulk    = 3
)

// Transfer is one USB interrupt/bulk transfer that carried data.
type Transfer struct {
	Time     float64 // seconds since first packet
	FromHost bool
	Data     []byte
}

// Read returns every interrupt/bulk transfer with a payload in a USBPcap pcapng file.
func Read(path string) ([]Transfer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var (
		order     binary.ByteOrder = binary.LittleEndian
		linkTypes []uint16
		tsRes     []float64
		out       []Transfer
		first     = -1.0
	)
	for off := 0; off+12 <= len(raw); {
		btype := binary.LittleEndian.Uint32(raw[off:])
		if btype == blockSHB {
			if binary.BigEndian.Uint32(raw[off+8:]) == 0x1A2B3C4D {
				order = binary.BigEndian
			} else {
				order = binary.LittleEndian
			}
			linkTypes, tsRes = nil, nil
		}
		blen := int(order.Uint32(raw[off+4:]))
		if blen < 12 || off+blen > len(raw) {
			return nil, fmt.Errorf("%s: corrupt block at offset %d", path, off)
		}
		body := raw[off+8 : off+blen-4]
		switch order.Uint32(raw[off:]) {
		case blockIDB:
			linkTypes = append(linkTypes, order.Uint16(body))
			tsRes = append(tsRes, ifTsResol(body[8:], order))
		case blockEPB:
			iface := int(order.Uint32(body))
			if iface >= len(linkTypes) || linkTypes[iface] != linkTypeUSBPcap {
				break
			}
			ts := float64(uint64(order.Uint32(body[4:]))<<32|uint64(order.Uint32(body[8:]))) * tsRes[iface]
			if first < 0 {
				first = ts
			}
			capLen := int(order.Uint32(body[12:]))
			if t, ok := parseUSBPcap(body[20 : 20+capLen]); ok {
				t.Time = ts - first
				out = append(out, t)
			}
		}
		off += blen
	}
	if out == nil {
		return nil, errors.New("no USBPcap interrupt/bulk data found in " + path)
	}
	return out, nil
}

// ifTsResol reads the if_tsresol option (default microseconds).
func ifTsResol(opts []byte, order binary.ByteOrder) float64 {
	for len(opts) >= 4 {
		code, l := order.Uint16(opts), int(order.Uint16(opts[2:]))
		if code == 0 || 4+l > len(opts) {
			break
		}
		if code == 9 && l >= 1 {
			v := opts[4]
			if v&0x80 != 0 {
				return 1 / float64(uint64(1)<<(v&0x7F))
			}
			r := 1.0
			for i := byte(0); i < v; i++ {
				r /= 10
			}
			return r
		}
		opts = opts[4+(l+3)&^3:]
	}
	return 1e-6
}

// parseUSBPcap decodes the USBPcap pseudo-header:
// u16 hdrLen, u64 irp, u32 status, u16 function, u8 info, u16 bus, u16 device,
// u8 endpoint, u8 transfer, u32 dataLen.  info bit0 set = device->host.
func parseUSBPcap(pkt []byte) (Transfer, bool) {
	if len(pkt) < 27 {
		return Transfer{}, false
	}
	hdrLen := int(binary.LittleEndian.Uint16(pkt))
	if (pkt[22] != transferIntr && pkt[22] != transferBulk) || hdrLen > len(pkt) || hdrLen == len(pkt) {
		return Transfer{}, false
	}
	return Transfer{FromHost: pkt[16]&1 == 0, Data: pkt[hdrLen:]}, true
}

var opNames = map[byte]string{
	protocol.OpWrite: "WRITE", protocol.OpRead: "READ", protocol.OpCommit: "COMMIT", protocol.OpInit: "INIT",
}

// Describe annotates a 0x4D config frame, or returns "" for anything else.
func Describe(fromHost bool, data []byte) string {
	pkt, err := protocol.ParsePacket(data)
	if err != nil || pkt.Cmd != protocol.CmdConfig || len(pkt.Payload) == 0 {
		return ""
	}
	op, body := pkt.Payload[0], pkt.Payload[1:]
	name, ok := opNames[op]
	if !ok {
		name = fmt.Sprintf("op 0x%02x", op)
	}
	switch {
	case fromHost && op == protocol.OpRead && len(body) >= 2:
		return fmt.Sprintf("%s page 0x%02x size %d", name, body[0], body[1])
	case fromHost && op == protocol.OpWrite && len(body) >= 2:
		return fmt.Sprintf("%s page 0x%02x size %d data=%x", name, body[0], body[1], body[2:])
	case !fromHost && op == protocol.OpRead:
		page, d, err := protocol.ParseReadResponse(pkt.Payload)
		if err != nil {
			return name + " reply (bad): " + err.Error()
		}
		return fmt.Sprintf("%s reply status 0x%02x page 0x%02x data=%x", name, body[0], page, d)
	case fromHost:
		return fmt.Sprintf("%s request %x", name, body)
	default:
		return fmt.Sprintf("%s reply %x", name, body)
	}
}

// LastPages returns the most recent contents of each page seen (read replies and writes).
func LastPages(path string) (map[byte][]byte, error) {
	transfers, err := Read(path)
	if err != nil {
		return nil, err
	}
	pages := map[byte][]byte{}
	for _, t := range transfers {
		pkt, err := protocol.ParsePacket(t.Data)
		if err != nil || pkt.Cmd != protocol.CmdConfig || len(pkt.Payload) == 0 {
			continue
		}
		switch op := pkt.Payload[0]; {
		case op == protocol.OpRead && !t.FromHost:
			if page, d, err := protocol.ParseReadResponse(pkt.Payload); err == nil {
				pages[page] = d
			}
		case op == protocol.OpWrite && t.FromHost && len(pkt.Payload) >= 3:
			page, size := pkt.Payload[1], int(pkt.Payload[2])
			if 3+size <= len(pkt.Payload) {
				pages[page] = pkt.Payload[3 : 3+size]
			}
		}
	}
	return pages, nil
}

// Dump prints the annotated config conversation.
func Dump(w io.Writer, path string) error {
	transfers, err := Read(path)
	if err != nil {
		return err
	}
	for _, t := range transfers {
		if s := Describe(t.FromHost, t.Data); s != "" {
			dir := "dev->host"
			if t.FromHost {
				dir = "host->dev"
			}
			fmt.Fprintf(w, "%9.3f  %s  seq %02x  %s\n", t.Time, dir, t.Data[2], s)
		}
	}
	return nil
}
