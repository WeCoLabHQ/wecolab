package warden

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"wecolab.io/wecolab/api/v1alpha1"
)

// The simulation plays random sequences of moves (planned, retargeted before and after the token,
// cancelled, forced, also to a site without the database) and database events (writes, lost databases,
// sites installed again with no record of theirs, sites going away, sites acting on an old copy of Git)
// at three sites, through the rules as the code has them: RoleAt and DBPatch for what each site renders,
// PlanAt and MayDestroy for what it destroys, RememberDemotion and ReportApp for what it says, WriterStep
// for the writer, MoveGates, PlannedMove and ForcedMove for a person. CloudNativePG and the vault are a
// small model: a history is a list of records, a demotion token is the history at the demotion, and a
// replica replays another site's archive only while its own history is a prefix of it.
//
// Invariants, after every step: never two uncondemned sites whose settings allow promotion, and never two
// uncondemned writable databases (a condemned site is one Git ordered rebuilt, so its history is dropped);
// a database is made by initdb once only; none is destroyed unless Git, as that site has it, condemned it
// and another of the same database, writable with a base backup, holds the data (R4). And once every
// site is up and nobody moves anything, every move completes.
func TestSimulation(t *testing.T) {
	seen := map[string]int{}
	for seed := int64(1); seed <= 600; seed++ {
		s := newSim(t, seed)
		for i := 0; i < 400 && !t.Failed(); i++ {
			s.step(seen)
		}
		s.converge(seen)
		if t.Failed() {
			t.Fatalf("seed %d:\n%s", seed, strings.Join(s.log, "\n"))
		}
	}
	for k, n := range map[string]int{"planned": 100, "retarget": 50, "retarget after token refused": 10, "cancel": 50,
		"forced": 100, "forced to a site without the database": 20, "token": 50, "done": 30,
		"rebuilt": 100, "lost": 100, "reinstalled": 50, "restored": 30} {
		if seen[k] < n {
			t.Errorf("the simulation made %q only %d times: %v", k, seen[k], seen)
		}
	}
}

// Once the old primary's token is in Git the target may promote with it at any moment: a planned move to
// another site then could fork the history, so it is refused until the move completes (or a person forces).
func TestNoRetargetAfterTheToken(t *testing.T) {
	s := newSim(t, 0)
	seen := map[string]int{}
	round := func(sites ...string) {
		for _, x := range sites {
			s.sites[x].view = len(s.git) - 1
			s.warden(x, seen)
			s.flux(x)
			s.cnpg(x)
			s.check()
		}
	}
	p, _ := PlannedMove(s.latest(), "b", "m1")
	s.commit(p, "planned to b")
	round("a", "b", "c")
	round("a", "b", "c")
	s.write(seen)
	if s.latest().Handover == nil || s.latest().Handover.Token == "" {
		t.Fatalf("the token should be in Git:\n%s", strings.Join(s.log, "\n"))
	}
	if _, err := PlannedMove(s.latest(), "c", "m2"); err == nil {
		t.Fatal("a planned move elsewhere after the token must be refused")
	}
	if f, err := ForcedMove(s.latest(), "c"); err != nil || f.Handover != nil || f.Archive["b"] < 2 {
		t.Fatalf("forcing stays possible and condemns every other site: %+v %v", f, err)
	}
}

type simDB struct {
	hist          []int // one number per record; a promotion and a demotion add one too
	primary       bool  // writable
	stuck         bool  // its source's history went another way
	demotion      string
	lastPromotion string
	server        string // the archive it writes
}

type simSite struct {
	up        bool
	view      int // the Git version its cluster copy has
	status    v1alpha1.AppStatus
	db        *simDB
	created   bool // Flux's inventory lists the database
	suspended bool
	rendered  *v1alpha1.AppSpec // what Warden rendered from, with local
	local     Local
	role      Role              // what Flux applied
	archives  map[string]string // site -> the archive Flux applied for it
}

type folder struct {
	hist   []int
	backup bool
}

type sim struct {
	t      *testing.T
	rng    *rand.Rand
	names  []string
	git    []v1alpha1.AppSpec
	sites  map[string]*simSite
	vault  map[string]*folder
	tokens map[string][]int
	writer bool // the writer is up
	born   bool // initdb made the app's database once
	lost   bool // a database was lost: a move may then need a person
	n, ids int
	log    []string
}

