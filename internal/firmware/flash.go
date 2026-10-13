package firmware

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"time"

	"xbelite2-configurator/internal/protocol"
)

// Link carries firmware frames to a controller. device.FirmwareLink is the
// USB one; FakeBootloader is the in-memory one.
type Link interface {
	Send(frame []byte) error
	// Recv returns the next cmd 0x0c frame or ACK of one, or ErrTimeout.
	Recv(timeout time.Duration) (protocol.Packet, error)
	// Reset reboots the controller, waits for it to re-enumerate, re-claims
	// it, and sends power-on and LED-on.
	Reset() error
	NextSeq() byte
	Close()
}

const (
	replyTimeout = 2 * time.Second
	ackTimeout   = 2 * time.Second
	writeTimeout = 5 * time.Second
)

// Event reports progress. Pass is 1 (region 4) or 2 (region 3), 0 outside.
type Event struct {
	Pass, Done, Total, Written, Skipped int
	Rebooting                           bool
}

type Options struct {
	// FromBootloader skips the normal-mode preamble because the controller is
	// already in its bootloader, e.g. after an interrupted flash.
	FromBootloader bool
}

// State is what the controller reports for each region. Zero means unknown.
type State struct {
	Staging, App, Boot Version
}

// Probe reads region versions with op 01, which both normal mode and the
// bootloader answer. Region 2 is best-effort.
func Probe(link Link) (State, error) {
	s := session{link: link}
	var st State
	var err error
	if st.Staging, err = s.regionVersion(RegionStaging); err != nil {
		return st, err
	}
	if st.App, err = s.regionVersion(RegionApp); err != nil {
		return st, err
	}
	st.Boot, _ = s.regionVersion(RegionBoot)
	return st, nil
}

// Flash writes region 4 then region 3 of img, the way Xbox Accessories does:
// pages whose SHA-1 already matches are skipped, each written page is
// re-hashed, and the controller is rebooted between passes. Region 2 (the
// bootloader) is never written, so re-running after a failure resumes.
func Flash(link Link, img *Image, opts Options, progress func(Event)) error {
	if progress == nil {
		progress = func(Event) {}
	}
	s := &session{link: link, progress: progress}
	if !opts.FromBootloader {
		if err := link.Send(EnterUpdateRequest(link.NextSeq())); err != nil {
			return err
		}
		if err := s.checkInfo(); err != nil {
			return err
		}
	}
	for i, typ := range []uint32{RegionStaging, RegionApp} {
		if err := s.pass(i+1, img.Regions[typ]); err != nil {
			return fmt.Errorf("pass %d of 2: %w", i+1, err)
		}
	}
	got, err := s.regionVersion(RegionApp)
	if err != nil {
		return fmt.Errorf("checking the new firmware: %w", err)
	}
	if want := img.Regions[RegionApp].Version; got != want {
		return fmt.Errorf("controller reports firmware %s after flashing, expected %s", got, want)
	}
	last := s.last
	last.Pass, last.Rebooting = 2, true
	progress(last)
	if err := link.Reset(); err != nil {
		return fmt.Errorf("firmware %s installed, but the controller didn't come back after the final reboot: %w", img.Regions[RegionApp].Version, err)
	}
	return nil
}

type session struct {
	link     Link
	progress func(Event)
	last     Event // last non-reboot event of the latest pass
}

func (s *session) pass(n int, r *Region) error {
	if err := s.start(n, r); err != nil {
		return err
	}
	ev := Event{Pass: n, Total: r.Pages()}
	s.progress(ev)
	for i := 0; i < ev.Total; i++ {
		addr := r.Addr + uint32(i*PageSize)
		page := r.Data[i*PageSize : (i+1)*PageSize]
		want := sha1.Sum(page)
		have, err := s.hash(addr)
		if err != nil {
			return fmt.Errorf("page %d of %d: %w", i+1, ev.Total, err)
		}
		if have == want {
			ev.Skipped++
		} else {
			if err := s.writePage(addr, page); err != nil {
				return fmt.Errorf("page %d of %d: %w", i+1, ev.Total, err)
			}
			if have, err = s.hash(addr); err != nil {
				return fmt.Errorf("page %d of %d: %w", i+1, ev.Total, err)
			}
			if have != want {
				return fmt.Errorf("page %d of %d: verify failed, the controller holds different data", i+1, ev.Total)
			}
			ev.Written++
		}
		ev.Done = i + 1
		s.last = ev
		s.progress(ev)
	}
	status, _, err := s.call(FinishRequest(s.link.NextSeq()), OpFinish, nil, replyTimeout)
	switch {
	case err != nil:
		return err
	case status == StatusReset:
		ev.Rebooting = true
		s.progress(ev)
		return s.link.Reset()
	case status != StatusOK:
		return fmt.Errorf("controller rejected finishing region %d (status 0x%02x)", r.Type, status)
	}
	return nil
}

