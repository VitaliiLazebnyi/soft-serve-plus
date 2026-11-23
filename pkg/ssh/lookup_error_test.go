package ssh

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/log/v2"
)

// A user lookup canceled because the client went away is logged at Debug,
// not as a server error.
func TestUserLookupFailureCanceledLoggedAtDebug(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := log.New(&buf)
	logger.SetLevel(log.DebugLevel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	logUserLookupFailure(ctx, logger, context.Canceled)
	if out := buf.String(); !strings.Contains(out, "user lookup canceled") || strings.Contains(out, "ERRO") {
		t.Errorf("logs = %q, want the canceled lookup at Debug only", out)
	}

	buf.Reset()
	logUserLookupFailure(context.Background(), logger, errors.New("db down"))
	if out := buf.String(); !strings.Contains(out, "ERRO") {
		t.Errorf("logs = %q, want the failed lookup at Error", out)
	}
}
