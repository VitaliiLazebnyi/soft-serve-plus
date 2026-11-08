package webhook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/matryer/is"
)

// A body cut off at maxResponseBody is cut before a character the limit
// falls inside, so a UTF-8 body stays valid UTF-8.
func TestReadResponseBodyCutsOnRuneBoundary(t *testing.T) {
	is := is.New(t)
	// "€" is three bytes and maxResponseBody is not a multiple of three, so
	// the limit falls inside one.
	is.True(maxResponseBody%3 != 0)
	body := strings.Repeat("€", maxResponseBody/3+10)

	got, err := readResponseBody(strings.NewReader(body))
	is.NoErr(err)
	is.True(utf8.ValidString(got))
	is.Equal(got, strings.Repeat("€", maxResponseBody/3)+truncatedMarker)
}

func TestStorableText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, want string
	}{
		{"", ""},
		{"plain", "plain"},
		{"caf\xe9", "caf�"},
		{"a\x00b", "ab"},
		{"\xe2\x82", "�"},
		{"ok €", "ok €"},
	} {
		got := storableText(tc.in)
		if got != tc.want {
			t.Errorf("storableText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A response with a Latin-1 header and a body holding a NUL and invalid UTF-8
// is still recorded, as text Postgres accepts.
func TestSendWebhookStoresInvalidUTF8ResponseAsText(t *testing.T) {
	is := is.New(t)
	useHTTPClient(t)
	e := newTestEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Name", "caf\xe9")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("a\x00b\xffc"))
	}))
	t.Cleanup(srv.Close)
	w := e.newWebhook(t, srv.URL, "", ContentTypeJSON, EventPush)

	is.NoErr(SendWebhook(e.ctx, w, EventPush, samplePayloadFor(e)))

	ds := e.deliveries(t, w.ID)
	is.Equal(len(ds), 1)
	d := ds[0]
	is.Equal(d.ResponseStatus, http.StatusOK)
	is.Equal(d.ResponseBody, "ab�c")
	is.True(strings.Contains(d.ResponseHeaders, "X-Name: caf�\n"))
	for _, s := range []string{d.RequestHeaders, d.RequestBody, d.ResponseHeaders, d.ResponseBody, d.RequestError.String} {
		is.True(utf8.ValidString(s))
		is.True(!strings.Contains(s, "\x00"))
	}
}
