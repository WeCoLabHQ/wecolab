package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// OfferSpec is capacity a site's manager shares: a size per holder at a site, and an audience. A
// project may place work at a site its project owns, or at a site offering to it.
type OfferSpec struct {
	Site string `json:"site"`
	// Boxes are node names at the site; empty means any box.
	Boxes   []string `json:"boxes,omitempty"`
	CPU     string   `json:"cpu"`
	Memory  string   `json:"memory"`
	Storage string   `json:"storage,omitempty"`
	// To are the projects holding the offer.
	To []string `json:"to,omitempty"`
	// Pools the offer goes into: every project drawing from a pool holds it.
	Pools []string `json:"pools,omitempty"`
	// BestEffort marks capacity that comes and goes (laptops, idle time): holders' pods run at
	// best-effort priority and tolerate the laptop and idle taints. Databases never land there.
	BestEffort bool `json:"bestEffort,omitempty"`
	// Keys are unredeemed single-use codes, as hashes; redeeming one adds the project to To.
	Keys []OfferKey `json:"keys,omitempty"`
}

// OfferKey is the hash of a code the Console minted.
type OfferKey struct {
	Hash    string      `json:"hash"`
	Expires metav1.Time `json:"expires"`
}

type OfferStatus struct {
	Holders    int                `json:"holders,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Site",type=string,JSONPath=`.spec.site`

// Offer is capacity plus an audience.
type Offer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              OfferSpec   `json:"spec,omitempty"`
	Status            OfferStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type OfferList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Offer `json:"items"`
}

// PoolSpec is a tier of shared capacity: offers go in, projects draw.
type PoolSpec struct {
	Projects []string   `json:"projects,omitempty"`
	Quota    PoolQuota  `json:"quota,omitempty"`
	Keys     []OfferKey `json:"keys,omitempty"`
}

// PoolQuota is what each project drawing from the pool gets at each site offering into it.
type PoolQuota struct {
	CPU     string `json:"cpu,omitempty"`
	Memory  string `json:"memory,omitempty"`
	Storage string `json:"storage,omitempty"`
}

type PoolStatus struct {
	Sites      []string           `json:"sites,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status

// Pool is shared capacity many projects draw from.
type Pool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PoolSpec   `json:"spec,omitempty"`
	Status            PoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type PoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Pool `json:"items"`
}
