package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"icloud-api/internal/domain"
)

func TestAliasDeletionJobClearCompletedPersistsAndPreservesResults(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "history.db")
	s := openAliasDeletionJobTestStore(t, path)
	owner := createAliasDeletionJobTestAdmin(t, s, "history-owner")
	other := createAliasDeletionJobTestAdmin(t, s, "history-other")
	var originals []domain.AliasDeletionJob
	for index, status := range []string{
		domain.AliasDeletionJobQueued, domain.AliasDeletionJobRunning, domain.AliasDeletionJobInterrupted,
		domain.AliasDeletionJobCompleted, domain.AliasDeletionJobCompleted, domain.AliasDeletionJobCompleted,
	} {
		job := aliasDeletionJobTestFixture([]string{"queued", "running", "interrupted", "succeeded", "failed", "cancelled"}[index], owner.ID)
		job.Status = status
		job.CreatedAt = job.CreatedAt.Add(time.Duration(index) * time.Second)
		job.UpdatedAt = job.CreatedAt
		for itemIndex := range job.Items {
			item := &job.Items[itemIndex]
			item.Done = status == domain.AliasDeletionJobCompleted
			if item.Done {
				item.State = []string{domain.AliasDeletionWorkSucceeded, domain.AliasDeletionWorkFailed, domain.AliasDeletionWorkCancelled}[index-3]
				item.Deleted = index == 3
			}
		}
		mustCreateAliasDeletionJob(t, s, job)
		originals = append(originals, job)
	}
	// Two administrators may use the same operation ID independently.
	otherJob := originals[3]
	otherJob.AdminID = other.ID
	mustCreateAliasDeletionJob(t, s, otherJob)
	// Simulate a pre-feature installation and verify additive convergence.
	if _, err := s.db.Exec(`DROP TABLE alias_deletion_job_clearances`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	cleared, ids, err := s.ClearCompletedAliasDeletionJobs(t.Context(), owner.ID)
	wantIDs := []string{"cancelled", "failed", "succeeded"}
	if err != nil || cleared != 3 || !slices.Equal(ids, wantIDs) {
		t.Fatalf("clear completed = %d, %v, %v", cleared, ids, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openAliasDeletionJobTestStore(t, path)
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ListAliasDeletionJobs(t.Context(), owner.ID, 1)
	if err != nil || len(jobs) != 3 {
		t.Fatalf("visible jobs after restart = %#v, %v", jobs, err)
	}
	for _, job := range jobs {
		if job.Status == domain.AliasDeletionJobCompleted {
			t.Fatalf("cleared job reappeared: %#v", job)
		}
	}
	latest, err := s.GetLatestAliasDeletionJob(t.Context(), owner.ID)
	if err != nil || latest.ID != "interrupted" {
		t.Fatalf("visible latest = %#v, %v", latest, err)
	}
	for _, job := range append(originals, otherJob) {
		assertAliasDeletionJobEqual(t, mustGetAliasDeletionJob(t, s, job), job)
		if err := s.CreateAliasDeletionJob(t.Context(), job); !errors.Is(err, ErrAliasDeletionJobConflict) {
			t.Fatalf("operation ID %s was reusable after clear: %v", job.ID, err)
		}
	}
	jobs, err = s.ListAliasDeletionJobs(t.Context(), other.ID, 20)
	if err != nil || len(jobs) != 1 || jobs[0].ID != otherJob.ID {
		t.Fatalf("other administrator's history = %#v, %v", jobs, err)
	}
	cleared, ids, err = s.ClearCompletedAliasDeletionJobs(t.Context(), owner.ID)
	if err != nil || cleared != 0 || !slices.Equal(ids, wantIDs) {
		t.Fatalf("repeat clear = %d, %v, %v", cleared, ids, err)
	}
	// A job finishing after the previous clear remains visible until cleared again.
	finished := originals[0]
	finished.Status = domain.AliasDeletionJobCompleted
	for index := range finished.Items {
		finished.Items[index].Done, finished.Items[index].Deleted = true, true
	}
	if err := s.SaveAliasDeletionJob(t.Context(), finished, nil); err != nil {
		t.Fatal(err)
	}
	jobs, err = s.ListAliasDeletionJobs(t.Context(), owner.ID, 20)
	if err != nil || !slices.ContainsFunc(jobs, func(job domain.AliasDeletionJob) bool { return job.ID == finished.ID }) {
		t.Fatalf("new completion was hidden: %#v, %v", jobs, err)
	}
	cleared, ids, err = s.ClearCompletedAliasDeletionJobs(t.Context(), owner.ID)
	if err != nil || cleared != 1 || !slices.Equal(ids, []string{"cancelled", "failed", "queued", "succeeded"}) {
		t.Fatalf("clear new completion = %d, %v, %v", cleared, ids, err)
	}
	cleared, ids, err = s.ClearCompletedAliasDeletionJobs(t.Context(), other.ID)
	if err != nil || cleared != 1 || !slices.Equal(ids, []string{otherJob.ID}) {
		t.Fatalf("clear other owner = %d, %v, %v", cleared, ids, err)
	}
	if _, err := s.GetLatestAliasDeletionJob(t.Context(), other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty latest history = %v", err)
	}
	jobs, err = s.ListAliasDeletionJobs(t.Context(), other.ID, 20)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("empty history = %#v, %v", jobs, err)
	}
}

func TestAliasDeletionJobClearCompletedPreservesSharedWorkAndQuota(t *testing.T) {
	t.Parallel()
	s := openAliasDeletionJobTestStore(t, ":memory:")
	owner := createAliasDeletionJobTestAdmin(t, s, "cleared-subscriber")
	other := createAliasDeletionJobTestAdmin(t, s, "active-subscriber")
	target := deletionTarget(1, "A")
	first := enqueueDeletionFixture(t, s, owner, "cleared", target)
	second := enqueueDeletionFixture(t, s, other, "shared", target)
	if first.Items[0].WorkID != second.Items[0].WorkID {
		t.Fatal("fixture did not share work")
	}
	cancelled, err := s.CancelAliasDeletionJob(t.Context(), first.ID, owner.ID)
	if err != nil || cancelled.Status != domain.AliasDeletionJobCompleted {
		t.Fatalf("cancelled subscription = %#v, %v", cancelled, err)
	}
	now := time.Now()
	quota, err := s.ReserveAliasDeletionQuota(t.Context(), target.AppleSubject, "kept-reservation", now)
	if err != nil {
		t.Fatal(err)
	}
	cleared, ids, err := s.ClearCompletedAliasDeletionJobs(t.Context(), owner.ID)
	if err != nil || cleared != 1 || !slices.Equal(ids, []string{first.ID}) {
		t.Fatalf("clear cancelled subscription = %d, %v, %v", cleared, ids, err)
	}
	wanted, err := s.AliasDeletionWorkWanted(t.Context(), second.Items[0].WorkID)
	if err != nil || !wanted {
		t.Fatalf("clear affected shared execution: %t, %v", wanted, err)
	}
	retry := enqueueDeletionFixture(t, s, owner, first.ID, target)
	if !reflect.DeepEqual(retry, cancelled) {
		t.Fatalf("retry changed completed subscription: %#v, want %#v", retry, cancelled)
	}
	work := claimDeletionFixture(t, s, second.Items[0].WorkID)
	work.Status, work.Deleted = domain.AliasDeletionWorkSucceeded, true
	if err := s.SaveAliasDeletionWork(t.Context(), work); err != nil {
		t.Fatal(err)
	}
	got := mustGetAliasDeletionJob(t, s, second)
	if got.Status != domain.AliasDeletionJobCompleted || !got.Items[0].Deleted {
		t.Fatalf("shared execution did not complete: %#v", got)
	}
	retained := mustGetAliasDeletionJob(t, s, first)
	if retained.Status != cancelled.Status || retained.CancelRequested != cancelled.CancelRequested || !reflect.DeepEqual(retained.Items, cancelled.Items) {
		t.Fatalf("shared result changed cleared subscription: %#v", retained)
	}
	retainedQuota, err := s.GetAliasDeletionQuota(t.Context(), target.AppleSubject, now)
	if err != nil || retainedQuota != quota {
		t.Fatalf("quota changed after clearing history: %#v, want %#v, %v", retainedQuota, quota, err)
	}
	jobs, err := s.ListAliasDeletionJobs(t.Context(), owner.ID, 20)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("retry or shared result restored cleared history: %#v, %v", jobs, err)
	}
}
