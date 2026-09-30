package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/validate"
)

// Members are fabric objects: the record of what a person may do. Projects are
// tenant namespaces on the Fabric. The identity provider proves who someone is;
// when it is the mesh, Warden mirrors each Member to a mesh user and mints the
// invite whose link lands on the join page below and sets the password there.

// inviteToken is what a NetBird invite token may look like; nothing else from a link reaches its API.
var inviteToken = regexp.MustCompile(`^[A-Za-z0-9_-]{8,256}$`)

// meshInvite calls NetBird's public invite endpoints (/api/users/invites/<token>[/accept]). The token is
// the credential there: the Console's own API token is never sent to them.
func (s *server) meshInvite(ctx context.Context, method, token, suffix string, body any, out any) error {
	if !inviteToken.MatchString(token) {
		return errors.New("this is not an invite link")
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, s.meshURL+"/api/users/invites/"+url.PathEscape(token)+suffix, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// slug turns an email into an object name.
func slug(email string) string {
	b := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(email))
	b = strings.Trim(b, "-")
	if len(b) > 63 {
		b = b[:63]
	}
	return b
}

func (s *server) members(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	ctx := r.Context()
	ml := &v1alpha1.MemberList{}
	if err := s.c.List(ctx, ml); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	type member struct {
		Name, Email, FullName, Role, Phase, Invite, InviteExpires string
		Blocked                                                   bool
		Projects                                                  []string
		Keys                                                      int
	}
	out := []member{}
	count := map[string]int{}
	for _, m := range ml.Items {
		x := member{Name: m.Name, Email: m.Spec.Email, FullName: m.Spec.Name, Role: m.Spec.Role, Phase: m.Status.Phase, Blocked: m.Spec.Blocked, Projects: m.Spec.Projects, Keys: len(m.Spec.SSHKeys)}
		if x.Projects == nil {
			x.Projects = []string{}
		}
		if m.Status.Invite != "" && s.meshURL != "" {
			x.Invite = s.publicURL + "/invite?token=" + url.QueryEscape(m.Status.Invite)
			if m.Status.InviteExpires != nil {
				x.InviteExpires = m.Status.InviteExpires.UTC().Format(time.RFC3339)
			}
		}
		for _, p := range m.Spec.Projects {
			count[p]++
		}
		out = append(out, x)
	}
	nsl := &corev1.NamespaceList{}
	_ = s.c.List(ctx, nsl, client.HasLabels{"wecolab.io/tenant"})
	type project struct {
		Name    string
		Members int
	}
	projects := []project{}
	for _, n := range nsl.Items {
		if n.DeletionTimestamp.IsZero() {
			projects = append(projects, project{Name: n.Name, Members: count[n.Name]})
		}
	}
	writeJSON(w, map[string]any{"members": out, "projects": projects})
}

func (s *server) createProject(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	var in struct{ Name string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if err := validate.Name(in.Name); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.applyProject(r.Context(), in.Name); err != nil {
		answer(w, err, 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// inviteMember creates the Member; Warden mints the invite on the identity provider
// and the link appears on the Members page until it is used.
func (s *server) inviteMember(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	var in struct {
		Email, Name, Role string
		Projects          []string
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if err := validate.Email(in.Email); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	projects, err := labels(in.Projects)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if in.Role != "admin" {
		in.Role = "member"
	}
	m := &v1alpha1.Member{ObjectMeta: metav1.ObjectMeta{Name: slug(in.Email)}, Spec: v1alpha1.MemberSpec{Email: in.Email, Name: strings.TrimSpace(in.Name), Role: in.Role, Projects: projects}}
	if err := s.create(r.Context(), m, "member "+m.Spec.Email+" invited as "+m.Spec.Role); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "member": m.Name})
}

// setMember updates projects, role or the blocked flag on the Member as Git holds it, only the fields
// sent: the copy here lags Git, and writing it back would undo a change made meanwhile. The mirror follows.
func (s *server) setMember(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	var in struct {
		Projects *[]string
		SSHKeys  *[]string
		Role     *string
		Blocked  *bool
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var projects []string
	if in.Projects != nil {
		p, err := labels(*in.Projects)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		projects = p
	}
	m := &v1alpha1.Member{ObjectMeta: metav1.ObjectMeta{Name: r.PathValue("id")}}
	err := s.editObj(r.Context(), m, "member "+m.Name+" changed", func(exists bool) (client.Object, error) {
		switch {
		case !exists:
			return nil, fail(404, "no such member")
		case m.Spec.Role == "owner":
			return nil, fail(403, "the owner is not edited here")
		}
		if in.Projects != nil {
			m.Spec.Projects = projects
		}
		if in.SSHKeys != nil {
			m.Spec.SSHKeys = clean(*in.SSHKeys)
		}
		if in.Role != nil && (*in.Role == "admin" || *in.Role == "member") {
			m.Spec.Role = *in.Role
		}
		if in.Blocked != nil {
			m.Spec.Blocked = *in.Blocked
		}
		return m, nil
	})
	if err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// labels cleans a list of project names and refuses one a project could not have (a system namespace
// among them: a member's RBAC is bound in each of their projects).
func labels(in []string) ([]string, error) {
	out := clean(in)
	for _, p := range out {
		if err := validate.Name(p); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// myKeys lets anyone signed in set the SSH keys on their own profile.
func (s *server) myKeys(w http.ResponseWriter, r *http.Request) {
	var in struct{ SSHKeys []string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	for _, k := range in.SSHKeys {
		if k = strings.TrimSpace(k); k != "" && !strings.HasPrefix(k, "ssh-") && !strings.HasPrefix(k, "ecdsa-") && !strings.HasPrefix(k, "sk-") {
			http.Error(w, "each line must be an OpenSSH public key", 400)
			return
		}
	}
	ctx, email := s.elevated(r.Context()), s.who(r).Email
	cur, err := s.member(ctx, email) // only its name: the change is made on the Member as Git holds it
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	m, keys := &v1alpha1.Member{ObjectMeta: metav1.ObjectMeta{Name: cur.Name}}, clean(in.SSHKeys)
	err = s.editObj(ctx, m, "member "+email+": SSH keys", func(exists bool) (client.Object, error) {
		switch {
		case !exists || !strings.EqualFold(m.Spec.Email, email):
			return nil, fail(404, "%s is not a member of this fabric", email)
		case m.Spec.Blocked: // the copy here may not know yet
			return nil, fail(403, "this account is blocked")
		}
		m.Spec.SSHKeys = keys
		return m, nil
	})
	if err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "keys": len(keys)})
}

func (s *server) deleteMember(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	m := &v1alpha1.Member{ObjectMeta: metav1.ObjectMeta{Name: r.PathValue("id")}}
	err := s.editObj(r.Context(), m, "member "+m.Name+" removed", func(exists bool) (client.Object, error) {
		switch {
		case !exists:
			return nil, fail(404, "no such member")
		case m.Spec.Role == "owner":
			return nil, fail(403, "the owner cannot be removed")
		}
		return nil, nil
	})
	if err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

var joinPage = template.Must(template.New("join").Parse(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Join {{.Domain}}</title><link rel="icon" type="image/svg+xml" href="/icon.svg">
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;background:#0f1115;color:#e6e8ee;font:15px/1.5 system-ui,sans-serif}form{width:min(92vw,380px);background:#171a21;border:1px solid #262a33;border-radius:12px;padding:24px;display:grid;gap:12px}h1{font-size:18px;margin:0}p{margin:0;color:#9aa1ad}input{background:#0f1115;border:1px solid #2b303a;color:#e6e8ee;border-radius:8px;padding:10px 12px;font:inherit}button{background:#5b8def;border:0;color:#fff;border-radius:8px;padding:10px;font:inherit;cursor:pointer}.err{color:#f87171}</style>
<form method="post" action="/invite"><h1>Join {{.Domain}}</h1><p>{{if .Email}}Set a password for <b>{{.Email}}</b>.{{else}}Set your password.{{end}} At least 8 characters with an uppercase letter, a digit and a symbol.</p>
<input type="hidden" name="token" value="{{.Token}}"><input type="password" name="password" placeholder="password" required minlength="8" autofocus><input type="password" name="confirm" placeholder="again" required minlength="8">
{{if .Error}}<div class="err">{{.Error}}</div>{{end}}<button>Create my account</button></form>`))

// invitePage is a person's invite link: it sets their password on the people mesh's identity provider,
// then sends them to sign in.
func (s *server) invitePage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Domain": s.domain, "Token": r.URL.Query().Get("token")}
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		data["Token"] = r.Form.Get("token")
		if r.Form.Get("password") != r.Form.Get("confirm") {
			data["Error"] = "the passwords differ"
		} else if err := s.meshInvite(r.Context(), http.MethodPost, r.Form.Get("token"), "/accept", map[string]any{"password": r.Form.Get("password")}, nil); err != nil {
			data["Error"] = "the mesh refused: " + err.Error()
		} else {
			http.Redirect(w, r, "/oauth/login", http.StatusFound)
			return
		}
	} else {
		var info map[string]any
		if err := s.meshInvite(r.Context(), http.MethodGet, r.URL.Query().Get("token"), "", nil, &info); err != nil {
			data["Error"] = err.Error()
		}
		data["Email"] = str(info, "email")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = joinPage.Execute(w, data)
}
