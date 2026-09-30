package fabric

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestOnMain(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/fabric/fabric/commits" || r.URL.Query().Get("sha") != "main" {
			http.NotFound(w, r)
			return
		}
		pages++
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		out := []map[string]string{}
		for i := (p - 1) * 50; i < min(p*50, 120); i++ { // main has 120 commits, c0 the newest
			out = append(out, map[string]string{"sha": fmt.Sprintf("c%d", i)})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	g := &Git{URL: srv.URL, Token: "t", Repo: "fabric/fabric", HTTP: srv.Client()}
	ctx := context.Background()
	for _, c := range []struct {
		sha   string
		depth int
		want  bool
		pages int
	}{
		{"c0", 200, true, 1},
		{"c110", 200, true, 3},
		{"c60", 50, false, 1},
		{"x", 1000, false, 3},
	} {
		pages = 0
		if got, err := g.OnMain(ctx, c.sha, c.depth); err != nil || got != c.want || pages != c.pages {
			t.Errorf("OnMain(%s, %d) = %v %v after %d pages", c.sha, c.depth, got, err, pages)
		}
	}
}
