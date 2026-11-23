package access

import (
	"encoding"
	"errors"
)

// Level is the level of access allowed to a repo.
type Level int

const (
	// NoAccess does not allow access to the repo.
	NoAccess Level = iota

	// ReadOnlyAccess allows read-only access to the repo.
	ReadOnlyAccess

	// ReadWriteAccess allows read and write access to the repo.
	ReadWriteAccess

	// AdminAccess allows read, write, and admin access to the repo.
	AdminAccess
)

// String returns the string representation of the access level.
func (a Level) String() string {
	switch a {
	case NoAccess:
		return "no-access"
	case ReadOnlyAccess:
		return "read-only"
	case ReadWriteAccess:
		return "read-write"
	case AdminAccess:
		return "admin-access"
	default:
		return "unknown"
	}
}

// ParseAccessLevel parses an access level string: no-access, read-only,
// read-write or admin-access. Anything else is ErrInvalidAccessLevel.
func ParseAccessLevel(s string) (Level, error) {
	switch s {
	case "no-access":
		return NoAccess, nil
	case "read-only":
		return ReadOnlyAccess, nil
	case "read-write":
		return ReadWriteAccess, nil
	case "admin-access":
		return AdminAccess, nil
	default:
		return NoAccess, ErrInvalidAccessLevel
	}
}

var (
	_ encoding.TextMarshaler   = Level(0)
	_ encoding.TextUnmarshaler = (*Level)(nil)
)

// ErrInvalidAccessLevel is returned when an invalid access level is provided.
var ErrInvalidAccessLevel = errors.New("invalid access level")

// UnmarshalText implements encoding.TextUnmarshaler.
func (a *Level) UnmarshalText(text []byte) error {
	l, err := ParseAccessLevel(string(text))
	if err != nil {
		return err
	}

	*a = l

	return nil
}

// MarshalText implements encoding.TextMarshaler.
//
// It returns ErrInvalidAccessLevel for a level outside the known set, rather
// than writing "unknown", which UnmarshalText would then reject; this keeps
// every marshalled level round-trippable.
func (a Level) MarshalText() (text []byte, err error) {
	switch a {
	case NoAccess, ReadOnlyAccess, ReadWriteAccess, AdminAccess:
		return []byte(a.String()), nil
	default:
		return nil, ErrInvalidAccessLevel
	}
}
