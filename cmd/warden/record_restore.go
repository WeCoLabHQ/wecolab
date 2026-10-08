package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"wecolab.io/wecolab/internal/fabric"
	"wecolab.io/wecolab/internal/warden"
)

// authorizeRestore verifies the kubeconfig's authenticated identity and RBAC
// before this process obtains privileged writer credentials. A file-supplied
// actor or unverified --actor never grants authority.
func authorizeRestore(ctx context.Context, k kubernetes.Interface, ns, app, requested string) (string, error) {
	review, err := k.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("verify operator identity: %w", err)
	}
	actor := review.Status.UserInfo.Username
	if actor == "" {
		return "", fmt.Errorf("Kubernetes did not verify operator identity")
	}
	if requested != "" && requested != actor {
		return "", fmt.Errorf("--actor must match authenticated Kubernetes identity")
	}
	access, err := k.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: ns, Name: app, Group: "wecolab.io", Resource: "apps", Verb: "update"}}}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("verify App update permission: %w", err)
	}
	if !access.Status.Allowed || access.Status.Denied || access.Status.EvaluationError != "" {
		return "", fmt.Errorf("operator is not authorized to update App in namespace %s", ns)
	}
	return actor, nil
}

// recordRestore uses the operator's kubeconfig for authorization and fresh
// primary observation; Git credentials must be explicitly supplied in env and
// are not fetched or read before authorization succeeds.
func recordRestore(args []string) error {
	fs := flag.NewFlagSet("record-restore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	ns := fs.String("namespace", "", "App project namespace")
	app := fs.String("app", "", "App name")
	evidence := fs.String("evidence", "", "completed sanitized JSON scenario result")
	actorFlag := fs.String("actor", "", "authenticated operator username (optional when derived from kubeconfig)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *ns == "" || *app == "" || *evidence == "" {
		return fmt.Errorf("usage: warden record-restore --namespace NS --app APP --evidence FILE [--actor KUBERNETES-USERNAME]")
	}
	file, err := os.Open(*evidence)
	if err != nil {
		return fmt.Errorf("restore evidence: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return err
	}
	a, err := warden.ParseRestoreArtifact(raw)
	if err != nil {
		return err
	}
	if a.Namespace != *ns || a.App != *app {
		return fmt.Errorf("restore evidence names another App")
	}
	if err := warden.ValidateRestoreArtifact(a, time.Now()); err != nil {
		return err
	}
	config, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("operator kubeconfig required: %w", err)
	}
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	ctx := interrupted()
	actor, err := authorizeRestore(ctx, kube, *ns, *app, strings.TrimSpace(*actorFlag))
	if err != nil {
		return err
	}
	reader, err := client.New(config, client.Options{Scheme: scheme()})
	if err != nil {
		return err
	}
	g := fabric.GitFromEnv()
	if g == nil {
		return fmt.Errorf("WECOLAB_GIT_URL and WECOLAB_GIT_TOKEN required after operator authorization")
	}
	sha, err := warden.RecordRestore(ctx, g, raw, actor, func(ctx context.Context, site string) (*warden.SiteStatus, error) {
		// A new Peers instance avoids reusing a cached response after a Git conflict.
		status, ok := (&warden.Peers{Client: reader}).Get(ctx, site)
		if !ok {
			return nil, fmt.Errorf("current primary %s did not provide a live status", site)
		}
		return status, nil
	})
	if err != nil {
		return err
	}
	fmt.Println("operator-recorded restore committed", sha)
	return nil
}
