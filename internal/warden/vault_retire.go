package warden

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

type retirementPhase struct {
	Version string   `json:"version"`
	Keys    []string `json:"keys"`
}

func (w *Writer) vaultApps(ctx context.Context, project, ageKey string, key map[string]string) ([]v1alpha1.App, error) {
	names, err := w.Git.List(ctx, "fabric/apps/"+project)
	if err != nil {
		return nil, err
	}
	apps := []v1alpha1.App{}
	for _, p := range names {
		if !strings.HasSuffix(p, ".yaml") {
			continue
		}
		b, ok, err := w.Git.Read(ctx, p)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("app %s disappeared during retirement", p)
		}
		var a v1alpha1.App
		if err := yaml.Unmarshal(b, &a); err != nil {
			return nil, err
		}
		if a.Spec.Database == "" {
			continue
		}
		sp := fabric.AppFolder(project, a.Name) + "/secret-" + a.Name + ".sops.yaml"
		enc, ok, err := w.Git.Read(ctx, sp)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("database app secret missing: %s", sp)
		}
		data, err := openVaultFile(enc, path.Base(sp), ageKey)
		if err != nil {
			return nil, err
		}
		if err := fabric.CheckKeyVersion(data, key); err != nil {
			return nil, err
		}
		if data["key-version"] != key["key-version"] || data["b2-key-id"] != key["b2-key-id"] || data["b2-key"] != key["b2-key"] {
			return nil, fmt.Errorf("app %s has not converged to vault key version", p)
		}
		apps = append(apps, a)
	}
	return apps, nil
}

func (w *Writer) retireEvidence(ctx context.Context, project, ageKey string, v map[string]string, acquiring bool) error {
	apps, err := w.vaultApps(ctx, project, ageKey, v)
	if err != nil {
		return err
	}
	if acquiring {
		for _, a := range apps {
			if a.Spec.Deleted {
				return fmt.Errorf("database app %s/%s is pending deletion", project, a.Name)
			}
		}
	}
	// fetch bypasses Peers' short-lived cache: after a persisted claim each
	// provider call needs a new actual status response from every target site.
	reports := map[string]*SiteStatus{}
	for _, a := range apps {
		for _, site := range a.Spec.Sites {
			if _, seen := reports[site]; !seen {
				st, err := w.Peers.fetch(ctx, site)
				if err != nil {
					return err
				}
				reports[site] = st
			}
		}
	}
	if waiting := Retirable(v["b2-key-id"], v["key-version"], apps, func(site string) *SiteStatus { return reports[site] }); len(waiting) != 0 {
		return fmt.Errorf("waiting for current key and version at %s", strings.Join(waiting, ","))
	}
	return nil
}

func (w *Writer) vaultUpdate(ctx context.Context, project, ageKey string, fn func(map[string]string) (bool, error)) error {
	p := "secrets/vault-" + project + ".sops.yaml"
	_, err := w.Git.Edit(ctx, fabric.Author{Name: "Warden", Email: "warden@" + w.Site}, "vault "+project+" retirement checkpoint", []string{p}, func(s *fabric.Snapshot) ([]fabric.FileChange, error) {
		enc, ok := s.Get(p)
		if !ok {
			return nil, fmt.Errorf("vault %s missing", project)
		}
		changed := false
		out, err := reseal(enc, path.Base(p), ageKey, func(v map[string]string) error { var e error; changed, e = fn(v); return e })
		if err != nil {
			return nil, err
		}
		if !changed {
			return nil, nil
		}
		return []fabric.FileChange{{Path: p, Content: out}}, nil
	})
	return err
}

// retire has no external side effects inside a retried Git callback. The
// persisted phase excludes rotations/deployments, survives process crashes, and
// each provider deletion is followed by a separate conditional Git checkpoint.
func (w *Writer) retire(ctx context.Context) error {
	paths, err := w.Git.List(ctx, "secrets")
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	ageKey, err := siteAgeKey(ctx, w.Client, w.Site)
	if err != nil {
		return err
	}
	errs := []error{}
	for _, p := range paths {
		name := path.Base(p)
		project, ok := strings.CutPrefix(name, "vault-")
		if !ok || !strings.HasSuffix(project, ".sops.yaml") {
			continue
		}
		project = strings.TrimSuffix(project, ".sops.yaml")
		if err := w.retireVault(ctx, project, ageKey); err != nil {
			errs = append(errs, fmt.Errorf("vault %s: %w", project, err))
		}
	}
	return errors.Join(errs...)
}

