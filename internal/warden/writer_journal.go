package warden

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/util/retry"
)

const recreationAnnotation = "wecolab.io/git-recreation"

type recreationJournal struct {
	Nonce        string `json:"nonce"`
	RepositoryID int64  `json:"repositoryID"`
}

func readRecreation(ctx context.Context, coordination coordinationclient.CoordinationV1Interface) (*recreationJournal, error) {
	if coordination == nil {
		return nil, errors.New("writer transition lease: coordination client is nil")
	}
	lease, err := coordination.Leases(SystemNS).Get(ctx, "wecolab-writer-transition", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read recreation journal: %w", err)
	}
	value, exists := lease.Annotations[recreationAnnotation]
	if !exists {
		return nil, nil
	}
	var journal recreationJournal
	if err := json.Unmarshal([]byte(value), &journal); err != nil || journal.Nonce == "" || journal.RepositoryID <= 0 {
		return nil, errors.New("invalid recreation journal on writer transition lease")
	}
	return &journal, nil
}

// mutateRecreation reads the Lease directly; a renewal and an annotation update
// may race, so conflicts are retried without ever changing the lease spec.
func mutateRecreation(ctx context.Context, coordination coordinationclient.CoordinationV1Interface, expected *recreationJournal, next *recreationJournal) error {
	holder, err := writerHolder(ctx)
	if err != nil {
		return err
	}
	if coordination == nil {
		return errors.New("writer transition lease: coordination client is nil")
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		leases := coordination.Leases(SystemNS)
		lease, err := leases.Get(ctx, "wecolab-writer-transition", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
			return errors.New("writer transition lease: holder changed before recreation journal update")
		}
		current := lease.Annotations[recreationAnnotation]
		if expected == nil {
			if _, present := lease.Annotations[recreationAnnotation]; present {
				return errors.New("recreation journal already pending")
			}
		} else {
			var found recreationJournal
			if err := json.Unmarshal([]byte(current), &found); err != nil || found != *expected || current == "" {
				return errors.New("recreation journal changed; refusing stale clear")
			}
		}
		if lease.Annotations == nil {
			lease.Annotations = make(map[string]string)
		}
		if next == nil {
			delete(lease.Annotations, recreationAnnotation)
		} else {
			encoded, err := json.Marshal(next)
			if err != nil {
				return err
			}
			lease.Annotations[recreationAnnotation] = string(encoded)
		}
		_, err = leases.Update(ctx, lease, metav1.UpdateOptions{})
		return err
	})
}

func (w *Writer) recreate(ctx context.Context) error {
	id, err := w.Git.RepositoryID(ctx)
	if err != nil {
		return fmt.Errorf("identify repository before recreation: %w", err)
	}
	if id <= 0 {
		return errors.New("cannot recreate an absent repository")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	journal := recreationJournal{Nonce: hex.EncodeToString(nonce[:]), RepositoryID: id}
	if err := mutateRecreation(ctx, w.Coordination, nil, &journal); err != nil {
		return fmt.Errorf("persist recreation journal: %w", err)
	}
	if err := w.Git.Recreate(ctx, ForgejoMirror); err != nil {
		return fmt.Errorf("recreate repository (journal retained): %w", err)
	}
	if err := mutateRecreation(ctx, w.Coordination, &journal, nil); err != nil {
		return fmt.Errorf("clear recreation journal: %w", err)
	}
	return nil
}

// Recovery never calls DELETE. If Forgejo left an orphan directory, Ensure's
// error leaves the journal intact for operator intervention.
func (w *Writer) recoverRecreation(ctx context.Context, journal recreationJournal) error {
	if err := w.drainForgejo(ctx); err != nil {
		return fmt.Errorf("drain before recreation recovery: %w", err)
	}
	if err := w.Git.Ensure(ctx, ForgejoMirror); err != nil {
		return fmt.Errorf("ensure repository during recreation recovery: %w", err)
	}
	if err := w.Git.Protect(ctx, []string{ForgejoMirror}); err != nil {
		return fmt.Errorf("protect repository during recreation recovery: %w", err)
	}
	if err := mutateRecreation(ctx, w.Coordination, &journal, nil); err != nil {
		return fmt.Errorf("clear recreation journal after recovery: %w", err)
	}
	return nil
}
