package main

import (
	"context"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestRecordRestoreAuthorizationBeforeWriterAccess(t *testing.T) {
	for _, tc := range []struct {
		name, identity, requested string
		allowed                   bool
		wantError                 bool
	}{
		{"authorized derived actor", "operator@example.org", "", true, false},
		{"authorized matching actor", "operator@example.org", "operator@example.org", true, false},
		{"unauthorized", "operator@example.org", "", false, true},
		{"actor substitution", "operator@example.org", "admin@example.org", true, true},
		{"unidentified user", "", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			var reviews int
			client.PrependReactor("create", "selfsubjectreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
				return true, &authenticationv1.SelfSubjectReview{Status: authenticationv1.SelfSubjectReviewStatus{UserInfo: authenticationv1.UserInfo{Username: tc.identity}}}, nil
			})
			client.PrependReactor("create", "selfsubjectaccessreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
				reviews++
				attrs := a.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview).Spec.ResourceAttributes
				if attrs.Namespace != "team" || attrs.Name != "docs" || attrs.Resource != "apps" || attrs.Group != "wecolab.io" || attrs.Verb != "update" {
					t.Fatalf("wrong authorization scope: %+v", attrs)
				}
				return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: tc.allowed}}, nil
			})
			actor, err := authorizeRestore(context.Background(), client, "team", "docs", tc.requested)
			if (err != nil) != tc.wantError {
				t.Fatalf("actor=%q err=%v", actor, err)
			}
			if err == nil && actor != tc.identity {
				t.Fatalf("unverified actor %q", actor)
			}
			if strings.Contains(tc.name, "substitution") && reviews != 0 {
				t.Fatal("actor substitution reached privileged authorization")
			}
		})
	}
}

func TestRecordRestoreRejectsMissingArgumentsWithoutCredentials(t *testing.T) {
	t.Setenv("WECOLAB_GIT_URL", "")
	t.Setenv("WECOLAB_GIT_TOKEN", "")
	if err := recordRestore([]string{"--namespace", "team", "--app", "docs"}); err == nil || !strings.Contains(err.Error(), "evidence") {
		t.Fatalf("missing evidence reached credentials: %v", err)
	}
	if err := recordRestore([]string{"--namespace", "team", "--app", "docs", "--evidence", "/not/real"}); err == nil {
		t.Fatal("missing evidence file accepted")
	}
}
