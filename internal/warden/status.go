package warden

import (
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"wecolab.io/wecolab/api/v1alpha1"
)

// Inputs is everything Compute needs, gathered by the caller so the logic
// stays a pure function.
type Inputs struct {
	Now          time.Time
	ClusterReady map[string]bool           // whether each site answered
	DB           map[string]map[string]any // CNPG Cluster .status as reported by each site
	Archive      map[string]string         // the archive each site's database was built for, as it reports
	// Target is the site the standby gates are judged for: a move's target; "" for the first standby.
	Target string
	// Routed is nil when this warden owns no Door; RouteMsg explains the value.
	Routed   *bool
	RouteMsg string
	// MaxBackupAge is how old the newest base backup may be before VaultFresh is false.
	MaxBackupAge time.Duration
	// Vault is what the vault itself reports; nil when the app has no database.
	Vault *VaultStatus
	// RPO is the app's declared objective, used against the vault's newest WAL.
	RPO time.Duration
}

const cnpgHealthy = "Cluster in healthy state"

func cond(t string, ok bool, reason, msg string) metav1.Condition {
	st := metav1.ConditionFalse
	if ok {
		st = metav1.ConditionTrue
	}
	return metav1.Condition{Type: t, Status: st, Reason: reason, Message: msg}
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

func num(m map[string]any, k string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[k].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case int:
		return int64(v)
	}
	return 0
}

// cnpgCondition reads status.conditions[type].status from a CNPG status map.
func cnpgCondition(m map[string]any, typ string) string {
	list, _ := m["conditions"].([]any)
	for _, c := range list {
		cm, _ := c.(map[string]any)
		if str(cm, "type") == typ {
			return str(cm, "status")
		}
	}
	return ""
}

