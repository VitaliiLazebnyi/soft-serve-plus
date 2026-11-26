package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestTokenCreateExpiresIn(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		expiresIn  string
		wantErr    string
		wantNotice []string
	}{
		{"negative", "-1h", "must be a positive duration", nil},
		{"zero", "0s", "must be a positive duration", nil},
		{"positive", "1h", "", []string{"Access token created (expires ", " from now)\n"}},
		{"unset", "", "", []string{"Access token created\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, be := newAuthTestContext(t)
			ctx = withUser(t, ctx, be, "alice", false)

			args := []string{"create"}
			if tc.expiresIn != "" {
				args = append(args, "--expires-in", tc.expiresIn)
			}
			args = append(args, "tok")

			c := cmdsOf(ctx).TokenCommand()
			c.SilenceUsage = true
			var outBuf, errBuf bytes.Buffer
			c.SetOut(&outBuf)
			c.SetErr(&errBuf)
			c.SetArgs(args)
			err := c.ExecuteContext(ctx)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got err %v, want %q", err, tc.wantErr)
				}
				user, uerr := be.User(ctx, "alice")
				if uerr != nil {
					t.Fatal(uerr)
				}
				tokens, lerr := be.ListAccessTokens(ctx, user)
				if lerr != nil {
					t.Fatal(lerr)
				}
				if len(tokens) != 0 {
					t.Fatalf("created %d tokens, want none", len(tokens))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(outBuf.String(), "ss_") {
				t.Errorf("stdout = %q, want a token", outBuf.String())
			}
			// humanize.Time already says "from now", so the notice must not
			// read "expires in ... from now".
			if strings.Contains(errBuf.String(), "expires in") {
				t.Errorf("stderr = %q, want no \"expires in\"", errBuf.String())
			}
			for _, want := range tc.wantNotice {
				if !strings.Contains(errBuf.String(), want) {
					t.Errorf("stderr = %q, want it to contain %q", errBuf.String(), want)
				}
			}
		})
	}
}

// parseExpiresIn reads the units of time.ParseDuration plus d, w, mo and y,
// and refuses what does not fit a time.Duration.
func TestParseExpiresIn(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	for in, want := range map[string]time.Duration{
		"1y":           365 * day,
		"3mo":          90 * day,
		"2w":           14 * day,
		"5d4h":         5*day + 4*time.Hour,
		"1h30m":        90 * time.Minute,
		"1.5d":         36 * time.Hour,
		".5y":          365 * 12 * time.Hour,
		"10mo5d3h2m1s": 305*day + 3*time.Hour + 2*time.Minute + time.Second,
		"1y1y":         730 * day,
		"300ms":        300 * time.Millisecond,
		"1ns":          time.Nanosecond,
		"-1d":          -day,
		"+2h":          2 * time.Hour,
		"0":            0,
		"292y":         292 * 365 * day,
	} {
		got, err := parseExpiresIn(in)
		if err != nil || got != want {
			t.Errorf("parseExpiresIn(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-", "d", "1", "1x", "1.d1", "1d1", "1..5h", "293y", "106752d"} {
		if got, err := parseExpiresIn(in); err == nil {
			t.Errorf("parseExpiresIn(%q) = %v, want an error", in, got)
		}
	}
}
