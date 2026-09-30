package warden

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"wecolab.io/wecolab/api/v1alpha1"
)

// DomainReconciler verifies a project's domain by DNS alone, at the writer: the wildcard must resolve
// to the fabric's Doors (its public sites), and _wecolab.<domain> TXT must name the project. Nothing
// is written to anyone's DNS; the member creates two records once.
type DomainReconciler struct {
	client.Client
	Site   string
	Resync time.Duration
	Lookup func(ctx context.Context, kind, name string) []string // A or TXT; nil means public resolvers
}

// Doors are the public sites' addresses: what a project's wildcard must resolve to.
func Doors(sites []v1alpha1.Site) []string {
	out := []string{}
	for _, s := range sites {
		if s.Spec.Public != nil && s.Spec.Public.Address != "" {
			out = append(out, s.Spec.Public.Address)
		}
	}
	slices.Sort(out)
	return out
}

const domainProbe = "wecolab-probe"

// DomainVerdict decides from the lookups. txts are the TXT records at _wecolab.<domain>.
func DomainVerdict(doors, wildcard, txts []string, project string) (bool, string) {
	if len(doors) == 0 {
		return false, "the fabric's Doors could not be resolved"
	}
	if len(wildcard) == 0 {
		return false, "*." + "<domain> does not resolve yet"
	}
	for _, d := range doors {
		if !slices.Contains(wildcard, d) {
			return false, fmt.Sprintf("the wildcard resolves to %s, not to the Doors %s", strings.Join(wildcard, ","), strings.Join(doors, ","))
		}
	}
	want := "wecolab=project:" + project
	for _, t := range txts {
		if strings.TrimSpace(t) == want {
			return true, "wildcard and ownership record verified"
		}
	}
	return false, "_wecolab.<domain> TXT must be exactly " + want
}

// publicResolvers answer the way the world sees the records, not the way the seed's
// own cache does; a member who has just created a record is not made to wait out a
// stale negative answer.
var publicResolvers = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}

func (r *DomainReconciler) lookup(ctx context.Context, kind, name string) []string {
	if r.Lookup != nil {
		return r.Lookup(ctx, kind, name)
	}
	for _, addr := range publicResolvers {
		res := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
		}}
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		var v []string
		var err error
		if kind == "TXT" {
			v, err = res.LookupTXT(c, name)
		} else {
			v, err = res.LookupHost(c, name)
		}
		cancel()
		if err == nil || isNotFound(err) {
			return v
		}
	}
	return nil
}

func isNotFound(err error) bool {
	var d *net.DNSError
	return errors.As(err, &d) && d.IsNotFound
}

func (r *DomainReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	d := &v1alpha1.Domain{}
	if err := r.Get(ctx, req.NamespacedName, d); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if s, err := ReadSettings(ctx, r); err != nil || s.Writer != r.Site {
		return ctrl.Result{RequeueAfter: r.Resync}, err // the writer checks domains
	}
	now := time.Now()
	sites := &v1alpha1.SiteList{}
	if err := r.List(ctx, sites); err != nil {
		return ctrl.Result{}, err
	}
	doors := Doors(sites.Items)
	wildcard := r.lookup(ctx, "A", domainProbe+"."+d.Spec.Name)
	txts := r.lookup(ctx, "TXT", "_wecolab."+d.Spec.Name)
	ok, reason := DomainVerdict(doors, wildcard, txts, d.Spec.Project)
	changed := d.Status.Verified != ok
	d.Status.Verified, d.Status.Reason = ok, strings.ReplaceAll(reason, "<domain>", d.Spec.Name)
	d.Status.LastChecked = &metav1.Time{Time: now}
	phase := "Pending"
	if ok {
		phase = "Verified"
	}
	SetCondition(&d.Status.Conditions, cond("Verified", ok, phase, d.Status.Reason), now, d.Generation)
	if changed {
		log.FromContext(ctx).Info("domain", "domain", d.Spec.Name, "project", d.Spec.Project, "verified", ok, "reason", reason)
	}
	after := r.Resync
	if !ok {
		after = 2 * time.Minute // a person is probably creating records right now
	}
	return ctrl.Result{RequeueAfter: after}, r.Status().Update(ctx, d)
}

func (r *DomainReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("domain").For(&v1alpha1.Domain{}).Complete(r)
}
