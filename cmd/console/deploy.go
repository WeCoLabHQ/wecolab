package main

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/validate"
	"wecolab.io/wecolab/internal/warden"
)

// deployRequest is the Deploy form: image and hostname to a running App.
type deployRequest struct {
	Catalog                                 string // a catalog slug: image, port, database, env, volumes and sidecars come from the entry
	Name, Project, Image, Hostname, Primary string
	Port                                    int
	Sites                                   []string
	Database, Mesh                          bool
	RPO                                     string
	// Vault is needed once per project when Database is set: stored as Secret "vault".
	Vault struct{ KeyID, Key, Bucket, Endpoint string }
}

func obj(apiVersion, kind, name, ns string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetName(name)
	u.SetNamespace(ns)
	if spec != nil {
		u.Object["spec"] = spec
	}
	return u
}

// deploy creates or updates everything an App needs on the fabric. Placement, roles, routes and
// gates come from Warden once the App exists. Every decision is made from Git: where the project may
// place work, which names are taken, and the App and Secret a redeploy starts from.
func (s *server) deploy(w http.ResponseWriter, r *http.Request) {
	var in deployRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	in.Name, in.Project = strings.TrimSpace(in.Name), strings.TrimSpace(in.Project)
	if !s.can(w, r, in.Project) {
		return
	}
	var entry *catalogEntry
	if in.Catalog != "" {
		e, ok := s.catalog[in.Catalog]
		if !ok {
			http.Error(w, "no such catalog entry", 404)
			return
		}
		entry = &e
		in.Image, in.Port, in.Database = e.Image, e.Port, e.Database != nil
		if in.Name == "" {
			in.Name = e.Slug
		}
		if e.MeshOnly {
			in.Hostname, in.Mesh = "", true
		}
	}
	switch {
	case validate.Name(in.Name) != nil || validate.Name(in.Project) != nil:
		http.Error(w, "name and project: lowercase letters, digits and inner dashes, at most 32 characters, not a name the fabric uses", 400)
		return
	case strings.TrimSpace(in.Image) == "":
		http.Error(w, "image required", 400)
		return
	case !slices.Contains(in.Sites, in.Primary):
		http.Error(w, "pick sites and a primary among them", 400)
		return
	}
	if in.Port == 0 {
		in.Port = 8080
	}
	in.Hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.Hostname), "."))
	if in.Hostname == "" && (entry == nil || !entry.MeshOnly) {
		in.Hostname = in.Name + "." + s.domain
	}
	ctx := r.Context()
	placementBefore, placementExists, err := s.git.Read(ctx, fabric.PlacementRevisionPath)
	if err != nil || !placementExists {
		answer(w, cmpErr(err, fail(409, "placement migration required")), 502)
		return
	}
	sites, err := s.sitesInGit(ctx)
	if err != nil {
		answer(w, err, 502)
		return
	}
	offers, err := readDir[v1alpha1.Offer](ctx, s.git, "fabric/offers")
	if err != nil {
		answer(w, err, 502)
		return
	}
	pools, err := readDir[v1alpha1.Pool](ctx, s.git, "fabric/pools")
	if err != nil {
		answer(w, err, 502)
		return
	}
	allowed := warden.Allowed(in.Project, sites, offers, pools)
	for _, x := range in.Sites {
		if !allowed[x] {
			http.Error(w, "project "+in.Project+" owns nothing and holds no offer at site "+x, 403)
			return
		}
	}
	apps, err := s.appsInGit(ctx)
	if err != nil {
		answer(w, err, 502)
		return
	}
	if in.Hostname != "" {
		if err := s.hostnameAllowed(ctx, in.Project, in.Name, in.Hostname, apps); err != nil {
			answer(w, err, 400)
			return
		}
	}
	if in.Mesh {
		if err := meshFree(apps, in.Project, in.Name); err != nil {
			answer(w, err, 400)
			return
		}
	}
	app := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: in.Project}}
	if exists, err := s.fromGit(ctx, app); err != nil || exists && app.Spec.Primary != in.Primary {
		answer(w, cmpErr(err, fail(409, "the primary is %s: move it on the Apps page", app.Spec.Primary)), 502)
		return
	}
	if app.Spec.Database != "" && app.Spec.ArchiveID == "" {
		http.Error(w, "database archive identity migration required", 409)
		return
	}
	if app.Spec.Database != "" && !in.Database {
		http.Error(w, "cannot remove an existing database by redeploy; delete the app after data recovery", 409)
		return
	}
	preexistingArchive, preexistingDatabase, oldHostname, oldMesh := app.Spec.ArchiveID, app.Spec.Database, app.Spec.Hostname, app.Spec.Mesh
	archiveCandidate := ""
	if in.Database && app.Spec.ArchiveID == "" {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			answer(w, err, 500)
			return
		}
		archiveCandidate = "id-" + hex.EncodeToString(raw)
	}
	// Applied with the tenant labels every time: a server-side apply without them
	// would remove the ones this owner set before.
	if err := s.applyProject(s.elevated(ctx), in.Project); err != nil { // the namespace is the fabric's to keep
		answer(w, err, 500)
		return
	}
	var workload []*unstructured.Unstructured // the app's objects, written together at the end

	// Vault credentials for the project, kept once in the Fabric; each app carries a copy in its own
	// Secret, because only the app's objects travel to the sites.
	var vault map[string]string
	var vaultFile []byte // the vault as read: the app carries its key only if Git still holds it at the commit
	if in.Database {
		p := secretPath(vaultSecret(in.Project))
		b, exists, err := s.git.Read(ctx, p)
		switch {
		case err != nil:
			answer(w, err, 502)
			return
		case exists:
			vaultFile = b
			if vault, err = s.openSecret(ctx, b, p); err != nil {
				answer(w, err, 500)
				return
			}
			if _, err := fabric.Version(vault, "key-version"); err != nil {
				answer(w, err, 409)
				return
			}
		case in.Vault.KeyID == "" || in.Vault.Key == "" || in.Vault.Bucket == "" || in.Vault.Endpoint == "":
			http.Error(w, "this project has no vault yet: create one on the Storage page, or give a bucket, endpoint and key once", 400)
			return
		case !bucketRe.MatchString(in.Vault.Bucket):
			http.Error(w, "the vault bucket is not a bucket name: 3 to 63 lowercase letters, digits, dots and dashes", 400)
			return
		default:
			if err := vaultEndpoint(ctx, in.Vault.Endpoint); err != nil && !s.dev {
				answer(w, err, 400)
				return
			}
			vault = map[string]string{"bucket": in.Vault.Bucket, "endpoint": in.Vault.Endpoint, "b2-key-id": in.Vault.KeyID, "b2-key": in.Vault.Key, "key-version": "0", "mutation-revision": "0"}
			if err := s.putSecret(ctx, "project "+in.Project+": its vault, bucket "+in.Vault.Bucket, vaultSecret(in.Project), vault, false); err != nil {
				answer(w, err, 502)
				return
			}
			through(s.localSecret(r, vaultSecret(in.Project), vault), "vault "+in.Project)
		}
		db := in.Name + "-db"
		vs := obj("barmancloud.cnpg.io/v1", "ObjectStore", db+"-vault", in.Project, map[string]any{
			"configuration": map[string]any{
				"destinationPath": fmt.Sprintf("s3://%s/%s/%s/", vault["bucket"], in.Project, in.Name),
				"endpointURL":     vault["endpoint"],
				"s3Credentials": map[string]any{
					"accessKeyId":     map[string]any{"name": in.Name, "key": "b2-key-id"},
					"secretAccessKey": map[string]any{"name": in.Name, "key": "b2-key"},
				},
				// A switchover needs .partial segments: the old primary archives its last one as .partial
				// when it demotes, and the new primary replays it to the token. A site that restores one
				// archives it again, whole (archive_mode=always), so a folder can hold a segment both ways;
				// barman-cloud-wal-restore then spools the second copy, by default in /var/tmp, which the
				// Barman Cloud plugin's read-only filesystem refuses. Its spool goes to the pod's scratch
				// volume instead.
				"wal": map[string]any{"compression": "gzip", "restoreAdditionalCommandArgs": []any{"--spool-dir", "/controller/walrestore"}},
			},
			"retentionPolicy": "30d",
		})
		metrics := obj("v1", "ConfigMap", db+"-recovery-metrics", in.Project, nil)
		metrics.SetLabels(map[string]string{"cnpg.io/reload": "true"})
		metrics.Object["data"] = map[string]any{"queries": warden.RecoveryQuery}
		cl := obj("postgresql.cnpg.io/v1", "Cluster", db, in.Project, map[string]any{
			"instances":         int64(1),
			"priorityClassName": "wecolab-protected",
			"storage":           map[string]any{"size": "20Gi"},
			"resources":         map[string]any{"requests": map[string]any{"cpu": "250m", "memory": "512Mi"}, "limits": map[string]any{"memory": "1Gi"}},
			"postgresql":        map[string]any{"parameters": map[string]any{"archive_timeout": "60s"}},
			"monitoring":        map[string]any{"customQueriesConfigMap": []any{map[string]any{"name": db + "-recovery-metrics", "key": "queries"}}, "metricsQueriesTTL": "0s"},
			"plugins":           []any{map[string]any{"name": "barman-cloud.cloudnative-pg.io", "isWALArchiver": true, "parameters": map[string]any{"barmanObjectName": db + "-vault", "serverName": "PLACEHOLDER"}}},
			"replica":           map[string]any{"primary": in.Primary, "self": "PLACEHOLDER", "source": "PLACEHOLDER"},
			"bootstrap":         map[string]any{},
			"externalClusters":  []any{},
		})
		// The file in the Fabric is the primary's own Cluster, valid on its own; every site patches in
		// its role (warden.AppKustomization).
		roles := app.DeepCopy()
		merge(roles, in)
		if roles.Spec.ArchiveID == "" {
			roles.Spec.ArchiveID = archiveCandidate
		}
		for _, ov := range warden.DBPatch(roles, in.Primary, warden.Local{}) {
			setPath(cl.Object, ov["path"].(string), ov["value"])
		}
		sb := obj("postgresql.cnpg.io/v1", "ScheduledBackup", db+"-backup", in.Project, map[string]any{
			"schedule": "0 0 2 * * *", "immediate": true, "backupOwnerReference": "self",
			"cluster": map[string]any{"name": db}, "method": "plugin",
			"pluginConfiguration": map[string]any{"name": "barman-cloud.cloudnative-pg.io"},
		})
		workload = append(workload, metrics, vs, cl, sb)
	}

	// A redeploy keeps the secrets the first deploy generated: data may be encrypted with them.
	secretFile := fabric.AppFolder(in.Project, in.Name) + "/secret-" + in.Name + ".sops.yaml" // as fabric.WorkloadFiles names it
	oldSecret, hadSecret, err := s.git.Read(ctx, secretFile)
	if err != nil {
		answer(w, err, 502)
		return
	}
	generated := map[string]string{} // secrets generated for the app, by catalog name
	if hadSecret {
		old, err := s.openSecret(ctx, oldSecret, secretFile)
		if err != nil {
			answer(w, err, 500)
			return
		}
		generated = reusable(old)
	}
	kept := maps.Clone(generated)
	if in.Database { // its password is generated once, and kept like the app's other generated values
		if generated["db-password"] == "" {
			generated["db-password"] = randHexStr(32)
		}
		workload = append(workload, dbOwner(in.Project, in.Name, generated["db-password"]))
	}

	// The workload. Pods run in user namespaces: root inside a container is an
	// unprivileged uid on the box, so catalog images that expect root work unchanged.
	db := ""
	if in.Database {
		db = in.Name + "-db"
	}
	secrets := map[string]string{} // what the app's own Secret holds
	var containers, volumes []any
	var claims []map[string]any
	if entry != nil {
		vars := map[string]string{"hostname": in.Hostname, "domain": s.domain, "TZ": "UTC", "admin_user": "admin", "admin_email": s.who(r).Email, "ip": in.Hostname}
		var err error
		containers, volumes, claims, err = catalogPod(*entry, in.Name, db, vars, generated, secrets)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
	} else {
		c := map[string]any{"name": in.Name, "image": in.Image, "ports": []any{map[string]any{"containerPort": int64(in.Port)}},
			"securityContext": map[string]any{"allowPrivilegeEscalation": false},
			"resources":       map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "128Mi"}, "limits": map[string]any{"memory": "512Mi"}}}
		if in.Database {
			c["env"] = []any{map[string]any{"name": "DATABASE_URL", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": db + "-app", "key": "uri", "optional": true}}}}
		}
		containers = []any{c}
	}
	maps.Copy(secrets, generated)
	if vault != nil {
		secrets["b2-key-id"], secrets["b2-key"], secrets["key-version"] = vault["b2-key-id"], vault["b2-key"], vault["key-version"]
	}
	spec := map[string]any{
		"priorityClassName": "wecolab-protected",
		"hostUsers":         false,
		"securityContext":   map[string]any{"seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"containers":        containers,
	}
	if entry != nil && entry.Runtime == "kata" { // its own kernel instead: Kata refuses user namespaces
		spec["runtimeClassName"] = kataRuntime
		delete(spec, "hostUsers")
	}
	if len(volumes) > 0 {
		spec["volumes"] = volumes
	}
	dep := obj("apps/v1", "Deployment", in.Name, in.Project, map[string]any{
		"replicas": int64(1),
		"strategy": map[string]any{"type": "Recreate"},
		"selector": map[string]any{"matchLabels": map[string]any{"app": in.Name}},
		"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": in.Name}}, "spec": spec},
	})
	svc := obj("v1", "Service", in.Name, in.Project, map[string]any{
		"selector": map[string]any{"app": in.Name},
		"ports":    []any{map[string]any{"port": int64(80), "targetPort": int64(in.Port)}},
	})
	sec := obj("v1", "Secret", in.Name, in.Project, nil) // the App's own secret travels with it
	sec.Object["type"] = "Opaque"
	if len(secrets) > 0 {
		sd := map[string]any{}
		for k, v := range secrets {
			sd[k] = v
		}
		sec.Object["stringData"] = sd
	}
	objs := []*unstructured.Unstructured{sec, svc}
	for _, c := range claims {
		pvc := obj("v1", "PersistentVolumeClaim", c["name"].(string), in.Project, map[string]any{
			"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "5Gi"}}})
		pvc.SetLabels(map[string]string{"wecolab.io/app": in.Name})
		objs = append(objs, pvc)
	}
	workload = append(workload, append(objs, dep)...)
	for _, u := range workload { // writer validates schema and security, but never remote PVC quota
		if u.GetKind() == "PersistentVolumeClaim" {
			continue
		}
		if err := s.dryRun(ctx, u); err != nil {
			answer(w, fmt.Errorf("%s %s: %w", u.GetKind(), u.GetName(), err), 400)
			return
		}
	}
	if err := admitClaims(in.Project, in.Sites, sites, offers, pools, workload); err != nil {
		answer(w, err, 400)
		return
	}
	recipients, err := s.recipients(ctx, sites, in.Sites, nil)
	if err != nil {
		answer(w, err, 502)
		return
	}
	files, err := fabric.WorkloadFiles(in.Project, in.Name, workload, recipients)
	if err != nil {
		answer(w, err, 500)
		return
	}
	// A redeploy replaces the folder: files the new workload does not have go.
	old, err := s.git.List(ctx, fabric.AppFolder(in.Project, in.Name))
	if err != nil {
		answer(w, err, 502)
		return
	}
	appPath, _ := s.pathOf(app)
	sitePaths, err := s.sitePaths(ctx) // read, so that the stewards the secrets were encrypted for are checked
	if err != nil {
		answer(w, err, 502)
		return
	}
	paths := append([]string{appPath, secretPath(vaultSecret(in.Project)), fabric.PlacementRevisionPath}, sitePaths...)
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	for _, p := range old {
		if !slices.Contains(paths, p) {
			files, paths = append(files, fabric.FileChange{Path: p}), append(paths, p)
		}
	}
	owner := in.Project + "/" + in.Name
	claimPaths := []string{}
	for _, host := range []string{oldHostname, in.Hostname} {
		if host != "" {
			claimPaths = append(claimPaths, fabric.PublicClaimPath(host))
		}
	}
	if oldMesh || in.Mesh {
		claimPaths = append(claimPaths, fabric.MeshClaimPath(meshName(in.Project, in.Name)))
	}
	paths = append(paths, claimPaths...)
	created := false
	err = s.edit(ctx, fmt.Sprintf("%s/%s: deployed %s", in.Project, in.Name, in.Image), paths, func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
		b, ok := snap.Get(appPath)
		exists, err := decode(b, ok, app)
		if err != nil {
			return nil, err
		}
		if exists && app.Spec.ArchiveID != preexistingArchive {
			return nil, fail(409, "app archive identity changed during deploy")
		}
		if exists && app.Spec.Database != preexistingDatabase {
			return nil, fabric.ErrConflict
		}
		if exists && (app.Spec.Hostname != oldHostname || app.Spec.Mesh != oldMesh) {
			return nil, fabric.ErrConflict
		}
		if !exists && preexistingArchive != "" {
			return nil, fabric.ErrConflict
		}
		if b, _ := snap.Get(fabric.PlacementRevisionPath); !bytes.Equal(b, placementBefore) {
			return nil, fabric.ErrConflict
		}
		if exists && app.Spec.Deleted {
			return nil, fail(409, "%s/%s is still being deleted at its sites; deploy it again once it is gone", in.Project, in.Name)
		}
		if exists && app.Spec.Primary != in.Primary {
			return nil, fail(409, "the primary is %s: move it on the Apps page", app.Spec.Primary)
		}
		if b, _ := snap.Get(secretFile); !bytes.Equal(b, oldSecret) {
			return nil, fabric.ErrConflict // deployed meanwhile: its secrets are not the ones reused here
		}
		if b, _ := snap.Get(secretPath(vaultSecret(in.Project))); vaultFile != nil && !bytes.Equal(b, vaultFile) {
			return nil, fabric.ErrConflict // the vault's key rotated meanwhile: the app would carry the old one
		}
		var vaultChange *fabric.FileChange
		if in.Database {
			vp := secretPath(vaultSecret(in.Project))
			vb, ok := snap.Get(vp)
			if !ok {
				return nil, fail(409, "vault missing")
			}
			nowVault, err := s.openSecret(ctx, vb, vp)
			if err != nil {
				return nil, err
			}
			if nowVault["retirement-phase"] != "" {
				return nil, fail(409, "vault retirement in progress")
			}
			if nowVault["key-version"] != secrets["key-version"] || nowVault["b2-key-id"] != secrets["b2-key-id"] {
				return nil, fabric.ErrConflict
			}
			if err := fabric.BumpVault(nowVault); err != nil {
				return nil, fail(409, "%v", err)
			}
			ageKey, err := s.siteKey(ctx)
			if err != nil {
				return nil, err
			}
			enc, err := warden.ResealSecret(vb, path.Base(vp), ageKey, func(v map[string]string) error { v["mutation-revision"] = nowVault["mutation-revision"]; return nil })
			if err != nil {
				return nil, err
			}
			vaultChange = &fabric.FileChange{Path: vp, Content: enc}
		}
		now, err := sitesOf(snap, paths)
		if err != nil {
			return nil, err
		}
		if r, err := s.recipients(ctx, now, in.Sites, nil); err != nil || !slices.Equal(r, recipients) {
			return nil, cmpErr(err, fabric.ErrConflict) // the stewards changed: the secrets are encrypted for others
		}
		created = !exists
		if in.Database && app.Spec.ArchiveID == "" {
			if app.Spec.Database != "" {
				return nil, fail(409, "database archive identity migration required")
			}
			app.Spec.ArchiveID = archiveCandidate
		}
		merge(app, in)
		if err := s.dryRun(ctx, app); err != nil {
			return nil, err
		}
		f, err := s.fileOf(app, false)
		if err != nil {
			return nil, err
		}
		out := append(slices.Clone(files), f)
		if oldHostname != "" && oldHostname != in.Hostname {
			release, err := fabric.ReleaseRoute(snap, fabric.PublicClaimPath(oldHostname), oldHostname, owner)
			if err != nil {
				return nil, fail(409, "%v", err)
			}
			out = append(out, release)
		}
		if in.Hostname != "" {
			acquire, err := fabric.ClaimRoute(snap, fabric.PublicClaimPath(in.Hostname), in.Hostname, owner)
			if err != nil {
				return nil, fail(409, "%v", err)
			}
			out = append(out, acquire)
		}
		mesh := meshName(in.Project, in.Name)
		if oldMesh && !in.Mesh {
			release, err := fabric.ReleaseRoute(snap, fabric.MeshClaimPath(mesh), mesh, owner)
			if err != nil {
				return nil, fail(409, "%v", err)
			}
			out = append(out, release)
		}
		if in.Mesh {
			acquire, err := fabric.ClaimRoute(snap, fabric.MeshClaimPath(mesh), mesh, owner)
			if err != nil {
				return nil, fail(409, "%v", err)
			}
			out = append(out, acquire)
		}
		if vaultChange != nil {
			out = append(out, *vaultChange)
		}
		return out, nil
	})
	if err != nil {
		answer(w, err, 502)
		return
	}
	if created { // for the page, before Flux brings it
		through(s.c.Create(ctx, app.DeepCopy()), "app "+in.Name)
	}
	out := map[string]any{"ok": true, "app": in.Name, "project": in.Project, "hostname": in.Hostname}
	if pw := generated["admin_password"]; pw != "" && kept["admin_password"] == "" {
		out["admin"] = map[string]string{"user": "admin", "password": pw} // shown once; it lives in the app's Secret
	}
	writeJSON(w, out)
}

