package common

import (
	"errors"

	tea "charm.land/bubbletea/v2"
)

// ErrMissingRepo indicates that the requested repository could not be found.
var ErrMissingRepo = NewUserError("missing repo")

// genericErrorMessage is what the error screen shows for an error that is not
// meant for the user, such as one from git or the backend.
const genericErrorMessage = "something went wrong"

// userError is an error whose message is meant to be shown to the user.
type userError struct {
	msg string
}

func (e *userError) Error() string { return e.msg }

// NewUserError returns an error whose message the error screen shows as is.
// Any other error is shown as a generic message, since it may carry server
// details such as paths on disk.
func NewUserError(msg string) error {
	return &userError{msg: msg}
}

// ErrorMessage returns the text the error screen shows for err: its message
// when it is a user error, or a generic message otherwise.
func ErrorMessage(err error) string {
	if ue, ok := errors.AsType[*userError](err); ok {
		return ue.msg
	}
	return genericErrorMessage
}

// IsUserError reports whether err's message is meant to be shown to the user.
func IsUserError(err error) bool {
	_, ok := errors.AsType[*userError](err)
	return ok
}

// ErrorMsg is a Bubble Tea message that represents an error.
type ErrorMsg error

// ErrorCmd returns an ErrorMsg from error.
func ErrorCmd(err error) tea.Cmd {
	return func() tea.Msg {
		return ErrorMsg(err)
	}
}
