package fabric

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// Git's smart HTTP client is installed once. Only a private request-context
// key selects an optional Git.HTTP transport for this operation; concurrent
// transfers never swap the global registry. Redirects always fail closed.
type refTransportKey struct{}
type refTransportRouter struct{ fallback http.RoundTripper }

func (router refTransportRouter) RoundTrip(req *http.Request) (*http.Response, error) {
	transport := router.fallback
	if selected, ok := req.Context().Value(refTransportKey{}).(http.RoundTripper); ok && selected != nil {
		transport = selected
	}
	response, err := transport.RoundTrip(req)
	if response != nil && response.Body != nil {
		response.Body = &boundedRefResponse{ReadCloser: response.Body, remaining: 2 * refHistoryBytes}
	}
	return response, err
}

func init() {
	base := http.DefaultTransport.(*http.Transport).Clone()
	httpClient := githttp.NewClientWithOptions(&http.Client{Transport: refTransportRouter{fallback: base}, Timeout: 2 * time.Minute},
		&githttp.ClientOptions{RedirectPolicy: githttp.NoFollowRedirects})
	client.InstallProtocol("http", httpClient)
	client.InstallProtocol("https", httpClient)
}

func (g *Git) refContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if g.HTTP == nil {
		return ctx, func() {}
	}
	if g.HTTP.Transport != nil {
		ctx = context.WithValue(ctx, refTransportKey{}, g.HTTP.Transport)
	}
	if g.HTTP.Timeout > 0 {
		return context.WithTimeout(ctx, g.HTTP.Timeout)
	}
	return ctx, func() {}
}

// RefSnapshot pins the exact advertised object IDs and their full reachable
// histories in memory. Its refs and object store are immutable by contract:
// callers may reuse it across destinations but must not mutate its contents.
type RefSnapshot struct {
	store *memory.Storage
	refs  map[string]string
	local map[string]plumbing.ReferenceName
}

type fabricTokenAuth struct{ token string }

func (a fabricTokenAuth) Name() string            { return "forgejo-token" }
func (a fabricTokenAuth) String() string          { return "forgejo-token" }
func (a fabricTokenAuth) SetAuth(r *http.Request) { r.Header.Set("Authorization", "token "+a.token) }

func (g *Git) refEndpoint() (string, transport.AuthMethod, error) {
	if g == nil {
		return "", nil, fmt.Errorf("invalid Git remote")
	}
	u, err := url.Parse(g.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || (u.Scheme != "http" && u.Scheme != "https") || !validPath(g.Repo) || strings.ContainsAny(g.Repo, "?#\\") || strings.Contains(g.Repo, "%") || strings.Contains(g.URL, "#") || strings.Contains(g.URL, "?") {
		return "", nil, fmt.Errorf("invalid Git remote")
	}
	for _, segment := range strings.Split(g.Repo, "/") {
		if segment == "." || segment == ".." || strings.Contains(segment, ":") {
			return "", nil, fmt.Errorf("invalid Git remote")
		}
	}
	if u.Path != "" && (u.Path != "/" || u.RawPath != "") {
		return "", nil, fmt.Errorf("invalid Git remote")
	}
	remote := strings.TrimRight(g.URL, "/") + "/" + g.Repo + ".git"
	var auth transport.AuthMethod = fabricTokenAuth{token: g.Token}
	if g.User != "" {
		auth = &githttp.BasicAuth{Username: g.User, Password: g.Token}
	}
	return remote, auth, nil
}

// transportFailure does not return the remote's error: its URL, HTTP response
// and redirect target may contain credentials supplied by an untrusted server.
func transportFailure(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("git %s: %w", operation, err)
	}
	return fmt.Errorf("git %s failed", operation)
}

// Refs reports all real advertised refs, excluding symbolic HEAD and peeled
// pseudo-refs. An empty repository is distinct from a failed listing.
func (g *Git) Refs(ctx context.Context) (map[string]string, error) {
	address, auth, err := g.refEndpoint()
	if err != nil {
		return nil, err
	}
	ctx, cancel := g.refContext(ctx)
	defer cancel()
	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "destination", URLs: []string{address}})
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth, PeelingOption: git.IgnorePeeled})
	if err != nil && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return nil, transportFailure(ctx, "list refs")
	}
	result := make(map[string]string, len(refs))
	for _, ref := range refs {
		name := ref.Name().String()
		if !strings.HasPrefix(name, "refs/") || ref.Type() != plumbing.HashReference || strings.HasSuffix(name, "^{}") {
			continue
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("duplicate Git ref")
		}
		result[name] = ref.Hash().String()
	}
	return result, nil
}

