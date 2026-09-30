package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/validate"
)

// Domains a project brings. The member creates two records at their DNS provider,
// a wildcard to the Door and a TXT naming the project, and Warden verifies them.
// The fabric's own domain is shared: <app>.<domain> works for every project.

// doorIPs are the public addresses of the Doors: what the fabric's own names resolve to.
func (s *server) doorIPs() []string {
	ips, _ := net.LookupHost("console." + s.domain)
	sort.Strings(ips)
	return ips
}

// doorNames are names under the fabric's zone the Door uses, besides the fabric's reserved names.
var doorNames = []string{"door", "hello"}

func under(host, domain string) bool { return host == domain || strings.HasSuffix(host, "."+domain) }

// verified are the claims in Git that Warden verified. Verification is status, which only the copy
// here has; it counts only for the same name and project as the claim in Git.
func (s *server) verified(ctx context.Context, claims []v1alpha1.Domain) []v1alpha1.Domain {
	dl := &v1alpha1.DomainList{}
	_ = s.c.List(s.elevated(ctx), dl)
	out := []v1alpha1.Domain{}
	for _, c := range claims {
		if slices.ContainsFunc(dl.Items, func(d v1alpha1.Domain) bool { return d.Name == c.Name && d.Spec == c.Spec && d.Status.Verified }) {
			out = append(out, c)
		}
	}
	return out
}

// hostnameAllowed says whether an app may use a hostname: a host name under the fabric's domain (one
// label, not reserved) or under one of the project's verified domains, and no other app has it.
// apps are the apps in Git.
func (s *server) hostnameAllowed(ctx context.Context, project, app, hostname string, apps []v1alpha1.App) error {
	if err := validate.DNSName(hostname); err != nil {
		return fail(400, "hostname: %v", err)
	}
	if hostname == s.domain { // the Door never routes it, whichever verified domain it is under
		return fail(400, "%s is the fabric's own name", hostname)
	}
	for _, a := range apps {
		if a.Spec.Hostname == hostname && (a.Namespace != project || a.Name != app) {
			return fail(409, "%s is used by another app", hostname)
		}
	}
	if label, ok := strings.CutSuffix(hostname, "."+s.domain); ok {
		if strings.Contains(label, ".") || validate.Reserved(label) || slices.Contains(doorNames, label) {
			return fail(400, "under %s a hostname is one free label", s.domain)
		}
		return nil
	}
	claims, err := readDir[v1alpha1.Domain](ctx, s.git, "fabric/domains")
	if err != nil {
		return err
	}
	for _, d := range s.verified(ctx, claims) {
		if d.Spec.Project == project && under(hostname, d.Spec.Name) {
			return nil
		}
	}
	return fail(400, "%s is not under %s or a verified domain of project %s", hostname, s.domain, project)
}

func (s *server) domains(w http.ResponseWriter, r *http.Request) {
	id := s.who(r)
	dl := &v1alpha1.DomainList{}
	if err := s.c.List(r.Context(), dl); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	doors := s.doorIPs()
	type dom struct {
		Name, Domain, Project, Phase, Reason, LastChecked string
		Verified                                          bool
		Records                                           []string
	}
	out := []dom{}
	for _, d := range dl.Items {
		if !id.Admin && !slices.Contains(id.Projects, d.Spec.Project) {
			continue
		}
		phase := "Pending"
		if d.Status.Verified {
			phase = "Verified"
		}
		x := dom{Name: d.Name, Domain: d.Spec.Name, Project: d.Spec.Project, Phase: phase, Reason: d.Status.Reason, Verified: d.Status.Verified,
			Records: []string{"*." + d.Spec.Name + "  CNAME  door." + s.domain + "   (or A " + strings.Join(doors, " / ") + ")", "_wecolab." + d.Spec.Name + "  TXT  \"wecolab=project:" + d.Spec.Project + "\""}}
		if d.Status.LastChecked != nil {
			x.LastChecked = d.Status.LastChecked.UTC().Format("2006-01-02 15:04:05Z")
		}
		out = append(out, x)
	}
	writeJSON(w, map[string]any{"fabric": s.domain, "doors": doors, "domains": out})
}

// addDomain records a project's claim to a domain. Claims by several projects may wait side by side
// (a squatter's unverified claim never blocks the real owner); the one whose TXT record names it is
// verified, and once one is, no other project may claim that domain or a name under it.
func (s *server) addDomain(w http.ResponseWriter, r *http.Request) {
	var in struct{ Domain, Project string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	in.Domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.Domain), "."))
	in.Project = strings.TrimSpace(in.Project)
	if !s.can(w, r, in.Project) {
		return
	}
	switch {
	case validate.DNSName(in.Domain) != nil || !strings.Contains(in.Domain, "."):
		http.Error(w, "a domain such as apps.example.com", 400)
		return
	case under(in.Domain, s.domain):
		http.Error(w, "names under "+s.domain+" need no domain: use <app>."+s.domain, 400)
		return
	}
	ctx := r.Context()
	claims, err := readDir[v1alpha1.Domain](ctx, s.git, "fabric/domains")
	if err != nil {
		answer(w, err, 502)
		return
	}
	for _, d := range claims {
		if d.Spec.Name == in.Domain && d.Spec.Project == in.Project {
			http.Error(w, in.Domain+" is already claimed by project "+in.Project, 409)
			return
		}
	}
	for _, d := range s.verified(ctx, claims) {
		if d.Spec.Project != in.Project && under(in.Domain, d.Spec.Name) {
			http.Error(w, in.Domain+" is verified for another project", 409)
			return
		}
	}
	d := &v1alpha1.Domain{ObjectMeta: metav1.ObjectMeta{Name: in.Project + "." + in.Domain}, Spec: v1alpha1.DomainSpec{Name: in.Domain, Project: in.Project}}
	if validate.DNSName(d.Name) != nil {
		http.Error(w, in.Domain+" is too long", 400)
		return
	}
	if err := s.create(s.elevated(ctx), d, "domain "+d.Spec.Name+" for project "+d.Spec.Project); err != nil {
		answer(w, err, 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "records": []string{"*." + in.Domain + "  CNAME  door." + s.domain, "_wecolab." + in.Domain + "  TXT  \"wecolab=project:" + in.Project + "\""}})
}

func (s *server) removeDomain(w http.ResponseWriter, r *http.Request) {
	d := &v1alpha1.Domain{ObjectMeta: metav1.ObjectMeta{Name: r.PathValue("domain")}}
	if validate.DNSName(d.Name) != nil {
		http.Error(w, "no such domain", 404)
		return
	}
	if exists, err := s.fromGit(r.Context(), d); err != nil || !exists {
		answer(w, cmpErr(err, fail(404, "no such domain")), 502)
		return
	}
	if !s.can(w, r, d.Spec.Project) {
		return
	}
	if err := s.remove(s.elevated(r.Context()), d, "domain "+d.Spec.Name+" removed"); err != nil {
		answer(w, err, 502)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
