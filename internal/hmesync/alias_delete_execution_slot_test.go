package hmesync

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestAliasDeletionExecutionSlotCancellationDoesNotReleaseOtherOwners(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := make(chan struct{}, 2)
		first := aliasDeletionExecutionSlot{pool: pool}
		second := aliasDeletionExecutionSlot{pool: pool}
		for _, slot := range []*aliasDeletionExecutionSlot{&first, &second} {
			if err := slot.acquire(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer slot.release()
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		third := aliasDeletionExecutionSlot{pool: pool}
		done := make(chan error, 1)
		go func() { done <- third.acquire(ctx) }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting slot error=%v", err)
		}
		third.release()
		if len(pool) != 2 || third.held {
			t.Fatal("cancelled waiter released another account's slot")
		}
		first.release()
		first.release()
		if len(pool) != 1 {
			t.Fatal("slot cleanup released more than its own permit")
		}
		if err := third.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
		third.release()
	})
}

func TestAliasDeletionRecoveryCancellationWhileReacquiringExecutionSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newAliasDeletionRecoveryFixture(t, 1)
		locker := f.service.locker.(*fakeAcquiringLocker)
		pool := make(chan struct{}, 2)
		first := aliasDeletionExecutionSlot{pool: pool}
		second := aliasDeletionExecutionSlot{pool: pool}
		defer first.release()
		defer second.release()
		WithAliasDeletionWaiter(func(ctx context.Context, delay time.Duration) error {
			// Model two healthy accounts taking the slots freed during cooldown
			// and remaining in flight when this account's timer expires.
			if err := first.acquire(ctx); err != nil {
				return err
			}
			if err := second.acquire(ctx); err != nil {
				return err
			}
			return f.clock.wait(ctx, delay)
		})(f.service)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- f.service.withAliasDeletionAccount(ctx, 3, pool, func(wait func(context.Context, time.Duration) error) error {
				return wait(ctx, time.Minute)
			})
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled slot reacquisition error=%v", err)
		}
		if !first.held || !second.held || len(pool) != 2 {
			t.Fatal("cooldown did not yield its slot or cancelled recovery released another account's slot")
		}
		if len(locker.token) != 1 || len(f.service.operationLock) != 0 {
			t.Fatal("cancelled slot reacquisition leaked the account or Apple operation lock")
		}
	})
}

func TestAliasDeletionOperationLockWaitDoesNotTakeExecutionSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newAliasDeletionRecoveryFixture(t, 1)
		releaseOperation, err := f.service.acquireOperation(context.Background(), 3)
		if err != nil {
			t.Fatal(err)
		}
		defer releaseOperation()
		pool := make(chan struct{}, 2)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- f.service.withAliasDeletionAccount(ctx, 3, pool, func(func(context.Context, time.Duration) error) error {
				t.Error("waiting account bypassed its Apple operation lock")
				return nil
			})
		}()
		synctest.Wait()
		if len(pool) != 0 {
			t.Error("waiting on another batch's account consumed an execution slot")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled operation lock wait error=%v", err)
		}
		if len(pool) != 0 {
			t.Fatal("cancelled operation lock wait leaked an execution slot")
		}
	})
}