// merge puts the Deploy form's choices on the App as Git holds it: a redeploy keeps its archive
// generations and any handover in flight, which only moves and rebuilds change.
func merge(app *v1alpha1.App, in deployRequest) {
	app.Spec.Sites, app.Spec.Primary, app.Spec.Hostname, app.Spec.Workload, app.Spec.Mesh = in.Sites, in.Primary, in.Hostname, in.Name, in.Mesh
	app.Spec.Database = ""
	if in.Database {
		app.Spec.Database = in.Name + "-db"
	}
	if d, err := parseDuration(in.RPO); err == nil {
		app.Spec.RPO = d
	}
}

// reusable are the generated values of an app's Secret: all but what a deploy derives again (env
// values that embed them, the vault's keys).
func reusable(old map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range old {
		if !strings.HasPrefix(k, "env-") && k != "b2-key-id" && k != "b2-key" && k != "key-version" {
			out[k] = v
		}
	}
	return out
}

// dbOwner is the Secret an app's database owner signs in with, the same at every site: the Cluster
// names it (warden.DBPatch), so CNPG makes none of its own. Its name and keys are those of the Secret
// CNPG would make, which apps read (catalogEnv.DB).
func dbOwner(ns, app, password string) *unstructured.Unstructured {
	host := app + "-db-rw." + ns
	s := obj("v1", "Secret", app+"-db-app", ns, nil)
	s.Object["type"] = "kubernetes.io/basic-auth"
	s.Object["stringData"] = map[string]any{"username": app, "user": app, "password": password, "dbname": app, "host": host, "port": "5432",
		"uri": fmt.Sprintf("postgresql://%s:%s@%s:5432/%s", app, password, host, app)}
	return s
}

