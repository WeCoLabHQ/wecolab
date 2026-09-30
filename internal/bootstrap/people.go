package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/internal/fabric"
)

// People is the people step of a new fabric (install.sh people, warden people): the owner's NetBird
// account, a service token for the fabric, the people group admitted to the Door and NetBird's own
// "Default" policy gone, and the Door's box on the people mesh. Every step checks before it acts. What
// NetBird shows only once (the setup token, the service token) waits in State, mode 0600, until the
// service token is in the Fabric.
type People struct {
	NetBird   string // NetBird's server on this box, http://127.0.0.1:8081
	Zone      string
	Email     string
	PeopleNet string
	State     string
	HTTP      *http.Client
	Password  func() (string, error)                            // asked only while NetBird has no owner
	Commit    func(ctx context.Context, token, id string) error // the service token and its id into the Fabric
	JoinDoor  func(ctx context.Context, setupKey string) error  // this box's NetBird client, as the Door
}

type peopleState struct {
	PAT     string `json:"pat,omitempty"`     // NetBird's setup token: admin for a day
	Token   string `json:"token,omitempty"`   // the fabric's service token, until it is in the Fabric
	TokenID string `json:"tokenId,omitempty"` // its id: the writer's rotation tells it from newer ones
	Done    bool   `json:"done,omitempty"`
}

