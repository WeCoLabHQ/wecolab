package warden

import (
	"context"
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

// ConvergeVaultApps repairs all current database Apps from Git's current vault.
// Every update also changes the same vault file; an old, paused repair cannot
// overwrite a newer rotation or a concurrent retirement claim.
func ConvergeVaultApps(ctx context.Context, g *fabric.Git, ageKey, project string) error {
	vp := "secrets/vault-" + project + ".sops.yaml"
	names, err := g.List(ctx, "fabric/apps/"+project)
	if err != nil {
		return err
	}
	for _, ap := range names {
		if !strings.HasSuffix(ap, ".yaml") {
			continue
		}
		b, ok, err := g.Read(ctx, ap)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		var app v1alpha1.App
		if err := yaml.Unmarshal(b, &app); err != nil {
			return err
		}
		if app.Spec.Database == "" {
			continue
		}
		sp := fabric.AppFolder(project, app.Name) + "/secret-" + app.Name + ".sops.yaml"
		_, err = g.Edit(ctx, fabric.Author{Name: "Warden", Email: "warden@wecolab"}, "converge vault key for "+project+"/"+app.Name, []string{vp, ap, sp}, func(s *fabric.Snapshot) ([]fabric.FileChange, error) {
			ab, aok := s.Get(ap)
			if !aok {
				return nil, nil
			}
			var now v1alpha1.App
			if err := yaml.Unmarshal(ab, &now); err != nil {
				return nil, err
			}
			if now.Spec.Database == "" {
				return nil, nil
			}
			enc, ok := s.Get(sp)
			if !ok {
				return nil, fmt.Errorf("database app %s secret missing", ap)
			}
			vb, ok := s.Get(vp)
			if !ok {
				return nil, fmt.Errorf("project %s vault missing", project)
			}
			vault, err := openVaultFile(vb, path.Base(vp), ageKey)
			if err != nil {
				return nil, err
			}
			current, err := openVaultFile(enc, path.Base(sp), ageKey)
			if err != nil {
				return nil, err
			}
			if err := fabric.CheckKeyVersion(current, vault); err != nil {
				return nil, err
			}
			if current["key-version"] == vault["key-version"] && current["b2-key-id"] == vault["b2-key-id"] && current["b2-key"] == vault["b2-key"] {
				return nil, nil
			}
			if err := fabric.BumpVault(vault); err != nil {
				return nil, err
			}
			newVault, err := reseal(vb, path.Base(vp), ageKey, func(v map[string]string) error { v["mutation-revision"] = vault["mutation-revision"]; return nil })
			if err != nil {
				return nil, err
			}
			out, err := reseal(enc, path.Base(sp), ageKey, func(v map[string]string) error {
				v["b2-key-id"], v["b2-key"], v["key-version"] = vault["b2-key-id"], vault["b2-key"], vault["key-version"]
				return nil
			})
			if err != nil {
				return nil, err
			}
			return []fabric.FileChange{{Path: vp, Content: newVault}, {Path: sp, Content: out}}, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func openVaultFile(enc []byte, name, ageKey string) (map[string]string, error) {
	plain, err := fabric.Decrypt(enc, name, ageKey)
	if err != nil {
		return nil, err
	}
	var sec corev1.Secret
	if err := yaml.Unmarshal(plain, &sec); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, b := range sec.Data {
		out[k] = string(b)
	}
	for k, v := range sec.StringData {
		out[k] = v
	}
	return out, nil
}

// ResealSecret preserves all SOPS recipients while changing encrypted fields.
func ResealSecret(enc []byte, name, ageKey string, change func(map[string]string) error) ([]byte, error) {
	return reseal(enc, name, ageKey, change)
}
