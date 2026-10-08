package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/warden"
)

// A member's app at <app>.<zone> is same-site to the Console, so a form it posts would carry the admin's
// cookies: cross-origin browser requests are refused. install.sh's POST /join sends no Origin and passes.
func TestCrossOriginRefused(t *testing.T) {
	h := (&server{}).handler()
	for _, c := range []struct {
		path, site string
		want       int
	}{
		{"/api/me/keys", "same-site", 403},
		{"/api/deploy", "cross-site", 403},
		{"/join", "cross-site", 403},
		{"/join", "", 400}, // reaches the handler, which refuses the empty request
	} {
		req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader("{}"))
		if c.site != "" {
			req.Header.Set("Sec-Fetch-Site", c.site)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.want {
			t.Errorf("POST %s from %q: %d, want %d", c.path, c.site, w.Code, c.want)
		}
	}
}

// A member's app at <app>.<zone> must not frame the Console either: clicks in a framed page are
// same-origin. Every answer says so, a refusal by the cross-origin layer too.
func TestNotFramed(t *testing.T) {
	h := (&server{auth: &auth{key: []byte("0123456789abcdef0123456789abcdef")}}).handler()
	refused := httptest.NewRequest(http.MethodPost, "/api/deploy", nil)
	refused.Header.Set("Sec-Fetch-Site", "cross-site")
	for _, req := range []*http.Request{httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRequest(http.MethodGet, "/api/me", nil), refused} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("Content-Security-Policy") != "frame-ancestors 'none'" {
			t.Errorf("%s %s (%d) may be framed: %v", req.Method, req.URL.Path, w.Code, w.Header())
		}
	}
}

// A session says what someone was at sign-in; the Member says what they are now. A demoted admin's
// week-old cookie must not pass the Console's own admin checks.
func TestDemotedAdminLosesAdmin(t *testing.T) {
	sc := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(sc)
	for role, want := range map[string]int{"member": 403, "admin": 200} {
		m := &v1alpha1.Member{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: v1alpha1.MemberSpec{Email: "a@example.org", Role: role}}
		s := &server{c: fake.NewClientBuilder().WithScheme(sc).WithObjects(m).Build(), auth: &auth{key: []byte("0123456789abcdef0123456789abcdef")}}
		req := httptest.NewRequest(http.MethodGet, "/api/network", nil)
		req.AddCookie(cookie(sessionCookie, s.auth.sign("session", identity{Email: "a@example.org", Role: "admin", Admin: true, Exp: time.Now().Add(time.Hour).Unix()}), 60))
		w := httptest.NewRecorder()
		s.handler().ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("an admin session of a Member now %s: %d, want %d", role, w.Code, want)
		}
	}
}

// The Mesh page flags devices nobody owns; the Door joins with a setup key into the door group and is not
// one of them. The service token is the one the netbird Secret holds now: the writer rotates it.
func TestDoorIsNoStrayDevice(t *testing.T) {
	mesh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token rotated" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/api/peers":
			_, _ = w.Write([]byte(`[{"name":"door","groups":[{"id":"g","name":"door"}]},{"name":"laptop","user_id":"u"},{"name":"stray","groups":[{"id":"a","name":"All"}]}]`))
		case "/api/users":
			_, _ = w.Write([]byte(`[{"id":"u","email":"a@example.org"}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer mesh.Close()
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: warden.SystemNS, Name: "netbird"}, Data: map[string][]byte{"token": []byte("rotated")}}
	s := &server{meshURL: mesh.URL, c: fake.NewClientBuilder().WithObjects(sec).Build()}
	w := httptest.NewRecorder()
	s.network(w, httptest.NewRequest(http.MethodGet, "/api/network", nil).WithContext(withIdentity(context.Background(), &identity{Admin: true})))
	var out struct {
		Devices []struct{ Name, Owner string }
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatal(err, w.Body)
	}
	want := map[string]string{"door": "the Door", "laptop": "a@example.org", "stray": ""}
	for _, d := range out.Devices {
		if d.Owner != want[d.Name] {
			t.Errorf("%s: owner %q, want %q", d.Name, d.Owner, want[d.Name])
		}
	}
	if len(out.Devices) != len(want) {
		t.Errorf("%d devices, want %d", len(out.Devices), len(want))
	}
}

func TestCookiesAreHostOnly(t *testing.T) {
	for _, name := range []string{sessionCookie, oauthCookie} {
		if c := cookie(name, "v", 60); !strings.HasPrefix(c.Name, "__Host-") || !c.Secure || c.Path != "/" || c.Domain != "" {
			t.Errorf("%+v is not a __Host- cookie", c)
		}
	}
}

func TestUnverifiedEmailRefused(t *testing.T) {
	for _, c := range []struct {
		info map[string]any
		ok   bool
	}{
		{map[string]any{"email": "a@example.org"}, true},
		{map[string]any{"email": "a@example.org", "email_verified": true}, true},
		{map[string]any{"email": "a@example.org", "email_verified": "true"}, true},
		{map[string]any{"email": "a@example.org", "email_verified": false}, false},
		{map[string]any{"email": "a@example.org", "email_verified": "false"}, false},
		{map[string]any{"email_verified": true}, false},
	} {
		if _, err := verifiedEmail(c.info); (err == nil) != c.ok {
			t.Errorf("%v: %v", c.info, err)
		}
	}
}

// An invite token from a link is data: anything but a token never reaches the mesh, a token is one path
// segment, and the Console's own API token is not sent to the public invite endpoints.
func TestInviteTokenIsData(t *testing.T) {
	got := make(chan *http.Request, 10)
	mesh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got <- r }))
	defer mesh.Close()
	s := &server{meshURL: mesh.URL}
	for _, bad := range []string{"", "../../peers", "abcdefgh/../x", "abcdefgh?x=1", "abcdefgh%2F", "abcdefgh#x", "abc"} {
		if s.meshInvite(context.Background(), http.MethodGet, bad, "", nil, nil) == nil {
			t.Errorf("token %q was accepted", bad)
		}
	}
	if len(got) != 0 {
		t.Fatal("a refused token reached the mesh")
	}
	if err := s.meshInvite(context.Background(), http.MethodPost, "nbi_Ab3-x_9Zq", "/accept", map[string]any{"password": "p"}, nil); err != nil {
		t.Fatal(err)
	}
	if r := <-got; r.URL.Path != "/api/users/invites/nbi_Ab3-x_9Zq/accept" || r.Header.Get("Authorization") != "" {
		t.Errorf("%s with Authorization %q", r.URL.Path, r.Header.Get("Authorization"))
	}
}

// A member's projects are where their RBAC is bound: never a system namespace.
func TestMemberProjectsAreNames(t *testing.T) {
	if p, err := labels([]string{" alpha ", ""}); err != nil || len(p) != 1 || p[0] != "alpha" {
		t.Errorf("%v %v", p, err)
	}
	for _, bad := range []string{"kube-system", "wecolab-system", "Alpha", "a/b"} {
		if _, err := labels([]string{"alpha", bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