// bucketRe is an S3 bucket name: it becomes part of the vault's paths.
var bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// meshName is an app's name on the people mesh, <app>-<project>.mesh.<zone>: one label, which two
// projects' apps of the same name do not share.
func meshName(ns, app string) string { return app + "-" + ns }

// meshFree says whether an app may take its mesh name: a label no other app on the mesh has.
func meshFree(apps []v1alpha1.App, ns, app string) error {
	n := meshName(ns, app)
	if validate.Label(n, 63) != nil {
		return fail(400, "%s is too long for a name on the mesh", n)
	}
	for _, a := range apps {
		if a.Spec.Mesh && meshName(a.Namespace, a.Name) == n && (a.Namespace != ns || a.Name != app) {
			return fail(409, "%s is on the mesh already, as app %s of project %s", n, a.Name, a.Namespace)
		}
	}
	return nil
}

// vaultSecret is where a project's vault keys live at stewards: the Console copies them into each
// database app's own Secret.
func vaultSecret(project string) string { return "vault-" + project }

// deleteApp removes an App and everything that was created for it.
func (s *server) deleteApp(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("app")
	if !s.can(w, r, ns) {
		return
	}
	if validate.Label(ns, 0) != nil || validate.Label(name, 0) != nil {
		http.Error(w, "no such app", 404)
		return
	}
	ctx := r.Context()
	a := &v1alpha1.App{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	appPath, _ := s.pathOf(a)
	// The App is marked deleted: each site's Warden removes what ran there, data included, and the writer
	// removes the App and its folder from the Fabric once every site is done. Only this mark deletes
	// data; an App missing from Git never does (docs/plans/2026-09-29-hardening.md, R4).
	err := s.edit(ctx, ns+"/"+name+": deleted", []string{appPath}, func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
		b, ok := snap.Get(appPath)
		app := &v1alpha1.App{}
		if exists, err := decode(b, ok, app); err != nil || !exists {
			return nil, cmp.Or(err, fail(404, "no such app"))
		}
		if err := s.c.Delete(ctx, a.DeepCopy(), client.DryRunAll); client.IgnoreNotFound(err) != nil {
			return nil, err
		}
		if app.Spec.Deleted {
			return nil, nil
		}
		app.Namespace, app.Name, app.Spec.Deleted = ns, name, true
		out, err := fabric.YAML(app, v1alpha1.GroupVersion.WithKind("App"))
		return []fabric.FileChange{{Path: appPath, Content: out}}, err
	})
	if err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// setPath sets a JSON-pointer path such as /spec/plugins/0/parameters/serverName in an object.
func setPath(obj map[string]any, path string, value any) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	var cur any = obj
	for i, p := range parts {
		last := i == len(parts)-1
		switch c := cur.(type) {
		case map[string]any:
			if last {
				c[p] = value
				return
			}
			if _, ok := c[p]; !ok {
				c[p] = map[string]any{}
			}
			cur = c[p]
		case []any:
			n, err := strconv.Atoi(p)
			if err != nil || n >= len(c) {
				return
			}
			if last {
				c[n] = value
				return
			}
			cur = c[n]
		}
	}
}

