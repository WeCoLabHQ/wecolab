package warden

import (
	"context"
	"fmt"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

// A planned move in three steps (docs/plans/2026-09-29-hardening.md, R1). A person sets spec.primary and a
// handover {id, from}; every site derives its role from that (RoleAt) and the old primary demotes. The old
// primary publishes the demotion token its database minted for that handover (Demoted), and the writer
// copies it into spec.handover.token (WriterStep); the new primary promotes with it. Once the new
// primary reports it promoted with the token, the writer clears the handover.

// RememberDemotion keeps, in the old primary's own App status and before it demotes, the demotion token its
// database (db, its CloudNativePG status) showed when the site first saw the handover, if it was then
// writable: a leftover from before its last promotion, never to be handed over as this one's. A database
// already demoted then did so for an earlier handover from here that never finished (its site may never
// have seen that one cancelled), and has replayed nothing since: its token is this handover's too.
// Forgotten once no handover starts here.
func RememberDemotion(app *v1alpha1.App, self string, db map[string]any) {
	switch h := app.Spec.Handover; {
	case h == nil || h.From != self:
		app.Status.Demoting = v1alpha1.Demoting{}
	case app.Status.Demoting.Handover != h.ID && writable(db):
		app.Status.Demoting = v1alpha1.Demoting{Handover: h.ID, Stale: str(db, "demotionToken")}
	case app.Status.Demoting.Handover != h.ID:
		app.Status.Demoting = v1alpha1.Demoting{Handover: h.ID}
	}
}

// Demoted is what the old primary hands over for the handover in flight: its database's demotion token,
// once it demoted and the token is not the leftover.
func Demoted(app *v1alpha1.App, self string, db map[string]any) *Demotion {
	h, d, token := app.Spec.Handover, app.Status.Demoting, str(db, "demotionToken")
	if h == nil || h.From != self || d.Handover != h.ID || token == "" || token == d.Stale || writable(db) {
		return nil
	}
	return &Demotion{Handover: h.ID, Token: token}
}

// WriterStep is the writer's step for one app's database, made on the App as Git has it, from what the
// sites say about themselves (report is nil for a site that did not answer). A new database is recorded
// as made once its primary proves it holds it with a base backup, so it is never made again empty
// (NewDatabase). In a planned move, without a token it copies the old primary's demotion token for this
// handover; with one, it clears the handover once the primary reports it promoted with it and is
// healthy. It returns what it did, "" for nothing.
func WriterStep(a *v1alpha1.App, report func(site string) *SiteStatus) string {
	h, key := a.Spec.Handover, a.Namespace+"/"+a.Name
	switch {
	case a.Spec.Database == "":
		return ""
	case h == nil && NewDatabase(a.Spec) && proven(a, report(a.Spec.Primary)) == nil:
		a.Spec.Archive = map[string]int{a.Spec.Primary: 1}
		return fmt.Sprintf("%s: %s made the app's database and backed it up; it is never made again empty", key, a.Spec.Primary)
	case h == nil:
		return ""
	}
	if h.Token == "" {
		st := report(h.From)
		if st == nil || st.Site != h.From {
			return ""
		}
		d := st.Apps[key].Demoted
		if d == nil || d.Handover != h.ID || d.Token == "" {
			return ""
		}
		h2 := *h
		h2.Token = d.Token
		a.Spec.Handover = &h2
		return fmt.Sprintf("%s: %s demoted; %s promotes with its token", key, h.From, a.Spec.Primary)
	}
	st := report(a.Spec.Primary)
	if st == nil || st.Site != a.Spec.Primary {
		return ""
	}
	db := st.DB[a.Namespace+"/"+a.Spec.Database]
	if str(db, "lastPromotionToken") != h.Token || str(db, "phase") != cnpgHealthy {
		return ""
	}
	a.Spec.Handover = nil
	return fmt.Sprintf("%s: %s promoted with the token from %s; the move is done", key, a.Spec.Primary, h.From)
}

// Handovers makes the writer's steps (WriterStep) for every app's database, at the writer: Writer.lead
// calls Steps, so the steps follow the one writer election.
type Handovers struct {
	Client client.Client // the Apps and Sites as Flux applied them: where to look, never what to write
	Site   string
	Git    *fabric.Git
	Peers  *Peers
}

// Steps makes every step due, each decided again on what Git holds. Only the writer calls it.
func (h *Handovers) Steps(ctx context.Context) error {
	apps := &v1alpha1.AppList{}
	if err := h.Client.List(ctx, apps); err != nil {
		return err
	}
	report := func(site string) *SiteStatus {
		st, _ := h.Peers.Get(ctx, site)
		return st
	}
	sites := &v1alpha1.SiteList{}
	if err := h.Client.List(ctx, sites); err != nil {
		return err
	}
	for i := range apps.Items {
		a := &apps.Items[i]
		if a.Spec.Deleted {
			if DeletionDone(a, sites.Items, report) {
				if err := h.forget(ctx, a.Namespace, a.Name); err != nil {
					return err
				}
			}
			continue
		}
		msg := WriterStep(a.DeepCopy(), report) // this site's copy only says where a step may be due
		if msg == "" {
			continue
		}
		done, err := h.step(ctx, a.Namespace, a.Name, msg, report)
		if err != nil {
			return err
		}
		if done {
			log.FromContext(ctx).Info(msg)
		}
	}
	return nil
}

// step makes the step msg names on the App's file in Git, if Git still calls for that one: where the copy
// lags, Git's step is made on a later pass, under its own message.
func (h *Handovers) step(ctx context.Context, ns, name, msg string, report func(string) *SiteStatus) (bool, error) {
	gvk := v1alpha1.GroupVersion.WithKind("App")
	p, _ := fabric.Path(gvk, ns, name)
	sha, err := h.Git.Edit(ctx, fabric.Author{Name: "Warden", Email: "warden@" + h.Site}, msg, []string{p}, func(s *fabric.Snapshot) ([]fabric.FileChange, error) {
		b, ok := s.Get(p)
		if !ok {
			return nil, nil
		}
		a := &v1alpha1.App{}
		if err := yaml.Unmarshal(b, a); err != nil {
			return nil, err
		}
		a.Namespace, a.Name = ns, name
		if WriterStep(a, report) != msg {
			return nil, nil
		}
		out, err := fabric.YAML(a, gvk)
		return []fabric.FileChange{{Path: p, Content: out}}, err
	})
	return sha != "", err
}

// DeletionDone is whether every site of a deleted app has removed its part: each answers, and none still
// reports the app. A site that is down holds the App in Git until it is back and has removed its part; a
// site no longer in the fabric is not waited for.
func DeletionDone(a *v1alpha1.App, sites []v1alpha1.Site, report func(site string) *SiteStatus) bool {
	key := a.Namespace + "/" + a.Name
	for _, name := range a.Spec.Sites {
		if !slices.ContainsFunc(sites, func(s v1alpha1.Site) bool { return s.Name == name }) {
			continue
		}
		st := report(name)
		if st == nil || st.Site != name {
			return false
		}
		if _, ok := st.Apps[key]; ok {
			return false
		}
	}
	return true
}

// forget removes a deleted app's App and folder from the Fabric, if Git still marks it deleted.
func (h *Handovers) forget(ctx context.Context, ns, name string) error {
	gvk := v1alpha1.GroupVersion.WithKind("App")
	p, _ := fabric.Path(gvk, ns, name)
	folder, err := h.Git.List(ctx, fabric.AppFolder(ns, name))
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("%s/%s: removed from the Fabric; every site deleted its part", ns, name)
	sha, err := h.Git.Edit(ctx, fabric.Author{Name: "Warden", Email: "warden@" + h.Site}, msg, append([]string{p}, folder...), func(s *fabric.Snapshot) ([]fabric.FileChange, error) {
		b, ok := s.Get(p)
		if !ok {
			return nil, nil
		}
		a := &v1alpha1.App{}
		if err := yaml.Unmarshal(b, a); err != nil || !a.Spec.Deleted {
			return nil, err
		}
		out := []fabric.FileChange{{Path: p}}
		for _, f := range folder {
			out = append(out, fabric.FileChange{Path: f})
		}
		return out, nil
	})
	if sha != "" {
		log.FromContext(ctx).Info(msg)
	}
	return err
}