// start sends op 02. In normal mode the controller answers 03: reboot into
// the bootloader, then ask again. Like the capture, region versions are
// re-read before each start.
func (s *session) start(n int, r *Region) error {
	for attempt := 1; ; attempt++ {
		for _, t := range []uint32{RegionStaging, RegionApp} {
			if _, err := s.regionVersion(t); err != nil {
				return err
			}
			if t == r.Type {
				break
			}
		}
		status, _, err := s.call(StartRequest(s.link.NextSeq(), r), OpStart, nil, replyTimeout)
		switch {
		case err != nil:
			return err
		case status == StatusOK:
			return nil
		case status == StatusReset && attempt == 1:
			s.progress(Event{Pass: n, Rebooting: true})
			if err := s.link.Reset(); err != nil {
				return err
			}
		case status == StatusReset:
			return errors.New("controller still asks for a reboot after rebooting")
		default:
			return fmt.Errorf("controller refused to start region %d (status 0x%02x)", r.Type, status)
		}
	}
}

func (s *session) checkInfo() error {
	status, body, err := s.call(InfoRequest(s.link.NextSeq()), OpInfo, nil, replyTimeout)
	if err != nil {
		return err
	}
	in, err := parseInfo(body)
	if err != nil || status != StatusOK {
		return fmt.Errorf("unexpected device info reply (status 0x%02x)", status)
	}
	if in.VID != VendorID || in.PID != ProductID || in.PageSize != PageSize {
		return fmt.Errorf("controller reports %04x:%04x with %d-byte pages, expected 045e:0b00 with 4096",
			in.VID, in.PID, in.PageSize)
	}
	return nil
}

func (s *session) regionVersion(t uint32) (Version, error) {
	status, body, err := s.call(RegionRequest(s.link.NextSeq(), t), OpRegion, le32(t), replyTimeout)
	if err != nil {
		return Version{}, err
	}
	if status != StatusOK {
		return Version{}, fmt.Errorf("controller rejected the region %d query (status 0x%02x)", t, status)
	}
	_, v, err := parseRegionVersion(body)
	return v, err
}

func (s *session) hash(addr uint32) ([20]byte, error) {
	var h [20]byte
	status, body, err := s.call(HashRequest(s.link.NextSeq(), addr), OpHash, nil, replyTimeout)
	if err != nil {
		return h, err
	}
	if status != StatusOK || len(body) < len(h) {
		return h, fmt.Errorf("controller rejected the hash query for %#x (status 0x%02x)", addr, status)
	}
	copy(h[:], body)
	return h, nil
}

func (s *session) writePage(addr uint32, page []byte) error {
	w := ChunkPage(s.link.NextSeq(), addr, page)
	for i, win := range w.Windows {
		for _, c := range win {
			if err := s.link.Send(c); err != nil {
				return err
			}
		}
		if err := s.awaitAck(w.Seq, w.AckAt[i]); err != nil {
			return err
		}
	}
	if err := s.link.Send(w.Terminator); err != nil {
		return err
	}
	status, _, err := s.await(OpWrite, nil, writeTimeout)
	if err != nil {
		return err
	}
	if status != StatusOK {
		return fmt.Errorf("controller rejected the write (status 0x%02x)", status)
	}
	return nil
}

func (s *session) call(req []byte, op byte, key []byte, timeout time.Duration) (byte, []byte, error) {
	if err := s.link.Send(req); err != nil {
		return 0, nil, err
	}
	return s.await(op, key, timeout)
}

// await returns the reply to op whose body starts with key, skipping ACKs
// and stale replies.
func (s *session) await(op byte, key []byte, timeout time.Duration) (byte, []byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		p, err := s.recv(deadline)
		if err != nil {
			return 0, nil, fmt.Errorf("%w (op 0x%02x)", err, op)
		}
		if p.Cmd != CmdFirmware || len(p.Payload) < 2 || p.Payload[0] != op || !bytes.HasPrefix(p.Payload[2:], key) {
			continue
		}
		return p.Payload[1], p.Payload[2:], nil
	}
}

func (s *session) awaitAck(seq byte, received int) error {
	deadline := time.Now().Add(ackTimeout)
	for {
		p, err := s.recv(deadline)
		if err != nil {
			return fmt.Errorf("%w (ACK at byte %d)", err, received)
		}
		if a, ok := ParseAck(p); ok && a.Seq == seq {
			if a.Received != received {
				return fmt.Errorf("controller acknowledged %d of %d bytes", a.Received, received)
			}
			return nil
		}
		if p.Cmd == CmdFirmware && len(p.Payload) >= 2 && p.Payload[0] == OpWrite {
			return fmt.Errorf("controller rejected the write (status 0x%02x)", p.Payload[1])
		}
	}
}

func (s *session) recv(deadline time.Time) (protocol.Packet, error) {
	left := time.Until(deadline)
	if left <= 0 {
		return protocol.Packet{}, ErrTimeout
	}
	return s.link.Recv(left)
}
