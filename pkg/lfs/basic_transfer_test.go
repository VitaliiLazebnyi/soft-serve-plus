package lfs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBasicTransferErrorResponse checks that a failed transfer reports the
// HTTP status along with any message the server sent, and that a body which
// is not an LFS error object is logged to the request's logger at debug level.
func TestBasicTransferErrorResponse(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		status   int
		body     string
		wantErr  string
		wantLogs []string
	}{
		"message": {
			status:  http.StatusNotFound,
			body:    `{"message":"object not found"}`,
			wantErr: "request failed with status 404 Not Found: object not found",
		},
		"empty object": {
			status:  http.StatusForbidden,
			body:    `{}`,
			wantErr: "request failed with status 403 Forbidden",
		},
		"not json": {
			status:   http.StatusNotFound,
			body:     "404 page not found\n",
			wantErr:  "request failed with status 404 Not Found",
			wantLogs: []string{"DEBU", "error decoding error response", "404 Not Found"},
		},
		"empty body": {
			status:   http.StatusInternalServerError,
			wantErr:  "request failed with status 500 Internal Server Error",
			wantLogs: []string{"DEBU", "error decoding error response"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)
			a := &BasicTransferAdapter{srv.Client()}
			ctx, logs := debugContext(t)

			rc, err := a.Download(ctx, testPointer, &Link{Href: srv.URL + "/obj"})
			if err == nil {
				_ = rc.Close()
				t.Fatal("expected the download to fail")
			}
			if err.Error() != tt.wantErr {
				t.Errorf("error = %q, want %q", err, tt.wantErr)
			}
			for _, want := range tt.wantLogs {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("logs %q do not contain %q", logs, want)
				}
			}
			if strings.Contains(logs.String(), "ERRO") {
				t.Errorf("logs %q contain an error-level entry", logs)
			}
		})
	}
}
