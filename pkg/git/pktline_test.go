package git

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// pktReadCases are pkt-line streams and what reading the first line gives:
// its payload, or an error. They are what go-git's scanner, which the
// daemon used before, gave for the same input.
var pktReadCases = []struct {
	name, in string
	payload  string
	err      error
	errText  string // for an ERR line
}{
	{name: "line", in: "000ahello\n", payload: "hello\n"},
	{name: "line then more", in: "0009abcde0000", payload: "abcde"},
	{name: "upper-case length", in: "000Ahello\n", payload: "hello\n"},
	{name: "flush", in: "0000", payload: ""},
	{name: "empty input", in: "", err: io.EOF},
	{name: "cut length", in: "00", err: ErrInvalidPktLen},
	{name: "not hex", in: "00g5x", err: ErrInvalidPktLen},
	{name: "sign", in: "+005x", err: ErrInvalidPktLen},
	{name: "length one", in: "0001", err: ErrInvalidPktLen},
	{name: "length four", in: "0004", err: ErrInvalidPktLen},
	{name: "cut payload", in: "000ahel", err: io.ErrUnexpectedEOF},
	{name: "no payload", in: "000a", err: io.ErrUnexpectedEOF},
	{name: "longest", in: "fff4" + strings.Repeat("x", 0xfff4-4), payload: strings.Repeat("x", 0xfff4-4)},
	{name: "too long", in: "fff5" + strings.Repeat("x", 0xfff5-4), err: ErrInvalidPktLen},
	{name: "error line", in: "000fERR foobar\n", errText: "foobar"},
	{name: "ERR without space", in: "0007ERR", payload: "ERR"},
}

func TestReadPkt(t *testing.T) {
	t.Parallel()
	for _, tc := range pktReadCases {
		got, err := ReadPkt(strings.NewReader(tc.in))
		switch {
		case tc.errText != "":
			var pe *PktlineError
			if !errors.As(err, &pe) || pe.Text != tc.errText {
				t.Errorf("%s: err = %v, want ERR %q", tc.name, err, tc.errText)
			}
		case tc.err != nil:
			if !errors.Is(err, tc.err) {
				t.Errorf("%s: err = %v, want %v", tc.name, err, tc.err)
			}
		case err != nil || string(got) != tc.payload:
			t.Errorf("%s: = %.20q, %v; want %.20q", tc.name, got, err, tc.payload)
		}
	}
}

func TestWritePkt(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	if err := WritePkt(&b, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFlush(&b); err != nil {
		t.Fatal(err)
	}
	// An empty payload is a flush, which reads back as an empty line.
	if err := WritePkt(&b, nil); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "000ahello\n00000000" {
		t.Errorf("wrote %q", got)
	}
	for _, want := range []string{"hello\n", "", ""} {
		if got, err := ReadPkt(&b); err != nil || string(got) != want {
			t.Errorf("read back %q, %v; want %q", got, err, want)
		}
	}
	if err := WritePkt(&b, make([]byte, maxPktPayload)); err != nil {
		t.Errorf("largest payload: %v", err)
	}
	if err := WritePkt(&b, make([]byte, maxPktPayload+1)); err == nil {
		t.Error("payload over the limit was written")
	}
	// What is written reads back.
	payload := strings.Repeat("y", maxPktPayload)
	b.Reset()
	if err := WritePkt(&b, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadPkt(&b); err != nil || string(got) != payload {
		t.Errorf("read back %d bytes, %v", len(got), err)
	}
}
