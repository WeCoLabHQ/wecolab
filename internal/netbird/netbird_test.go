package netbird

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Invite returns only a bare token, never the invite link: the Console puts it in /invite?token=.
func TestInviteReturnsOnlyTheToken(t *testing.T) {
	for body, want := range map[string]string{
		`{"invite_token":"nbi_Ab3-x_9Zq","invite_link":"https://mesh.example.com/invite/nbi_Ab3-x_9Zq"}`: "nbi_Ab3-x_9Zq",
		`{"invite_link":"https://mesh.example.com/invite/nbi_Ab3-x_9Zq"}`:                                "",
		`{"invite_token":"https://mesh.example.com/x"}`:                                                  "",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
		got, err := (&Client{URL: srv.URL, HTTP: srv.Client()}).Invite(context.Background(), "a@b.c", "A", "user", nil, 60)
		srv.Close()
		if got != want || (want == "") != (err != nil) {
			t.Errorf("%s: got %q, %v; want %q", body, got, err, want)
		}
	}
}
