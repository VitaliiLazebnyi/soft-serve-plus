package cmd

import (
	"bytes"
	"strings"
	"testing"
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

			c := TokenCommand()
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
