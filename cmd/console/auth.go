package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/validate"
)

// Sign-in is the mesh's own identity. The Console runs the OIDC authorization-code
// flow with PKCE against the embedded provider, as the mesh dashboard's public
// client (the only client that provider can register; the Console's callback is
// listed in the mesh config's dashboardRedirectURIs). The identity that comes back
// is looked up on the mesh: owners and admins see everything; anyone else is a
// member of the projects whose group (project-<name>) is among their auto_groups.
// Sessions are a signed cookie; the key lives in a Secret on the Fabric so a
// restart or a second replica keeps everyone signed in.

type identity struct {
	Email, Name, Role string
	Admin             bool
	Projects          []string
	Exp               int64
}

type auth struct {
	issuer, clientID, clientSecret, callback string
	key                                      []byte
	mu                                       sync.Mutex
	disc                                     *discovery
}

type discovery struct{ Auth, Token, Userinfo string }

// httpc calls the identity provider and the mesh: another service that hangs must not hold a request
// (and its goroutine) forever.
var httpc = &http.Client{Timeout: 15 * time.Second}

// The cookies are host-only, Secure and for the whole path (the __Host- prefix makes browsers insist), so
// no app under the zone can set or overwrite them.
const (
	sessionCookie = "__Host-wcl_session"
	oauthCookie   = "__Host-wcl_oauth"
)

// newAuth: any OIDC provider. The mesh's embedded one is the default; it registers
// only the dashboard's public client, so that is the client the Console uses there.
func newAuth(issuer, clientID, clientSecret, callback string, key []byte) *auth {
	if clientID == "" {
		clientID = "netbird-dashboard"
	}
	return &auth{issuer: issuer, clientID: clientID, clientSecret: clientSecret, callback: callback, key: key}
}

