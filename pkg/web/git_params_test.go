package web

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// countingReader records how many bytes were read from the request body.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// The service on the ref advertisement comes from the query string only.
// withParams runs before the method check and authentication, so reading the
// body there let an anonymous multipart POST to any …/info/refs spool its file
// parts to temp files that were never removed.
func TestInfoRefsDoesNotReadBody(t *testing.T) {
	ctx, _, _ := newLFSTestContext(t)
	router := lfsTestRouter(ctx)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("service", "git-upload-pack"); err != nil {
		t.Fatal(err)
	}
	fw, err := mw.CreateFormFile("file", "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(bytes.Repeat([]byte("x"), 1<<20)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	body := &countingReader{r: &buf}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/anything/info/refs", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if body.n != 0 {
		t.Errorf("read %d bytes of the request body, want 0", body.n)
	}
}

func TestGetServiceType(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, target, body string
		want               string
	}{
		{"query", "/r/info/refs?service=git-upload-pack", "", "git-upload-pack"},
		{"not a git service", "/r/info/refs?service=upload-pack", "", ""},
		{"body ignored", "/r/info/refs", "service=git-receive-pack", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, tc.target, bytes.NewBufferString(tc.body)) //nolint:noctx
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if got := getServiceType(req).String(); got != tc.want {
				t.Errorf("getServiceType = %q, want %q", got, tc.want)
			}
		})
	}
}