// Compute derives the App conditions for the site holding the history and, for the gates a move needs,
// its target.
func Compute(app *v1alpha1.App, primary string, in Inputs) []metav1.Condition {
	standby := in.Target
	if standby == "" {
		standby = app.Spec.Standby(primary)
	}
	hasDB := app.Spec.Database != ""
	pdb, sdb := in.DB[primary], in.DB[standby]
	var out []metav1.Condition

	// PrimaryHealthy
	switch {
	case !in.ClusterReady[primary]:
		out = append(out, cond(v1alpha1.CondPrimaryHealthy, false, "SiteNotReady", primary+" does not answer"))
	case hasDB && (str(pdb, "phase") != cnpgHealthy || str(pdb, "currentPrimary") == ""):
		out = append(out, cond(v1alpha1.CondPrimaryHealthy, false, "DatabaseNotHealthy", fmt.Sprintf("postgres at %s: %q", primary, str(pdb, "phase"))))
	default:
		out = append(out, cond(v1alpha1.CondPrimaryHealthy, true, "Healthy", "site and database ready at "+primary))
	}

	// StandbyStaged: the standby site is up and its replica, built for its current archive, holds capacity.
	switch built := in.Archive[standby]; {
	case standby == "":
		out = append(out, cond(v1alpha1.CondStandbyStaged, false, "NoStandby", "spec.sites needs a second site"))
	case !in.ClusterReady[standby]:
		out = append(out, cond(v1alpha1.CondStandbyStaged, false, "SiteNotReady", standby+" does not answer"))
	case hasDB && built != "" && built != ArchiveName(app, standby):
		out = append(out, cond(v1alpha1.CondStandbyStaged, false, "Rebuilding", standby+"'s database waits to be rebuilt from the vault"))
	case hasDB && num(sdb, "readyInstances") < 1:
		out = append(out, cond(v1alpha1.CondStandbyStaged, false, "ReplicaNotReady", "postgres replica at "+standby+" has no ready instance"))
	default:
		out = append(out, cond(v1alpha1.CondStandbyStaged, true, "Staged", "standby running at "+standby))
	}

	// WithinRPO: the vault's newest archived WAL is the honest measure for an
	// archive-fed replica. The replica must also be healthy to apply it.
	// ponytail: replica replay position is not read yet; add it for a true end-to-end lag.
	switch {
	case !hasDB:
		out = append(out, cond(v1alpha1.CondWithinRPO, true, "NoDatabase", "no database to replicate"))
	case cnpgCondition(pdb, "ContinuousArchiving") != "True":
		out = append(out, cond(v1alpha1.CondWithinRPO, false, "ArchiveStalled", "primary is not archiving WAL"))
	case str(sdb, "phase") != cnpgHealthy:
		out = append(out, cond(v1alpha1.CondWithinRPO, false, "ReplicaNotHealthy", fmt.Sprintf("replica at %s: %q", standby, str(sdb, "phase"))))
	case in.Vault != nil && in.Vault.Err == "" && in.RPO > 0:
		// Archiving is healthy (a failed archive flips ContinuousArchiving above) and
		// the replica is healthy, so the only WAL not yet in the vault is the one
		// still being written. An idle primary archives nothing, which is zero
		// exposure, not staleness. The WAL age is reported for the operator.
		// ponytail: a true lag needs the primary's write position; the reference app's
		// heartbeat row (docs/architecture.md) turns WAL age into a real number.
		age := in.Now.Sub(in.Vault.LatestWAL).Round(time.Second)
		switch {
		case in.Vault.LatestWAL.IsZero():
			out = append(out, cond(v1alpha1.CondWithinRPO, false, "NoWAL", "no WAL segment in the vault yet"))
		case age > in.RPO:
			out = append(out, cond(v1alpha1.CondWithinRPO, true, "Idle", fmt.Sprintf("archiving healthy, replica healthy; newest archived WAL %s old (no newer writes), objective %s", age, in.RPO)))
		default:
			out = append(out, cond(v1alpha1.CondWithinRPO, true, "Archiving", fmt.Sprintf("newest archived WAL %s old, objective %s, replica healthy", age, in.RPO)))
		}
	default:
		out = append(out, cond(v1alpha1.CondWithinRPO, true, "Archiving", "WAL archived continuously, replica healthy"))
	}

	// VaultFresh: judged by the vault, not by the primary.
	switch {
	case !hasDB:
		out = append(out, cond(v1alpha1.CondVaultFresh, true, "NoDatabase", "no database to back up"))
	case in.Vault == nil:
		out = append(out, cond(v1alpha1.CondVaultFresh, false, "NotChecked", "vault not inspected"))
	case in.Vault.Err != "":
		out = append(out, cond(v1alpha1.CondVaultFresh, false, "VaultUnreachable", in.Vault.Err))
	case in.Vault.ObjectLock == "":
		out = append(out, cond(v1alpha1.CondVaultFresh, false, "NoObjectLock", "bucket has no default Object Lock retention"))
	case in.Vault.LatestBackup.IsZero():
		out = append(out, cond(v1alpha1.CondVaultFresh, false, "NoBackup", "no base backup in the vault"))
	case in.Now.Sub(in.Vault.LatestBackup) > in.MaxBackupAge:
		out = append(out, cond(v1alpha1.CondVaultFresh, false, "Stale", fmt.Sprintf("newest base backup %s old, Object Lock %s", in.Now.Sub(in.Vault.LatestBackup).Round(time.Minute), in.Vault.ObjectLock)))
	default:
		out = append(out, cond(v1alpha1.CondVaultFresh, true, "Fresh", fmt.Sprintf("newest base backup %s old, Object Lock %s", in.Now.Sub(in.Vault.LatestBackup).Round(time.Minute), in.Vault.ObjectLock)))
	}

	// Routed
	switch {
	case in.Routed == nil:
		out = append(out, cond(v1alpha1.CondRouted, true, "NoDoor", "no public door managed by this warden"))
	case *in.Routed:
		out = append(out, cond(v1alpha1.CondRouted, true, "Door", in.RouteMsg))
	default:
		out = append(out, cond(v1alpha1.CondRouted, false, "DoorUnreachable", in.RouteMsg))
	}
	return out
}

// Ready folds the gate conditions into one. It is never stored as an input.
func Ready(conds []metav1.Condition) metav1.Condition {
	for _, c := range conds {
		if c.Type != v1alpha1.CondReady && c.Type != v1alpha1.CondPromotion && c.Status != metav1.ConditionTrue {
			return cond(v1alpha1.CondReady, false, c.Type, c.Message)
		}
	}
	return cond(v1alpha1.CondReady, true, "AllGatesPass", "protected")
}

