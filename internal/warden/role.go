package warden

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"wecolab.io/wecolab/api/v1alpha1"
)

// AppState is what a site publishes about one app placed there: what it applied and saw of itself.
// Others use it only within that site's scope (docs/plans/2026-09-29-hardening.md, R1).
type AppState struct {
	// Active is the site this site's database takes for the primary (RoleAt): itself where it is one.
	Active string `json:"active"`
	// Archive is the archive this site's database was built for: not the spec's while it waits to be
	// rebuilt.
	Archive string `json:"archive,omitempty"`
	// Demoted is, at a planned move's old primary, the demotion token its database minted for the
	// handover: what the writer copies into Git.
	Demoted *Demotion `json:"demoted,omitempty"`
	// Promotion is this site's Promotion condition: how its part of a move stands, or why it holds back.
	Promotion *metav1.Condition `json:"promotion,omitempty"`
	// Vault is the database's vault as the site holding its history sees it with its own keys.
	Vault *VaultStatus `json:"vault,omitempty"`
	// Endpoints are Nebula address:port pairs where a ready pod of the app answers at this site.
	Endpoints []string `json:"endpoints,omitempty"`
	// Deleting is set while this site still holds something of an app a person deleted.
	Deleting bool `json:"deleting,omitempty"`
}

// Demotion is a demotion token and the handover it was minted for.
type Demotion struct {
	Handover string `json:"handover"`
	Token    string `json:"token"`
}

// Role is one site's CloudNativePG replica settings for an app's database.
type Role struct {
	Primary string // replica.primary: the site whose database is the primary; this site's own promotes
	Source  string // replica.source: the archive it replays while it is not the primary
	Token   string // replica.promotionToken
}

// RoleAt is a site's role, from the App's spec in Git alone (R1). Sites never agree through each other, so
// a site that lags or cannot reach the others never makes a second primary by itself:
//   - no handover: every site takes spec.primary for the primary;
//   - a handover without its token: the old primary (from) takes spec.primary, so it demotes and mints
//     the token, and every other site keeps taking from, so none promotes;
//   - a handover with its token: every site takes spec.primary, and the primary alone carries the token
//     (CloudNativePG refuses one on a spec that is not the primary's). The token is not bound to a
//     target: any replica at exactly its point may promote with it.
//
// While a handover is in flight every site replays the old primary's archive, which ends where the token
// was minted: whichever site is the target by then is exactly there, and no site, the old primary
// included (for which it is its own), goes on along a target's history before the move is done, when a
// retarget may yet drop that history.
func RoleAt(s v1alpha1.AppSpec, self string) Role {
	h := s.Handover
	r := Role{Primary: s.Primary}
	if h != nil && h.Token == "" && self != h.From {
		r.Primary = h.From
	}
	switch {
	case h != nil:
		r.Source = h.From
	case r.Primary != self:
		r.Source = r.Primary
	default:
		r.Source = s.Standby(self) // whom it would follow once demoted
	}
	if h != nil && h.Token != "" && self == s.Primary {
		r.Token = h.Token
	}
	return r
}

// Serving is the site whose database holds the app's history now, by Git: a planned move's old primary
// until its token is in Git, then the primary.
func Serving(s v1alpha1.AppSpec) string {
	if h := s.Handover; h != nil && h.Token == "" {
		return h.From
	}
	return s.Primary
}

// Promotion is where a move stands, by Git.
func Promotion(s v1alpha1.AppSpec) metav1.Condition {
	switch h := s.Handover; {
	case h == nil:
		return cond(v1alpha1.CondPromotion, true, "Applied", "primary is "+s.Primary)
	case h.Token == "":
		return cond(v1alpha1.CondPromotion, false, "Demoting", fmt.Sprintf("%s demotes toward %s and hands over its token", h.From, s.Primary))
	default:
		return cond(v1alpha1.CondPromotion, false, "Promoting", fmt.Sprintf("%s promotes with the token from %s", s.Primary, h.From))
	}
}

// NewDatabase says Git has no sign the app's database was ever made: no generation recorded (the writer
// records the primary's once it has a base backup, and every move records its target) and no handover.
// Only then may the primary make it by initdb.
func NewDatabase(s v1alpha1.AppSpec) bool { return len(s.Archive) == 0 && s.Handover == nil }

// Local is what this site's own database adds to its role.
type Local struct {
	// Created is set once Flux has made the database here: even for a new database Git knows nothing
	// of yet, a primary's is then never made again by initdb.
	Created bool
	// Archive is the archive it keeps writing while it waits to be rebuilt; "" for the spec's.
	Archive string
}

// DBPatch is one site's part in the database topology, as JSON-patch operations on the app's Cluster:
// its role, the archive it writes, every site's archive it may read, and how it is first made.
func DBPatch(app *v1alpha1.App, self string, l Local) []map[string]any {
	db, r := app.Spec.Database, RoleAt(app.Spec, self)
	externals := []any{}
	for _, s := range app.Spec.Sites {
		externals = append(externals, external(s, db, ArchiveName(app, s)))
	}
	// A new database starts empty at the primary and from its source's archive everywhere else. Where
	// the history is held, a database that is not there comes back only from its own archive, never
	// empty and never from another's (and the app is held until a person moves the primary: PlanAt).
	// Every way names the app's database, its owner and the owner's Secret, which comes from Git and is
	// the same at every site (the Console writes it). CNPG would otherwise make its own Secret at each
	// site ("app" by default): the password a primary sets replicates, and a site promoted later keeps
	// a password its own Secret does not hold, since CNPG applies a Secret only when it changes.
	secret := map[string]any{"name": db + "-app"}
	recovery := func(source string) map[string]any {
		return map[string]any{"recovery": map[string]any{"source": source, "database": app.Spec.Workload, "owner": app.Spec.Workload, "secret": secret}}
	}
	bootstrap := recovery(r.Source)
	switch {
	case r.Primary == self && !l.Created && NewDatabase(app.Spec):
		bootstrap = map[string]any{"initdb": map[string]any{"database": app.Spec.Workload, "owner": app.Spec.Workload, "secret": secret}}
	case r.Primary == self || Serving(app.Spec) == self:
		bootstrap = recovery(self)
	}
	archive := ArchiveName(app, self)
	if l.Archive != "" {
		archive = l.Archive
	}
	ops := []map[string]any{
		add("/spec/replica/primary", r.Primary),
		add("/spec/replica/self", self),
		add("/spec/replica/source", r.Source),
		add("/spec/plugins/0/parameters/serverName", archive),
		add("/spec/bootstrap", bootstrap),
		add("/spec/externalClusters", externals),
	}
	if r.Token != "" {
		ops = append(ops, add("/spec/replica/promotionToken", r.Token))
	}
	return ops
}

// add sets a field whether or not it exists.
func add(path string, value any) map[string]any {
	return map[string]any{"op": "add", "path": path, "value": value}
}

// ArchiveName is the Barman serverName a site's database archives under.
func ArchiveName(app *v1alpha1.App, site string) string {
	return archiveName(app.Spec.Database, site, app.Spec.Archive[site])
}

func archiveName(db, site string, gen int) string {
	if gen > 1 {
		return fmt.Sprintf("%s-%s-g%d", db, site, gen)
	}
	return db + "-" + site
}

// external renders a CNPG externalClusters entry reached through the vault.
func external(name, db, serverName string) map[string]any {
	return map[string]any{"name": name, "plugin": map[string]any{
		"name": "barman-cloud.cloudnative-pg.io", "parameters": map[string]any{"barmanObjectName": db + "-vault", "serverName": serverName}}}
}