func newSim(t *testing.T, seed int64) *sim {
	s := &sim{t: t, rng: rand.New(rand.NewSource(seed)), names: []string{"a", "b", "c"}, sites: map[string]*simSite{},
		vault: map[string]*folder{}, tokens: map[string][]int{}, writer: true}
	s.git = []v1alpha1.AppSpec{{Sites: s.names, Primary: "a", Workload: "docs", Database: "docs-db"}}
	for _, n := range s.names {
		s.sites[n] = &simSite{up: true}
	}
	s.converge(map[string]int{}) // the app's first deployment
	if NewDatabase(s.latest()) {
		t.Fatalf("the writer never recorded the database as made:\n%s", strings.Join(s.log, "\n"))
	}
	return s
}

func (s *sim) latest() v1alpha1.AppSpec { return s.git[len(s.git)-1] }

func (s *sim) app(spec v1alpha1.AppSpec, st v1alpha1.AppStatus) *v1alpha1.App {
	a := &v1alpha1.App{Spec: spec, Status: st}
	a.Namespace, a.Name = "vince", "docs"
	return a
}

func (s *sim) logf(format string, args ...any) {
	s.log = append(s.log, fmt.Sprintf(format, args...))
}

func (s *sim) commit(spec v1alpha1.AppSpec, why string) {
	s.git = append(s.git, spec)
	h := "-"
	if spec.Handover != nil {
		h = fmt.Sprintf("%+v", *spec.Handover)
	}
	s.logf("git v%d %s: primary %s handover %s archive %v", len(s.git)-1, why, spec.Primary, h, spec.Archive)
}

func (s *sim) folder(name string) *folder {
	if s.vault[name] == nil {
		s.vault[name] = &folder{}
	}
	return s.vault[name]
}

// report is what a site says of itself, nil when it is down.
func (s *sim) report(x string) *SiteStatus {
	site := s.sites[x]
	if !site.up {
		return nil
	}
	view := s.git[site.view]
	st := &SiteStatus{Site: x, DB: map[string]map[string]any{}, Apps: map[string]AppState{}}
	db, built := site.db.status(), ""
	if site.db != nil {
		st.DB["vince/docs-db"], built = db, site.db.server
	}
	a := ReportApp(s.app(view, site.status), x, db, built)
	if Serving(view) == x { // its look at its own current archive
		a.Vault = &VaultStatus{}
		if s.folder(ArchiveName(s.app(view, site.status), x)).backup {
			a.Vault.LatestBackup = time.Unix(1, 0)
		}
	}
	st.Apps["vince/docs"] = a
	return st
}

// status is the database's CloudNativePG status, nil when there is none.
func (d *simDB) status() map[string]any {
	if d == nil {
		return nil
	}
	phase, ready, archiving := cnpgHealthy, int64(1), "False"
	if d.stuck { // PostgreSQL will not start on a history its source left
		phase, ready = "Waiting for the source's history", 0
	}
	if d.primary && !d.stuck {
		archiving = "True"
	}
	return map[string]any{"phase": phase, "currentPrimary": "db-1", "readyInstances": ready, "demotionToken": d.demotion, "lastPromotionToken": d.lastPromotion,
		"instancesReportedState": map[string]any{"db-1": map[string]any{"isPrimary": d.primary}},
		"conditions":             []any{map[string]any{"type": "ContinuousArchiving", "status": archiving}}}
}

// condemned: Git ordered this site's database rebuilt, so its history is dropped.
func (s *sim) condemned(x string) bool {
	l := s.latest()
	d := s.sites[x].db
	return d != nil && x != l.Primary && d.server != ArchiveName(s.app(l, v1alpha1.AppStatus{}), x)
}

