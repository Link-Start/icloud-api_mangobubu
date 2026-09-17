package hmesync

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/apple"
	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

type recoveryRepository struct {
	*fakeRepository
	keyCreatedAt time.Time
	discardErr   error
	onDiscard    func()
	discardCalls int
}

func (r *recoveryRepository) GetPendingAutoAliasConfirmation(ctx context.Context, accountID int64) (domain.PendingAliasAPIKey, error) {
	pending, err := r.fakeRepository.GetPendingAutoAliasConfirmation(ctx, accountID)
	pending.CreatedAt = r.keyCreatedAt
	return pending, err
}

func (r *recoveryRepository) DiscardPendingAutoAlias(ctx context.Context, accountID, aliasID int64) error {
	r.discardCalls++
	if r.onDiscard != nil {
		r.onDiscard()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.discardErr != nil {
		return r.discardErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil || r.pending.AccountID != accountID || r.pending.ID != aliasID ||
		r.pending.Enabled || r.pending.LastSyncError != domain.AppleAliasConfirmationPending {
		return store.ErrNotFound
	}
	r.pending = nil
	return nil
}

func newRecoveryFixture(t *testing.T, locker AccountLocker) (*Service, *recoveryRepository, *fakeAppleClient, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 17, 5, 0, 0, 0, time.UTC)
	base := newFakeRepository(domain.Account{ID: 3, Email: "primary@icloud.com", Enabled: true}, now)
	base.pending = &domain.Alias{
		ID: 77, AccountID: 3, Address: "missing-candidate@icloud.com", Enabled: false,
		LastSyncError: domain.AppleAliasConfirmationPending, CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now, // A recent unrelated write must not extend the grace period.
	}
	repo := &recoveryRepository{fakeRepository: base}
	client := &fakeAppleClient{
		validate: func(_ context.Context, session apple.Session) (apple.Session, error) { return session, nil },
		list: func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			session.SessionToken = "latest-directory-session"
			return aliasDeletionDirectory(), session, nil
		},
		create: func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
			return apple.Alias{HME: "next-created@icloud.com", IsActive: true, ForwardToEmail: "primary@icloud.com"}, session, nil
		},
	}
	service := newTestService(t, repo, client, locker, func() time.Time { return now })
	service.autoCreateConfirmationDelays = []time.Duration{0, 0, 0}
	storeSession(t, service, base, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal})
	return service, repo, client, &now
}

func assertRecoveryPending(t *testing.T, err error, want bool) {
	t.Helper()
	var marker interface{ PendingConfirmation() bool }
	got := errors.As(err, &marker) && marker.PendingConfirmation()
	if got != want {
		t.Fatalf("pending marker = %v, want %v; err=%v", got, want, err)
	}
}

