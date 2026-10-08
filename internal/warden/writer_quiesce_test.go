package warden

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"wecolab.io/wecolab/internal/fabric"
)

func testReadyForgejo(t *testing.T) []client.Object {
	t.Helper()
	return []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "forgejo", Namespace: SystemNS, Generation: 1,
			Annotations: map[string]string{gitCustodyRepositoryAnnotation: "1"}},
			Spec: appsv1.DeploymentSpec{Replicas: new(int32(1)), Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "forgejo"}},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "forgejo"}, Annotations: map[string]string{gitTransferAnnotation: "initial"}}}},
			Status: appsv1.DeploymentStatus{ObservedGeneration: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "forgejo-initial", Namespace: SystemNS, Labels: map[string]string{"app": "forgejo"}, Annotations: map[string]string{gitTransferAnnotation: "initial"}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
	}
}

type rolloutClient struct {
	client.Client
	updates chan string
}

func (c *rolloutClient) Update(ctx context.Context, obj client.Object, options ...client.UpdateOption) error {
	d, ok := obj.(*appsv1.Deployment)
	if !ok {
		return c.Client.Update(ctx, obj, options...)
	}
	old := &appsv1.Deployment{}
	if err := c.Client.Get(ctx, client.ObjectKeyFromObject(d), old); err != nil {
		return err
	}
	marker := d.Spec.Template.Annotations[gitTransferAnnotation]
	if marker != old.Spec.Template.Annotations[gitTransferAnnotation] {
		d.Generation = old.Generation + 1
	}
	if err := c.Client.Update(ctx, d, options...); err != nil {
		return err
	}
	if marker != old.Spec.Template.Annotations[gitTransferAnnotation] {
		c.updates <- marker
	}
	return nil
}

type rolloutFixture struct {
	client  client.Client
	updates <-chan string
}

// A rollout only completes when the caller explicitly advances both the
// Deployment controller's observed status and the live Pod set.
func newRolloutFixture(t *testing.T, initialAnnotations map[string]string) *rolloutFixture {
	t.Helper()
	objects := testReadyForgejo(t)
	if len(initialAnnotations) > 0 {
		objects[0].(*appsv1.Deployment).Annotations = initialAnnotations
	}
	updates := make(chan string, 32)
	inner := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&appsv1.Deployment{}).WithObjects(objects...).Build()
	return &rolloutFixture{client: &rolloutClient{Client: inner, updates: updates}, updates: updates}
}

func (f *rolloutFixture) completeRollout(ctx context.Context, marker string) error {
	d := &appsv1.Deployment{}
	if err := f.client.Get(ctx, forgejoDeployment, d); err != nil {
		return err
	}
	if d.Spec.Template.Annotations[gitTransferAnnotation] != marker {
		return fmt.Errorf("rollout marker %q is not current", marker)
	}
	pods := &corev1.PodList{}
	if err := f.client.List(ctx, pods, client.InNamespace(SystemNS), client.MatchingLabels{"app": "forgejo"}); err != nil {
		return err
	}
	for i := range pods.Items {
		if err := f.client.Delete(ctx, &pods.Items[i]); err != nil {
			return err
		}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "forgejo-" + marker, Namespace: SystemNS,
		Labels: map[string]string{"app": "forgejo"}, Annotations: map[string]string{gitTransferAnnotation: marker}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	if err := f.client.Create(ctx, pod); err != nil {
		return err
	}
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	return f.client.Status().Update(ctx, d)
}

func awaitRollout(t *testing.T, ctx context.Context, f *rolloutFixture) string {
	t.Helper()
	select {
	case marker := <-f.updates:
		return marker
	case <-ctx.Done():
		t.Fatal("Forgejo rollout not requested:", ctx.Err())
		return ""
	}
}

func TestQuiescePersistsPendingBeforeDeletingMirrorsDespitePreviousRollout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f := newRolloutFixture(t, nil)
	copy := &fakeCopy{exists: true, mirrors: []fabric.PushMirror{{Name: "legacy"}}}
	server := httptest.NewServer(copy)
	defer server.Close()
	g := &fabric.Git{URL: server.URL, Repo: "fabric/fabric", Token: "token", HTTP: server.Client()}
	coordination := testWriterCoordination().CoordinationV1()
	w := &Writer{Client: f.client, APIReader: f.client, Git: g, Coordination: coordination}
	// A previously successful marker must not justify deleting a newly found mirror.
	finished := make(chan error, 1)
	go func() { finished <- withWriterLock(ctx, coordination, w.quiesceMirrors) }()
	marker := awaitRollout(t, ctx, f)
	d := &appsv1.Deployment{}
	if err := f.client.Get(ctx, forgejoDeployment, d); err != nil {
		t.Fatal(err)
	}
	if d.Annotations[gitMirrorPendingAnnotation] != "" || marker == "initial" || len(copy.mirrors) != 0 {
		t.Fatalf("pending must be cleared only in fresh post-delete rollout: deployment=%v mirror=%v", d.Annotations, copy.mirrors)
	}
	if err := f.completeRollout(ctx, marker); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestObservedRolloutRequiresGenerationAndOldPodsGone(t *testing.T) {
	ctx := context.Background()
	f := newRolloutFixture(t, nil)
	w := &Writer{Client: f.client, APIReader: f.client}
	marker := "next"
	d := &appsv1.Deployment{}
	if err := f.client.Get(ctx, forgejoDeployment, d); err != nil {
		t.Fatal(err)
	}
	d.Spec.Template.Annotations[gitTransferAnnotation] = marker
	if err := f.client.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	if ready, err := w.forgejoRolloutReady(ctx, marker, true); err != nil || ready {
		t.Fatalf("old observed generation cannot be ready: %v %v", ready, err)
	}
	if err := f.client.Get(ctx, forgejoDeployment, d); err != nil {
		t.Fatal(err)
	}
	d.Status.ObservedGeneration = d.Generation
	if err := f.client.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	if ready, err := w.forgejoRolloutReady(ctx, marker, true); err != nil || ready {
		t.Fatalf("old pod cannot count as rollout completion: %v %v", ready, err)
	}
	if err := f.completeRollout(ctx, marker); err != nil {
		t.Fatal(err)
	}
	if ready, err := w.forgejoRolloutReady(ctx, marker, true); err != nil || !ready {
		t.Fatalf("observed generation and only new ready pod must complete: %v %v", ready, err)
	}

}
