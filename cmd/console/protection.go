package main

import (
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

// fileScope reads desired manifests, not currently running PVCs: a pending or
// unavailable local file volume must not look like an app with no files.
func (s *server) fileScope(ctx context.Context, a *v1alpha1.App) (known, persistent bool) {
	paths, err := s.git.List(ctx, fabric.AppFolder(a.Namespace, a.Name))
	if err != nil || len(paths) == 0 {
		return false, false
	}
	workload := false
	for _, p := range paths {
		if strings.HasSuffix(p, ".sops.yaml") || !strings.HasSuffix(p, ".yaml") {
			continue
		}
		b, ok, err := s.git.Read(ctx, p)
		if err != nil || !ok {
			return false, false
		}
		var u unstructured.Unstructured
		if err := yaml.Unmarshal(b, &u.Object); err != nil {
			return false, false
		}
		if u.GetKind() == "PersistentVolumeClaim" {
			persistent = true
		}
		switch u.GetKind() {
		case "Deployment", "StatefulSet", "DaemonSet":
			workload = true
			volumes, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "volumes")
			for _, v := range volumes {
				if m, ok := v.(map[string]any); ok && m["persistentVolumeClaim"] != nil {
					persistent = true
				}
			}
			claims, _, _ := unstructured.NestedSlice(u.Object, "spec", "volumeClaimTemplates")
			if len(claims) > 0 {
				persistent = true
			}
		}
	}
	return workload, persistent
}