func TestAutoAliasRecoveryDiscardsLocallyAndNextAttemptCreates(t *testing.T) {
	for _, acquiring := range []bool{false, true} {
		name := "publication lock"
		var locker AccountLocker = &fakeLocker{}
		if acquiring {
			name = "non-reentrant account lock"
			locker = newFakeAcquiringLocker()
		}
		t.Run(name, func(t *testing.T) {
			service, repo, client, _ := newRecoveryFixture(t, locker)
			repo.onDiscard = func() {
				switch lock := locker.(type) {
				case *fakeLocker:
					if lock.held.Load() != 1 {
						t.Fatal("discard ran outside the publication lock")
					}
				case *fakeAcquiringLocker:
					if len(lock.token) != 0 {
						t.Fatal("discard ran outside the account lock")
					}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			alias, err := service.CreateAutoAlias(ctx, 3)
			if Code(err) != CodeAliasCandidateDiscarded || !errors.Is(err, ErrAliasCandidateDiscarded) || alias.ID != 0 {
				t.Fatalf("discard result = %#v, err=%v code=%s", alias, err, Code(err))
			}
			assertRecoveryPending(t, err, false)
			if repo.pending != nil || repo.discardCalls != 1 || client.createCalls.Load() != 0 ||
				client.deactivateCalls.Load() != 0 || client.deleteCalls.Load() != 0 {
				t.Fatal("discard retained the candidate or performed a remote mutation")
			}
			assertStoredAppleSessionToken(t, service, repo.fakeRepository, 3, "latest-directory-session")
			created, err := service.CreateAutoAlias(ctx, 3)
			if err != nil || !created.Enabled || created.Address != "next-created@icloud.com" ||
				client.createCalls.Load() != 1 || repo.discardCalls != 1 || repo.confirms.Load() != 1 {
				t.Fatalf("next attempt failed to create once: alias=%#v err=%v creates=%d discards=%d", created, err, client.createCalls.Load(), repo.discardCalls)
			}
		})
	}
}

func TestAutoAliasRecoveryGracePeriodUsesCreationNotUpdateTime(t *testing.T) {
	for _, test := range []struct {
		name       string
		aliasAge   time.Duration
		keyAge     time.Duration
		missingAge bool
		wantDrop   bool
	}{
		{name: "one instant before grace", aliasAge: 5*time.Minute - time.Nanosecond},
		{name: "exact grace boundary", aliasAge: 5 * time.Minute, wantDrop: true},
		{name: "old candidate after restart", aliasAge: 24 * time.Hour, wantDrop: true},
		{name: "future timestamp", aliasAge: -time.Minute},
		{name: "missing timestamps", missingAge: true},
		{name: "pending key creation takes precedence", aliasAge: time.Hour, keyAge: time.Minute},
		{name: "old pending key with recent alias update", aliasAge: time.Minute, keyAge: time.Hour, wantDrop: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, repo, client, now := newRecoveryFixture(t, &fakeLocker{})
			repo.pending.CreatedAt = now.Add(-test.aliasAge)
			if test.missingAge {
				repo.pending.CreatedAt = time.Time{}
			}
			if test.keyAge != 0 {
				repo.keyCreatedAt = now.Add(-test.keyAge)
			}
			_, err := service.CreateAutoAlias(context.Background(), 3)
			wantCode := CodeAliasConfirmationPending
			if test.wantDrop {
				wantCode = CodeAliasCandidateDiscarded
			}
			if Code(err) != wantCode || (repo.pending == nil) != test.wantDrop || client.createCalls.Load() != 0 {
				t.Fatalf("grace result: code=%s want=%s removed=%v", Code(err), wantCode, repo.pending == nil)
			}
			assertRecoveryPending(t, err, !test.wantDrop)
		})
	}
}

func TestAutoAliasRecoveryRetainsCandidateOnFailure(t *testing.T) {
	for _, name := range []string{
		"directory 503", "directory 429", "directory malformed", "validation 503", "expired session",
		"directory DSID changed", "directory Apple ID changed", "mailbox mismatch", "duplicate directory",
		"checkpoint failure", "discard failure", "published before discard", "account changed", "account disabled", "cancelled",
	} {
		t.Run(name, func(t *testing.T) {
			service, repo, client, _ := newRecoveryFixture(t, &fakeLocker{})
			wantCode := CodeUpstreamError
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			directory := aliasDeletionDirectory()
			var listErr error
			client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
				switch name {
				case "directory DSID changed":
					session.DSID = "other-dsid"
				case "directory Apple ID changed":
					session.AppleID = "another-owner@example.com"
				case "account changed":
					repo.setAccountEmail("changed@icloud.com")
				case "account disabled":
					repo.account.Enabled = false
				case "cancelled":
					cancel()
				}
				return directory, session, listErr
			}
			switch name {
			case "directory 503":
				listErr = &apple.Error{Kind: apple.ErrService, StatusCode: 503, Retryable: true}
			case "directory 429":
				listErr = &apple.Error{Kind: apple.ErrService, StatusCode: 429, Retryable: true}
				wantCode = CodeRateLimited
			case "directory malformed":
				listErr = &apple.Error{Kind: apple.ErrInvalidResponse, StatusCode: 200}
			case "validation 503":
				client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
					return session, &apple.Error{Kind: apple.ErrService, StatusCode: 503, Retryable: true}
				}
			case "expired session":
				client.validate = func(_ context.Context, session apple.Session) (apple.Session, error) {
					return session, apple.ErrInvalidSession
				}
				wantCode = CodeSessionExpired
			case "directory DSID changed", "directory Apple ID changed", "mailbox mismatch":
				wantCode = CodeAccountMismatch
				if name == "mailbox mismatch" {
					directory = apple.ListResult{SelectedForwardTo: "other@example.com"}
				}
			case "duplicate directory":
				directory.Aliases = []apple.Alias{{HME: "duplicate@icloud.com"}, {HME: "duplicate@icloud.com"}}
			case "checkpoint failure":
				repo.upsertSessionErr = errors.New("checkpoint database failure")
				wantCode = CodePersistenceError
			case "discard failure":
				repo.discardErr = errors.New("discard database failure")
				wantCode = CodePersistenceError
			case "published before discard":
				repo.onDiscard = func() { repo.pending.Enabled = true }
				wantCode = CodePersistenceError
			case "account changed":
				wantCode = CodeAccountChanged
			case "account disabled":
				wantCode = CodeAccountDisabled
			case "cancelled":
				wantCode = ""
			}
			_, err := service.CreateAutoAlias(ctx, 3)
			if Code(err) != wantCode || repo.pending == nil || client.createCalls.Load() != 0 ||
				client.deactivateCalls.Load() != 0 || client.deleteCalls.Load() != 0 {
				t.Fatalf("failure lost candidate or diagnostic: err=%v code=%s want=%s pending=%v", err, Code(err), wantCode, repo.pending != nil)
			}
			if name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error = %v", err)
			}
			assertRecoveryPending(t, err, true)
		})
	}
}

