package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"icloud-api/internal/domain"
)

func openAliasDeletionQuotaTestStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func reserveAliasDeletionQuotaForTest(t *testing.T, s *Store, principal, id string, now time.Time) domain.AliasDeletionQuota {
	t.Helper()
	quota, err := s.ReserveAliasDeletionQuota(context.Background(), principal, id, now)
	if err != nil {
		t.Fatal(err)
	}
	if !quota.RetryAt.IsZero() {
		t.Fatalf("unexpected quota wait: %+v", quota)
	}
	return quota
}

func commitAliasDeletionQuotaForTest(t *testing.T, s *Store, principal, id string, now time.Time) {
	t.Helper()
	if err := s.CommitAliasDeletionQuota(context.Background(), principal, id, now); err != nil {
		t.Fatal(err)
	}
}

func getAliasDeletionQuotaForTest(t *testing.T, s *Store, principal string, now time.Time) domain.AliasDeletionQuota {
	t.Helper()
	quota, err := s.GetAliasDeletionQuota(context.Background(), principal, now)
	if err != nil {
		t.Fatal(err)
	}
	if quota.Limit != 200 {
		t.Fatalf("limit = %d, want 200", quota.Limit)
	}
	return quota
}

func fillAliasDeletionQuotaForTest(t *testing.T, s *Store, principal string, now time.Time) {
	t.Helper()
	for i := range 200 {
		id := fmt.Sprintf("deletion-%d", i)
		reserveAliasDeletionQuotaForTest(t, s, principal, id, now)
		commitAliasDeletionQuotaForTest(t, s, principal, id, now)
	}
}

