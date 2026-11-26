package access

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in  string
		out Level
		err error
	}{
		{"", NoAccess, ErrInvalidAccessLevel},
		{"foo", NoAccess, ErrInvalidAccessLevel},
		{"unknown", NoAccess, ErrInvalidAccessLevel},
		{"admin-access", AdminAccess, nil},
		{"read-only", ReadOnlyAccess, nil},
		{"read-write", ReadWriteAccess, nil},
		{"no-access", NoAccess, nil},
	}

	for _, c := range cases {
		out, err := ParseLevel(c.in)
		if out != c.out || !errors.Is(err, c.err) || (err == nil) != (c.err == nil) {
			t.Errorf("ParseLevel(%q) => (%d, %v), want (%d, %v)", c.in, out, err, c.out, c.err)
		}
	}
}

// The names are the ones the CLI and the config accept.
func TestMarshalTextRoundTrip(t *testing.T) {
	for l, want := range map[Level]string{
		NoAccess:        "no-access",
		ReadOnlyAccess:  "read-only",
		ReadWriteAccess: "read-write",
		AdminAccess:     "admin-access",
	} {
		text, err := l.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText(%d) => error %v", l, err)
		}
		if string(text) != want {
			t.Errorf("MarshalText(%d) => %q, want %q", l, text, want)
		}
		var got Level
		if err := got.UnmarshalText(text); err != nil {
			t.Fatalf("UnmarshalText(%q) => error %v", text, err)
		}
		if got != l {
			t.Errorf("round trip of %d => %d", l, got)
		}
	}
}

func TestMarshalTextRejectsInvalidLevel(t *testing.T) {
	for _, l := range []Level{-1, AdminAccess + 1, 100} {
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
		Level Level `json:"level"`
	}{Level(-1)}); !errors.Is(err, ErrInvalidAccessLevel) {
		t.Errorf("json.Marshal of invalid level => %v, want ErrInvalidAccessLevel", err)
	}

	b, err := json.Marshal(map[string]Level{"level": ReadWriteAccess})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"level":"read-write"}` {
		t.Errorf("json.Marshal => %s", b)
	}
}
