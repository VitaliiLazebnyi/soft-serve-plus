package utils

import (
	"fmt"
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SanitizeRepo returns the canonical repository name; the result is a fixed point.
func SanitizeRepo(repo string) string {
	// We're using path instead of filepath here because this is not OS dependent
	// looking at you Windows
	segs := strings.Split(path.Clean("/" + Sanitize(repo))[1:], "/")
	for len(segs) > 0 {
		last := segs[len(segs)-1]
		for strings.HasSuffix(last, ".git") {
			last = strings.TrimSuffix(last, ".git")
		}
		switch last {
		case "", ".":
			segs = segs[:len(segs)-1]
		case "..":
			segs = segs[:max(len(segs)-2, 0)]
		default:
			segs[len(segs)-1] = last
			return strings.Join(segs, "/")
		}
	}
	return ""
}

// Sanitize returns s without ANSI escape sequences (CSI, OSC, DCS, APC, SOS
// and PM strings included) and without any other C0 or C1 control character
// except newline and tab, so that the result can be written to a terminal
// without the terminal acting on it.
//
// Invalid UTF-8 is replaced with U+FFFD first, so a stray 8-bit byte from a
// legacy-encoded file cannot act as a C1 introducer. Escape sequences are
// matched within a single line, and a string sequence (DCS, OSC, SOS, PM,
// APC) is removed only when it is terminated on that line; otherwise only its
// ESC is dropped and the rest stays visible as plain text. An unterminated
// sequence therefore never hides the content that follows it.
func Sanitize(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = stripLineEscapes(line)
	}
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.Join(lines, "\n"))
}

// stripLineEscapes removes the complete 7-bit escape sequences in line. An
// incomplete sequence loses only its ESC; the control characters left behind
// are removed by Sanitize.
func stripLineEscapes(line string) string {
	if !strings.ContainsRune(line, '\x1b') {
		return line
	}
	var b strings.Builder
	b.Grow(len(line))
	for i := 0; i < len(line); {
		if line[i] != '\x1b' {
			b.WriteByte(line[i])
			i++
			continue
		}
		if n := escapeLen(line[i:]); n > 0 {
			i += n
		} else {
			i++ // drop the lone ESC, keep what follows
		}
	}
	return b.String()
}

// escapeLen returns the length of the complete escape sequence at the start of
// s (which begins with ESC), or 0 when the sequence is incomplete.
func escapeLen(s string) int {
	if len(s) < 2 {
		return 0
	}
	switch s[1] {
	case '[': // CSI: parameters and intermediates, then a final byte.
		for j := 2; j < len(s); j++ {
			switch c := s[j]; {
			case c >= 0x20 && c <= 0x3f:
			case c >= 0x40 && c <= 0x7e:
				return j + 1
			default:
				return 0
			}
		}
		return 0
	case 'P', ']', 'X', '^', '_': // DCS, OSC, SOS, PM, APC: up to ST or BEL.
		for j := 2; j < len(s); j++ {
			switch s[j] {
			case '\a':
				return j + 1
			case '\x1b':
				if j+1 < len(s) && s[j+1] == '\\' {
					return j + 2
				}
			}
		}
		return 0
	default: // ESC, intermediates, then a final byte (e.g. ESC ( B, ESC 7).
		for j := 1; j < len(s); j++ {
			switch c := s[j]; {
			case c >= 0x20 && c <= 0x2f:
			case c >= 0x30 && c <= 0x7e:
				return j + 1
			default:
				return 0
			}
		}
		return 0
	}
}

// ValidateUsername returns an error if any of the given usernames are invalid.
func ValidateUsername(username string) error {
	if username == "" {
		return fmt.Errorf("username cannot be empty")
	}

	// Lookups accept a name whose first byte, read as Latin-1, is a letter,
	// as older versions did, so that accounts they created stay reachable,
	// and a name whose first character is a letter, as ValidateNewUsername
	// requires.
	if first, _ := utf8.DecodeRuneInString(username); !unicode.IsLetter(first) && !unicode.IsLetter(rune(username[0])) {
		return fmt.Errorf("username must start with a letter")
	}

	for _, r := range username {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' {
			return fmt.Errorf("username can only contain letters, numbers, and hyphens")
		}
	}

	return nil
}

