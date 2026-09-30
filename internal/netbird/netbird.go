// Package netbird is the part of NetBird's management API WeCoLab uses: people, their groups and
// invites, and the fabric's own service token (docs/architecture.md, "Networking"). People's devices
// join by signing in, so they belong to their person and go with them; nothing here mints setup keys.
package netbird

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"
)

// Client is a NetBird management API. Only the writer's Warden and the Console hold its
// token.
type Client struct {
	URL   string // https://mesh.example.com
	Token string
	HTTP  *http.Client
}

// FromEnv is nil when there is no people mesh. The token may be empty: on a new fabric it reaches the
// netbird Secret only after the people step, and it is rotated, so users read it from the Secret.
func FromEnv() *Client {
	u, t := os.Getenv("NETBIRD_URL"), os.Getenv("NETBIRD_API_TOKEN")
	if u == "" {
		return nil
	}
	return &Client{URL: u, Token: t, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type nbObj struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (n *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, n.URL+"/api"+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+n.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e struct{ Message string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("netbird %s %s: %s %s", method, path, resp.Status, e.Message)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out) // "null" for empty lists decodes to nil slices
}

func (n *Client) list(ctx context.Context, path string) ([]nbObj, error) {
	var out []nbObj
	return out, n.do(ctx, http.MethodGet, path, nil, &out)
}

func find(list []nbObj, name string) (nbObj, bool) {
	for _, o := range list {
		if o.Name == name {
			return o, true
		}
	}
	return nbObj{}, false
}

// EnsureGroup returns the group ID, creating the group if needed.
func (n *Client) EnsureGroup(ctx context.Context, name string) (string, error) {
	gs, err := n.list(ctx, "/groups")
	if err != nil {
		return "", err
	}
	if g, ok := find(gs, name); ok {
		return g.ID, nil
	}
	var g nbObj
	return g.ID, n.do(ctx, http.MethodPost, "/groups", map[string]any{"name": name, "peers": []string{}}, &g)
}

// Users, invites: the mesh's people, mirrored from Member objects.

func (n *Client) Users(ctx context.Context) ([]map[string]any, error) {
	var out []map[string]any
	return out, n.do(ctx, http.MethodGet, "/users", nil, &out)
}

func (n *Client) Invites(ctx context.Context) ([]map[string]any, error) {
	var out []map[string]any
	return out, n.do(ctx, http.MethodGet, "/users/invites", nil, &out)
}

// Invite mints a single-use invite; the token returned sets the person's password once.
func (n *Client) Invite(ctx context.Context, email, name, role string, groups []string, expiresSec int) (token string, err error) {
	if groups == nil {
		groups = []string{}
	}
	var out map[string]any
	if err := n.do(ctx, http.MethodPost, "/users/invites", map[string]any{"email": email, "name": name, "role": role, "auto_groups": groups, "expires_in": expiresSec}, &out); err != nil {
		return "", err
	}
	// Only the bare token: the Console builds /invite?token=<it> and refuses anything else (never the link).
	for _, k := range []string{"invite_token", "token"} {
		if v, _ := out[k].(string); inviteToken.MatchString(v) {
			return v, nil
		}
	}
	return "", fmt.Errorf("netbird invite: no token in response")
}

// inviteToken is the shape the Console's /invite page accepts (cmd/console/members.go).
var inviteToken = regexp.MustCompile(`^[A-Za-z0-9_-]{8,256}$`)

func (n *Client) UpdateUser(ctx context.Context, id, role string, groups []string, blocked bool) error {
	if groups == nil {
		groups = []string{}
	}
	return n.do(ctx, http.MethodPut, "/users/"+url.PathEscape(id), map[string]any{"role": role, "auto_groups": groups, "is_blocked": blocked}, nil)
}

func (n *Client) DeleteUser(ctx context.Context, id string) error {
	return n.do(ctx, http.MethodDelete, "/users/"+url.PathEscape(id), nil, nil)
}

func (n *Client) DeleteInvite(ctx context.Context, id string) error {
	return n.do(ctx, http.MethodDelete, "/users/invites/"+url.PathEscape(id), nil, nil)
}

// GroupNames maps group ids to names.
func (n *Client) GroupNames(ctx context.Context) (map[string]string, error) {
	groups, err := n.list(ctx, "/groups")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, g := range groups {
		out[g.ID] = g.Name
	}
	return out, nil
}

// Token is one of a user's personal access tokens.
type Token struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Expires time.Time `json:"expiration_date"`
	Created time.Time `json:"created_at"`
}

// ServiceUser is the id of the service user called name, "" when there is none.
func (n *Client) ServiceUser(ctx context.Context, name string) (string, error) {
	var us []nbObj
	if err := n.do(ctx, http.MethodGet, "/users?service_user=true", nil, &us); err != nil {
		return "", err
	}
	if u, ok := find(us, name); ok {
		return u.ID, nil
	}
	return "", nil
}

func (n *Client) Tokens(ctx context.Context, user string) ([]Token, error) {
	var out []Token
	return out, n.do(ctx, http.MethodGet, "/users/"+url.PathEscape(user)+"/tokens", nil, &out)
}

// CreateToken mints a token for a user, valid for days (NetBird allows 1 to 365); plain is shown once.
func (n *Client) CreateToken(ctx context.Context, user, name string, days int) (plain string, t Token, err error) {
	var out struct {
		Plain string `json:"plain_token"`
		Token Token  `json:"personal_access_token"`
	}
	err = n.do(ctx, http.MethodPost, "/users/"+url.PathEscape(user)+"/tokens", map[string]any{"name": name, "expires_in": days}, &out)
	if err == nil && (out.Plain == "" || out.Token.ID == "") {
		err = fmt.Errorf("netbird token: no token in the response")
	}
	return out.Plain, out.Token, err
}

func (n *Client) DeleteToken(ctx context.Context, user, id string) error {
	return n.do(ctx, http.MethodDelete, "/users/"+url.PathEscape(user)+"/tokens/"+url.PathEscape(id), nil, nil)
}
