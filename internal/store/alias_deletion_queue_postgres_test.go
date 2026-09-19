package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"icloud-api/internal/domain"
)

// This opt-in test uses a fresh schema, leaving any other data in the supplied
// test database untouched. CI without PostgreSQL still runs the SQLite suite.
func TestAliasDeletionQueuePostgresRuntime(t *testing.T) {
	source := os.Getenv("ICLOUD_API_TEST_POSTGRES_URL")
	if source == "" {
		t.Skip("ICLOUD_API_TEST_POSTGRES_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	connection, err := sql.Open("pgx", source)
	if err != nil {
		t.Fatal(err)
	}
	id, err := newDeletionID()
	if err != nil {
		t.Fatal(err)
	}
	schema := "queue_test_" + strings.ReplaceAll(id, "-", "")
	if _, err := connection.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := connection.ExecContext(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
		_ = connection.Close()
	})
	parsed, err := url.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	s, err := OpenContext(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	other, err := OpenContext(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	admin := createAliasDeletionJobTestAdmin(t, s, "pg-queue")
	first := enqueueDeletionFixture(t, s, admin, "first", deletionTarget(1, "A"), deletionTarget(2, "A"))
	second := enqueueDeletionFixture(t, other, admin, "second", deletionTarget(1, "A"), deletionTarget(3, "B"))
	if first.Items[0].WorkID != second.Items[0].WorkID {
		t.Fatal("duplicate work was created")
	}
	if _, err := s.ClaimAliasDeletionWork(ctx, first.Items[1].WorkID, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FIFO was bypassed: %v", err)
	}
	work := claimDeletionFixture(t, s, first.Items[0].WorkID)
	if _, err := other.CancelAliasDeletionJob(ctx, first.ID, admin.ID); err != nil {
		t.Fatal(err)
	}
	if wanted, err := s.AliasDeletionWorkWanted(ctx, work.ID); err != nil || !wanted {
		t.Fatalf("shared cancellation: %t %v", wanted, err)
	}
	work.Status, work.Deleted = domain.AliasDeletionWorkSucceeded, true
	if err := s.SaveAliasDeletionWork(ctx, work); err != nil {
		t.Fatal(err)
	}
	remaining := claimDeletionFixture(t, other, second.Items[1].WorkID)
	if err := s.RecoverAliasDeletionQueue(ctx); err != nil {
		t.Fatal(err)
	}
	recovered := claimDeletionFixture(t, s, remaining.ID)
	if !recovered.Reconcile || recovered.ClaimToken == remaining.ClaimToken {
		t.Fatal("restart lost reconciliation fencing")
	}
	remaining.Status = domain.AliasDeletionWorkSucceeded
	if err := other.SaveAliasDeletionWork(ctx, remaining); !errors.Is(err, ErrAliasDeletionJobConflict) {
		t.Fatalf("stale claim accepted: %v", err)
	}

	// Distinct pooled connections contend on the same PostgreSQL principal row.
	now := time.Now().UTC()
	var accepted atomic.Int32
	var tasks sync.WaitGroup
	for index := range 220 {
		tasks.Go(func() {
			db := s
			if index%2 == 0 {
				db = other
			}
			quota, err := db.ReserveAliasDeletionQuota(ctx, "pg-principal", fmt.Sprint(index), now)
			if err != nil {
				t.Error(err)
				return
			}
			if quota.RetryAt.IsZero() {
				accepted.Add(1)
			}
		})
	}
	tasks.Wait()
	if accepted.Load() != 200 {
		t.Fatalf("PostgreSQL admitted %d of 220 requests", accepted.Load())
	}
	if err := s.MarkAliasDeletionQuotaSent(ctx, "pg-principal", "0", now); err != nil {
		// Reservation 0 may be among the rejected requests; reserve on a fresh
		// principal below to exercise the sending transition deterministically.
		if !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
	if _, err := s.ReserveAliasDeletionQuota(ctx, "sent-principal", "sent", now); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAliasDeletionQuotaSent(ctx, "sent-principal", "sent", now); err != nil {
		t.Fatal(err)
	}
	if err := other.ReleaseAliasDeletionQuota(ctx, "sent-principal", "sent"); err != nil {
		t.Fatal(err)
	}
	quota, err := s.GetAliasDeletionQuota(ctx, "sent-principal", now.Add(90*time.Minute))
	if err != nil || quota.Used != 1 {
		t.Fatalf("sent checkpoint was lost: %+v %v", quota, err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("repeat PostgreSQL migration: %v", err)
	}
}
