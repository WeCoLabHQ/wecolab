package warden

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestB2BodyDeadlineDoesNotBlockNextCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stall" {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		io.WriteString(w, `{"key":"current"}`)
	}))
	defer server.Close()
	old := b2HTTPClient
	b2HTTPClient = &http.Client{Timeout: 50 * time.Millisecond}
	defer func() { b2HTTPClient = old }()
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/stall", nil)
	var body map[string]string
	if err := doJSON(request, &body); err == nil {
		t.Fatal("stalled provider body succeeded")
	}
	request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/ready", nil)
	if err := doJSON(request, &body); err != nil || body["key"] != "current" {
		t.Fatalf("next request did not recover: %v %v", body, err)
	}
}

func TestB2RejectsInvalidBodies(t *testing.T) {
	for _, body := range []string{`{"key":`, `{} {}`, strings.Repeat(" ", 1<<20) + `{}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		var out map[string]any
		if err := doJSON(req, &out); err == nil {
			t.Errorf("invalid body accepted (length %d)", len(body))
		}
		srv.Close()
	}
}