// Run does what is left of the people step.
func (p *People) Run(ctx context.Context) error {
	st := &peopleState{}
	if b, err := os.ReadFile(p.State); err == nil {
		if err := json.Unmarshal(b, st); err != nil {
			return fmt.Errorf("%s: %w", p.State, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if st.Done {
		return nil
	}
	var inst struct {
		SetupRequired bool `json:"setup_required"`
	}
	for i := 0; ; i++ {
		err := p.call(ctx, "", http.MethodGet, "/api/instance", nil, &inst)
		if err == nil {
			break
		}
		if i == 60 {
			return fmt.Errorf("NetBird is not answering at %s: %w", p.NetBird, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	if inst.SetupRequired {
		pw, err := p.Password()
		if err != nil {
			return err
		}
		var out struct {
			PAT string `json:"personal_access_token"`
		}
		if err := p.call(ctx, "", http.MethodPost, "/api/setup", map[string]any{"email": p.Email, "name": "Owner", "password": pw,
			"create_pat": true, "pat_expire_in": 1}, &out); err != nil {
			return fmt.Errorf("NetBird refused the owner: %w", err)
		}
		if out.PAT == "" {
			return fmt.Errorf("NetBird made the owner but returned no setup token")
		}
		st.PAT = out.PAT
		if err := p.save(st); err != nil {
			return err
		}
	}
	if st.PAT == "" {
		return fmt.Errorf("NetBird has an owner already and %s holds no setup token for it: the people step ran elsewhere, or its state is lost (docs/operations.md)", p.State)
	}
	api := func(method, path string, body, out any) error { return p.call(ctx, st.PAT, method, path, body, out) }

	if st.Token == "" {
		id, err := p.ensure(api, "/api/users?service_user=true", "wecolab", "/api/users",
			map[string]any{"name": "wecolab", "role": "admin", "auto_groups": []string{}, "is_service_user": true})
		if err != nil {
			return err
		}
		var tok struct {
			Token string `json:"plain_token"`
			PAT   struct {
				ID string `json:"id"`
			} `json:"personal_access_token"`
		}
		if err := api(http.MethodPost, "/api/users/"+id+"/tokens", map[string]any{"name": "wecolab", "expires_in": 365}, &tok); err != nil {
			return err
		}
		st.Token, st.TokenID = tok.Token, tok.PAT.ID
		if err := p.save(st); err != nil {
			return err
		}
	}
	var accounts []map[string]any
	if err := api(http.MethodGet, "/api/accounts", nil, &accounts); err != nil || len(accounts) == 0 {
		return fmt.Errorf("NetBird's account: %v", err)
	}
	set, _ := accounts[0]["settings"].(map[string]any)
	if set == nil {
		set = map[string]any{}
	}
	if set["network_range"] != p.PeopleNet || set["groups_propagation_enabled"] != true {
		set["network_range"], set["groups_propagation_enabled"] = p.PeopleNet, true
		if err := api(http.MethodPut, fmt.Sprintf("/api/accounts/%v", accounts[0]["id"]), map[string]any{"settings": set}, nil); err != nil {
			return err
		}
	}
	people, err := p.ensure(api, "/api/groups", "people", "/api/groups", map[string]any{"name": "people", "peers": []string{}})
	if err != nil {
		return err
	}
	door, err := p.ensure(api, "/api/groups", "door", "/api/groups", map[string]any{"name": "door", "peers": []string{}})
	if err != nil {
		return err
	}
	if _, err := p.ensure(api, "/api/policies", "people to the Door", "/api/policies", map[string]any{"name": "people to the Door", "enabled": true,
		"rules": []map[string]any{{"name": "https", "enabled": true, "action": "accept", "bidirectional": false, "protocol": "tcp",
			"ports": []string{"443"}, "sources": []string{people}, "destinations": []string{door}}}}); err != nil {
		return err
	}
	if id, err := p.find(api, "/api/policies", "Default"); err != nil {
		return err
	} else if id != "" { // lets every peer reach every other; people reach only the Door
		if err := api(http.MethodDelete, "/api/policies/"+id, nil, nil); err != nil {
			return err
		}
	}
	if id, err := p.find(api, "/api/peers", "door"); err != nil {
		return err
	} else if id == "" { // a machine, not a person: it joins with a single-use key
		var key struct {
			Key string `json:"key"`
		}
		if err := api(http.MethodPost, "/api/setup-keys", map[string]any{"name": "door", "type": "one-off", "expires_in": 86400,
			"auto_groups": []string{door}, "usage_limit": 1, "ephemeral": false}, &key); err != nil {
			return err
		}
		if err := p.JoinDoor(ctx, key.Key); err != nil {
			return fmt.Errorf("this box on the people mesh: %w", err)
		}
	}
	if err := p.Commit(ctx, st.Token, st.TokenID); err != nil {
		return fmt.Errorf("the service token into the Fabric: %w", err)
	}
	return p.save(&peopleState{Done: true})
}

// find is the id of the object named name in a NetBird list, "" when there is none.
func (p *People) find(api func(method, path string, body, out any) error, list, name string) (string, error) {
	var objs []struct{ ID, Name string }
	if err := api(http.MethodGet, list, nil, &objs); err != nil {
		return "", err
	}
	for _, o := range objs {
		if o.Name == name {
			return o.ID, nil
		}
	}
	return "", nil
}

// ensure is the id of the object named name in list, made with body at create when there is none.
func (p *People) ensure(api func(method, path string, body, out any) error, list, name, create string, body any) (string, error) {
	id, err := p.find(api, list, name)
	if err != nil || id != "" {
		return id, err
	}
	var o struct {
		ID string `json:"id"`
	}
	if err := api(http.MethodPost, create, body, &o); err != nil {
		return "", err
	}
	return o.ID, nil
}

func (p *People) call(ctx context.Context, token, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.NetBird+path, rd)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Token "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	c := p.HTTP
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode/100 != 2 {
		var e struct{ Message string }
		_ = json.Unmarshal(b, &e)
		return fmt.Errorf("netbird %s %s: %s %s", method, path, res.Status, e.Message)
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

// save writes the state as a whole or not at all, readable by root only.
func (p *People) save(st *peopleState) error {
	b, _ := json.Marshal(st)
	f, err := os.CreateTemp(filepath.Dir(p.State), ".people-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p.State) // CreateTemp made it 0600
}

// CheckPassword is the owner's password rule: typed the same twice, at least 8 characters (NetBird may
// ask for more, and says so).
func CheckPassword(pw, again string) error {
	if pw != again {
		return fmt.Errorf("the passwords differ")
	}
	if len(pw) < 8 {
		return fmt.Errorf("the password needs at least 8 characters")
	}
	return nil
}

// AskPassword asks for the owner's password on the terminal, not on stdin, which is often the
// script piped into bash. WECOLAB_PASSWORD answers for an unattended install.
func AskPassword(email string) (string, error) {
	if pw := os.Getenv("WECOLAB_PASSWORD"); pw != "" {
		return pw, CheckPassword(pw, pw)
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("no terminal to ask for the password in: run  sudo bash install.sh people  in a terminal")
	}
	defer tty.Close()
	read := func(prompt string) (string, error) {
		fmt.Fprint(tty, prompt)
		b, err := term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		return string(b), err
	}
	pw, err := read("Choose your password for the Console (" + email + "): ")
	if err != nil {
		return "", err
	}
	again, err := read("The same password again: ")
	if err != nil {
		return "", err
	}
	return pw, CheckPassword(pw, again)
}

// JoinDoor puts this box on the people mesh as the Door with a single-use setup key, handed to the
// NetBird client in a file readable by root only, never on its command line.
func JoinDoor(zone string) func(ctx context.Context, key string) error {
	return func(ctx context.Context, key string) error {
		f, err := os.CreateTemp("", "wecolab-setup-key-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		if _, err := f.WriteString(key); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		out, err := exec.CommandContext(ctx, "netbird", "up", "--management-url", "https://mesh."+zone, "--setup-key-file", f.Name(), "--hostname", "door").CombinedOutput()
		if err != nil {
			return fmt.Errorf("netbird up: %v: %s", err, bytes.TrimSpace(out))
		}
		return nil
	}
}

// netbirdSecret is the people mesh's Secret in the Fabric; the service token joins it.
const netbirdSecret = "secrets/netbird.sops.yaml"

// CommitToken puts the service token into the Fabric's netbird Secret, read and written with Edit so
// a change meanwhile is never overwritten, encrypted again to the recipients the file already has.
func CommitToken(ctx context.Context, g *fabric.Git, ageKey string, who fabric.Author, token, id string) error {
	name := filepath.Base(netbirdSecret)
	_, err := g.Edit(ctx, who, "the people mesh's service token", []string{netbirdSecret}, func(s *fabric.Snapshot) ([]fabric.FileChange, error) {
		enc, ok := s.Get(netbirdSecret)
		if !ok {
			return nil, fmt.Errorf("%s is not in the Fabric", netbirdSecret)
		}
		plain, err := fabric.Decrypt(enc, name, ageKey)
		if err != nil {
			return nil, err
		}
		out, changed, err := withToken(plain, token, id)
		if err != nil || !changed {
			return nil, err
		}
		recips, err := sopsRecipients(enc)
		if err != nil {
			return nil, err
		}
		b, err := fabric.Encrypt(out, name, recips)
		return []fabric.FileChange{{Path: netbirdSecret, Content: b}}, err
	})
	return err
}

// withToken is a Secret's manifest with stringData.token and tokenId set, and whether that changed it.
func withToken(plain []byte, token, id string) ([]byte, bool, error) {
	s := &corev1.Secret{}
	if err := yaml.Unmarshal(plain, s); err != nil {
		return nil, false, err
	}
	if s.StringData["token"] == token && s.StringData["tokenId"] == id {
		return plain, false, nil
	}
	if s.StringData == nil {
		s.StringData = map[string]string{}
	}
	s.StringData["token"], s.StringData["tokenId"] = token, id
	b, err := yaml.Marshal(s)
	return b, true, err
}

// sopsRecipients are the age recipients a SOPS file is encrypted to (the writer keeps them in step).
func sopsRecipients(enc []byte) ([]string, error) {
	var f struct {
		Sops struct {
			Age []struct {
				Recipient string `json:"recipient"`
			} `json:"age"`
		} `json:"sops"`
	}
	if err := yaml.Unmarshal(enc, &f); err != nil {
		return nil, err
	}
	out := []string{}
	for _, a := range f.Sops.Age {
		out = append(out, a.Recipient)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no age recipients in the SOPS file")
	}
	return out, nil
}