// FetchRefs pins exactly expected refs into one in-memory object store, with
// their complete reachable histories. A moved or missing source is rejected.
func (g *Git) FetchRefs(ctx context.Context, expected map[string]string) (*RefSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	address, auth, err := g.refEndpoint()
	if err != nil {
		return nil, err
	}
	ctx, cancel := g.refContext(ctx)
	defer cancel()
	store := memory.NewStorage()
	snapshot := &RefSnapshot{store: store, refs: make(map[string]string, len(expected)), local: make(map[string]plumbing.ReferenceName, len(expected))}
	if len(expected) == 0 {
		return snapshot, nil
	}
	keys := make([]string, 0, len(expected))
	for name, hash := range expected {
		if !strings.HasPrefix(name, "refs/") || strings.HasSuffix(name, "^{}") || plumbing.ReferenceName(name).Validate() != nil || !plumbing.IsHash(hash) || plumbing.NewHash(hash).IsZero() {
			return nil, fmt.Errorf("invalid pinned Git ref")
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	specs := make([]config.RefSpec, 0, len(keys))
	for i, name := range keys {
		local := plumbing.ReferenceName(fmt.Sprintf("refs/heads/pinned-%d", i))
		snapshot.local[name] = local
		snapshot.refs[name] = expected[name]
		specs = append(specs, config.RefSpec(name+":"+local.String()))
		// The zero-valued tracking ref is the immutable expected-absent lease
		// for this snapshot's local source ref on any destination.
		tracking := plumbing.ReferenceName("refs/remotes/destination/" + strings.TrimPrefix(local.String(), "refs/heads/"))
		if err := store.SetReference(plumbing.NewHashReference(tracking, plumbing.ZeroHash)); err != nil {
			return nil, fmt.Errorf("prepare Git lease failed")
		}
	}
	fetch := git.NewRemote(&boundedRefStorage{Storage: store, ctx: ctx}, &config.RemoteConfig{Name: "source", URLs: []string{address}})
	err = fetch.FetchContext(ctx, &git.FetchOptions{Auth: auth, RefSpecs: specs, Tags: git.NoTags})
	if errors.Is(err, errRefHistoryBudget) {
		return nil, errRefHistoryBudget
	}
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, transportFailure(ctx, "fetch refs")
	}
	for _, name := range keys {
		ref, err := store.Reference(snapshot.local[name])
		if err != nil || ref.Type() != plumbing.HashReference || ref.Hash().String() != expected[name] {
			return nil, fmt.Errorf("source Git refs changed: %w", ErrConflict)
		}
	}
	return snapshot, nil
}

func (s *RefSnapshot) pushRemote(destination *Git) (*git.Remote, transport.AuthMethod, error) {
	address, auth, err := destination.refEndpoint()
	if err != nil {
		return nil, nil, err
	}
	return git.NewRemote(s.store, &config.RemoteConfig{Name: "destination", URLs: []string{address}}), auth, nil
}

// PushMain advances only main by an ordinary fast-forward push, or succeeds
// without a write when the destination already equals the pinned commit.
func (s *RefSnapshot) PushMain(ctx context.Context, destination *Git) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.store == nil {
		return fmt.Errorf("missing Git snapshot")
	}
	local, ok := s.local["refs/heads/main"]
	if !ok {
		return fmt.Errorf("snapshot has no main ref")
	}
	remote, auth, err := s.pushRemote(destination)
	if err != nil {
		return err
	}
	ctx, cancel := destination.refContext(ctx)
	defer cancel()
	err = remote.PushContext(ctx, &git.PushOptions{RemoteName: remote.Config().Name, Auth: auth, RefSpecs: []config.RefSpec{config.RefSpec(local.String() + ":refs/heads/main")}})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return transportFailure(ctx, "push main")
		}
		return fmt.Errorf("main push refused: %w", ErrConflict)
	}
	return nil
}

// PushPreserved creates only superseded-* refs for pinned source refs. An
// existing identical ref is idempotent; no differing ref may be overwritten.
func (s *RefSnapshot) PushPreserved(ctx context.Context, destination *Git, targets map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.store == nil {
		return fmt.Errorf("missing Git snapshot")
	}
	for target, source := range targets {
		if !strings.HasPrefix(target, "refs/heads/superseded-") || len(target) == len("refs/heads/superseded-") || plumbing.ReferenceName(target).Validate() != nil {
			return fmt.Errorf("invalid preservation target")
		}
		if _, ok := s.local[source]; !ok {
			return fmt.Errorf("source ref is not in Git snapshot")
		}
	}
	if len(targets) == 0 {
		return nil
	}
	remote, auth, err := s.pushRemote(destination)
	if err != nil {
		return err
	}
	ctx, cancel := destination.refContext(ctx)
	defer cancel()
	keys := make([]string, 0, len(targets))
	for target := range targets {
		keys = append(keys, target)
	}
	sort.Strings(keys)
	for _, target := range keys {
		local := s.local[targets[target]]
		// ForceWithLease with zero Hash consults the local zero-valued
		// tracking ref established at snapshot creation. An advertised
		// destination target must be absent, even if its tip is an ancestor.
		err := remote.PushContext(ctx, &git.PushOptions{RemoteName: remote.Config().Name, Auth: auth, RefSpecs: []config.RefSpec{config.RefSpec("+" + local.String() + ":" + target)}, ForceWithLease: &git.ForceWithLease{RefName: plumbing.ReferenceName(target)}})
		if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
			continue
		}
		// A failed create-only push may be an identical retry (or a concurrent
		// creator of the same object). Check it without relaxing the lease.
		refs, listErr := destination.Refs(ctx)
		if listErr == nil && refs[target] == s.refs[targets[target]] {
			continue
		}
		if ctx.Err() != nil {
			return transportFailure(ctx, "push preserved refs")
		}
		return fmt.Errorf("preservation ref push refused: %w", ErrConflict)
	}
	// Independently verify every installed target. A partial push or changed
	// target can never be mistaken for a durable custody receipt.
	refs, err := destination.Refs(ctx)
	if err != nil {
		return err
	}
	for _, target := range keys {
		if refs[target] != s.refs[targets[target]] {
			return fmt.Errorf("preservation ref verification failed: %w", ErrConflict)
		}
	}
	return nil
}