// warden is one reconcile of the app at site x, as AppReconciler.Reconcile makes it.
func (s *sim) warden(x string, seen map[string]int) {
	site := s.sites[x]
	view := s.git[site.view]
	a := s.app(view, site.status)
	here := Here{Exists: site.db != nil, Created: site.created}
	if site.db != nil {
		here.Archive = site.db.server
	}
	var primary *SiteStatus
	if here.Exists && here.Archive != ArchiveName(a, x) {
		primary = s.report(view.Primary)
	}
	p := PlanAt(a, x, here, primary)
	if site.db != nil {
		RememberDemotion(a, x, site.db.status())
	}
	site.status = a.Status
	site.rendered, site.local, site.suspended = &view, p.Local, p.Suspend
	if p.Destroy {
		s.checkDestroy(x)
		s.logf("%s destroys its database (%s) to rebuild it", x, site.db.server)
		site.db = nil
		seen["rebuilt"]++
	}
	if d := site.db; d != nil && view.Primary == x && view.Handover == nil && !d.stuck && !p.Suspend { // ensureBaseBackup
		if f := s.folder(d.server); !f.backup { // a base backup holds the database as it is
			f.hist, f.backup = slices.Clone(d.hist), true
		}
	}
}

// flux applies what Warden rendered at x, making the database when there is none.
func (s *sim) flux(x string) {
	site := s.sites[x]
	if site.suspended || site.rendered == nil {
		return
	}
	ops := DBPatch(s.app(*site.rendered, v1alpha1.AppStatus{}), x, site.local)
	archives := map[string]string{}
	for _, e := range opValue(ops, "/spec/externalClusters").([]any) {
		m := e.(map[string]any)
		archives[m["name"].(string)] = m["plugin"].(map[string]any)["parameters"].(map[string]any)["serverName"].(string)
	}
	server := opValue(ops, "/spec/plugins/0/parameters/serverName").(string)
	boot := opValue(ops, "/spec/bootstrap").(map[string]any)
	if site.db == nil {
		if _, ok := boot["initdb"]; ok {
			if s.born {
				s.t.Errorf("%s made the app's database again empty", x)
			}
			s.born = true
			site.db = &simDB{hist: []int{s.record()}, primary: true, server: server}
		} else {
			src := boot["recovery"].(map[string]any)["source"].(string)
			f := s.vault[archives[src]]
			if f == nil || !f.backup {
				return // nothing to recover from yet
			}
			site.db = &simDB{hist: slices.Clone(f.hist), server: server}
			s.logf("%s made its database from %s's archive %s", x, src, archives[src])
		}
		site.created = true
	}
	site.db.server, site.archives = server, archives
	tok, _ := opValue(ops, "/spec/replica/promotionToken").(string)
	site.role = Role{Primary: opValue(ops, "/spec/replica/primary").(string), Source: opValue(ops, "/spec/replica/source").(string), Token: tok}
}

func (s *sim) record() int { s.n++; return s.n }

// cnpg is CloudNativePG at x acting on the settings Flux applied.
func (s *sim) cnpg(x string) {
	site := s.sites[x]
	d, r := site.db, site.role
	if d == nil || r.Primary == "" {
		return
	}
	switch {
	case r.Primary == x && d.primary:
	case r.Primary == x && r.Token == "":
		s.promote(x, "")
	case r.Primary == x:
		s.follow(x, r.Source) // until exactly the token's point, never past it: the old primary stopped there
		if slices.Equal(d.hist, s.tokens[r.Token]) {
			s.promote(x, r.Token)
		}
	case d.primary:
		d.hist, d.primary = append(d.hist, s.record()), false
		d.demotion = fmt.Sprintf("T%d", len(s.tokens))
		s.tokens[d.demotion] = slices.Clone(d.hist)
		s.folder(d.server).hist = slices.Clone(d.hist) // archived before the token is minted
		s.logf("%s demotes: %s", x, d.demotion)
	default:
		s.follow(x, r.Source)
	}
	if d.primary {
		s.folder(d.server).hist = slices.Clone(d.hist)
	}
}

func (s *sim) promote(x, tok string) {
	d := s.sites[x].db
	d.hist, d.primary, d.stuck = append(d.hist, s.record()), true, false
	if tok != "" {
		d.lastPromotion = tok
	}
	s.logf("%s promotes (token %q)", x, tok)
}

func (s *sim) follow(x, src string) {
	d, f := s.sites[x].db, s.vault[s.sites[x].archives[src]]
	switch {
	case f == nil:
	case isPrefix(d.hist, f.hist):
		d.hist, d.stuck = slices.Clone(f.hist), false
	default:
		d.stuck = !isPrefix(f.hist, d.hist)
	}
}