func TestAutoAliasRecoveryNeverDiscardsRemotePresentCandidate(t *testing.T) {
	for _, active := range []bool{false, true} {
		service, repo, client, _ := newRecoveryFixture(t, &fakeLocker{})
		client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{Aliases: []apple.Alias{{
				HME: repo.pending.Address, IsActive: active, ForwardToEmail: "primary@icloud.com",
			}}}, session, nil
		}
		alias, err := service.CreateAutoAlias(context.Background(), 3)
		if active {
			if err != nil || !alias.Enabled || repo.confirms.Load() != 1 {
				t.Fatalf("present candidate was not confirmed: alias=%#v err=%v", alias, err)
			}
		} else {
			if Code(err) != CodeAliasInactive || repo.pending == nil || repo.pending.Enabled {
				t.Fatalf("inactive candidate lost state: err=%v pending=%#v", err, repo.pending)
			}
			assertRecoveryPending(t, err, true)
		}
		if repo.discardCalls != 0 || client.createCalls.Load() != 0 || client.deleteCalls.Load() != 0 {
			t.Fatal("remote-present candidate triggered discard or another reserve")
		}
	}
}

func TestAutoAliasRecoveryProtectsNewAmbiguousReserveUntilLaterPlan(t *testing.T) {
	service, repo, client, now := newRecoveryFixture(t, &fakeLocker{})
	repo.pending = nil
	client.create = func(_ context.Context, session apple.Session, _, _ string) (apple.Alias, apple.Session, error) {
		return apple.Alias{HME: "ambiguous-candidate@icloud.com"}, session, ambiguousAliasMutationError("reserve")
	}
	listCalls := 0
	client.list = func(_ context.Context, session apple.Session) (apple.ListResult, apple.Session, error) {
		listCalls++
		return aliasDeletionDirectory(), session, nil
	}
	_, err := service.CreateAutoAlias(context.Background(), 3)
	if Code(err) != CodeAliasConfirmationPending || repo.pending == nil || repo.discardCalls != 0 ||
		client.createCalls.Load() != 1 || listCalls != 5 {
		t.Fatalf("new ambiguous candidate was not protected: err=%v reserves=%d lists=%d", err, client.createCalls.Load(), listCalls)
	}
	assertRecoveryPending(t, err, true)
	// The base fake does not assign timestamps; production storage always does.
	repo.pending.CreatedAt = *now
	*now = now.Add(5 * time.Minute)
	_, err = service.CreateAutoAlias(context.Background(), 3)
	if Code(err) != CodeAliasCandidateDiscarded || repo.pending != nil || client.createCalls.Load() != 1 {
		t.Fatalf("later plan did not retire the ambiguous candidate: err=%v pending=%v", err, repo.pending != nil)
	}
}

func TestAutoAliasRecoveryRequiresSuccessfulCompleteDirectory(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		drop bool
	}{
		{name: "complete empty directory", body: `{"success":true,"result":{"hmeEmails":[],"selectedForwardTo":"primary@icloud.com"}}`, drop: true},
		{name: "null alias array", body: `{"success":true,"result":{"hmeEmails":null}}`},
		{name: "partial directory", body: `{"success":true,"result":{"hmeEmails":[],"total":1,"selectedForwardTo":"primary@icloud.com"}}`},
		{name: "paginated directory", body: `{"success":true,"result":{"hmeEmails":[],"nextCursor":"fixture","selectedForwardTo":"primary@icloud.com"}}`},
		{name: "invalid entry", body: `{"success":true,"result":{"hmeEmails":[{"hme":"other@icloud.com","isActive":false,"forwardToEmail":""}],"selectedForwardTo":"primary@icloud.com"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, repo, _, _ := newRecoveryFixture(t, &fakeLocker{})
			requests := 0
			client, err := apple.NewClient(apple.Config{Transport: aliasDeletionRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				body := test.body
				switch request.URL.Path {
				case "/setup/ws/1/validate":
					body = `{"dsInfo":{"dsid":"42","primaryEmail":"owner@example.com","hsaVersion":2},"hsaTrustedBrowser":true,"webservices":{"premiummailsettings":{"url":"https://p01-maildomainws.icloud.com"}}}`
				case "/v2/hme/list":
				default:
					t.Fatalf("recovery performed unexpected remote mutation: %s", request.URL.Path)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			service.client = client
			storeSession(t, service, repo.fakeRepository, 3, apple.Session{AppleID: "owner@example.com", Region: apple.RegionGlobal, DSID: "42"})
			_, err = service.CreateAutoAlias(context.Background(), 3)
			wantCode := CodeUpstreamError
			if test.drop {
				wantCode = CodeAliasCandidateDiscarded
			}
			if Code(err) != wantCode || (repo.pending == nil) != test.drop || requests != 2 {
				t.Fatalf("real directory recovery: err=%v code=%s drop=%v requests=%d", err, Code(err), repo.pending == nil, requests)
			}
			assertRecoveryPending(t, err, !test.drop)
		})
	}
}
