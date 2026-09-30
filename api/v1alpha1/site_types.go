package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// SiteSpec is one site: one k3s cluster and the boxes it is made of. Written by the Console when the
// site and its boxes join; every box's Nebula identity is here, so any steward can renew it.
type SiteSpec struct {
	// Owner is the project that owns the site: its members manage it.
	Owner string `json:"owner"`
	// Index places the site's boxes in the Nebula network: 10.77.<index>.0/24 by default.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=254
	Index int `json:"index"`
	// Steward sites hold the fabric: its secrets, the Nebula CA, and the right to become the writer.
	Steward bool `json:"steward,omitempty"`
	// Public is set when the site's manager has a public address: it then runs the Door, Names and a
	// Nebula lighthouse and relay.
	Public *Public `json:"public,omitempty"`
	// Boxes are the site's machines; the first is its manager.
	Boxes []Box `json:"boxes,omitempty"`
	// NextBox is the host number the next box gets in the site's network. Numbers are never reused, so a
	// removed box's address never belongs to anyone else while its certificate may still be valid.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=255
	NextBox int `json:"nextBox,omitempty"`
}

// Public is how the internet reaches a public site.
type Public struct {
	// Address is the manager's public IPv4 address.
	// +kubebuilder:validation:Format=ipv4
	Address string `json:"address"`
}

// Box is one machine of a site, as Nebula knows it.
type Box struct {
	// Name is the box's Nebula name and Kubernetes node name: <site>-<host>, unique across the fabric.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// IP is the box's Nebula address.
	// +kubebuilder:validation:Format=ipv4
	IP string `json:"ip"`
	// Role is manager (the site's k3s server) or node.
	// +kubebuilder:validation:Enum=manager;node
	Role string `json:"role"`
	// Laptop marks capacity that yields to its person: best-effort work only.
	Laptop bool `json:"laptop,omitempty"`
	// Key is the box's Nebula public key, PEM. Renewals only ever re-sign this key.
	Key string `json:"key"`
	// Added is when the box joined.
	Added metav1.Time `json:"added"`
	// Certs are the certificates the Console signed for the box when it joined, newest last, so removing
	// the box blocks them wherever the stewards' own records cannot be reached.
	// +kubebuilder:validation:MaxItems=8
	Certs []IssuedCert `json:"certs,omitempty"`
}

// IssuedCert is one certificate issued for a box.
type IssuedCert struct {
	Fingerprint string      `json:"fingerprint"`
	NotAfter    metav1.Time `json:"notAfter"`
}

type SiteStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Steward",type=boolean,JSONPath=`.spec.steward`
// +kubebuilder:printcolumn:name="Public",type=string,JSONPath=`.spec.public.address`

// Site is one site of the fabric.
type Site struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SiteSpec   `json:"spec,omitempty"`
	Status            SiteStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type SiteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Site `json:"items"`
}

// Manager is the site's manager box, or nil before it has joined.
func (s *Site) Manager() *Box {
	for i := range s.Spec.Boxes {
		if s.Spec.Boxes[i].Role == "manager" {
			return &s.Spec.Boxes[i]
		}
	}
	return nil
}