func (w *Writer) retireVault(ctx context.Context, project, ageKey string) error {
	if err := ConvergeVaultApps(ctx, w.Git, ageKey, project); err != nil {
		return err
	}
	p := "secrets/vault-" + project + ".sops.yaml"
	load := func() (map[string]string, error) {
		b, ok, err := w.Git.Read(ctx, p)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("vault missing")
		}
		return openVaultFile(b, path.Base(p), ageKey)
	}
	v, err := load()
	if err != nil {
		return err
	}
	if _, err := fabric.Version(v, "key-version"); err != nil {
		return err
	}
	if len(strings.Fields(v["retiring"])) == 0 && v["retirement-phase"] == "" {
		return nil
	}
	if v["b2-key-id"] == "" || v["b2-key"] == "" {
		return fmt.Errorf("vault has no usable current key")
	}
	var phase retirementPhase
	if v["retirement-phase"] == "" {
		if err := w.retireEvidence(ctx, project, ageKey, v, true); err != nil {
			w.note(ctx, project, "old keys wait for current Git apps and fresh site reports", "reason", err)
			return nil
		}
		keys := strings.Fields(v["retiring"])
		slices.Sort(keys)
		keys = slices.Compact(keys)
		for _, id := range keys {
			if id == v["b2-key-id"] {
				return fmt.Errorf("retiring list contains the current key")
			}
		}
		phase = retirementPhase{Version: v["key-version"], Keys: keys}
		raw, _ := json.Marshal(phase)
		if err := w.vaultUpdate(ctx, project, ageKey, func(cur map[string]string) (bool, error) {
			if cur["retirement-phase"] != "" || cur["key-version"] != v["key-version"] || cur["b2-key-id"] != v["b2-key-id"] || cur["mutation-revision"] != v["mutation-revision"] || cur["retiring"] != v["retiring"] {
				return false, fabric.ErrConflict
			}
			cur["retirement-phase"] = string(raw)
			return true, nil
		}); err != nil {
			return err
		}
	} else if err := json.Unmarshal([]byte(v["retirement-phase"]), &phase); err != nil {
		return err
	}
	acct := &corev1.Secret{}
	if err := w.Client.Get(ctx, types.NamespacedName{Namespace: SystemNS, Name: StorageSecret}, acct); err != nil {
		return err
	}
	for _, id := range slices.Clone(phase.Keys) {
		v, err = load()
		if err != nil {
			return err
		}
		var live retirementPhase
		if err := json.Unmarshal([]byte(v["retirement-phase"]), &live); err != nil {
			return err
		}
		if live.Version != phase.Version || !slices.Equal(live.Keys, phase.Keys) || v["key-version"] != phase.Version || id == v["b2-key-id"] {
			return fmt.Errorf("retirement phase changed; refusing provider deletion")
		}
		if err := w.retireEvidence(ctx, project, ageKey, v, false); err != nil {
			return err
		}
		if err := DeleteVaultKey(ctx, string(acct.Data["key-id"]), string(acct.Data["key"]), v["bucket"], id); err != nil {
			return err
		}
		if err := w.vaultUpdate(ctx, project, ageKey, func(cur map[string]string) (bool, error) {
			if cur["retirement-phase"] != v["retirement-phase"] || cur["b2-key-id"] != v["b2-key-id"] {
				return false, fabric.ErrConflict
			}
			left := slices.DeleteFunc(strings.Fields(cur["retiring"]), func(k string) bool { return k == id })
			if len(left) == 0 {
				delete(cur, "retiring")
			} else {
				cur["retiring"] = strings.Join(left, " ")
			}
			remaining := slices.DeleteFunc(slices.Clone(phase.Keys), func(k string) bool { return k == id })
			if len(remaining) == 0 {
				delete(cur, "retirement-phase")
			} else {
				raw, _ := json.Marshal(retirementPhase{Version: phase.Version, Keys: remaining})
				cur["retirement-phase"] = string(raw)
			}
			return true, nil
		}); err != nil {
			return err
		}
		phase.Keys = slices.DeleteFunc(phase.Keys, func(k string) bool { return k == id })
		log.FromContext(ctx).Info("vault old key confirmed deleted", "project", project, "key", id)
	}
	return nil
}
