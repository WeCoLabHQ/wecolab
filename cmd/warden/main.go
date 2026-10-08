// Command warden is WeCoLab's controller, the same binary at every site (docs/architecture.md):
//
//	warden site         a site's Warden: apps, projects, people, status, certificates, the writer's jobs
//	warden entrance     in the Door's pod at a public site: routes and the fabric's DNS zone
//	warden node-agent   on a laptop node: whether its person is using it
//	warden bootstrap    a new fabric's first commit (install.sh)
//	warden people       the people mesh on a new fabric's first box (install.sh people)
//	warden upgrade      system/ and crds/ of this version, committed to the Fabric
//	warden takeover     this steward becomes the writer, when no Console can be reached (install.sh takeover)
//	warden image        an image tarball of static binaries (install.sh, hack/dev)
//	warden entrance-init the Door's pod before it starts: Names' Corefile, an empty zone, credentials
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	coordinationclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	_ "golang.org/x/crypto/x509roots/fallback" // images carry no CA bundle of their own

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/bootstrap"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/nebula"
	"wecolab.io/wecolab/internal/netbird"
	"wecolab.io/wecolab/internal/warden"
)

// Version is set at build time.
var Version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: warden site|entrance|entrance-init|node-agent|bootstrap|people|upgrade|record-restore|takeover|image|version [flags]")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "site":
		err = site(args)
	case "entrance":
		err = entrance(args)
	case "node-agent":
		err = nodeAgent(args)
	case "bootstrap":
		err = boot(args)
	case "people":
		err = people(args)
	case "upgrade":
		err = upgrade(args)
	case "record-restore":
		err = recordRestore(args)
	case "takeover":
		err = takeover(args)
	case "image":
		err = image(args)
	case "entrance-init":
		err = entranceInit(args)
	case "version":
		fmt.Println(Version)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "warden:", err)
		os.Exit(1)
	}
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

// logger: one JSON line per event at info level, errors with a stack trace, times in RFC 3339.
func logger() {
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{TimeEncoder: zapcore.RFC3339TimeEncoder})))
}

func site(args []string) error {
	fs := flag.NewFlagSet("site", flag.ExitOnError)
	name := fs.String("site", "", "this site's name")
	statusAddr := fs.String("status-addr", ":8093", "where status listens: the manager's Nebula address; the certificate service listens there on port 8094")
	resync := fs.Duration("resync", 15*time.Second, "how often to look again")
	_ = fs.Parse(args)
	logger()
	if *name == "" {
		return fmt.Errorf("--site is required")
	}
	// The manager and the writer use the same local cluster configuration.
	config := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		return err
	}
	coordination, err := coordinationclient.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("writer coordination: %w", err)
	}
	c := mgr.GetClient()
	peers := &warden.Peers{Client: c}
	mesh := netbird.FromEnv()
	for _, r := range []interface{ SetupWithManager(ctrl.Manager) error }{
		&warden.AppReconciler{Client: c, Site: *name, Peers: peers, Resync: *resync},
		&warden.ProjectReconciler{Client: c, Site: *name, Resync: 5 * time.Minute},
		&warden.PeopleReconciler{Client: c, Site: *name, Mesh: mesh, Resync: 5 * time.Minute},
		&warden.DomainReconciler{Client: c, Site: *name, Resync: 30 * time.Minute},
		&warden.SiteReconciler{Client: c, Site: *name, Resync: time.Minute},
	} {
		if err := r.SetupWithManager(mgr); err != nil {
			return err
		}
	}
	// Status for managers on 8093, certificates for every box on 8094: the Nebula firewall tells them
	// apart by port (docs/plans/2026-09-29-hardening.md, R6).
	host, _, err := net.SplitHostPort(*statusAddr)
	if err != nil {
		return fmt.Errorf("--status-addr: %w", err)
	}
	status, certs := http.NewServeMux(), http.NewServeMux()
	g := fabric.GitFromEnv()
	agent := &warden.SiteAgent{Client: mgr.GetAPIReader(), Site: *name, Git: g, SchemaVersion: Version}
	status.Handle("/status", agent)
	status.Handle("/healthz", agent)
	certs.Handle("/nebula/", &warden.CertService{Client: c})
	certs.Handle("/healthz", agent)
	if g != nil {
		w := &warden.Writer{Client: c, APIReader: mgr.GetAPIReader(), Site: *name, Git: g, Peers: peers, Coordination: coordination}
		status.Handle("/fabric/", w) // ancestry observations and serialized history custody
		_ = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error { w.Run(ctx, 20*time.Second); return nil }))
	}
	serve(mgr, *statusAddr, status)
	serve(mgr, net.JoinHostPort(host, strconv.Itoa(nebula.PortCerts)), certs)
	ctrl.Log.Info("warden", "site", *name, "status", *statusAddr, "version", Version)
	return mgr.Start(ctrl.SetupSignalHandler())
}

