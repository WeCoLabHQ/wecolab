package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// DomainSpec is a domain a project brings: its wildcard points at the Door and one TXT record binds
// it to the project. Hostnames under a verified domain may be used by that project's apps.
type DomainSpec struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:MaxLength=253
	Name    string `json:"name"`
	Project string `json:"project"`
}

// DomainStatus is kept by the writer's Warden, which looks the records up.
type DomainStatus struct {
	Verified    bool               `json:"verified,omitempty"`
	Reason      string             `json:"reason,omitempty"`
	LastChecked *metav1.Time       `json:"lastChecked,omitempty"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.project`
// +kubebuilder:printcolumn:name="Verified",type=boolean,JSONPath=`.status.verified`

// Domain is a project's own domain.
type Domain struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DomainSpec   `json:"spec,omitempty"`
	Status            DomainStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type DomainList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Domain `json:"items"`
}
