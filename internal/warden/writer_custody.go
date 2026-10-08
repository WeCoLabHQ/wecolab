package warden

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/nebula"
)

const mainRef = "refs/heads/main"
const preservedPrefix = "refs/heads/superseded-"

// An observation of an object is not a receipt. Only the receiver installs these
// refs under its transition lease, and subsequent handoffs carry them all forward.
type preservationRequest struct {
	Source       string            `json:"source"`
	Claim        Claim             `json:"claim"`
	Refs         map[string]string `json:"refs"`
	PreserveMain bool              `json:"preserveMain"`
}

type preservationReceipt struct {
	Claim        Claim             `json:"claim"`
	RepositoryID int64             `json:"repositoryID"`
	Refs         map[string]string `json:"refs"`
}

func claimAt(ctx context.Context, g *fabric.Git, head string) (Claim, error) {
	b, ok, err := g.ReadAt(ctx, SettingsPath, head)
	if err != nil || !ok {
		return Claim{}, err
	}
	settings, err := settingsIn(b)
	if err != nil {
		return Claim{}, err
	}
	return Claim{Writer: settings.Writer, Epoch: settings.Epoch}, nil
}

func handoffOrder(source string, before, after Claim) bool {
	if before.Writer == "" || after.Writer == "" || before.Epoch < 0 || after.Epoch < 0 {
		return false
	}
	// An already-following site cannot receive custody, so equal-claim edges
	// cannot make a cycle. Writers hand off only to a strictly stronger claim.
	return before == after && before.Writer != source ||
		after.Epoch > before.Epoch || after.Epoch == before.Epoch && after.Writer < before.Writer
}

func preservationTargets(request preservationRequest) (map[string]string, error) {
	if len(validation.IsDNS1123Label(request.Source)) != 0 || request.Refs[mainRef] == "" {
		return nil, fmt.Errorf("invalid preservation source")
	}
	targets := make(map[string]string, len(request.Refs))
	for ref, sha := range request.Refs {
		if !commitID.MatchString(sha) || strings.Trim(sha, "0") == "" {
			return nil, fmt.Errorf("invalid preservation commit")
		}
		var target string
		switch {
		case ref == mainRef:
			if request.PreserveMain {
				target = preservedPrefix + request.Source + "-" + sha
			}
		case strings.HasPrefix(ref, preservedPrefix) && len(ref) > len(preservedPrefix):
			target = ref
		default:
			return nil, fmt.Errorf("unmanaged Git ref prevents repository recreation: %s", ref)
		}
		if target != "" {
			if previous, exists := targets[target]; exists && request.Refs[previous] != sha {
				return nil, fmt.Errorf("preservation target name collides with different history")
			}
			targets[target] = ref
		}
	}
	return targets, nil
}

func (w *Writer) peerGit(ip, password string) *fabric.Git {
	return &fabric.Git{URL: "http://" + net.JoinHostPort(ip, strconv.Itoa(nebula.PortForgejo)),
		Repo: w.Git.Repo, User: ForgejoMirror, Token: password, HTTP: w.Git.HTTP}
}

// replicate updates only main. One pinned object store serves changed peers;
// no scheduled job can later delete a receiver's preservation refs.
func (w *Writer) replicate(ctx context.Context, sites []v1alpha1.Site, head string, own Claim) error {
	if head == "" || own.Writer != w.Site {
		return nil
	}
	var snapshot *fabric.RefSnapshot
	var errs []error
	for _, site := range sites {
		manager := site.Manager()
		if manager == nil || site.Name == w.Site {
			continue
		}
		password := w.mirrorPassword(ctx, site.Name)
		if password == "" {
			continue
		}
		destination := w.peerGit(manager.IP, password)
		at, err := destination.Head(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s's main failed", site.Name))
			continue
		}
		if at == head {
			continue
		}
		if snapshot == nil {
			current, err := claimAt(ctx, w.Git, head)
			if err != nil || current != own {
				return errors.Join(append(errs, fmt.Errorf("writer claim changed before replication"))...)
			}
			snapshot, err = w.Git.FetchRefs(ctx, map[string]string{mainRef: head})
			if err != nil {
				return errors.Join(append(errs, err)...)
			}
		}
		if err := snapshot.PushMain(ctx, destination); err != nil {
			errs = append(errs, fmt.Errorf("replicate to %s: %w", site.Name, err))
		}
	}
	return errors.Join(errs...)
}

