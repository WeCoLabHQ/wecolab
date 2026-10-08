package main

import (
	"context"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/fabric"
)

func TestParallelRouteClaimOnlyOneOwner(t *testing.T) {
	s, copy := testServer(t, inGit(t))
	git := s.git
	p := fabric.PublicClaimPath("app.fab.example")
	owners := []string{"a/app", "b/app"}
	var wg sync.WaitGroup
	success := make(chan string, 2)
	for _, owner := range owners {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			appPath := "fabric/apps/" + owner + ".yaml"
			_, err := git.Edit(context.Background(), fabric.Author{}, "claim "+owner, []string{p, appPath}, func(s *fabric.Snapshot) ([]fabric.FileChange, error) {
				f, err := fabric.ClaimRoute(s, p, "app.fab.example", owner)
				if err != nil {
					return nil, err
				}
				return []fabric.FileChange{f, {Path: appPath, Content: []byte(owner)}}, nil
			})
			if err == nil {
				success <- owner
			}
		}(owner)
	}
	wg.Wait()
	close(success)
	if len(success) != 1 {
		t.Fatalf("expected exactly one app+claim commit, got %d", len(success))
	}
	winner := <-success
	for _, owner := range owners {
		exists := copy.get("fabric/apps/"+owner+".yaml") != ""
		if exists != (owner == winner) {
			t.Fatalf("app %s committed=%v, winner %s", owner, exists, winner)
		}
	}
}

func TestDestinationStorageAdmissionDoesNotUseWriterQuota(t *testing.T) {
	site := func(name, owner string) v1alpha1.Site {
		return v1alpha1.Site{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.SiteSpec{Owner: owner}}
	}
	sites := []v1alpha1.Site{site("writer", "other"), site("remote", "other")}
	offer := v1alpha1.Offer{ObjectMeta: metav1.ObjectMeta{Name: "capacity"}}
	offer.Spec.Site = "remote"
	offer.Spec.To = []string{"p"}
	offer.Spec.Storage = "6Gi"
	pvc := obj("v1", "PersistentVolumeClaim", "data", "p", map[string]any{"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "5Gi"}}})
	if err := admitClaims("p", []string{"remote"}, sites, []v1alpha1.Offer{offer}, nil, []*unstructured.Unstructured{pvc}); err != nil {
		t.Fatalf("remote grant denied by writer-local quota: %v", err)
	}
	if err := admitClaims("p", []string{"writer"}, sites, []v1alpha1.Offer{offer}, nil, []*unstructured.Unstructured{pvc}); err == nil {
		t.Fatal("ungranted writer site accepted")
	}
	offer.Spec.Storage = "4Gi"
	if err := admitClaims("p", []string{"remote"}, sites, []v1alpha1.Offer{offer}, nil, []*unstructured.Unstructured{pvc}); err == nil {
		t.Fatal("undersized remote grant accepted")
	}
	second := obj("v1", "PersistentVolumeClaim", "cache", "p", map[string]any{"accessModes": []any{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]any{"storage": "2Gi"}}})
	offer.Spec.Storage = "6Gi"
	if err := admitClaims("p", []string{"remote"}, sites, []v1alpha1.Offer{offer}, nil, []*unstructured.Unstructured{pvc, second}); err == nil {
		t.Fatal("multiple PVC requests were not summed")
	}
}