func TestAliasDeletionQuotaConcurrentReservationsAcrossStores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "quota.db")
	stores := []*Store{
		openAliasDeletionQuotaTestStore(t, path),
		openAliasDeletionQuotaTestStore(t, path),
	}
	now := time.Date(2026, 9, 19, 8, 30, 0, 0, time.UTC)
	type result struct {
		quota domain.AliasDeletionQuota
		err   error
	}
	results := make(chan result, 220)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range 220 {
		workers.Go(func() {
			<-start
			s := stores[i%len(stores)]
			id := fmt.Sprintf("concurrent-%d", i)
			quota, err := s.ReserveAliasDeletionQuota(ctx, "principal-a", id, now)
			if err == nil && quota.RetryAt.IsZero() {
				err = s.CommitAliasDeletionQuota(ctx, "principal-a", id, now)
			}
			results <- result{quota: quota, err: err}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	admitted, delayed := 0, 0
	for result := range results {
		if result.err != nil {
			t.Errorf("concurrent reservation: %v", result.err)
			continue
		}
		if result.quota.RetryAt.IsZero() {
			admitted++
		} else {
			delayed++
		}
	}
	if admitted != 200 || delayed != 20 {
		t.Fatalf("admitted=%d delayed=%d, want 200/20", admitted, delayed)
	}
	quota := getAliasDeletionQuotaForTest(t, stores[0], "principal-a", now)
	if quota.Used != 200 || !quota.RetryAt.Equal(now.Add(aliasDeletionQuotaWindow)) {
		t.Fatalf("shared quota = %+v", quota)
	}
	var count int
	if err := stores[1].db.QueryRow(`SELECT COUNT(*) FROM alias_deletion_quota_reservations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 200 {
		t.Fatalf("reservation rows = %d, want 200", count)
	}
}

func TestAliasDeletionQuotaUsesRollingWindowAndIndependentPrincipals(t *testing.T) {
	t.Parallel()
	s := openAliasDeletionQuotaTestStore(t, ":memory:")
	now := time.Date(2026, 9, 19, 8, 59, 50, 0, time.UTC)
	fillAliasDeletionQuotaForTest(t, s, "principal-a", now)
	for _, at := range []time.Time{now, now.Add(10 * time.Second), now.Add(time.Hour), now.Add(aliasDeletionQuotaWindow - time.Nanosecond)} {
		quota, err := s.ReserveAliasDeletionQuota(context.Background(), "principal-a", "overflow", at)
		if err != nil {
			t.Fatal(err)
		}
		if quota.Used != 200 || !quota.RetryAt.Equal(now.Add(aliasDeletionQuotaWindow)) {
			t.Fatalf("quota at %v = %+v; the hour boundary must not reset it", at, quota)
		}
	}
	// Distinct local tasks and entry points may retry the same reservation even
	// when it owns the last slot, without charging twice.
	quota := reserveAliasDeletionQuotaForTest(t, s, "principal-a", "deletion-0", now)
	if quota.Used != 200 {
		t.Fatalf("idempotent quota = %+v", quota)
	}
	quota = reserveAliasDeletionQuotaForTest(t, s, "principal-b", "deletion-0", now)
	if quota.Used != 1 {
		t.Fatalf("independent principal quota = %+v", quota)
	}
	quota = reserveAliasDeletionQuotaForTest(t, s, "principal-a", "overflow", now.Add(aliasDeletionQuotaWindow))
	if quota.Used != 1 {
		t.Fatalf("quota after rolling expiry = %+v", quota)
	}
}

func TestAliasDeletionQuotaPersistsReservationsAndCooldownAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.db")
	s := openAliasDeletionQuotaTestStore(t, path)
	now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	reserveAliasDeletionQuotaForTest(t, s, "principal", "sent", now)
	commitAliasDeletionQuotaForTest(t, s, "principal", "sent", now)
	reserveAliasDeletionQuotaForTest(t, s, "principal", "reserved", now)
	until := now.Add(90 * time.Minute)
	if err := s.DeferAliasDeletionQuota(ctx, "principal", until); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAliasDeletionQuotaTestStore(t, path)
	quota := getAliasDeletionQuotaForTest(t, s, "principal", now.Add(30*time.Minute))
	if quota.Used != 2 || !quota.RetryAt.Equal(until) {
		t.Fatalf("quota after restart = %+v", quota)
	}
	if err := s.ReleaseAliasDeletionQuota(ctx, "principal", "sent"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseAliasDeletionQuota(ctx, "principal", "reserved"); err != nil {
		t.Fatal(err)
	}
	quota = getAliasDeletionQuotaForTest(t, s, "principal", now.Add(30*time.Minute))
	if quota.Used != 1 || !quota.RetryAt.Equal(until) {
		t.Fatalf("release must preserve sent deletion and cooldown: %+v", quota)
	}
}

func TestAliasDeletionQuotaCooldownOnlyExtendsAndDoesNotReserve(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openAliasDeletionQuotaTestStore(t, ":memory:")
	now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	for _, delay := range []time.Duration{5 * time.Minute, 30 * time.Minute, 10 * time.Minute} {
		if err := s.DeferAliasDeletionQuota(ctx, "principal", now.Add(delay)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeferAliasDeletionQuota(ctx, "principal", time.Time{}); err != nil {
		t.Fatal(err)
	}
	quota, err := s.ReserveAliasDeletionQuota(ctx, "principal", "deferred", now)
	if err != nil {
		t.Fatal(err)
	}
	if quota.Used != 0 || !quota.RetryAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("deferred quota = %+v", quota)
	}
	quota = reserveAliasDeletionQuotaForTest(t, s, "principal", "deferred", now.Add(30*time.Minute))
	if quota.Used != 1 {
		t.Fatalf("resumed quota = %+v", quota)
	}
	if err := s.DeferAliasDeletionQuota(ctx, "principal", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	quota, err = s.ReserveAliasDeletionQuota(ctx, "principal", "deferred", now.Add(30*time.Minute))
	if err != nil || quota.Used != 1 || !quota.RetryAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("existing reservation must respect upstream cooldown: quota=%+v err=%v", quota, err)
	}
}

func TestAliasDeletionQuotaRetryAtUsesLaterOfWindowAndCooldown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openAliasDeletionQuotaTestStore(t, ":memory:")
	now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	fillAliasDeletionQuotaForTest(t, s, "principal", now)
	for _, delay := range []time.Duration{5 * time.Minute, 2 * time.Hour} {
		if err := s.DeferAliasDeletionQuota(ctx, "principal", now.Add(delay)); err != nil {
			t.Fatal(err)
		}
		quota, err := s.ReserveAliasDeletionQuota(ctx, "principal", "overflow", now)
		if err != nil {
			t.Fatal(err)
		}
		if !quota.RetryAt.Equal(now.Add(max(aliasDeletionQuotaWindow, delay))) {
			t.Fatalf("RetryAt = %v for cooldown %v", quota.RetryAt, delay)
		}
	}
}

func TestAliasDeletionQuotaCommitExtendsFromSendAndCompletion(t *testing.T) {
	t.Parallel()
	s := openAliasDeletionQuotaTestStore(t, ":memory:")
	now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	reserveAliasDeletionQuotaForTest(t, s, "principal", "deletion", now)
	sentAt := now.Add(5 * time.Minute)
	if err := s.MarkAliasDeletionQuotaSent(context.Background(), "principal", "deletion", sentAt); err != nil {
		t.Fatal(err)
	}
	completedAt := now.Add(7 * time.Minute)
	commitAliasDeletionQuotaForTest(t, s, "principal", "deletion", completedAt)
	// An earlier retry timestamp must not shorten a previously committed event.
	commitAliasDeletionQuotaForTest(t, s, "principal", "deletion", sentAt)
	for _, at := range []time.Time{now.Add(aliasDeletionQuotaWindow), sentAt.Add(aliasDeletionQuotaWindow), completedAt.Add(aliasDeletionQuotaWindow - time.Nanosecond)} {
		if quota := getAliasDeletionQuotaForTest(t, s, "principal", at); quota.Used != 1 {
			t.Fatalf("sent/completed request expired early at %v: %+v", at, quota)
		}
	}
	if quota := getAliasDeletionQuotaForTest(t, s, "principal", completedAt.Add(aliasDeletionQuotaWindow)); quota.Used != 0 {
		t.Fatalf("completed request did not expire: %+v", quota)
	}
	if err := s.CommitAliasDeletionQuota(context.Background(), "principal", "missing", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("commit missing reservation = %v, want ErrNotFound", err)
	}
}

func TestAliasDeletionQuotaSentCheckpointSurvivesCrashDuringRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sent-crash.db")
	s := openAliasDeletionQuotaTestStore(t, path)
	now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	reserveAliasDeletionQuotaForTest(t, s, "principal", "in-flight", now)
	sentAt := now.Add(10 * time.Minute)
	if err := s.MarkAliasDeletionQuotaSent(ctx, "principal", "in-flight", sentAt); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAliasDeletionQuotaTestStore(t, path)
	// Apple can finish minutes after sending while the process has already
	// crashed. At send+60m5s the normal window would expire too early; the
	// durable sent checkpoint still reserves its conservative crash allowance.
	if err := s.ReleaseAliasDeletionQuota(ctx, "principal", "in-flight"); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{sentAt.Add(aliasDeletionQuotaWindow), sentAt.Add(aliasDeletionReservationLifetime - time.Nanosecond)} {
		quota := getAliasDeletionQuotaForTest(t, s, "principal", at)
		if quota.Used != 1 {
			t.Fatalf("in-flight request lost accounting after crash: at=%v quota=%+v", at, quota)
		}
	}
	if quota := getAliasDeletionQuotaForTest(t, s, "principal", sentAt.Add(aliasDeletionReservationLifetime)); quota.Used != 0 {
		t.Fatalf("abandoned sent request consumed quota forever: %+v", quota)
	}
}

func TestAliasDeletionQuotaSentCheckpointRejectsExpiredOrCommittedReservation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openAliasDeletionQuotaTestStore(t, ":memory:")
	now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	reserveAliasDeletionQuotaForTest(t, s, "principal", "expired", now)
	if err := s.MarkAliasDeletionQuotaSent(ctx, "principal", "expired", now.Add(aliasDeletionReservationLifetime)); err == nil {
		t.Fatal("expired reservation permitted sending without reacquiring capacity")
	}
	reserveAliasDeletionQuotaForTest(t, s, "principal", "completed", now)
	commitAliasDeletionQuotaForTest(t, s, "principal", "completed", now)
	if err := s.MarkAliasDeletionQuotaSent(ctx, "principal", "completed", now.Add(time.Minute)); err == nil {
		t.Fatal("committed reservation permitted replay")
	}
	if err := s.MarkAliasDeletionQuotaSent(ctx, "principal", "missing", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing sent reservation = %v, want ErrNotFound", err)
	}
	reserveAliasDeletionQuotaForTest(t, s, "principal", "sent", now)
	if err := s.MarkAliasDeletionQuotaSent(ctx, "principal", "sent", now); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAliasDeletionQuotaSent(ctx, "principal", "sent", now.Add(aliasDeletionReservationLifetime)); err == nil {
		t.Fatal("expired sent reservation permitted replay without capacity")
	}
}

func TestAliasDeletionQuotaAbandonedReservationExpiresConservatively(t *testing.T) {
	t.Parallel()
	s := openAliasDeletionQuotaTestStore(t, ":memory:")
	now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	reserveAliasDeletionQuotaForTest(t, s, "principal", "crashed", now)
	for _, at := range []time.Time{now.Add(aliasDeletionQuotaWindow), now.Add(aliasDeletionReservationLifetime - time.Nanosecond)} {
		if quota := getAliasDeletionQuotaForTest(t, s, "principal", at); quota.Used != 1 {
			t.Fatalf("abandoned reservation expired before conservative lease: %+v", quota)
		}
	}
	expiredAt := now.Add(aliasDeletionReservationLifetime)
	if quota := getAliasDeletionQuotaForTest(t, s, "principal", expiredAt); quota.Used != 0 {
		t.Fatalf("abandoned reservation consumed quota permanently: %+v", quota)
	}
	quota := reserveAliasDeletionQuotaForTest(t, s, "principal", "crashed", expiredAt)
	if quota.Used != 1 {
		t.Fatalf("expired reservation was not reacquired: %+v", quota)
	}
	quota = reserveAliasDeletionQuotaForTest(t, s, "principal", "crashed", expiredAt.Add(time.Second))
	if quota.Used != 1 {
		t.Fatalf("live retry charged twice: %+v", quota)
	}
	if err := s.ReleaseAliasDeletionQuota(context.Background(), "principal", "crashed"); err != nil {
		t.Fatal(err)
	}
	quota = getAliasDeletionQuotaForTest(t, s, "principal", expiredAt)
	if quota.Used != 0 {
		t.Fatalf("unsent reservation not released: %+v", quota)
	}
}

func TestAliasDeletionQuotaExpiredReservationRechecksCapacity(t *testing.T) {
	t.Parallel()
	s := openAliasDeletionQuotaTestStore(t, ":memory:")
	now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	reserveAliasDeletionQuotaForTest(t, s, "principal", "crashed", now)
	now = now.Add(aliasDeletionReservationLifetime)
	fillAliasDeletionQuotaForTest(t, s, "principal", now)
	quota, err := s.ReserveAliasDeletionQuota(context.Background(), "principal", "crashed", now)
	if err != nil || quota.Used != 200 || !quota.RetryAt.Equal(now.Add(aliasDeletionQuotaWindow)) {
		t.Fatalf("expired retry bypassed quota: quota=%+v err=%v", quota, err)
	}
}

func TestAliasDeletionQuotaGetDoesNotWriteAndKeysAreRequired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openAliasDeletionQuotaTestStore(t, ":memory:")
	now := time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC)
	if quota := getAliasDeletionQuotaForTest(t, s, "unknown", now); quota.Used != 0 || !quota.RetryAt.IsZero() {
		t.Fatalf("unknown principal quota = %+v", quota)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM alias_deletion_quota_principals`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("read created principal row: count=%d err=%v", count, err)
	}
	if _, err := s.ReserveAliasDeletionQuota(ctx, " ", "reservation", now); err == nil {
		t.Fatal("blank principal accepted")
	}
	if _, err := s.ReserveAliasDeletionQuota(ctx, "principal", " ", now); err == nil {
		t.Fatal("blank reservation ID accepted")
	}
	if _, err := s.GetAliasDeletionQuota(ctx, "", now); err == nil {
		t.Fatal("blank read principal accepted")
	}
	if err := s.ReleaseAliasDeletionQuota(ctx, "", "reservation"); err == nil {
		t.Fatal("blank release principal accepted")
	}
	if err := s.CommitAliasDeletionQuota(ctx, "principal", "", now); err == nil {
		t.Fatal("blank commit reservation ID accepted")
	}
	if err := s.DeferAliasDeletionQuota(ctx, "", now); err == nil {
		t.Fatal("blank deferred principal accepted")
	}
}

func TestAliasDeletionQuotaPostgresConvergence(t *testing.T) {
	t.Parallel()
	for _, version := range []int{0, 3, 4, 5, 6, 7, 8} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			t.Parallel()
			capture := &postgresMigrationCaptureDriver{version: version}
			driverName := fmt.Sprintf("alias-quota-postgres-%p", capture)
			sql.Register(driverName, capture)
			raw, err := sql.Open(driverName, "")
			if err != nil {
				t.Fatal(err)
			}
			raw.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = raw.Close() })
			s := newStore(raw, dialectPostgres)
			if err := s.Migrate(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, statement := range quotaSchemaStatements {
				if !containsNormalizedSQL(capture.statements, statement) {
					t.Fatalf("quota schema omitted on PostgreSQL v%d migration: %s", version, statement)
				}
			}
		})
	}
}