// serve runs an HTTP server for as long as the manager. Other sites and boxes reach it over Nebula, so
// none of them may hold a connection open for ever.
func serve(mgr ctrl.Manager, addr string, h http.Handler) {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: time.Minute, IdleTimeout: 2 * time.Minute}
	_ = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		go func() { <-ctx.Done(); _ = srv.Close() }()
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}
		return nil
	}))
}

func entrance(args []string) error {
	fs := flag.NewFlagSet("entrance", flag.ExitOnError)
	name := fs.String("site", "", "this site's name")
	routes := fs.String("routes", "/dynamic", "Traefik's file provider directory")
	zone := fs.String("zone-file", "/zones/db.zone", "the zone file Names serves")
	peopleNet := fs.String("people-net", warden.PeopleNet, "NetBird's address range")
	tls := fs.Bool("tls", true, "Let's Encrypt certificates (off in the development fabric)")
	acme := fs.String("acme-addr", "127.0.0.1:8095", "where Traefik asks for DNS challenges")
	auth := fs.String("auth-dir", "/door-auth", "the DNS challenge's credentials, from entrance-init")
	_ = fs.Parse(args)
	logger()
	if _, err := netip.ParsePrefix(*peopleNet); err != nil {
		return fmt.Errorf("--people-net: %w", err)
	}
	user, err := os.ReadFile(filepath.Join(*auth, "user"))
	if err != nil {
		return err
	}
	password, err := os.ReadFile(filepath.Join(*auth, "password"))
	if err != nil {
		return err
	}
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme()})
	if err != nil {
		return err
	}
	e := &warden.Entrance{Client: c, Peers: &warden.Peers{Client: c}, Site: *name, RoutesDir: *routes, ZoneFile: *zone,
		PeopleNet: *peopleNet, NetBirdIP: func() string { return interfaceAddr("wt0") }, TLS: *tls, User: string(user), Password: string(password)}
	ctrl.Log.Info("entrance", "site", *name, "version", Version)
	return e.Run(ctrl.SetupSignalHandler(), *acme)
}

// entranceInit prepares the Door's pod: Names' Corefile, bound by name to the interface of the default
// route (the box's network: the pod is on it); an empty zone until the entrance Warden writes one; and
// random credentials for Traefik's DNS-challenge requests to the entrance Warden, which only this pod's
// containers can read.
func entranceInit(args []string) error {
	fs := flag.NewFlagSet("entrance-init", flag.ExitOnError)
	zone := fs.String("zone", "", "the fabric's zone")
	zones := fs.String("zones", "/zones", "Names' directory: its Corefile and the zone file")
	auth := fs.String("auth-dir", "/door-auth", "where the DNS challenge's credentials go")
	_ = fs.Parse(args)
	routes, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return err
	}
	iface, err := warden.DefaultRouteInterface(string(routes))
	if err != nil {
		return err
	}
	zoneFile := filepath.Join(*zones, "db.zone")
	corefile, err := warden.Corefile(*zone, iface, zoneFile)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*zones, "Corefile"), []byte(corefile), 0o644); err != nil {
		return err
	}
	if _, err := os.Stat(zoneFile); err != nil {
		if err := os.WriteFile(zoneFile, []byte(warden.ZoneFile(*zone, 1, nil, "", "", nil)), 0o644); err != nil {
			return err
		}
	}
	for _, f := range []string{"user", "password"} {
		if _, err := os.Stat(filepath.Join(*auth, f)); err != nil {
			if err := os.WriteFile(filepath.Join(*auth, f), []byte(rand.Text()), 0o640); err != nil {
				return err
			}
		}
	}
	return nil
}

