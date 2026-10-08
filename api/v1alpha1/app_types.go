package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// AppSpec is what a project asks for: a workload (its folder in the Fabric) at some sites, one of
// them primary, with an optional PostgreSQL database replicated through the project's vault.
// +kubebuilder:validation:XValidation:rule="self.primary in self.sites",message="primary must be one of sites"
// +kubebuilder:validation:XValidation:rule="!has(self.handover) || self.handover.from in self.sites",message="handover.from must be one of sites"
type AppSpec struct {
	// Sites run the app: the primary, and standbys wherever it has a database.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:items:MaxLength=63
	Sites []string `json:"sites"`
	// Primary is the site that holds the writable database and receives traffic. Where it changes with
	// a Handover the move is planned (the old primary hands over a token); without one it is forced
	// (every other site's database is rebuilt from the vault under a new archive generation).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Primary string `json:"primary"`
	// Handover is a planned primary change in progress (docs/plans/2026-09-29-hardening.md, R1).
	Handover *Handover `json:"handover,omitempty"`
	// Deleted is a person's decision to delete the app: every site removes its part, data included, and
	// the writer then removes the App from the Fabric. An App that is merely missing from Git never
	// deletes data (R4); this is the only way a commit does.
	Deleted bool `json:"deleted,omitempty"`
	// RPO is the most data the owner accepts to lose; reported against the vault's newest WAL.
	RPO metav1.Duration `json:"rpo,omitempty"`
	// Hostname is the app's public name, served by the Door.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:MaxLength=253
	Hostname string `json:"hostname,omitempty"`
	// Mesh also serves the app at <name>-<project>.mesh.<zone>, to people on the NetBird mesh only.
	Mesh bool `json:"mesh,omitempty"`
	// Workload is the Deployment, and the same-named Service and Secret, in the app's folder.
	Workload string `json:"workload"`
	// Database is the CloudNativePG Cluster in the app's folder, if any.
	Database string `json:"database,omitempty"`
	// ArchiveID identifies this database incarnation. Legacy databases migrate to
	// their existing Database prefix; new databases use id-<128 random bits>.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="archiveID is immutable"
	ArchiveID string `json:"archiveID,omitempty"`
	// Force records the latest operator-asserted fencing decision, not physical proof.
	Force               *ForceRecord         `json:"force,omitempty"`
	RestoreVerification *RestoreVerification `json:"restoreVerification,omitempty"`
	// Archive is each site's archive generation: its database archives to <db>-<site>, or
	// <db>-<site>-g<n> from generation 2. A new generation rebuilds that site's database from the
	// vault; Object Lock never empties the old one.
	Archive map[string]int `json:"archive,omitempty"`
}

type ForceRecord struct {
	ID    string      `json:"id"`
	Actor string      `json:"actor"`
	From  string      `json:"from"`
	To    string      `json:"to"`
	At    metav1.Time `json:"at"`
	// +kubebuilder:validation:Enum=power-off;write-path-isolated
	Method string `json:"method"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Evidence string `json:"evidence"`
}

// RestoreVerification is operator-recorded evidence of an actual data readback.
// It is never inferred from a completed backup or recipe certification.
type RestoreVerification struct {
	ArchiveID   string      `json:"archiveID"`
	SystemID    string      `json:"systemID"`
	BackupID    string      `json:"backupID"`
	Timeline    uint32      `json:"timeline"`
	CompletedAt metav1.Time `json:"completedAt"`
	Actor       string      `json:"actor"`
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	EvidenceSHA256 string `json:"evidenceSHA256"`
	// +kubebuilder:validation:Enum=database;database-and-files
	Scope string `json:"scope"`
}

// Handover is a planned primary change: the old primary (From) demotes, the writer copies its demotion
// token into Token, and the primary promotes with it. ID tells one handover's token from an earlier one's.
type Handover struct {
	ID   string `json:"id"`
	From string `json:"from"`
	// Token is the demotion token From's database minted for this handover; empty until then.
	Token string `json:"token,omitempty"`
}

// AppStatus is a site's own view, kept by that site's Warden before it acts.
type AppStatus struct {
	// Active is the site this site has applied as primary.
	Active string `json:"active,omitempty"`
	// Demoting is kept at a planned move's old primary from the moment it first sees the handover.
	Demoting   Demoting           `json:"demoting,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Demoting is the handover a site demotes for, and the demotion token its database showed when the site
// first saw it, if it was then writable: a leftover from before its last promotion, never to be handed
// over as this one's. It must be in the App CRD: the API server drops a status field its schema lacks.
type Demoting struct {
	Handover string `json:"handover,omitempty"`
	Stale    string `json:"stale,omitempty"`
}

// Condition types. Ready is true only when every other one is.
const (
	CondPrimaryHealthy = "PrimaryHealthy"
	CondStandbyStaged  = "StandbyStaged"
	CondWithinRPO      = "WithinRPO"
	CondVaultFresh     = "VaultFresh"
	CondRouted         = "Routed"
	CondPromotion      = "Promotion"
	CondReady          = "Ready"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Primary",type=string,JSONPath=`.spec.primary`
// +kubebuilder:printcolumn:name="Active",type=string,JSONPath=`.status.active`
// +kubebuilder:printcolumn:name="Sites",type=string,JSONPath=`.spec.sites`

// App is an application placed at one or more sites.
type App struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AppSpec   `json:"spec,omitempty"`
	Status            AppStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type AppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []App `json:"items"`
}

// Standby is the first site that is not the given primary, or "" if none.
func (s AppSpec) Standby(primary string) string {
	for _, site := range s.Sites {
		if site != primary {
			return site
		}
	}
	return ""
}
