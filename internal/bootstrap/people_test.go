package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeNetBird is NetBird's management API as the people step uses it, counting what it is asked to change.
type fakeNetBird struct {
	mu      sync.Mutex
	owner   bool
	lists   map[string][]map[string]any // groups, policies, peers, users
	changes []string
}

func (f *fakeNetBird) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodGet {
		f.changes = append(f.changes, r.Method+" "+r.URL.Path)
	}
	auth := r.Header.Get("Authorization") == "Token pat"
	out := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	list := strings.TrimPrefix(r.URL.Path, "/api/")
	switch {
	case r.URL.Path == "/api/instance":
		out(map[string]any{"setup_required": !f.owner})
	case r.URL.Path == "/api/setup" && !f.owner:
		f.owner = true
		out(map[string]any{"personal_access_token": "pat"})
	case !auth:
		http.Error(w, `{"message":"unauthorized"}`, 401)
	case r.URL.Path == "/api/accounts/acc":
		f.lists["accounts"][0]["settings"] = body["settings"]
	case strings.HasSuffix(r.URL.Path, "/tokens"):
		out(map[string]any{"plain_token": "service-token", "personal_access_token": map[string]any{"id": "pat-1"}})
	case r.URL.Path == "/api/setup-keys":
		out(map[string]any{"key": "one-off"})
	case r.Method == http.MethodGet:
		out(f.lists[list])
	case r.Method == http.MethodPost:
		body["id"] = list + "-" + body["name"].(string)
		f.lists[list] = append(f.lists[list], body)
		out(body)
	case r.Method == http.MethodDelete:
		i := strings.LastIndex(list, "/")
		kept := []map[string]any{}
		for _, o := range f.lists[list[:i]] {
			if o["id"] != list[i+1:] {
				kept = append(kept, o)
			}
		}
		f.lists[list[:i]] = kept
	}
}

func TestPeopleResumesAndActsOnce(t *testing.T) {
	nb := &fakeNetBird{lists: map[string][]map[string]any{"policies": {{"id": "p0", "name": "Default"}},
		"accounts": {{"id": "acc", "settings": map[string]any{"network_range": "100.64.0.0/10"}}}}}
	srv := httptest.NewServer(nb)
	defer srv.Close()
	asked, joined, committed, committedID := 0, 0, "", ""
	commitErr := errors.New("the writer is away")
	p := &People{NetBird: srv.URL, Zone: "fab.example.org", Email: "owner@example.org", PeopleNet: "100.96.0.0/16",
		State:    filepath.Join(t.TempDir(), "people.json"),
		Password: func() (string, error) { asked++; return "correct horse", nil },
		JoinDoor: func(_ context.Context, key string) error {
			joined++
			nb.mu.Lock()
			nb.lists["peers"] = append(nb.lists["peers"], map[string]any{"id": "peer-door", "name": "door"})
			nb.mu.Unlock()
			return nil
		},
		Commit: func(_ context.Context, tok, id string) error { committed, committedID = tok, id; return commitErr },
	}
	ctx := context.Background()

	// The commit fails: the setup and service tokens stay on the box, readable by root only.
	if err := p.Run(ctx); err == nil {
		t.Fatal("a failed commit must fail the step")
	}
	fi, err := os.Stat(p.State)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state %v %v", fi, err)
	}
	b, _ := os.ReadFile(p.State)
	if !strings.Contains(string(b), `"pat":"pat"`) || !strings.Contains(string(b), `"token":"service-token"`) {
		t.Fatalf("state must keep what NetBird shows once: %s", b)
	}
	first := len(nb.changes)

	// Again: nothing NetBird already has is made twice; the owner is not asked again.
	commitErr = nil
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if again := nb.changes[first:]; len(again) != 0 {
		t.Fatalf("a re-run changed NetBird again: %v", again)
	}
	if asked != 1 || joined != 1 || committed != "service-token" || committedID != "pat-1" {
		t.Fatalf("asked %d, joined %d, committed %q (id %q)", asked, joined, committed, committedID)
	}
	for _, want := range []string{"POST /api/setup", "POST /api/users", "POST /api/users/users-wecolab/tokens", "PUT /api/accounts/acc",
		"POST /api/groups", "POST /api/policies", "DELETE /api/policies/p0", "POST /api/setup-keys"} {
		if !strings.Contains(strings.Join(nb.changes, "\n")+"\n", want+"\n") {
			t.Errorf("never did %s: %v", want, nb.changes)
		}
	}
	if b, _ := os.ReadFile(p.State); strings.Contains(string(b), "token") || strings.Contains(string(b), "pat") {
		t.Fatalf("done, the state keeps no secret: %s", b)
	}

	// Done: nothing is asked of NetBird at all.
	srv.Close()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("a finished step runs again without NetBird: %v", err)
	}
}

func TestCheckPassword(t *testing.T) {
	for pw, ok := range map[[2]string]bool{{"long enough", "long enough"}: true, {"short", "short"}: false, {"long enough", "long enougH"}: false} {
		if (CheckPassword(pw[0], pw[1]) == nil) != ok {
			t.Errorf("%q/%q: want ok=%v", pw[0], pw[1], ok)
		}
	}
}

func TestWithToken(t *testing.T) {
	plain := []byte("apiVersion: v1\nkind: Secret\nmetadata: {name: netbird, namespace: wecolab-system}\nstringData: {url: https://mesh.fab.example.org}\n")
	out, changed, err := withToken(plain, "tok", "id1")
	if err != nil || !changed || !strings.Contains(string(out), "token: tok") || !strings.Contains(string(out), "url: https://mesh.fab.example.org") {
		t.Fatalf("%v %v %s", err, changed, out)
	}
	if _, changed, _ := withToken(out, "tok", "id1"); changed {
		t.Fatal("the same token again is no change")
	}
	enc := []byte("stringData:\n  token: ENC[AES256_GCM,data:x]\nsops:\n  age:\n    - recipient: age1site\n      enc: x\n    - recipient: age1recovery\n      enc: y\n")
	if r, err := sopsRecipients(enc); err != nil || strings.Join(r, ",") != "age1site,age1recovery" {
		t.Fatalf("%v %v", r, err)
	}
}
