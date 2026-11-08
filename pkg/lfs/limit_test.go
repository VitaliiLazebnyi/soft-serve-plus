package lfs

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
)

func TestDecodeJSONLimit(t *testing.T) {
	t.Parallel()
	const body = `{"message":"hi"}`
	tests := map[string]struct {
		limit   int64
		wantErr error
	}{
		"under limit":   {limit: int64(len(body)) + 10},
		"exactly limit": {limit: int64(len(body))},
		"over limit":    {limit: int64(len(body)) - 1, wantErr: errResponseTooLarge},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// One byte reads exercise the limit on every call.
			var er ErrorResponse
			err := decodeJSONLimit(iotest.OneByteReader(strings.NewReader(body)), tt.limit, &er)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && er.Message != "hi" {
				t.Fatalf("message = %q, want %q", er.Message, "hi")
			}
		})
	}
}

func TestDecodeResponseErrorLimit(t *testing.T) {
	t.Parallel()
	body := `{"message":"` + strings.Repeat("x", maxErrorResponseSize) + `"}`
	if _, err := decodeResponseError(strings.NewReader(body)); !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("err = %v, want %v", err, errResponseTooLarge)
	}
}

// oversizedBatchBody is a valid batch response, padded with whitespace past
// maxBatchResponseSize.
func oversizedBatchBody() string {
	return `{"transfer":"basic","objects":[]` + strings.Repeat(" ", maxBatchResponseSize) + `}`
}

func TestBatchResponseSizeLimit(t *testing.T) {
	t.Parallel()
	tests := map[string]http.HandlerFunc{
		"plain": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", MediaType)
			io.WriteString(w, oversizedBatchBody()) //nolint: errcheck
		},
		// The transport decompresses a gzip response it asked for, so a
		// small compressed body can expand far past the limit.
		"gzip": func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				t.Error("client did not ask for gzip")
			}
			w.Header().Set("Content-Type", MediaType)
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			io.WriteString(gz, oversizedBatchBody()) //nolint: errcheck
			gz.Close()                               //nolint: errcheck
		},
	}
	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(handler)
			t.Cleanup(srv.Close)
			c := plainClient(t, srv.URL+"/x.git")
			_, err := c.batch(t.Context(), OperationDownload, []Pointer{testPointer})
			if !errors.Is(err, errResponseTooLarge) {
				t.Fatalf("err = %v, want %v", err, errResponseTooLarge)
			}
		})
	}
}

func TestBatchRejectsUnrequestedObjects(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		res := BatchResponse{Transfer: TransferBasic}
		for range 3 {
			res.Objects = append(res.Objects, &ObjectResponse{Pointer: testPointer})
		}
		w.Header().Set("Content-Type", MediaType)
		json.NewEncoder(w).Encode(res) //nolint: errcheck
	}))
	t.Cleanup(srv.Close)
	c := plainClient(t, srv.URL+"/x.git")

	called := 0
	err := c.Download(t.Context(), []Pointer{testPointer}, func(Pointer, io.ReadCloser, error) error {
		called++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "3 objects, 1 were requested") {
		t.Fatalf("err = %v, want a too many objects error", err)
	}
	if called != 0 {
		t.Fatalf("callback called %d times, want 0", called)
	}
}
