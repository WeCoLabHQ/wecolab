package warden

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"wecolab.io/wecolab/api/v1alpha1"
	"wecolab.io/wecolab/internal/netbird"
)

func TestPeopleTerminatingMemberLosesRBACDuringMeshOutage(t *testing.T) {
	for _, failedOperation := range []string{"users", "delete"} {
		t.Run(failedOperation, func(t *testing.T) {
			var broken atomic.Bool
			broken.Store(true)
			mesh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if broken.Load() && (failedOperation == "users" && r.URL.Path == "/api/users" || failedOperation == "delete" && r.Method == http.MethodDelete) {
					http.Error(w, `{"message":"unavailable"}`, 503)
					return
				}
				if r.URL.Path == "/api/users" {
					fmt.Fprint(w, `[{"id":"u1","email":"owner@example.org","role":"admin"}]`)
					return
				}
				if r.Method == http.MethodDelete {
					w.WriteHeader(204)
					return
				}
				fmt.Fprint(w, `[]`)
			}))
			defer mesh.Close()
			m := &v1alpha1.Member{ObjectMeta: metav1.ObjectMeta{Name: "owner", UID: types.UID("member-1"), Finalizers: []string{memberFinalizer}}, Spec: v1alpha1.MemberSpec{Email: "owner@example.org", Role: "admin"}}
			objects := []client.Object{m, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: SettingsName, Namespace: SystemNS}, Data: map[string]string{"zone": "fab.example.org", "network": "10.77.0.0/16", "writer": "pub"}}}
			for _, binding := range RenderBindings(m) {
				objects = append(objects, binding)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).Build()
			if err := c.Delete(context.Background(), m); err != nil {
				t.Fatal(err)
			}
			r := &PeopleReconciler{Client: c, Site: "pub", Mesh: &netbird.Client{URL: mesh.URL, Token: "disposable", HTTP: mesh.Client()}, checked: time.Now()}
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: m.Name}}
			if _, err := r.Reconcile(context.Background(), req); err == nil {
				t.Fatal("mesh outage was hidden")
			}
			if err := c.Get(context.Background(), req.NamespacedName, m); err != nil || m.DeletionTimestamp.IsZero() || len(m.Finalizers) != 1 {
				t.Fatalf("cleanup finalizer lost: %v %+v", err, m)
			}
			bindings := &rbacv1.ClusterRoleBindingList{}
			if err := c.List(context.Background(), bindings); err != nil {
				t.Fatal(err)
			}
			if len(bindings.Items) != 0 {
				t.Fatal("terminating administrator kept its RBAC while mesh failed")
			}
			broken.Store(false)
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(context.Background(), req.NamespacedName, m); !apierrors.IsNotFound(err) {
				t.Fatalf("successful cleanup did not release finalizer: %v", err)
			}
		})
	}
}