// interfaceAddr is an interface's first IPv4 address, "" when it has none.
func interfaceAddr(name string) string {
	i, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	as, _ := i.Addrs()
	for _, a := range as {
		if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() {
			return p.Addr().String()
		}
	}
	return ""
}

func nodeAgent(args []string) error {
	fs := flag.NewFlagSet("node-agent", flag.ExitOnError)
	mode := fs.String("mode-file", "/wecolab/mode", "written by WeCoLab for Mac: idle or active")
	_ = fs.Parse(args)
	logger()
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme()})
	if err != nil {
		return err
	}
	return warden.RunNodeAgent(ctrl.SetupSignalHandler(), c, os.Getenv("NODE_NAME"), *mode)
}

func boot(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	o := bootstrap.Options{}
	fs.StringVar(&o.Zone, "zone", "", "the fabric's DNS zone")
	fs.StringVar(&o.Email, "email", "", "the owner's email")
	fs.StringVar(&o.Site, "site", "", "the first site's name")
	fs.StringVar(&o.Project, "project", "", "the owner's project")
	fs.StringVar(&o.PublicAddress, "public", "", "the first box's public IPv4 address")
	fs.StringVar(&o.Host, "host", "", "the first box's host name")
	fs.StringVar(&o.Version, "version", Version, "WeCoLab's image tag")
	fs.BoolVar(&o.Dev, "dev", false, "the development fabric")
	network := fs.String("network", "10.77.0.0/16", "the Nebula network")
	keyFile := fs.String("box-key", "", "the first box's Nebula public key (PEM)")
	out := fs.String("out", "", "where to write the Fabric repository")
	state := fs.String("state", "", "where to write the private values the install step needs (mode 0600)")
	_ = fs.Parse(args)
	var err error
	if o.Network, err = netip.ParsePrefix(*network); err != nil {
		return err
	}
	if o.BoxKey, err = os.ReadFile(*keyFile); err != nil {
		return err
	}
	for f, v := range map[string]string{"zone": o.Zone, "email": o.Email, "site": o.Site, "project": o.Project, "host": o.Host, "out": *out, "state": *state} {
		if v == "" {
			return fmt.Errorf("--%s is required", f)
		}
	}
	res, err := bootstrap.Create(o, *out)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	return os.WriteFile(*state, b, 0o600)
}

// people is the people step of a new fabric, on its first box: the Forgejo there in WECOLAB_GIT_URL and
// WECOLAB_GIT_TOKEN, the password from the terminal or WECOLAB_PASSWORD, never from the command line.
func people(args []string) error {
	fs := flag.NewFlagSet("people", flag.ExitOnError)
	p := &bootstrap.People{}
	fs.StringVar(&p.Zone, "zone", "", "the fabric's DNS zone")
	fs.StringVar(&p.Email, "email", "", "the owner's email")
	fs.StringVar(&p.PeopleNet, "people-net", warden.PeopleNet, "NetBird's address range")
	fs.StringVar(&p.NetBird, "netbird", "http://127.0.0.1:8081", "NetBird's server on this box")
	fs.StringVar(&p.State, "state", "/var/lib/wecolab/people.json", "what the step keeps until it is done (mode 0600)")
	ageKey := fs.String("age-key", "", "this site's age key file, to open the Fabric's netbird secret")
	_ = fs.Parse(args)
	g := fabric.GitFromEnv()
	if g == nil || p.Zone == "" || p.Email == "" || *ageKey == "" {
		return fmt.Errorf("usage: WECOLAB_GIT_URL=… WECOLAB_GIT_TOKEN=… warden people --zone Z --email E --age-key FILE")
	}
	key, err := os.ReadFile(*ageKey)
	if err != nil {
		return err
	}
	p.Password = func() (string, error) { return bootstrap.AskPassword(p.Email) }
	p.JoinDoor = bootstrap.JoinDoor(p.Zone)
	p.Commit = func(ctx context.Context, token, id string) error {
		return bootstrap.CommitToken(ctx, g, strings.TrimSpace(string(key)), fabric.Author{Name: "Owner", Email: p.Email}, token, id)
	}
	return p.Run(interrupted())
}

