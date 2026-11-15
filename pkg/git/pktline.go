package git

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The pkt-line format is git's framing for its wire protocol: four
// lower-case hex digits giving the length of the line including themselves,
// then the payload. "0000" is a flush-pkt. See gitprotocol-common(5).

// maxPktPayload is the largest payload WritePkt writes, git's
// LARGE_PACKET_DATA_MAX.
const maxPktPayload = 65516

// maxPktPayloadRead is the largest payload ReadPkt accepts: four bytes over
// what git writes, as go-git allowed.
const maxPktPayloadRead = maxPktPayload + 4

// ErrInvalidPktLen is returned for a pkt-line whose length is not four hex
// digits, is too short to be a line, is too long, or is cut short.
var ErrInvalidPktLen = errors.New("invalid pkt-len found")

// errPktTooLong is returned for a payload too long for one pkt-line.
var errPktTooLong = errors.New("pkt-line payload is too long")

// PktlineError is an "ERR <message>" pkt-line received from the other side.
type PktlineError struct {
	Text string
}

// Error implements error.
func (e *PktlineError) Error() string { return e.Text }

// WritePkt writes payload to w as one pkt-line, in a single write. An empty
// payload has no pkt-line of its own ("0004" is not one git sends), so it
// is written as a flush-pkt, as go-git's encoder did.
func WritePkt(w io.Writer, payload []byte) error {
	if len(payload) == 0 {
		return WriteFlush(w)
	}
	if len(payload) > maxPktPayload {
		return errPktTooLong
	}
	b := make([]byte, 0, 4+len(payload))
	b = fmt.Appendf(b, "%04x", 4+len(payload))
	b = append(b, payload...)
	_, err := w.Write(b)
	return err
}

// WriteFlush writes a flush-pkt to w.
func WriteFlush(w io.Writer) error {
	_, err := io.WriteString(w, "0000")
	return err
}

// ReadPkt reads one pkt-line from r and returns its payload, which is empty
// for a flush-pkt. It returns io.EOF when r ends before the line starts,
// ErrInvalidPktLen for a malformed or cut length, io.ErrUnexpectedEOF for a
// cut payload, and a *PktlineError for an ERR line.
func ReadPkt(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrInvalidPktLen
		}
		return nil, err
	}
	// ParseUint accepts a sign or underscores; a pkt-len is hex digits only.
	for _, c := range hdr {
		if !strings.ContainsRune("0123456789abcdefABCDEF", rune(c)) {
			return nil, ErrInvalidPktLen
		}
	}
	n, _ := strconv.ParseUint(string(hdr[:]), 16, 16)
	switch {
	case n == 0:
		return []byte{}, nil
	case n <= 4, n-4 > maxPktPayloadRead:
		return nil, ErrInvalidPktLen
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if msg, ok := bytes.CutPrefix(payload, []byte("ERR ")); ok {
		return nil, &PktlineError{Text: strings.TrimSpace(string(msg))}
	}
	return payload, nil
}

// WritePktline writes its arguments, formatted as by fmt.Sprintln, as one
// pkt-line, followed by a flush-pkt.
func WritePktline(w io.Writer, v ...any) error {
	if err := WritePkt(w, []byte(fmt.Sprintln(v...))); err != nil {
		return fmt.Errorf("git: error writing pkt-line message: %w", err)
	}
	if err := WriteFlush(w); err != nil {
		return fmt.Errorf("git: error flushing pkt-line message: %w", err)
	}
	return nil
}

// WritePktlineErr writes an error pktline to the given writer.
func WritePktlineErr(w io.Writer, err error) error {
	return WritePktline(w, "ERR", err.Error())
}
