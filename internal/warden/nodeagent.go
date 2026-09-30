package warden

import (
	"context"
	"os"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// IdleTaint is on a laptop node while its person is using it, twice: NoSchedule, which nothing
// tolerates, keeps new pods off; NoExecute, which best-effort pods tolerate for a minute, drains them.
const IdleTaint = "wecolab.io/idle"

// Taints is a node's taints with the idle taints set or removed.
func Taints(cur []corev1.Taint, inUse bool) []corev1.Taint {
	out := []corev1.Taint{}
	for _, t := range cur {
		if t.Key != IdleTaint {
			out = append(out, t)
		}
	}
	if inUse {
		out = append(out, corev1.Taint{Key: IdleTaint, Value: "true", Effect: corev1.TaintEffectNoSchedule},
			corev1.Taint{Key: IdleTaint, Value: "true", Effect: corev1.TaintEffectNoExecute})
	}
	return out
}

// SameTaints compares taints by key, value and effect: when a NoExecute taint was added says nothing
// about whether it is the one wanted.
func SameTaints(a, b []corev1.Taint) bool {
	return slices.EqualFunc(a, b, func(x, y corev1.Taint) bool { return x.Key == y.Key && x.Value == y.Value && x.Effect == y.Effect })
}

// RunNodeAgent keeps the node's idle taint as the mode file says, every few seconds. Anything but "idle"
// counts as in use: no mode file means someone may be there.
func RunNodeAgent(ctx context.Context, c client.Client, node, modeFile string) error {
	lg := log.FromContext(ctx)
	for ctx.Err() == nil {
		b, _ := os.ReadFile(modeFile)
		mode := strings.TrimSpace(string(b))
		if changed, err := SyncTaints(ctx, c, node, mode != "idle"); err != nil {
			lg.Error(err, "setting the idle taint", "node", node)
		} else if changed {
			lg.Info("node mode", "node", node, "mode", mode)
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}

// SyncTaints gives the node the idle taints it should have, comparing with the node itself each time, so
// a node registered again or a taint removed by hand is put right. The patch carries the node's
// resourceVersion: taints are one list, so a change anyone made meanwhile fails it rather than being
// overwritten.
func SyncTaints(ctx context.Context, c client.Client, node string, inUse bool) (bool, error) {
	n := &corev1.Node{}
	if err := c.Get(ctx, types.NamespacedName{Name: node}, n); err != nil {
		return false, err
	}
	want := Taints(n.Spec.Taints, inUse)
	if SameTaints(want, n.Spec.Taints) {
		return false, nil
	}
	patch := client.MergeFromWithOptions(n.DeepCopy(), client.MergeFromWithOptimisticLock{})
	n.Spec.Taints = want
	return true, c.Patch(ctx, n, patch)
}
