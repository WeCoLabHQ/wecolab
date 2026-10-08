package warden

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coordinationclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

type writerHolderKey struct{}

// The identity is private to a successful lease callback, never supplied by a caller.
func writerHolder(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	holder, ok := ctx.Value(writerHolderKey{}).(string)
	if !ok || holder == "" {
		return "", errors.New("writer transition lease: mutation requires the holder context")
	}
	return holder, nil
}

// withWriterLock serializes local writer transitions across commands and
// processes sharing this cluster. The lease is released only after fn returns,
// including when its parent context is canceled during cleanup.
func withWriterLock(ctx context.Context, coordination coordinationclient.CoordinationV1Interface, fn func(context.Context) error) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if coordination == nil {
		return errors.New("writer transition lease: coordination client is nil")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("writer transition lease identity: %w", err)
	}
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Namespace: "wecolab-system", Name: "wecolab-writer-transition"},
		Client:     coordination,
		LockConfig: resourcelock.ResourceLockConfig{Identity: hex.EncodeToString(nonce[:])},
	}

	// Election must outlive parent cancellation while a mutation is cleaning
	// up. Its own context is canceled only after fn returns (or acquisition
	// fails); WithoutCancel retains the parent's context values.
	electionCtx, stopElection := context.WithCancel(context.WithoutCancel(ctx))
	started := make(chan context.Context, 1)
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		// Automatic release also runs on renewal failure, before the callback
		// context is canceled. Release explicitly only after mutation cleanup.
		ReleaseOnCancel: false,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaseCtx context.Context) { started <- leaseCtx },
			// Run invokes OnStoppedLeading even when acquisition never succeeds.
			OnStoppedLeading: func() {},
		},
	})
	if err != nil {
		stopElection()
		return fmt.Errorf("writer transition lease: %w", err)
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		elector.Run(electionCtx)
	}()
	defer func() {
		stopElection()
		<-finished
		if !elector.IsLeader() {
			return
		}
		releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancelRelease()
		record, _, err := lock.Get(releaseCtx)
		if err != nil {
			result = errors.Join(result, fmt.Errorf("read writer transition lease for release: %w", err))
			return
		}
		if record.HolderIdentity != lock.Identity() {
			return // Never release a successor's lease after losing ours.
		}
		record.HolderIdentity = ""
		record.LeaseDurationSeconds = 1
		record.RenewTime = metav1.Now()
		if err := lock.Update(releaseCtx, *record); err != nil {
			result = errors.Join(result, fmt.Errorf("release writer transition lease: %w", err))
		}
	}()

	var leaseCtx context.Context
	select {
	case leaseCtx = <-started:
	case <-ctx.Done():
		return ctx.Err()
	case <-finished:
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("writer transition lease: election stopped without acquisition")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if leaseCtx.Err() != nil {
		return errors.New("writer transition lease: lost before mutation")
	}

	mutationCtx, cancelMutation := context.WithCancel(ctx)
	defer cancelMutation()
	stopLossNotification := context.AfterFunc(leaseCtx, cancelMutation)
	defer stopLossNotification()
	mutationErr := fn(context.WithValue(mutationCtx, writerHolderKey{}, lock.Identity()))
	// Distinguish losing the lease from our own intentional cancellation of
	// the elector below. Lease loss overrides a successful mutation callback.
	lost := leaseCtx.Err() != nil
	parentErr := ctx.Err()
	if lost {
		return errors.New("writer transition lease: lost during mutation")
	}
	if parentErr != nil {
		return parentErr
	}
	return mutationErr
}
