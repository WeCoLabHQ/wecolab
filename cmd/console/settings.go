package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/warden"
)

// storageSecret holds the object storage account key (Settings): projects' vaults are made from it.
const storageSecret = warden.StorageSecret

// settingsPath is the fabric's settings in Git: the ConfigMap every site applies.
const settingsPath = warden.SettingsPath

// settingsOf is the settings file as read.
func settingsOf(snap *fabric.Snapshot) (*corev1.ConfigMap, *warden.Settings, error) {
	b, _ := snap.Get(settingsPath)
	return parseSettings(b)
}

func parseSettings(b []byte) (*corev1.ConfigMap, *warden.Settings, error) {
	cm := &corev1.ConfigMap{}
	if err := yaml.Unmarshal(b, cm); err != nil {
		return nil, nil, err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	set, err := warden.ParseSettings(cm.Data)
	return cm, set, err
}

// gitSettings are the settings as this site's copy of the Fabric holds them.
func (s *server) gitSettings(ctx context.Context) (*warden.Settings, error) {
	b, _, err := s.git.Read(ctx, settingsPath)
	if err != nil {
		return nil, err
	}
	_, set, err := parseSettings(b)
	return set, err
}

func settingsFile(cm *corev1.ConfigMap) (fabric.FileChange, error) {
	out, err := yaml.Marshal(cm)
	return fabric.FileChange{Path: settingsPath, Content: append([]byte("# The fabric's settings (docs/architecture.md). Changing the writer raises the epoch.\n"), out...)}, err
}

func (s *server) getSettings(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	ctx := r.Context()
	set, err := s.gitSettings(ctx)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	me := &v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: s.site}}
	if _, err := s.fromGit(ctx, me); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	st := map[string]any{"configured": false}
	sec := &corev1.Secret{}
	if err := s.c.Get(s.elevated(ctx), types.NamespacedName{Namespace: warden.SystemNS, Name: storageSecret}, sec); err == nil {
		st = map[string]any{"configured": true, "updated": string(sec.Data["updated"])}
	}
	writeJSON(w, map[string]any{"storage": st, "site": s.site, "writer": set.Writer, "epoch": set.Epoch, "zone": set.Zone,
		"isWriter": set.Writer == s.site, "isSteward": me.Spec.Steward})
}

// setStorage keeps the object storage account key in the Fabric, encrypted to the stewards and the
// recovery key, and here at once. It is never shown back.
func (s *server) setStorage(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	var in struct{ KeyID, Key string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.KeyID) == "" || strings.TrimSpace(in.Key) == "" {
		http.Error(w, "key id and key required", 400)
		return
	}
	ctx := r.Context()
	data := map[string]string{"key-id": strings.TrimSpace(in.KeyID), "key": strings.TrimSpace(in.Key), "updated": time.Now().UTC().Format(time.RFC3339)}
	if err := s.putSecret(ctx, "settings: the object storage account key", storageSecret, data, true); err != nil {
		answer(w, err, 502)
		return
	}
	through(s.localSecret(r, storageSecret, data), "storage key")
	writeJSON(w, map[string]any{"ok": true})
}

// putSecret commits a Secret of wecolab-system, encrypted to the stewards and the recovery key, and
// lists it in secrets/kustomization.yaml; replace false refuses one Git has already.
func (s *server) putSecret(ctx context.Context, msg, name string, data map[string]string, replace bool) error {
	return s.editSecret(ctx, msg, name, func(_ []byte, exists bool) (map[string]string, error) {
		if exists && !replace {
			return nil, fail(409, "%s exists already", name)
		}
		return data, nil
	})
}

// editSecret is putSecret with what the Secret holds decided by data from its file as Git holds it (and
// whether there is one), read again on every attempt of the edit.
func (s *server) editSecret(ctx context.Context, msg, name string, data func(enc []byte, exists bool) (map[string]string, error)) error {
	paths, err := s.sitePaths(ctx)
	if err != nil {
		return err
	}
	paths = append(paths, secretPath(name), kustomizationPath)
	return s.edit(ctx, msg, paths, func(snap *fabric.Snapshot) ([]fabric.FileChange, error) {
		v, err := data(snap.Get(secretPath(name)))
		if err != nil {
			return nil, err
		}
		sites, err := sitesOf(snap, paths)
		if err != nil {
			return nil, err
		}
		recips, err := s.recipients(ctx, sites, nil, nil)
		if err != nil {
			return nil, err
		}
		f, err := secretFile(name, v, recips)
		if err != nil {
			return nil, err
		}
		k, err := secretsKustomization(snap, path.Base(f.Path))
		return []fabric.FileChange{f, k}, err
	})
}

// localSecret writes a committed secret here too, so it is usable before Flux brings it.
func (s *server) localSecret(r *http.Request, name string, data map[string]string) error {
	sec := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: warden.SystemNS}, StringData: data}
	return s.c.Patch(s.elevated(r.Context()), sec, client.Apply, client.ForceOwnership, client.FieldOwner("wecolab-console"))
}

// takeover makes this steward the writer: its copy admits the Console, and the change of writer is
// committed here with an epoch above every one known, this copy's and every steward's claim (this
// copy may lag the old writer's). Its Warden then pushes to every other copy, and every other
// steward's Warden steps down when it sees the higher epoch (docs/operations.md, "The writer"). The
// copy is opened only for the commit: if it fails, it is closed again. Its Warden uses
// the same local lease when adjusting protection or following another writer.
func (s *server) takeover(w http.ResponseWriter, r *http.Request) {
	if !s.isAdmin(w, r) {
		return
	}
	who := author(r.Context())
	epoch, err := warden.TakeOver(s.elevated(r.Context()), s.git, s.site, s.peers, who, s.coordination)
	switch {
	case errors.Is(err, warden.ErrWriterAlready):
		http.Error(w, err.Error(), 409)
		return
	case errors.Is(err, warden.ErrNotSteward):
		http.Error(w, err.Error(), 403)
		return
	case err != nil:
		answer(w, err, 502)
		return
	}
	s.fetch(r.Context())
	writeJSON(w, map[string]any{"ok": true, "writer": s.site, "epoch": epoch})
}

//go:embed join.sh
var joinSh embed.FS

// joinScript is the script every box runs with its invite: the same one that installs the first box.
func (s *server) joinScript(w http.ResponseWriter, _ *http.Request) {
	b, _ := joinSh.ReadFile("join.sh")
	w.Header().Set("Content-Type", "text/x-shellscript")
	_, _ = w.Write(b)
}

// bootstrapFiles are the Fabric's files a joining site needs before its own copy exists: its Forgejo
// and Flux. Nothing secret.
var bootstrapFiles = []string{"system/wecolab/forgejo.yaml", "system/vendor/flux/manifest.yaml"}

func (s *server) fabricFile(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	if !slices.Contains(bootstrapFiles, p) {
		http.NotFound(w, r)
		return
	}
	b, ok, err := s.git.Read(r.Context(), p)
	if err != nil || !ok {
		http.Error(w, "not in the Fabric", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(b)
}

var distName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// download serves what a joining box needs before a registry holds WeCoLab's images: the image
// tarballs the first box built (decision 13).
func (s *server) download(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if !distName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.dist, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	http.ServeContent(w, r, name, st.ModTime(), f)
}