func (a *auth) discover(ctx context.Context) (*discovery, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.disc != nil {
		return a.disc, nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.issuer+"/.well-known/openid-configuration", nil)
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var d struct {
		Auth     string `json:"authorization_endpoint"`
		Token    string `json:"token_endpoint"`
		Userinfo string `json:"userinfo_endpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil || d.Auth == "" {
		return nil, fmt.Errorf("discovery at %s: %v", a.issuer, err)
	}
	a.disc = &discovery{d.Auth, d.Token, d.Userinfo}
	return a.disc, nil
}

// sign and verify bind each token to its purpose ("oauth" for the sign-in round trip, "session"), so
// one kind can never be presented as the other: the sign-in cookie is handed to anyone who asks.
func (a *auth) sign(purpose string, v any) string {
	b, _ := json.Marshal(v)
	p := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(purpose + "\x00" + p))
	return p + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *auth) verify(purpose, s string, v any) error {
	i := strings.LastIndex(s, ".")
	if i < 0 {
		return errors.New("malformed")
	}
	p, sig := s[:i], s[i+1:]
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(purpose + "\x00" + p))
	if !hmac.Equal([]byte(base64.RawURLEncoding.EncodeToString(m.Sum(nil))), []byte(sig)) {
		return errors.New("bad signature")
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func randB64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge}
}

// login starts the flow: state and verifier ride in a short signed cookie.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	d, err := s.auth.discover(r.Context())
	if err != nil {
		http.Error(w, "the mesh identity provider is unreachable: "+err.Error(), 502)
		return
	}
	state, verifier := randB64(16), randB64(32)
	http.SetCookie(w, cookie(oauthCookie, s.auth.sign("oauth", map[string]any{"state": state, "verifier": verifier, "exp": time.Now().Add(10 * time.Minute).Unix()}), 600))
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {s.auth.clientID}, "redirect_uri": {s.auth.callback}, "scope": {"openid profile email"},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, d.Auth+"?"+q.Encode(), http.StatusFound)
}

// callback finishes it: code for token, token for userinfo, userinfo for the mesh user.
func (s *server) callback(w http.ResponseWriter, r *http.Request) {
	q := noted(r.Context())
	q.event = "authn_login_fail" // until the session is set
	var st struct {
		State, Verifier string
		Exp             int64
	}
	c, err := r.Cookie(oauthCookie)
	if err != nil || s.auth.verify("oauth", c.Value, &st) != nil || st.Exp < time.Now().Unix() || st.State != r.URL.Query().Get("state") || r.URL.Query().Get("code") == "" {
		http.Error(w, "sign-in expired or did not match; start again at /oauth/login", 400)
		return
	}
	d, err := s.auth.discover(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {r.URL.Query().Get("code")}, "redirect_uri": {s.auth.callback}, "client_id": {s.auth.clientID}, "code_verifier": {st.Verifier}}
	if s.auth.clientSecret != "" {
		form.Set("client_secret", s.auth.clientSecret)
	}
	resp, err := httpc.PostForm(d.Token, form)
	if err != nil {
		http.Error(w, "token exchange: "+err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Desc        string `json:"error_description"`
	}
	body, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body, &tok)
	if tok.AccessToken == "" {
		http.Error(w, fmt.Sprintf("token exchange refused (%s): %s %s", resp.Status, tok.Error, tok.Desc), 502)
		return
	}
	// NetBird 0.79 leaves the first account half-made when it is created unattended; the owner's
	// first sign-in with a token repairs it (netbirdio/netbird#6416). Harmless afterwards.
	if s.meshURL != "" {
		if req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.meshURL+"/api/users/current", nil); err == nil {
			req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
			if resp, err := httpc.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, d.Userinfo, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	ui, err := httpc.Do(req)
	if err != nil {
		http.Error(w, "userinfo: "+err.Error(), 502)
		return
	}
	defer ui.Body.Close()
	var info map[string]any
	_ = json.NewDecoder(ui.Body).Decode(&info)
	email, err := verifiedEmail(info)
	if err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	q.who = email
	id, err := s.lookup(r.Context(), email)
	if err != nil {
		// The provider may still hold a session for someone who is no longer a member.
		http.Error(w, err.Error()+". If that is not who you are, sign out at "+s.auth.issuer+"/logout and try again.", 403)
		return
	}
	if id.Name == "" {
		id.Name = str(info, "name")
	}
	id.Exp = time.Now().Add(7 * 24 * time.Hour).Unix()
	q.event = "authn_login_success"
	http.SetCookie(w, cookie(sessionCookie, s.auth.sign("session", id), 7*24*3600))
	http.SetCookie(w, cookie(oauthCookie, "", -1))
	http.Redirect(w, r, "/", http.StatusFound)
}

// verifiedEmail is the address the provider vouches for. A provider that says it is unverified has not
// proved it belongs to whoever signed in; one that does not say is believed, as it must be.
func verifiedEmail(info map[string]any) (string, error) {
	email := str(info, "email")
	if email == "" {
		return "", errors.New("the identity provider returned no email")
	}
	if v, ok := info["email_verified"]; ok && v != true && v != "true" {
		return "", errors.New("the identity provider has not verified " + email)
	}
	return email, nil
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, cookie(sessionCookie, "", -1))
	http.Redirect(w, r, "/", http.StatusFound)
}

// lookup maps an email to the fabric's own record of that person. The provider
// proved who they are; the Member decides what they may do.
func (s *server) lookup(ctx context.Context, email string) (*identity, error) {
	m, err := s.member(ctx, email)
	if err != nil {
		return nil, err
	}
	if m.Spec.Blocked {
		return nil, errors.New("this account is blocked")
	}
	return identityOf(m), nil
}

func identityOf(m *v1alpha1.Member) *identity {
	return &identity{Email: m.Spec.Email, Name: m.Spec.Name, Role: m.Spec.Role, Admin: m.Admin(), Projects: append([]string{}, m.Spec.Projects...)}
}

func (s *server) member(ctx context.Context, email string) (*v1alpha1.Member, error) {
	ml := &v1alpha1.MemberList{}
	if err := s.c.List(ctx, ml); err != nil {
		return nil, fmt.Errorf("members: %w", err)
	}
	for i := range ml.Items {
		if strings.EqualFold(ml.Items[i].Spec.Email, email) {
			return &ml.Items[i], nil
		}
	}
	return nil, errors.New(email + " is not a member of this fabric")
}

// The Console acts as the person. Every API call made while serving a request goes to
// the Fabric with Impersonate-User set to that person's name (oidc:<email>), so the API server's
// RBAC (bound by Warden from Member records) is the last word, not this code. A few
// steps are the fabric's to take on the person's behalf, after this code has checked
// them (redeeming an offer key, minting invites, the project namespace itself): those
// use elevated(ctx), which carries no identity and so acts as the Console.

type identityKey struct{}

func withIdentity(ctx context.Context, id *identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

func identityFrom(ctx context.Context) *identity {
	id, _ := ctx.Value(identityKey{}).(*identity)
	return id
}

// elevated is the request's context without its person: the Console acting as itself.
func (s *server) elevated(ctx context.Context) context.Context { return withIdentity(ctx, nil) }

type impersonating struct{ next http.RoundTripper }

func (t impersonating) RoundTrip(req *http.Request) (*http.Response, error) {
	if id := identityFrom(req.Context()); id != nil && id.Email != "door" {
		req = req.Clone(req.Context())
		req.Header.Set("Impersonate-User", "oidc:"+strings.ToLower(id.Email))
	}
	return t.next.RoundTrip(req)
}

// who is the signed-in identity. Without sign-in (the development fabric) everyone is the fabric's
// owner Member, checked by the site's RBAC exactly as the owner would be.
func (s *server) who(r *http.Request) *identity {
	// Under /api, protect() has already refreshed the person from their Member; the cookie only says
	// what they were at sign-in, and a demoted admin must not keep the Console's own checks.
	if id := identityFrom(r.Context()); id != nil {
		return id
	}
	if s.auth == nil {
		ml := &v1alpha1.MemberList{}
		if s.c.List(s.elevated(r.Context()), ml) == nil {
			for _, m := range ml.Items {
				if m.Spec.Role == "owner" {
					return &identity{Email: m.Spec.Email, Name: m.Spec.Name, Role: "owner", Admin: true, Projects: m.Spec.Projects}
				}
			}
		}
		return &identity{Email: "door", Role: "owner", Admin: true}
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	var id identity
	if s.auth.verify("session", c.Value, &id) != nil || id.Exp < time.Now().Unix() || id.Email == "" {
		return nil
	}
	return &id
}

// protect demands a session for the API; the page itself is public and sends people to sign in.
func (s *server) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") { // joining and invites are outside /api: their codes are the credential
			id := s.who(r)
			if id != nil && s.auth != nil {
				// Who someone is comes from their Member now, not from when they signed in: a person
				// blocked, removed or demoted loses what they had at once.
				ml := &v1alpha1.MemberList{}
				if err := s.c.List(s.elevated(r.Context()), ml); err != nil {
					http.Error(w, "members: "+err.Error(), 503)
					return
				}
				email, exp := id.Email, id.Exp
				id = nil
				for i := range ml.Items {
					if m := &ml.Items[i]; strings.EqualFold(m.Spec.Email, email) && !m.Spec.Blocked {
						id = identityOf(m)
						id.Exp = exp
					}
				}
			}
			if id == nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(401)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "sign in", "login": "/oauth/login"})
				return
			}
			noted(r.Context()).who = id.Email
			r = r.WithContext(withAuthor(withIdentity(r.Context(), id), id))
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	id := s.who(r)
	keys := []string{}
	if m, err := s.member(s.elevated(r.Context()), id.Email); err == nil {
		keys = append(keys, m.Spec.SSHKeys...)
	}
	writeJSON(w, map[string]any{"auth": s.auth != nil, "email": id.Email, "name": id.Name, "role": id.Role, "admin": id.Admin, "projects": id.Projects, "sshKeys": keys})
}

func (s *server) isAdmin(w http.ResponseWriter, r *http.Request) bool {
	if id := s.who(r); id != nil && id.Admin {
		return true
	}
	http.Error(w, "admins only", 403)
	return false
}

// can: admins, or members of the project.
func (s *server) can(w http.ResponseWriter, r *http.Request, project string) bool {
	if id := s.who(r); id != nil && (id.Admin || slices.Contains(id.Projects, project)) {
		return true
	}
	http.Error(w, "not a member of project "+project, 403)
	return false
}

// canSite: admins, or members of the project that owns the site (its manager), as Git has it.
func (s *server) canSite(w http.ResponseWriter, r *http.Request, site string) bool {
	id := s.who(r)
	if id != nil && id.Admin {
		return true
	}
	if id != nil && validate.Label(site, 0) == nil {
		st := &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: site}}
		ok, err := s.fromGit(r.Context(), st)
		if err != nil {
			answer(w, err, 502)
			return false
		}
		if ok && slices.Contains(id.Projects, st.Spec.Owner) {
			return true
		}
	}
	http.Error(w, "not a manager of site "+site, 403)
	return false
}
