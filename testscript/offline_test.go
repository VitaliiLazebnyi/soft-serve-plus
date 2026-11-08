package testscript

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/txtar"
)

// remoteHost matches a URL-style or scp-style git remote and captures its
// host.
var remoteHost = regexp.MustCompile(`^(?:[a-z][a-z0-9+.-]*://(?:[^@/]*@)?(\[[^\]]*\]|[^:/]*)|[^@:/]+@(\[[^\]]*\]|[^:/]+):)`)

// TestScriptsNeedNoNetwork keeps the testscripts runnable offline: every
// command that is expected to succeed must name remotes on loopback, that is
// the test server itself, never a host on the internet.
func TestScriptsNeedNoNetwork(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.txtar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no testscripts found")
	}
	for _, file := range files {
		ar, err := txtar.ParseFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(ar.Comment), "\n") {
			for _, host := range externalRemotes(line) {
				t.Errorf("%s:%d: %q needs network access to %s; use a repository on the test server instead", file, i+1, strings.TrimSpace(line), host)
			}
		}
	}
}

func TestExternalRemotes(t *testing.T) {
	for _, tc := range []struct {
		line string
		want []string
	}{
		{"git clone https://github.com/charmbracelet/catwalk.git", []string{"github.com"}},
		{"soft repo import repo1 https://github.com/charmbracelet/catwalk.git", []string{"github.com"}},
		{"exec curl -s https://example.com/x", []string{"example.com"}},
		{"curl http://example.com:8080/x", []string{"example.com"}},
		{"soft repo webhook create repo1 http://example.com/hook -e push", []string{"example.com"}},
		{"soft repo create repo1 --lfs-endpoint http://[2001:db8::1]/lfs", []string{"2001:db8::1"}},
		{"git push git@example.com:repo.git", []string{"example.com"}},
		{"[!windows] ? curl https://example.com", []string{"example.com"}},
		{"git clone ssh://localhost:$SSH_PORT/repo1", nil},
		{"git clone http://$UTOKEN@localhost:$HTTP_PORT/repo1", nil},
		{"curl http://127.0.0.1:$HTTP_PORT/repo1 -H 'Origin: https://example.com'", nil},
		{"soft repo webhook create repo1 http://$RCV/hook -e push", nil},
		{"! soft repo import bad http://10.0.0.1/x.git", nil},
		{"stderr 'https://example.com'", nil},
		{"stdout https://example.com", nil},
		{"# git clone https://github.com/charmbracelet/catwalk.git", nil},
	} {
		if got := externalRemotes(tc.line); !slices.Equal(got, tc.want) {
			t.Errorf("externalRemotes(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// patternCommands are the script commands whose arguments are patterns
// matched against output, not addresses anything connects to.
var patternCommands = map[string]bool{
	"stdout":   true,
	"stderr":   true,
	"grep":     true,
	"envmatch": true,
}

// externalRemotes returns the non-loopback remote hosts that a script line
// expects to reach: the URL-style or scp-style remotes in the arguments of a
// command that is not expected to fail. A host given by a variable is set
// when the script runs and is not checked.
func externalRemotes(line string) []string {
	line = strings.TrimSpace(line)
	// Drop the [condition] prefixes.
	for strings.HasPrefix(line, "[") {
		end := strings.Index(line, "]")
		if end < 0 {
			return nil
		}
		line = strings.TrimSpace(line[end+1:])
	}
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
		return nil
	}
	// A command that may fail may also succeed.
	line = strings.TrimSpace(strings.TrimPrefix(line, "?"))
	args := scriptArgs(line)
	if len(args) == 0 || patternCommands[args[0]] {
		return nil
	}
	var hosts []string
	for _, arg := range args[1:] {
		m := remoteHost.FindStringSubmatch(strings.Trim(arg, `"`))
		if m == nil {
			continue
		}
		switch host := strings.Trim(m[1]+m[2], "[]"); {
		case host == "localhost", host == "127.0.0.1", host == "::1":
		case strings.HasPrefix(host, "$"):
		default:
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// scriptArgs splits a script line into its arguments as testscript does:
// at spaces outside single quotes, where a doubled quote inside quotes
// stands for one quote.
func scriptArgs(line string) []string {
	var args []string
	var arg strings.Builder
	inArg, quoted := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quoted && c == '\'':
			if i+1 < len(line) && line[i+1] == '\'' {
				arg.WriteByte(c)
				i++
				continue
			}
			quoted = false
		case !quoted && c == '\'':
			quoted, inArg = true, true
		case !quoted && (c == ' ' || c == '\t'):
			if inArg {
				args = append(args, arg.String())
				arg.Reset()
				inArg = false
			}
		default:
			arg.WriteByte(c)
			inArg = true
		}
	}
	if inArg {
		args = append(args, arg.String())
	}
	return args
}
