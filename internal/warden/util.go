// Package warden is WeCoLab's controller: the same binary at every site, deciding and wiring what
// the Fabric says for that site (docs/architecture.md, "Warden").
package warden

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// SystemNS holds Warden, the Console, Forgejo and the fabric's settings at every site.
	SystemNS = "wecolab-system"
	// FluxNS is where Flux runs and every Kustomization lives.
	FluxNS     = "flux-system"
	fieldOwner = "wecolab-warden"
	// TenantLabel marks a project's namespace.
	TenantLabel = "wecolab.io/tenant"
)

var (
	gvkKustomization = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}
	gvkDBCluster     = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
	gvkCNPGCluster   = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "ClusterList"}
	gvkObjectStore   = schema.GroupVersionKind{Group: "barmancloud.cnpg.io", Version: "v1", Kind: "ObjectStore"}
)

func newObj(apiVersion, kind, name, ns string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetName(name)
	if ns != "" {
		u.SetNamespace(ns)
	}
	if spec != nil {
		u.Object["spec"] = spec
	}
	return u
}

// TenantLabels mark a project namespace: Pod Security baseline, because every site's admission
// policy makes user namespaces mandatory there, and catalog images expect to be root.
func TenantLabels(project string) map[string]string {
	return map[string]string{TenantLabel: project, "pod-security.kubernetes.io/enforce": "baseline", "pod-security.kubernetes.io/warn": "baseline"}
}
