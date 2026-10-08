package warden

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	gitTransferAnnotation          = "wecolab.io/git-transfer"
	gitMirrorPendingAnnotation     = "wecolab.io/git-mirror-pending"
	gitCustodyRepositoryAnnotation = "wecolab.io/git-custody-repository"
)

var forgejoDeployment = types.NamespacedName{Namespace: SystemNS, Name: "forgejo"}

func (w *Writer) forgejoReader() client.Reader {
	if w.APIReader != nil {
		return w.APIReader
	}
	return w.Client
}

func (w *Writer) forgejoDeployment(ctx context.Context) (*appsv1.Deployment, error) {
	d := &appsv1.Deployment{}
	if err := w.forgejoReader().Get(ctx, forgejoDeployment, d); err != nil {
		return nil, fmt.Errorf("read Forgejo deployment: %w", err)
	}
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || d.Spec.Selector == nil ||
		len(d.Spec.Selector.MatchLabels) != 1 || d.Spec.Selector.MatchLabels["app"] != "forgejo" {
		return nil, errors.New("Forgejo deployment is not the expected isolated Recreate workload")
	}
	return d, nil
}

func (w *Writer) updateForgejo(ctx context.Context, change func(*appsv1.Deployment) error) (*appsv1.Deployment, error) {
	var updated *appsv1.Deployment
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		d, err := w.forgejoDeployment(ctx)
		if err != nil {
			return err
		}
		if err := change(d); err != nil {
			return err
		}
		if err := w.Client.Update(ctx, d); err != nil {
			return err
		}
		updated = d
		return nil
	})
	return updated, err
}

func (w *Writer) markMirrorsPending(ctx context.Context) error {
	_, err := w.updateForgejo(ctx, func(d *appsv1.Deployment) error {
		if d.Annotations == nil {
			d.Annotations = make(map[string]string)
		}
		d.Annotations[gitMirrorPendingAnnotation] = "true"
		return nil
	})
	return err
}

func rolloutNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// requestForgejoRollout returns the marker only after its Deployment update is
// acknowledged. Clearing pending and changing the pod template are one update.
func (w *Writer) requestForgejoRollout(ctx context.Context, clearPending bool) (string, error) {
	nonce, err := rolloutNonce()
	if err != nil {
		return "", err
	}
	_, err = w.updateForgejo(ctx, func(d *appsv1.Deployment) error {
		if d.Spec.Template.Annotations == nil {
			d.Spec.Template.Annotations = make(map[string]string)
		}
		d.Spec.Template.Annotations[gitTransferAnnotation] = nonce
		if clearPending {
			if d.Annotations == nil {
				d.Annotations = make(map[string]string)
			}
			delete(d.Annotations, gitMirrorPendingAnnotation)
		}
		return nil
	})
	return nonce, err
}