// follow runs under this site's transition lease. A writer's acknowledgement
// transfers custody of every preserved ref, not just the current losing head.
func (w *Writer) follow(ctx context.Context, sites []v1alpha1.Site, writer string, steward, claimed bool) error {
	if journal, err := readRecreation(ctx, w.Coordination); err != nil {
		return err
	} else if journal != nil {
		return fmt.Errorf("repository recreation pending")
	}
	if err := w.Git.Protect(ctx, []string{ForgejoMirror}); err != nil {
		return fmt.Errorf("protect main: %w", err)
	}
	var manager *v1alpha1.Box
	for i := range sites {
		if sites[i].Name == writer {
			manager = sites[i].Manager()
		}
	}
	head, err := w.Git.Head(ctx)
	if err != nil || manager == nil || head == "" {
		return err
	}
	at, err := w.ask(ctx, manager.IP, head)
	if err != nil || at.Head == "" {
		return err
	}
	if at.Claim.Writer != writer {
		return fmt.Errorf("selected peer is no longer the writer")
	}
	drained := !at.OnMain
	if !at.OnMain {
		// Protection alone cannot drain a write admitted before it changed.
		// No destructive snapshot is taken until the old Forgejo process is gone.
		if err := w.drainForgejo(ctx); err != nil {
			return err
		}
	}
	refs, err := w.Git.Refs(ctx)
	if err != nil || refs[mainRef] == "" {
		return err
	}
	head = refs[mainRef]
	own, err := claimAt(ctx, w.Git, head)
	if err != nil {
		return err
	}
	at, err = w.ask(ctx, manager.IP, head)
	if err != nil || at.Head == "" {
		return err
	}
	if at.Claim.Writer != writer {
		return fmt.Errorf("selected peer is no longer the writer")
	}
	if !at.OnMain && !drained {
		return fmt.Errorf("source main diverged after ancestry observation; retry after draining Forgejo")
	}
	request := preservationRequest{Source: w.Site, Claim: at.Claim, Refs: refs, PreserveMain: !at.OnMain}
	targets, err := preservationTargets(request)
	if err != nil {
		return err
	}
	if !steward && (claimed || own.Writer == w.Site || len(refs) != 1) {
		return fmt.Errorf("non-steward history requires operator custody before recreation")
	}
	repositoryID, err := w.Git.RepositoryID(ctx)
	if err != nil || repositoryID == 0 {
		return err
	}
	if steward && len(targets) != 0 {
		if !handoffOrder(w.Site, own, request.Claim) {
			return fmt.Errorf("preservation would not advance writer custody")
		}
		receipt, err := w.requestPreservation(ctx, manager.IP, request)
		if err != nil {
			return err
		}
		if receipt.Claim != request.Claim || receipt.RepositoryID <= 0 || len(receipt.Refs) != len(targets) {
			return fmt.Errorf("invalid preservation receipt")
		}
		for target, source := range targets {
			if receipt.Refs[target] != refs[source] {
				return fmt.Errorf("incomplete preservation receipt")
			}
		}
	}
	if at.OnMain {
		return nil // main can fast-forward; prior preservation refs were still handed on
	}
	currentRefs, err := w.Git.Refs(ctx)
	if err != nil || !maps.Equal(currentRefs, refs) {
		return errors.Join(err, fmt.Errorf("source refs changed during preservation"))
	}
	currentID, err := w.Git.RepositoryID(ctx)
	if err != nil || currentID != repositoryID {
		return errors.Join(err, fmt.Errorf("source repository changed during preservation"))
	}
	log.FromContext(ctx).Info("history custody transferred; taking the writer's history", "head", head, "writer", writer)
	return w.recreate(ctx)
}

