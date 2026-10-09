package main

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

type fileScopeResult struct {
	known, persistent bool
}

// scopeSnapshot caches only complete, successfully verified immutable archives.
// The mutex also serializes first reads for the same head.
func (s *server) scopeSnapshot(ctx context.Context, revision string) map[string]fileScopeResult {
	s.scopeMu.Lock()
	defer s.scopeMu.Unlock()
	if s.scopeHead == revision && s.scopes != nil {
		return s.scopes
	}
	s.scopeHead = revision
	s.scopes = nil
	scopes := make(map[string]fileScopeResult)
	seenManifests := make(map[string]bool)
	const maxManifest = 2 << 20
	err := s.git.VisitArchive(ctx, revision, func(p string, typ byte, content io.Reader) error {
		if typ == tar.TypeDir {
			return nil
		}
		dir := path.Dir(p)
		if !strings.HasPrefix(dir, "projects/") || strings.Count(dir, "/") != 2 {
			return nil
		}
		if strings.HasSuffix(p, ".sops.yaml") || !strings.HasSuffix(p, ".yaml") {
			return nil
		}
		if seenManifests[p] {
			return fmt.Errorf("duplicate desired manifest %s", p)
		}
		seenManifests[p] = true
		result := scopes[dir]
		if typ != tar.TypeReg && typ != tar.TypeRegA {
			return fmt.Errorf("unsupported desired manifest %s", p)
		}
		b, err := io.ReadAll(io.LimitReader(content, maxManifest+1))
		if err != nil {
			return fmt.Errorf("read desired manifest %s: %w", p, err)
		}
		if len(b) > maxManifest {
			return fmt.Errorf("desired manifest %s exceeds size limit", p)
		}
		var u unstructured.Unstructured
		if err := yaml.Unmarshal(b, &u.Object); err != nil {
			return fmt.Errorf("parse desired manifest %s: %w", p, err)
		}
		if u.GetKind() == "PersistentVolumeClaim" {
			result.persistent = true
		}
		switch u.GetKind() {
		case "Deployment", "StatefulSet", "DaemonSet":
			result.known = true
			volumes, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "volumes")
			for _, v := range volumes {
				if m, ok := v.(map[string]any); ok && m["persistentVolumeClaim"] != nil {
					result.persistent = true
				}
			}
			claims, _, _ := unstructured.NestedSlice(u.Object, "spec", "volumeClaimTemplates")
			if len(claims) > 0 {
				result.persistent = true
			}
		}
		scopes[dir] = result
		return nil
	})
	if err != nil {
		return nil // never publish a partially inspected archive
	}
	s.scopes = scopes
	return scopes
}
