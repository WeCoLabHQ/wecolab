package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// MemberSpec is a person: what they may do. Who they are is proven by the sign-in provider (NetBird's
// by default). Roles: owner, admin, member. Members belong to projects; a site is managed by the
// members of the project that owns it.
type MemberSpec struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	// +kubebuilder:validation:Enum=owner;admin;member
	Role     string   `json:"role"`
	Projects []string `json:"projects,omitempty"`
	// SSHKeys are installed on every box of the sites their projects own.
	SSHKeys []string `json:"sshKeys,omitempty"`
	Blocked bool     `json:"blocked,omitempty"`
}

// MemberStatus is kept by the writer's Warden from the people mesh.
type MemberStatus struct {
	// Phase: Invited, Active or Blocked.
	Phase         string             `json:"phase,omitempty"`
	Invite        string             `json:"invite,omitempty"`
	InviteExpires *metav1.Time       `json:"inviteExpires,omitempty"`
	Mesh          string             `json:"mesh,omitempty"`
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Email",type=string,JSONPath=`.spec.email`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`

// Member is a person of the fabric.
type Member struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MemberSpec   `json:"spec,omitempty"`
	Status            MemberStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type MemberList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Member `json:"items"`
}

// Admin is whether the member sees and may change everything.
func (m *Member) Admin() bool { return m.Spec.Role == "owner" || m.Spec.Role == "admin" }
