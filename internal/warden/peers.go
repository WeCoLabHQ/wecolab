package warden

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/nebula"
)

// Peers reads other sites' published status. Answers are kept a few seconds so every app's
// reconcile in one round shares a single request per site. An unreachable site is simply absent.
type Peers struct {
	Client client.Reader // the Sites, as Flux applied them from the Fabric
	HTTP   *http.Client

	mu    sync.Mutex
	cache map[string]peerAnswer
}

type peerAnswer struct {
	st *SiteStatus
	at time.Time
}

const peerTTL = 5 * time.Second

// Get is the site's status, or false when it has no address or did not answer.
func (p *Peers) Get(ctx context.Context, site string) (*SiteStatus, bool) {
	p.mu.Lock()
	if a, ok := p.cache[site]; ok && time.Since(a.at) < peerTTL {
		p.mu.Unlock()
		return a.st, a.st != nil
	}
	p.mu.Unlock()
	st, err := p.fetch(ctx, site)
	if err != nil {
		st = nil
	}
	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[string]peerAnswer{}
	}
	p.cache[site] = peerAnswer{st, time.Now()}
	p.mu.Unlock()
	return st, st != nil
}

func (p *Peers) fetch(ctx context.Context, site string) (*SiteStatus, error) {
	s := &v1alpha1.Site{}
	if err := p.Client.Get(ctx, types.NamespacedName{Name: site}, s); err != nil {
		return nil, err
	}
	m := s.Manager()
	if m == nil {
		return nil, fmt.Errorf("site %s has no manager yet", site)
	}
	addr := fmt.Sprintf("%s:%d", m.IP, nebula.PortWarden)
	hc := p.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 3 * time.Second}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/status", nil)
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", site, res.Status)
	}
	st := &SiteStatus{}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxStatus)).Decode(st); err != nil {
		return nil, err
	}
	if st.Site != site {
		return nil, fmt.Errorf("%s answered as %q", site, st.Site)
	}
	st.ReceivedAt = time.Now()
	return st, nil
}

// maxStatus bounds what a site's /status may make its peers read.
const maxStatus = 8 << 20

// Gather is what the app's sites say about it: whether each answered, its database's status there,
// and what it published about the app. self, when given, is read locally by the caller and skipped here.
func (p *Peers) Gather(ctx context.Context, app *v1alpha1.App, self string) (ready map[string]bool, db map[string]map[string]any, states map[string]AppState) {
	ready, db, states = map[string]bool{}, map[string]map[string]any{}, map[string]AppState{}
	key := app.Namespace + "/" + app.Name
	for _, site := range app.Spec.Sites {
		if site == self {
			continue
		}
		st, ok := p.Get(ctx, site)
		ready[site] = ok
		if !ok {
			continue
		}
		if d, ok := st.DB[app.Namespace+"/"+app.Spec.Database]; ok && app.Spec.Database != "" {
			db[site] = d
		}
		if s, ok := st.Apps[key]; ok {
			states[site] = s
		}
	}
	return ready, db, states
}
