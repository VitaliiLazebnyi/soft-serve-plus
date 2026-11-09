package lfs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const (
	blobSizeCutoff = 1024

	// HashAlgorithmSHA256 is the hash algorithm used for Git LFS.
	HashAlgorithmSHA256 = "sha256"

	// MetaFileIdentifier is the string appearing at the first line of LFS pointer files.
	// https://github.com/git-lfs/git-lfs/blob/master/docs/spec.md
	MetaFileIdentifier = "version https://git-lfs.github.com/spec/v1"

	// MetaFileOidPrefix appears in LFS pointer files on a line before the sha256 hash.
	MetaFileOidPrefix = "oid " + HashAlgorithmSHA256 + ":"
)

var (
	// ErrMissingPrefix occurs if the content lacks the LFS prefix
	ErrMissingPrefix = errors.New("content lacks the LFS prefix")

	// ErrInvalidStructure occurs if the content has an invalid structure
	ErrInvalidStructure = errors.New("content has an invalid structure")

	// ErrInvalidOIDFormat occurs if the oid has an invalid format
	ErrInvalidOIDFormat = errors.New("OID has an invalid format")

	// ErrInvalidSize occurs if the size is not a non-negative decimal integer
	ErrInvalidSize = errors.New("size has an invalid format")
)

// ReadPointer tries to read LFS pointer data from the reader
func ReadPointer(reader io.Reader) (Pointer, error) {
	buf := make([]byte, blobSizeCutoff)
	n, err := io.ReadFull(reader, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return Pointer{}, err
	}
	buf = buf[:n]

	return ReadPointerFromBuffer(buf)
}

var (
	oidPattern  = regexp.MustCompile(`^[a-f\d]{64}$`)
	sizePattern = regexp.MustCompile(`^\d+$`)
	// extPattern matches a pointer extension line, ext-{priority}-{name} {oid}.
	// https://github.com/git-lfs/git-lfs/blob/main/docs/extensions.md
	extPattern = regexp.MustCompile(`^ext-(\d)-\w+ ` + HashAlgorithmSHA256 + `:[a-f\d]{64}$`)
)

// ReadPointerFromBuffer will return a pointer if the provided byte slice is a pointer file or an error otherwise.
//
// The pointer must follow the spec exactly: the version line, then any
// extension lines in ascending priority, then the oid and size lines, each
// as "{key} {value}" terminated by a newline, with nothing else.
// https://github.com/git-lfs/git-lfs/blob/main/docs/spec.md#the-pointer
func ReadPointerFromBuffer(buf []byte) (Pointer, error) {
	var p Pointer

	headString := string(buf)
	if !strings.HasPrefix(headString, MetaFileIdentifier) {
		return p, ErrMissingPrefix
	}

	// The last line ends with a newline like every other line; git-lfs also
	// tolerates it missing.
	lines := strings.Split(strings.TrimSuffix(headString, "\n"), "\n")
	if lines[0] != MetaFileIdentifier {
		return p, ErrMissingPrefix
	}
	lines = lines[1:]

	// Extension lines sort before oid; their priorities are unique, so
	// sorted by key they are strictly ascending.
	prev := -1
	for len(lines) > 0 && strings.HasPrefix(lines[0], "ext-") {
		m := extPattern.FindStringSubmatch(lines[0])
		if m == nil {
			return p, ErrInvalidStructure
		}
		priority := int(m[1][0] - '0')
		if priority <= prev {
			return p, ErrInvalidStructure
		}
		prev = priority
		lines = lines[1:]
	}

	if len(lines) != 2 || !strings.HasPrefix(lines[0], "oid ") {
		return p, ErrInvalidStructure
	}
	oid, ok := strings.CutPrefix(lines[0], MetaFileOidPrefix)
	if !ok || !oidPattern.MatchString(oid) {
		return p, ErrInvalidOIDFormat
	}

	sizeStr, ok := strings.CutPrefix(lines[1], "size ")
	if !ok {
		return p, ErrInvalidStructure
	}
	if !sizePattern.MatchString(sizeStr) {
		return p, fmt.Errorf("%w: %q", ErrInvalidSize, sizeStr)
	}
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return p, fmt.Errorf("%w: %w", ErrInvalidSize, err)
	}

	p.Oid = oid
	p.Size = size

	return p, nil
}

// IsValid checks if the pointer has a valid structure.
// It doesn't check if the pointed-to-content exists.
func (p Pointer) IsValid() bool {
	if len(p.Oid) != 64 {
		return false
	}
	if !oidPattern.MatchString(p.Oid) {
		return false
	}
	if p.Size < 0 {
		return false
	}
	return true
}

// String returns the string representation of the pointer
// https://github.com/git-lfs/git-lfs/blob/main/docs/spec.md#the-pointer
func (p Pointer) String() string {
	return fmt.Sprintf("%s\n%s%s\nsize %d\n", MetaFileIdentifier, MetaFileOidPrefix, p.Oid, p.Size)
}

// RelativePath returns the relative storage path of the pointer
// https://github.com/git-lfs/git-lfs/blob/main/docs/spec.md#intercepting-git
func (p Pointer) RelativePath() string {
	if len(p.Oid) < 5 {
		return p.Oid
	}

	return path.Join(p.Oid[0:2], p.Oid[2:4], p.Oid)
}

// GeneratePointer generates a pointer for arbitrary content
func GeneratePointer(content io.Reader) (Pointer, error) {
	h := sha256.New()
	c, err := io.Copy(h, content)
	if err != nil {
		return Pointer{}, err
	}
	sum := h.Sum(nil)
	return Pointer{Oid: hex.EncodeToString(sum), Size: c}, nil
}