// upgrade requires an operator-controlled maintenance window: inventory and recoverability,
// additive schemas, restartable migration, then platform rollout and fresh site reports.
func upgrade(args []string) error {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	stage := fs.String("stage", "review", "review|begin|schemas|migrate|platform|complete")
	version := fs.String("version", Version, "same release version staged on every site")
	lock := fs.Bool("lock-flux", false, "lock down Flux after all sites run this version")
	snapshot := fs.String("snapshot", "", "operator's offline Fabric snapshot, required at begin")
	recovery := fs.String("recovery-material", "", "operator's offline recovery material, required at begin")
	ageKey := fs.String("age-key", "", "writer's age identity file, required for migration")
	proof := fs.String("backup-proof", "", "operator backup and actual restore readback receipt JSON")
	localSite := fs.String("local-site", "", "writer manager's site for direct current vault inspection")
	_ = fs.Parse(args)
	g := fabric.GitFromEnv()
	if g == nil {
		return fmt.Errorf("WECOLAB_GIT_URL and WECOLAB_GIT_TOKEN are required")
	}
	ctx := interrupted()
	report, err := fabric.UpgradeReview(ctx, g)
	if err != nil {
		return err
	}
	switch *stage {
	case "review":
		b, err := json.MarshalIndent(report, "", "  ")
		if err == nil {
			fmt.Println(string(b))
		}
		return err
	case "begin":
		for _, entry := range []struct{ name, path string }{{"snapshot", *snapshot}, {"recovery material", *recovery}} {
			if entry.path == "" {
				return fmt.Errorf("%s path required before maintenance", entry.name)
			}
			info, err := os.Stat(entry.path)
			if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				return fmt.Errorf("%s must be an existing nonempty regular file: %s", entry.name, entry.path)
			}
		}
		revision, err := g.Head(ctx)
		if err != nil {
			return err
		}
		if revision == "" {
			return fmt.Errorf("upgrade requires a nonempty writer Git main revision")
		}
		paths, err := g.ListAt(ctx, "fabric/sites", revision)
		if err != nil {
			return err
		}
		var sites []string
		for _, path := range paths {
			if strings.HasSuffix(path, ".yaml") {
				sites = append(sites, strings.TrimSuffix(filepath.Base(path), ".yaml"))
			}
		}
		if err := upgradeBackupPreflight(ctx, g, revision, sites, *proof, *snapshot, *recovery, *localSite); err != nil {
			return err
		}
		if err := fabric.BeginUpgrade(ctx, g, sites, revision); err != nil {
			return err
		}
		fmt.Println("maintenance gate active; apply additive schemas, migrate, then roll platform controllers")
		return nil
	case "schemas":
		if report.Phase != "maintenance" {
			return fmt.Errorf("begin maintenance before additive schema rollout")
		}
		sha, _, err := bootstrap.Upgrade(ctx, g, bootstrap.Options{Version: *version, SchemaOnly: true},
			fabric.Author{Name: "WeCoLab", Email: "fabric@wecolab"})
		if err == nil {
			fmt.Println("additive schema commit", sha)
		}
		return err
	case "platform":
		if report.Phase != "maintenance" {
			return fmt.Errorf("begin maintenance before platform rollout")
		}
		if _, ready, err := g.Read(ctx, fabric.MigrationCompletePath); err != nil || !ready {
			return fmt.Errorf("complete legacy migration before controller rollout: %v", err)
		}
		sha, kept, err := bootstrap.Upgrade(ctx, g, bootstrap.Options{Version: *version, LockFlux: *lock}, fabric.Author{Name: "WeCoLab", Email: "fabric@wecolab"})
		if err != nil {
			return err
		}
		fmt.Println("platform commit", sha)
		if kept {
			fmt.Println("Flux remains unlocked until all sites run the schema-aware Warden")
		}
		return nil
	case "migrate":
		if report.Phase != "maintenance" || *ageKey == "" {
			return fmt.Errorf("migration requires maintenance and --age-key PATH")
		}
		key, err := os.ReadFile(*ageKey)
		if err != nil {
			return err
		}
		return warden.MigrateLegacy(ctx, g, strings.TrimSpace(string(key)))
	case "complete":
		if report.Phase != "maintenance" {
			return fmt.Errorf("maintenance gate required to complete migration")
		}
		config, err := ctrl.GetConfig()
		if err != nil {
			return fmt.Errorf("site report kubeconfig: %w", err)
		}
		kube, err := client.New(config, client.Options{Scheme: scheme()})
		if err != nil {
			return err
		}
		peers := &warden.Peers{Client: kube}
		versions := make(map[string]bool, len(report.Sites))
		for _, site := range report.Sites {
			st, ok := peers.Get(ctx, site)
			if !ok || st == nil || st.Time.Before(time.Now().Add(-time.Minute)) || st.Time.After(time.Now().Add(time.Minute)) || st.SchemaVersion != *version {
				return fmt.Errorf("site %s has no fresh schema-aware report for %s", site, *version)
			}
			versions[site] = true
		}
		return fabric.CompleteUpgrade(ctx, g, versions)
	default:
		return fmt.Errorf("unknown upgrade stage %q", *stage)
	}
}