func isPrefix(a, b []int) bool { return len(a) <= len(b) && slices.Equal(a, b[:len(a)]) }

// step is one random event.
func (s *sim) step(seen map[string]int) {
	x := s.names[s.rng.Intn(len(s.names))]
	site := s.sites[x]
	h := s.latest().Handover
	switch k := s.rng.Intn(100); {
	case k < 2 || k < 16 && h != nil && h.Token != "": // a person is more likely to act while a token is out
		s.move(seen)
	case k < 16:
		site.up = !site.up
		s.logf("%s up=%v", x, site.up)
	case k < 18:
		s.writer = !s.writer
	case k < 19 && site.db != nil && s.databases() > 1 && s.rng.Intn(3) == 0:
		site.db, s.lost = nil, true
		seen["lost"]++
		s.logf("%s loses its database", x)
		if s.rng.Intn(2) == 0 { // installed again: Flux's record and the App's status here went too
			site.created, site.status = false, v1alpha1.AppStatus{}
			seen["reinstalled"]++
		}
	case k < 24 && site.db != nil && site.db.primary:
		site.db.hist = append(site.db.hist, s.record())
		s.folder(site.db.server).hist = slices.Clone(site.db.hist)
	case k < 40 && site.up:
		site.view += s.rng.Intn(len(s.git) - site.view)
	case k < 55 && site.up:
		s.warden(x, seen)
	case k < 67 && site.up:
		created := site.db == nil
		s.flux(x)
		if created && site.db != nil && Serving(s.git[site.view]) == x {
			seen["restored"]++
		}
	case k < 85 && site.up:
		s.cnpg(x)
	case k >= 85 && s.writer:
		s.write(seen)
	}
	s.check()
}

func (s *sim) databases() int {
	n := 0
	for _, site := range s.sites {
		if site.db != nil {
			n++
		}
	}
	return n
}

// write is the writer's step, on Git as it is now.
func (s *sim) write(seen map[string]int) {
	a := s.app(s.latest(), v1alpha1.AppStatus{})
	had := a.Spec.Handover
	if msg := WriterStep(a, s.report); msg != "" {
		switch {
		case had == nil:
			seen["born"]++
		case had.Token == "":
			seen["token"]++
		default:
			seen["done"]++
		}
		s.commit(a.Spec, msg)
	}
}

// staged is the Console's gate for a move's target, from what it says of itself.
func (s *sim) staged(to string) bool {
	st := s.report(to)
	if st == nil {
		return false
	}
	db, ok := st.DB["vince/docs-db"]
	return ok && str(db, "phase") == cnpgHealthy && st.Apps["vince/docs"].Archive == ArchiveName(s.app(s.latest(), v1alpha1.AppStatus{}), to)
}

func (s *sim) anyStaged() bool {
	return slices.ContainsFunc(s.names, s.staged)
}

// move is a person's move, made on Git as it is now: a planned one only through the Console's gate, a
// forced one mostly to a staged site, or to one that has its database when none is staged, but now and
// then anywhere.
func (s *sim) move(seen map[string]int) {
	l := s.latest()
	to := s.names[s.rng.Intn(len(s.names))]
	h := l.Handover
	if s.rng.Intn(6) == 0 {
		st := s.report(to)
		if s.rng.Intn(4) != 0 && !s.staged(to) && (st == nil || st.DB["vince/docs-db"] == nil || s.anyStaged()) {
			return
		}
		f, _ := ForcedMove(l, to)
		seen["forced"]++
		if s.sites[to].db == nil {
			seen["forced to a site without the database"]++
		}
		s.commit(f, "forced to "+to)
		return
	}
	kind := "planned"
	switch {
	case to == l.Primary:
		return
	case h != nil && to == h.From:
		kind = "cancel"
	case h != nil && h.Token != "":
		kind = "retarget after token"
	case h != nil:
		kind = "retarget"
	}
	reports := map[string]*SiteStatus{}
	for _, x := range s.names {
		reports[x] = s.report(x)
	}
	if MoveGates(s.app(l, v1alpha1.AppStatus{}), reports, to, time.Now()) != nil {
		return
	}
	s.ids++
	p, err := PlannedMove(l, to, fmt.Sprintf("m%d", s.ids))
	if err != nil {
		if kind == "retarget after token" {
			seen["retarget after token refused"]++
		}
		return
	}
	seen[kind]++
	if d := s.sites[l.Primary].db; kind == "retarget after token" && d != nil && d.lastPromotion == h.Token {
		seen["retarget after the target promoted"]++
	}
	s.commit(p, kind+" to "+to)
}