// ValidateNewUsername returns an error if username is not valid for a new
// account or a rename. On top of ValidateUsername, it requires the first
// character, not just its first byte, to be a letter, and it rejects letters from
// scripts that are not normally mixed, such as a Cyrillic "а" in "аdmin",
// which would make the name look like another one. Existing accounts are
// looked up with ValidateUsername, so they stay reachable.
func ValidateNewUsername(username string) error {
	if err := ValidateUsername(username); err != nil {
		return err
	}
	if first, _ := utf8.DecodeRuneInString(username); !unicode.IsLetter(first) {
		return fmt.Errorf("username must start with a letter")
	}
	if mixedScripts(username) {
		return fmt.Errorf("username cannot mix letters from different scripts")
	}
	return nil
}

// cjkScripts are the scripts that are written together, and with Latin, in
// Chinese, Japanese and Korean, as Unicode's "highly restrictive" profile
// (UTS #39) allows.
var cjkScripts = map[string]bool{
	"Han": true, "Hiragana": true, "Katakana": true, "Hangul": true, "Bopomofo": true,
}

// mixedScripts reports whether the letters of s come from more than one
// script, other than Latin and the CJK scripts written together. Letters of
// no particular script ("Common" or "Inherited") go with any.
func mixedScripts(s string) bool {
	var other string // the script other than Latin and CJK, if any
	var latinOrCJK bool
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		switch script := scriptOf(r); {
		case script == "", script == "Common", script == "Inherited":
		case script == "Latin", cjkScripts[script]:
			latinOrCJK = true
		case other == "":
			other = script
		case other != script:
			return true
		}
	}
	return other != "" && latinOrCJK
}

// scriptOf returns the name of the Unicode script r belongs to, or "" if
// none.
func scriptOf(r rune) string {
	if r < utf8.RuneSelf {
		if unicode.IsLetter(r) {
			return "Latin"
		}
		return ""
	}
	for name, table := range unicode.Scripts {
		if unicode.Is(table, r) {
			return name
		}
	}
	return ""
}

// ValidateRepo rejects names that are not canonical, whose segments end in
// ".git", which would nest one repository inside another, or that are not
// valid for a new repository (see ValidateRepoSegments).
func ValidateRepo(repo string) error {
	if err := ValidateExistingRepo(repo); err != nil {
		return err
	}
	return ValidateRepoSegments(repo)
}

// ValidateRepoSegments rejects repo path segments that start with a hyphen,
// which a git command would read as an option, or consist only of periods,
// such as "...", which Windows does not keep as a directory name.
func ValidateRepoSegments(repo string) error {
	for _, segment := range strings.Split(repo, "/") {
		if strings.HasPrefix(segment, "-") {
			return fmt.Errorf("repo path segments cannot start with a hyphen")
		}
		if strings.Trim(segment, ".") == "" {
			return fmt.Errorf("repo path segments cannot consist only of periods")
		}
	}
	return nil
}

// ValidateExistingRepo is ValidateRepo without the rules of
// ValidateRepoSegments, which repositories created before them may break.
// It is for naming a repository that already exists, such as the one being
// renamed.
func ValidateExistingRepo(repo string) error {
	if repo == "" {
		return fmt.Errorf("repo cannot be empty")
	}

	for _, r := range repo {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != '.' && r != '/' {
			return fmt.Errorf("repo can only contain letters, numbers, hyphens, underscores, periods, and slashes")
		}
	}

	if SanitizeRepo(repo) != repo {
		return fmt.Errorf("repo name is not in its canonical form")
	}

	for _, segment := range strings.Split(repo, "/") {
		if strings.HasSuffix(strings.ToLower(segment), ".git") {
			return fmt.Errorf("repo path segments cannot end in .git")
		}
	}

	return nil
}

// RedactURL returns rawURL without its user info, query and fragment, so it
// can be logged or returned in an error. Remotes and LFS endpoints can carry
// a token as the user name or password, and LFS hrefs are often presigned
// URLs with a signature in the query. A URL that cannot be parsed is
// replaced entirely; a string with no scheme or host to parse (such as an
// scp-style remote) is returned unchanged.
func RedactURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		if strings.Contains(rawURL, "://") {
			return "[redacted]"
		}
		return rawURL
	}
	if u.Host == "" {
		return rawURL
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}
