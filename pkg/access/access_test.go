package access

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParseAccessLevel(t *testing.T) {
	cases := []struct {
		in  string
		out AccessLevel
	}{
		{"", -1},
		{"foo", -1},
		{AdminAccess.String(), AdminAccess},
		{ReadOnlyAccess.String(), ReadOnlyAccess},
		{ReadWriteAccess.String(), ReadWriteAccess},
		{NoAccess.String(), NoAccess},
	}

	for _, c := range cases {
		out := ParseAccessLevel(c.in)
		if out != c.out {
			t.Errorf("ParseAccessLevel(%q) => %d, want %d", c.in, out, c.out)
		}
	}
}

func TestMarshalTextRoundTrip(t *testing.T) {
	for _, l := range []AccessLevel{NoAccess, ReadOnlyAccess, ReadWriteAccess, AdminAccess} {
		text, err := l.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%d) => error %v", l, err)
		}
		if string(text) != l.String() {
			t.Errorf("MarshalText(%d) => %q, want %q", l, text, l.String())
		}
		var got AccessLevel
		if err := got.UnmarshalText(text); err != nil {
			t.Fatalf("UnmarshalText(%q) => error %v", text, err)
		}
		if got != l {
			t.Errorf("round trip of %d => %d", l, got)
		}
	}
}

func TestMarshalTextRejectsInvalidLevel(t *testing.T) {
	for _, l := range []AccessLevel{-1, AdminAccess + 1, 100} {
		text, err := l.MarshalText()
		if !errors.Is(err, ErrInvalidAccessLevel) {
			t.Errorf("MarshalText(%d) => (%q, %v), want ErrInvalidAccessLevel", l, text, err)
		}
		if text != nil {
			t.Errorf("MarshalText(%d) => %q, want nil text", l, text)
		}
	}
}

func TestJSONMarshalRejectsInvalidLevel(t *testing.T) {
	if _, err := json.Marshal(struct {
		Level AccessLevel `json:"level"`
	}{AccessLevel(-1)}); !errors.Is(err, ErrInvalidAccessLevel) {
		t.Errorf("json.Marshal of invalid level => %v, want ErrInvalidAccessLevel", err)
	}

	b, err := json.Marshal(map[string]AccessLevel{"level": ReadWriteAccess})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"level":"read-write"}` {
		t.Errorf("json.Marshal => %s", b)
	}
}
