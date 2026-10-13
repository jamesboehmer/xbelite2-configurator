// Package firmware parses Elite Series 2 firmware images, finds official ones,
// and flashes them with the GIP firmware-update command 0x0c. It does no USB
// I/O itself; see Link. See ../../FIRMWARE.md.
package firmware

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
)

// Region types in an rdhm image.
const (
	RegionStaging uint32 = 4 // pass 1: NV/calibration programmer, loaded into the app slot
	RegionApp     uint32 = 3 // pass 2: the controller firmware
	RegionBoot    uint32 = 2 // bootloader; never written
)

const (
	AppBase = 0x10027000 // the one app slot both passes write
	// AppSlotSize is that slot's size; larger regions would run past it.
	AppSlotSize = 0x7E000
	PageSize    = 0x1000
	VendorID    = 0x045e
	ProductID   = 0x0b00

	headerSize = 0x100
	maxRegions = 3
)

// Version is major.minor.build.rev. Images and region queries carry rev 0.
type Version struct {
	Major, Minor uint16
	Build        uint32
	Rev          uint16
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d.%d", v.Major, v.Minor, v.Build, v.Rev)
}

func (v Version) IsZero() bool { return v == (Version{}) }

// Compare returns -1, 0 or 1.
func (v Version) Compare(o Version) int {
	a := [4]uint64{uint64(v.Major), uint64(v.Minor), uint64(v.Build), uint64(v.Rev)}
	b := [4]uint64{uint64(o.Major), uint64(o.Minor), uint64(o.Build), uint64(o.Rev)}
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

// Region is one entry of an image's region table.
type Region struct {
	Type    uint32
	Addr    uint32
	Version Version
	Data    []byte
}

func (r *Region) Pages() int { return len(r.Data) / PageSize }

// Image is a parsed rdhm firmware file.
type Image struct {
	Version Version
	Regions map[uint32]*Region
	SHA256  [32]byte
}

// Parse validates an rdhm image. Header (little-endian): magic, header
// version 1, VID, PID, version, and a region count at 0x1c; region
// descriptors are 0x40 bytes each from 0x40.
func Parse(b []byte) (*Image, error) {
	if len(b) < headerSize {
		return nil, errors.New("firmware file is too short")
	}
	if string(b[:4]) != "rdhm" {
		return nil, errors.New("not a firmware image (no rdhm header)")
	}
	le := binary.LittleEndian
	if v := le.Uint32(b[4:]); v != 1 {
		return nil, fmt.Errorf("unsupported firmware header version %d", v)
	}
	if vid, pid := le.Uint16(b[8:]), le.Uint16(b[0x0a:]); vid != VendorID || pid != ProductID {
		return nil, fmt.Errorf("firmware is for %04x:%04x, not the Elite Series 2 (045e:0b00)", vid, pid)
	}
	img := &Image{
		Version: Version{Major: le.Uint16(b[0x0c:]), Minor: le.Uint16(b[0x0e:]), Build: le.Uint32(b[0x10:])},
		Regions: map[uint32]*Region{},
		SHA256:  sha256.Sum256(b),
	}
	n := int(le.Uint32(b[0x1c:]))
	if n < 1 || n > maxRegions {
		return nil, fmt.Errorf("firmware has %d regions", n)
	}
	for i := 0; i < n; i++ {
		d := b[0x40+i*0x40:]
		r := &Region{
			Type:    le.Uint32(d),
			Addr:    le.Uint32(d[0x10:]),
			Version: Version{Major: le.Uint16(d[0x18:]), Minor: le.Uint16(d[0x1a:]), Build: le.Uint32(d[0x1c:])},
		}
		off, size := uint64(le.Uint32(d[0x08:])), uint64(le.Uint32(d[0x0c:]))
		if off < headerSize || off+size > uint64(len(b)) {
			return nil, fmt.Errorf("firmware region %d lies outside the file", r.Type)
		}
		r.Data = b[off : off+size]
		img.Regions[r.Type] = r
	}
	for _, t := range []uint32{RegionStaging, RegionApp} {
		r := img.Regions[t]
		switch {
		case r == nil:
			return nil, fmt.Errorf("firmware is missing region %d", t)
		case r.Addr != AppBase:
			return nil, fmt.Errorf("firmware region %d has load address %#x, expected %#x", t, r.Addr, AppBase)
		case len(r.Data) > AppSlotSize:
			return nil, fmt.Errorf("firmware region %d is %#x bytes, larger than the %#x-byte app slot", t, len(r.Data), AppSlotSize)
		case len(r.Data) == 0 || len(r.Data)%PageSize != 0:
			return nil, fmt.Errorf("firmware region %d size %#x is not a multiple of 4096", t, len(r.Data))
		}
	}
	return img, nil
}

// Build writes an rdhm image, laying regions out in order after the header.
// The demo and tests use it; real images come from Microsoft.
func Build(v Version, regions ...Region) []byte {
	le := binary.LittleEndian
	h := make([]byte, headerSize)
	copy(h, "rdhm")
	le.PutUint32(h[4:], 1)
	le.PutUint16(h[8:], VendorID)
	le.PutUint16(h[0x0a:], ProductID)
	le.PutUint16(h[0x0c:], v.Major)
	le.PutUint16(h[0x0e:], v.Minor)
	le.PutUint32(h[0x10:], v.Build)
	le.PutUint32(h[0x1c:], uint32(len(regions)))
	off := uint32(headerSize)
	for i, r := range regions {
		d := h[0x40+i*0x40:]
		le.PutUint32(d, r.Type)
		le.PutUint32(d[0x08:], off)
		le.PutUint32(d[0x0c:], uint32(len(r.Data)))
		le.PutUint32(d[0x10:], r.Addr)
		le.PutUint16(d[0x18:], r.Version.Major)
		le.PutUint16(d[0x1a:], r.Version.Minor)
		le.PutUint32(d[0x1c:], r.Version.Build)
		off += uint32(len(r.Data))
	}
	out := h
	for _, r := range regions {
		out = append(out, r.Data...)
	}
	return out
}

var nameRE = regexp.MustCompile(`^DELP_045E_0B00_v_(\d+)_(\d+)_(\d+)_(\d+)_h_([A-Za-z0-9_=-]+)\.bin$`)

// ParseFilename reads the version and SHA-256 from an official name such as
// DELP_045E_0B00_v_5_23_6_0_h_<base64url SHA-256>.bin.
func ParseFilename(name string) (Version, [32]byte, bool) {
	var sum [32]byte
	m := nameRE.FindStringSubmatch(filepath.Base(name))
	if m == nil {
		return Version{}, sum, false
	}
	var n [4]uint64
	for i := range n {
		v, err := strconv.ParseUint(m[i+1], 10, 32)
		if err != nil || (i != 2 && v > 0xffff) {
			return Version{}, sum, false
		}
		n[i] = v
	}
	h, err := base64.URLEncoding.DecodeString(m[5])
	if err != nil || len(h) != len(sum) {
		return Version{}, sum, false
	}
	copy(sum[:], h)
	return Version{uint16(n[0]), uint16(n[1]), uint32(n[2]), uint16(n[3])}, sum, true
}

// Filename is the official name for an image with this version and hash.
func Filename(v Version, sum [32]byte) string {
	return fmt.Sprintf("DELP_045E_0B00_v_%d_%d_%d_%d_h_%s.bin",
		v.Major, v.Minor, v.Build, v.Rev, base64.URLEncoding.EncodeToString(sum[:]))
}
