// Package v1alpha1 is the wecolab.io API: the objects the Fabric holds.
// +kubebuilder:object:generate=true
// +groupName=wecolab.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "wecolab.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&App{}, &AppList{}, &Site{}, &SiteList{}, &Member{}, &MemberList{},
		&Offer{}, &OfferList{}, &Pool{}, &PoolList{}, &Domain{}, &DomainList{})
}