func (w *Writer) requestPreservation(ctx context.Context, ip string, request preservationRequest) (preservationReceipt, error) {
	var receipt preservationReceipt
	body, err := json.Marshal(request)
	if err != nil {
		return receipt, err
	}
	url := "http://" + net.JoinHostPort(ip, strconv.Itoa(nebula.PortWarden)) + "/fabric/preserve"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return receipt, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := w.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 50 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	response, err := hc.Do(req)
	if err != nil {
		return receipt, fmt.Errorf("preservation request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return receipt, fmt.Errorf("preservation refused: HTTP %d", response.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(response.Body, maxStatus)).Decode(&receipt)
	return receipt, err
}

func (w *Writer) servePreservation(rw http.ResponseWriter, r *http.Request) {
	from, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		http.Error(rw, "registered manager required", http.StatusForbidden)
		return
	}
	var request preservationRequest
	decoder := json.NewDecoder(http.MaxBytesReader(rw, r.Body, maxStatus))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(rw, "invalid preservation request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(rw, "invalid preservation request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	receipt, err := w.receivePreservation(ctx, request, from.Addr().Unmap())
	if err != nil {
		log.FromContext(ctx).Error(err, "history preservation refused")
		http.Error(rw, "history preservation refused", http.StatusConflict)
		return
	}
	writeJSONResponse(rw, receipt)
}

func (w *Writer) receivePreservation(ctx context.Context, request preservationRequest, from netip.Addr) (preservationReceipt, error) {
	var receipt preservationReceipt
	targets, err := preservationTargets(request)
	if err != nil || request.Source == w.Site || request.Claim.Writer != w.Site || !from.IsValid() {
		return receipt, fmt.Errorf("invalid preservation request")
	}
	err = withWriterLock(ctx, w.Coordination, func(ctx context.Context) error {
		if journal, err := readRecreation(ctx, w.Coordination); err != nil {
			return err
		} else if journal != nil {
			return fmt.Errorf("repository recreation pending")
		}
		head, err := w.Git.Head(ctx)
		if err != nil || head == "" {
			return fmt.Errorf("receiver holds no Fabric")
		}
		own, err := claimAt(ctx, w.Git, head)
		if err != nil || own != request.Claim {
			return fmt.Errorf("receiver claim changed")
		}
		b, ok, err := w.Git.ReadAt(ctx, "fabric/sites/"+request.Source+".yaml", head)
		if err != nil || !ok {
			return fmt.Errorf("source is not registered")
		}
		var site v1alpha1.Site
		if yaml.Unmarshal(b, &site) != nil || site.Name != request.Source || !site.Spec.Steward || site.Manager() == nil {
			return fmt.Errorf("registered source steward required")
		}
		manager := site.Manager()
		ip, err := netip.ParseAddr(manager.IP)
		if err != nil || ip.Unmap() != from.Unmap() {
			return fmt.Errorf("source manager address mismatch")
		}
		password := w.mirrorPassword(ctx, site.Name)
		if password == "" {
			return fmt.Errorf("source Git credential unavailable")
		}
		if err := w.quiesceMirrors(ctx); err != nil {
			return err
		}
		repositoryID, err := w.Git.RepositoryID(ctx)
		if err != nil || repositoryID == 0 {
			return fmt.Errorf("receiver repository identity unavailable")
		}
		source := w.peerGit(manager.IP, password)
		before, err := claimAt(ctx, source, request.Refs[mainRef])
		if err != nil || !handoffOrder(site.Name, before, own) {
			return fmt.Errorf("source claim does not precede receiver custody")
		}
		observed, err := source.Refs(ctx)
		if err != nil || !maps.Equal(observed, request.Refs) {
			return fmt.Errorf("source refs changed or omitted")
		}
		if !request.PreserveMain {
			onMain, err := w.Git.OnMain(ctx, request.Refs[mainRef], onMainDepth)
			if err != nil || !onMain {
				return fmt.Errorf("source main requires a preservation ref")
			}
		}
		installed, err := w.Git.Refs(ctx)
		if err != nil {
			return err
		}
		missing := false
		for target, source := range targets {
			missing = missing || installed[target] != request.Refs[source]
		}
		if missing {
			snapshot, err := source.FetchRefs(ctx, request.Refs)
			if err != nil {
				return err
			}
			if err := snapshot.PushPreserved(ctx, w.Git, targets); err != nil {
				return err
			}
		}
		current, err := GitClaim(ctx, w.Git)
		if err != nil || current != own {
			return fmt.Errorf("receiver claim changed during preservation")
		}
		currentID, err := w.Git.RepositoryID(ctx)
		if err != nil || currentID != repositoryID {
			return fmt.Errorf("receiver repository changed during preservation")
		}
		receipt = preservationReceipt{Claim: own, RepositoryID: repositoryID, Refs: make(map[string]string, len(targets))}
		for target, source := range targets {
			receipt.Refs[target] = request.Refs[source]
		}
		return nil
	})
	// withWriterLock checks renewal loss before a receipt can escape.
	if err != nil {
		return preservationReceipt{}, err
	}
	return receipt, nil
}