// takeover makes this steward the writer from the box, for when the writer's site is gone and with it the
// Console people reach (docs/operations.md, "The writer"): root on a steward's manager is trusted with the
// fabric already.
func takeover(args []string) error {
	fs := flag.NewFlagSet("takeover", flag.ExitOnError)
	site := fs.String("site", "", "this site's name")
	_ = fs.Parse(args)
	g := fabric.GitFromEnv()
	if g == nil || *site == "" {
		return fmt.Errorf("usage: WECOLAB_GIT_URL=… WECOLAB_GIT_TOKEN=… warden takeover --site SITE")
	}
	config := ctrl.GetConfigOrDie()
	c, err := client.New(config, client.Options{Scheme: scheme()})
	if err != nil {
		return err
	}
	coordination, err := coordinationclient.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("writer coordination: %w", err)
	}
	epoch, err := warden.TakeOver(interrupted(), g, *site, &warden.Peers{Client: c}, fabric.Author{Name: "WeCoLab", Email: "fabric@" + *site}, coordination)
	if err != nil {
		return err
	}
	fmt.Printf("%s is the writer now (epoch %d); every site follows as its Warden sees the claim\n", *site, epoch)
	return nil
}

// interrupted ends at SIGINT or SIGTERM.
func interrupted() context.Context {
	c, _ := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return c
}

func image(args []string) error {
	fs := flag.NewFlagSet("image", flag.ExitOnError)
	ref := fs.String("name", "", "image reference, e.g. ghcr.io/wecolabhq/warden:v1")
	arch := fs.String("arch", "amd64", "amd64 or arm64")
	out := fs.String("out", "", "the tarball to write")
	_ = fs.Parse(args)
	files := []bootstrap.File{}
	for _, a := range fs.Args() { // from=to; the first is the entrypoint
		from, to, ok := strings.Cut(a, "=")
		if !ok {
			return fmt.Errorf("files are from=to, got %q", a)
		}
		files = append(files, bootstrap.File{From: from, To: to})
	}
	if *ref == "" || *out == "" || len(files) == 0 {
		return fmt.Errorf("usage: warden image --name REF --arch ARCH --out FILE from=to...")
	}
	return bootstrap.Image(*ref, *arch, files, *out)
}