// admitClaims validates generated PVCs as typed objects and the destination
// grant. The destination's ResourceQuota/actual capacity remains authoritative.
func admitClaims(project string, targets []string, sites []v1alpha1.Site, offers []v1alpha1.Offer, pools []v1alpha1.Pool, workload []*unstructured.Unstructured) error {
	total := resource.MustParse("0")
	for _, u := range workload {
		if u.GetKind() != "PersistentVolumeClaim" {
			continue
		}
		var pvc corev1.PersistentVolumeClaim
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &pvc); err != nil {
			return fail(400, "invalid PVC %s: %v", u.GetName(), err)
		}
		if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce || pvc.Spec.StorageClassName != nil {
			return fail(400, "PVC %s requires supported default storage class and ReadWriteOnce", u.GetName())
		}
		q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		if !ok || q.Sign() <= 0 {
			return fail(400, "PVC %s must request storage", u.GetName())
		}
		total.Add(q)
	}
	if total.Sign() == 0 {
		return nil
	}
	for _, site := range targets {
		st := warden.StandingAt(project, site, sites, offers, pools)
		if st.Owner {
			continue
		}
		if st.Held == nil {
			return fail(403, "project %s has no storage grant at %s", project, site)
		}
		size := st.Held.Storage
		if size == "" {
			size = "0"
		}
		grant, err := resource.ParseQuantity(size)
		if err != nil {
			return fail(409, "invalid storage grant at %s: %v", site, err)
		}
		if grant.Cmp(total) < 0 {
			return fail(403, "%s grants %s storage, app requests %s", site, grant.String(), total.String())
		}
	}
	return nil
}
