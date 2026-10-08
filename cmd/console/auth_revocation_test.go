package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"wecolab.io/wecolab/api/v1alpha1"
)

func TestTerminatingMemberSessionDenied(t *testing.T) {
	m := &v1alpha1.Member{ObjectMeta: metav1.ObjectMeta{Name: "owner", Finalizers: []string{"cleanup"}, DeletionTimestamp: &metav1.Time{Time: time.Now()}}, Spec: v1alpha1.MemberSpec{Email: "owner@example.org", Role: "owner"}}
	s, _ := testServer(t, nil, m)
	s.auth = &auth{key: []byte("0123456789abcdef0123456789abcdef")}
	sess := s.auth.sign("session", identity{Email: "OWNER@example.org", Admin: true, Exp: time.Now().Add(time.Hour).Unix()})
	for _, endpoint := range []string{"/api/me", "/api/sites"} {
		called := false
		h := logged(s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(200) })))
		r := httptest.NewRequest(http.MethodPost, endpoint, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || called {
			t.Fatalf("%s: code=%d invoked=%v", endpoint, w.Code, called)
		}
	}
	if _, err := s.lookup(context.Background(), "owner@example.org"); err == nil {
		t.Fatal("terminating Member could log in again")
	}
}

func TestDevTerminatingOwnerHasNoFallbackAuthority(t *testing.T) {
	m := &v1alpha1.Member{ObjectMeta: metav1.ObjectMeta{Name: "owner", Finalizers: []string{"cleanup"}, DeletionTimestamp: &metav1.Time{Time: time.Now()}}, Spec: v1alpha1.MemberSpec{Email: "owner@example.org", Role: "owner"}}
	s, _ := testServer(t, nil, m)
	if id := s.who(httptest.NewRequest(http.MethodGet, "/api/me", nil)); id != nil {
		t.Fatalf("deleted owner replaced with privileged fallback: %+v", id)
	}
}
