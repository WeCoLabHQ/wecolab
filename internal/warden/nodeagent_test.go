package warden

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTaints(t *testing.T) {
	laptop := corev1.Taint{Key: "wecolab.io/laptop", Value: "true", Effect: corev1.TaintEffectNoSchedule}
	inUse := Taints([]corev1.Taint{laptop}, true)
	if len(inUse) != 3 || inUse[0] != laptop || inUse[1].Effect != corev1.TaintEffectNoSchedule || inUse[2].Effect != corev1.TaintEffectNoExecute {
		t.Fatalf("in use: %v", inUse)
	}
	if again := Taints(inUse, true); len(again) != 3 {
		t.Fatalf("setting twice duplicates: %v", again)
	}
	if idle := Taints(inUse, false); len(idle) != 1 || idle[0] != laptop {
		t.Fatalf("idle: %v", idle)
	}
}

func TestSyncTaints(t *testing.T) {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	laptop := corev1.Taint{Key: "wecolab.io/laptop", Value: "true", Effect: corev1.TaintEffectNoSchedule}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "mac"}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{laptop}}}).Build()
	ctx := context.Background()
	taints := func() []corev1.Taint {
		n := &corev1.Node{}
		_ = c.Get(ctx, types.NamespacedName{Name: "mac"}, n)
		return n.Spec.Taints
	}
	if changed, err := SyncTaints(ctx, c, "mac", true); !changed || err != nil || len(taints()) != 3 {
		t.Fatalf("in use: %v %v %v", changed, err, taints())
	}
	if changed, _ := SyncTaints(ctx, c, "mac", true); changed {
		t.Fatal("nothing to change, no patch")
	}
	// Someone removed the taint while the mode stayed the same: it comes back.
	n := &corev1.Node{}
	_ = c.Get(ctx, types.NamespacedName{Name: "mac"}, n)
	n.Spec.Taints = []corev1.Taint{laptop}
	_ = c.Update(ctx, n)
	if changed, err := SyncTaints(ctx, c, "mac", true); !changed || err != nil || len(taints()) != 3 {
		t.Fatalf("put right: %v %v %v", changed, err, taints())
	}
	if _, err := SyncTaints(ctx, c, "mac", false); err != nil || !SameTaints(taints(), []corev1.Taint{laptop}) {
		t.Fatalf("idle: %v %v", err, taints())
	}
}
