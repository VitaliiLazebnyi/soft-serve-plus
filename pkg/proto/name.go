package proto

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/soft-serve/internal/termtext"
)

// SanitizeRepo returns the canonical repository name; the result is a fixed point.
func SanitizeRepo(repo string) string {
	// We're using path instead of filepath here because this is not OS dependent
	// looking at you Windows
	segs := strings.Split(path.Clean("/" + termtext.Sanitize(repo))[1:], "/")
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

// MaxRepoSegments is the most path segments a new repository name may have,
// as in "a/b/c" (three). The HTTP server resolves a go get path only within
// its first MaxRepoSegments segments, so the work of one request stays
// bounded however deep the package path is. A repository nested deeper,
// created before this limit, is found only when the import path names the
// repository itself, and a bearer token for it gets 403 on the go get paths
// of its subpackages.
const MaxRepoSegments = 32

// ErrInvalidRepo matches every error ValidateRepo, ValidateRepoSegments and
// ValidateExistingRepo return, so a caller can tell a refused name from a
// server fault. The errors keep their own messages.
var ErrInvalidRepo = errors.New("invalid repository name")

// invalidRepoError is a repository name refused by validation.
type invalidRepoError struct {
	msg string
}

func (e *invalidRepoError) Error() string { return e.msg }

// Is reports whether target is ErrInvalidRepo.
func (e *invalidRepoError) Is(target error) bool { return target == ErrInvalidRepo }

// invalidRepo returns an error matching ErrInvalidRepo with the message
// fmt.Sprintf(format, args...).
func invalidRepo(format string, args ...any) error {
	return &invalidRepoError{msg: fmt.Sprintf(format, args...)}
}

// ValidateRepoSegments rejects repo path segments that start with a hyphen,
// which a git command would read as an option, or consist only of periods,
// such as "...", which Windows does not keep as a directory name. It also
// rejects names of more than MaxRepoSegments segments.
func ValidateRepoSegments(repo string) error {
	segments := strings.Split(repo, "/")
	if len(segments) > MaxRepoSegments {
		return invalidRepo("repo name cannot have more than %d path segments", MaxRepoSegments)
	}
	for _, segment := range segments {
		if strings.HasPrefix(segment, "-") {
			return invalidRepo("repo path segments cannot start with a hyphen")
		}
		if strings.Trim(segment, ".") == "" {
			return invalidRepo("repo path segments cannot consist only of periods")
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
		return invalidRepo("repo cannot be empty")
	}

	for _, r := range repo {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_' && r != '.' && r != '/' {
			return invalidRepo("repo can only contain letters, numbers, hyphens, underscores, periods, and slashes")
		}
	}

	if SanitizeRepo(repo) != repo {
		return invalidRepo("repo name is not in its canonical form")
	}

	for _, segment := range strings.Split(repo, "/") {
		if strings.HasSuffix(strings.ToLower(segment), ".git") {
			return invalidRepo("repo path segments cannot end in .git")
		}
	}

	return nil
}