// quiesceMirrors records unfinished deletion before touching any mirror. An
// earlier successful marker cannot certify a later mirror deletion.
func (w *Writer) quiesceMirrors(ctx context.Context) error {
	if _, err := writerHolder(ctx); err != nil {
		return err
	}
	d, err := w.forgejoDeployment(ctx)
	if err != nil {
		return err
	}
	id, err := w.Git.RepositoryID(ctx)
	if err != nil {
		return err
	}
	if id == 0 {
		return errors.New("repository absent before custody fencing")
	}
	protected, err := w.Git.PreservationProtected(ctx, ForgejoOwner)
	if err != nil {
		return err
	}
	needsFence := !protected || d.Annotations[gitCustodyRepositoryAnnotation] != strconv.FormatInt(id, 10)
	if needsFence {
		// Persist before changing protection: an old receive-pack may already
		// have passed its hook, and must be drained even across a Warden crash.
		if err := w.markMirrorsPending(ctx); err != nil {
			return err
		}
		if err := w.Git.ProtectPreserved(ctx, ForgejoOwner); err != nil {
			return err
		}
	}
	mirrors, err := w.Git.PushMirrors(ctx)
	if err != nil {
		return err
	}
	hadMirrors := len(mirrors) != 0
	if len(mirrors) != 0 {
		if err := w.markMirrorsPending(ctx); err != nil {
			return fmt.Errorf("record pending mirror deletion: %w", err)
		}
		for _, mirror := range mirrors {
			if err := w.Git.DeletePushMirror(ctx, mirror.Name); err != nil {
				return fmt.Errorf("delete push mirror %q: %w", mirror.Name, err)
			}
		}
		mirrors, err = w.Git.PushMirrors(ctx)
		if err != nil {
			return err
		}
		if len(mirrors) != 0 {
			return errors.New("push mirrors remain after deletion")
		}
	}
	if needsFence || hadMirrors || d.Spec.Template.Annotations[gitTransferAnnotation] == "" ||
		d.Annotations[gitMirrorPendingAnnotation] != "" {
		marker, err := w.requestForgejoRollout(ctx, true)
		if err != nil {
			return fmt.Errorf("request post-mirror Forgejo rollout: %w", err)
		}
		if err := w.waitForgejoRollout(ctx, marker, true); err != nil {
			return err
		}
		_, err = w.updateForgejo(ctx, func(current *appsv1.Deployment) error {
			if current.Spec.Template.Annotations[gitTransferAnnotation] != marker {
				return errors.New("Forgejo changed before custody fence acknowledgement")
			}
			if current.Annotations == nil {
				current.Annotations = make(map[string]string)
			}
			current.Annotations[gitCustodyRepositoryAnnotation] = strconv.FormatInt(id, 10)
			return nil
		})
		return err
	}
	// The marker may be from an interrupted rollout; only a fresh observed
	// Deployment AND pod listing can establish its completion.
	return w.waitForgejoRollout(ctx, d.Spec.Template.Annotations[gitTransferAnnotation], true)
}

// drainForgejo always rolls the Forgejo process, including on retry, after
// writes have been closed. It does not clear pending mirror state.
func (w *Writer) drainForgejo(ctx context.Context) error {
	if _, err := writerHolder(ctx); err != nil {
		return err
	}
	marker, err := w.requestForgejoRollout(ctx, false)
	if err != nil {
		return fmt.Errorf("request Forgejo drain: %w", err)
	}
	return w.waitForgejoRollout(ctx, marker, false)
}

func (w *Writer) waitForgejoRollout(ctx context.Context, marker string, requireNoPending bool) error {
	if marker == "" {
		return errors.New("Forgejo rollout has no marker")
	}
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready, err := w.forgejoRolloutReady(ctx, marker, requireNoPending)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *Writer) forgejoRolloutReady(ctx context.Context, marker string, requireNoPending bool) (bool, error) {
	d, err := w.forgejoDeployment(ctx)
	if err != nil {
		return false, err
	}
	if marker == "" || d.Spec.Template.Annotations[gitTransferAnnotation] != marker ||
		(requireNoPending && d.Annotations[gitMirrorPendingAnnotation] != "") {
		return false, nil
	}
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	if replicas <= 0 || d.Generation <= 0 || d.Status.ObservedGeneration < d.Generation ||
		d.Status.UpdatedReplicas != replicas || d.Status.ReadyReplicas != replicas || d.Status.AvailableReplicas != replicas {
		return false, nil
	}
	pods := &corev1.PodList{}
	if err := w.forgejoReader().List(ctx, pods, client.InNamespace(SystemNS), client.MatchingLabels{"app": "forgejo"}); err != nil {
		return false, fmt.Errorf("list live Forgejo pods: %w", err)
	}
	var ready int32
	for _, pod := range pods.Items {
		if pod.Annotations[gitTransferAnnotation] != marker || pod.DeletionTimestamp != nil {
			return false, nil // an old process can still push or admit a write
		}
		if pod.Status.Phase != corev1.PodRunning {
			return false, nil
		}
		isReady := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				isReady = true
			}
		}
		if !isReady {
			return false, nil
		}
		ready++
	}
	return ready == replicas, nil
}