func (s *sim) check() {
	promoting, writable := []string{}, []string{}
	for _, x := range s.names {
		site := s.sites[x]
		if site.db == nil || s.condemned(x) {
			continue
		}
		if site.role.Primary == x {
			promoting = append(promoting, x)
		}
		if site.db.primary {
			writable = append(writable, x)
		}
	}
	if len(promoting) > 1 || len(writable) > 1 {
		s.t.Errorf("two histories: settings allow promotion at %v, writable at %v", promoting, writable)
	}
}

// checkDestroy: Git as x has it condemned x's database, and another of the same database (the same
// first record, as PostgreSQL's system identifier), writable with a base backup, holds the data it is
// rebuilt from. Either may be condemned or chosen by a newer commit x has not seen yet; the data then
// still lives in the one that proved itself, which is not destroyed before the next primary proves itself
// in turn.
func (s *sim) checkDestroy(x string) {
	site := s.sites[x]
	if view := s.app(s.git[site.view], v1alpha1.AppStatus{}); view.Spec.Primary == x || site.db.server == ArchiveName(view, x) {
		s.t.Errorf("%s destroyed a database Git did not condemn", x)
	}
	for _, y := range s.names {
		if d := s.sites[y].db; y != x && d != nil && d.primary && !d.stuck && s.folder(d.server).backup && d.hist[0] == site.db.hist[0] {
			return
		}
	}
	s.t.Errorf("%s destroyed its database with no writable copy of the same database backed up elsewhere", x)
}

// pick is where a person forces the primary: the target if it has a database, else one Git has not
// condemned, else the only one left.
func (s *sim) pick() string {
	for _, ok := range []func(string) bool{func(x string) bool { return !s.condemned(x) }, func(string) bool { return true }} {
		for _, x := range append([]string{s.latest().Primary}, s.names...) {
			if s.sites[x].db != nil && ok(x) {
				return x
			}
		}
	}
	return ""
}

// converge brings every site up with the newest Git and lets them work; then every move must have
// completed. A person forces the primary only where the site holding the history says its database is
// gone (DatabaseMissing), and, after a database was lost, where a move stalls.
func (s *sim) converge(seen map[string]int) {
	s.writer = true
	for _, site := range s.sites {
		site.up = true
	}
	quiet := 0
	for round := 0; round < 80; round++ {
		version := len(s.git)
		for _, x := range s.names {
			s.sites[x].view = len(s.git) - 1
			s.warden(x, seen)
			s.flux(x)
			s.cnpg(x)
			s.check()
		}
		s.write(seen)
		if quiet++; len(s.git) != version {
			quiet = 0
		}
		held := s.sites[Serving(s.latest())]
		if held.db == nil && held.suspended || s.lost && quiet > 20 && s.latest().Handover != nil {
			if x := s.pick(); x != "" {
				f, _ := ForcedMove(s.latest(), x)
				s.commit(f, "a person forces to "+x)
				quiet = 0
			}
		}
	}
	l := s.latest()
	p := s.sites[l.Primary]
	switch {
	case l.Handover != nil:
		s.t.Errorf("the move never completed: %+v", *l.Handover)
	case p.db == nil || !p.db.primary || p.suspended:
		s.t.Errorf("no writable primary at %s", l.Primary)
	}
	for _, x := range s.names {
		site := s.sites[x]
		switch {
		case x == l.Primary || p.db == nil:
		case site.db == nil || site.suspended:
			s.t.Errorf("%s has no database", x)
		case site.db.primary || !slices.Equal(site.db.hist, p.db.hist) || site.db.server != ArchiveName(s.app(l, v1alpha1.AppStatus{}), x):
			s.t.Errorf("%s is not a replica of %s: %+v", x, l.Primary, *site.db)
		}
	}
}