// SetCondition upserts by type, keeping LastTransitionTime stable when status is unchanged.
func SetCondition(list *[]metav1.Condition, c metav1.Condition, now time.Time, gen int64) {
	c.ObservedGeneration = gen
	for i := range *list {
		if (*list)[i].Type == c.Type {
			if (*list)[i].Status == c.Status {
				c.LastTransitionTime = (*list)[i].LastTransitionTime
			} else {
				c.LastTransitionTime = metav1.NewTime(now)
			}
			(*list)[i] = c
			return
		}
	}
	c.LastTransitionTime = metav1.NewTime(now)
	*list = append(*list, c)
}

// AppView is an app as a whole: the site holding its history by Git (Serving), the gates from what the
// sites report about themselves, judged for a planned move's target while one is in flight, and where the
// move stands.
func AppView(app *v1alpha1.App, sites map[string]*SiteStatus, now time.Time, maxBackupAge time.Duration) (string, []metav1.Condition) {
	active, in, states := observe(app, sites, now, maxBackupAge)
	if active != app.Spec.Primary {
		in.Target = app.Spec.Primary
	}
	p := Promotion(app.Spec)
	if c := states[active].Promotion; c != nil && c.Status != metav1.ConditionTrue && p.Status == metav1.ConditionTrue {
		p = *c // why the site holding the history holds back: its own words, shown as text only
	}
	return active, append(Compute(app, active, in), p)
}

// MoveGates says whether a planned move of the primary to `to` may go ahead (nil) or why not, from what the
// sites report about themselves and the App as Git has it: to's replica is staged, built for its current
// archive; and a new move starts from a writable, healthy primary that archives, with to's replica keeping
// up (WithinRPO). A retarget needs only the new target staged, since every site then replays the old
// primary's archive, and a cancel nothing. The Console asks it before PlannedMove.
func MoveGates(app *v1alpha1.App, sites map[string]*SiteStatus, to string, now time.Time) error {
	h := app.Spec.Handover
	if app.Spec.Database == "" || h != nil && h.Token == "" && to == h.From {
		return nil
	}
	active, in, _ := observe(app, sites, now, 0)
	in.Target = to
	gates := map[string]bool{v1alpha1.CondStandbyStaged: true}
	if h == nil {
		gates[v1alpha1.CondPrimaryHealthy], gates[v1alpha1.CondWithinRPO] = true, true
	}
	for _, c := range Compute(app, active, in) {
		if gates[c.Type] && c.Status != metav1.ConditionTrue {
			return fmt.Errorf("%s: %s", c.Type, c.Message)
		}
	}
	if h == nil && !writable(in.DB[active]) {
		return fmt.Errorf("%s: %s is not a writable primary yet", v1alpha1.CondPrimaryHealthy, active)
	}
	return nil
}

// observe gathers Compute's inputs for the app's sites from their reports, for the site holding the
// history (Serving), with the Door's view of it.
func observe(app *v1alpha1.App, sites map[string]*SiteStatus, now time.Time, maxBackupAge time.Duration) (string, Inputs, map[string]AppState) {
	ready, db, states, built := map[string]bool{}, map[string]map[string]any{}, map[string]AppState{}, map[string]string{}
	key := app.Namespace + "/" + app.Name
	for _, site := range app.Spec.Sites {
		st := sites[site]
		ready[site] = st != nil
		if st == nil {
			continue
		}
		if d, ok := st.DB[app.Namespace+"/"+app.Spec.Database]; ok && app.Spec.Database != "" {
			db[site] = d
		}
		if a, ok := st.Apps[key]; ok {
			states[site], built[site] = a, a.Archive
		}
	}
	active := Serving(app.Spec)
	in := Inputs{Now: now, ClusterReady: ready, DB: db, Archive: built, MaxBackupAge: maxBackupAge, RPO: app.Spec.RPO.Duration}
	if app.Spec.Database != "" {
		in.Vault = states[active].Vault
		if in.Vault == nil {
			in.Vault = &VaultStatus{Err: "the primary has not looked at its vault yet"}
		}
	}
	routed := len(states[active].Endpoints) > 0
	in.Routed = &routed
	in.RouteMsg = "no ready pod answers at " + active
	if routed {
		in.RouteMsg = "answers at " + active + ": " + strings.Join(states[active].Endpoints, ", ")
	}
	return active, in, states
}
