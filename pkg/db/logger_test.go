package db

import (
	"bytes"
	"strings"
	"testing"

	"charm.land/log/v2"
)

func TestTraceRedactsArgs(t *testing.T) {
	var buf bytes.Buffer
	l := log.New(&buf)
	l.SetLevel(log.DebugLevel)

	const secret = "s3cr3t-webhook-token"
	trace(l, "INSERT INTO webhooks (url, secret)\n\t\tVALUES (?, ?)", "https://example.com/hook?token="+secret, secret)

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("trace logged an argument value: %q", out)
	}
	if !strings.Contains(out, "args=2") {
		t.Errorf("trace output %q does not contain the argument count", out)
	}
	if !strings.Contains(out, "INSERT INTO webhooks (url, secret) VALUES (?, ?)") {
		t.Errorf("trace output %q does not contain the collapsed query", out)
	}
}

func TestTraceNilLogger(t *testing.T) {
	trace(nil, "SELECT 1", "x")
}
