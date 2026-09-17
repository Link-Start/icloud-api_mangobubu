package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestFreshAutoAliasKeepsCursorAndArchivesNextUID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, _ := openArchiveV2Store(t, 1<<20)
	account := createAccount(t, ctx, db, "Fresh automatic alias", "fresh-cursor@icloud.com")
	existing := createAlias(t, ctx, db, account.ID, "existing-fresh@icloud.com", bytes.Repeat([]byte{0x71}, 32))
	observed := time.Date(2026, 9, 11, 7, 0, 0, 0, time.UTC)
	existingRaw := []byte("To: existing-fresh@icloud.com\r\nSubject: already archived\r\n\r\noriginal body")
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{existing}, []domain.ArchivedMessage{{
		AccountID: account.ID, UIDValidity: 1, UID: 352, InternalDate: observed,
		Subject: "already archived", RawMIME: existingRaw, AliasIDs: []int64{existing.ID},
	}}, 1, 352, true)
	before, err := db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	cursorBefore, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending, session := createPendingFreshCursorAlias(t, ctx, db, before)
	afterCandidate, err := db.GetAccount(ctx, account.ID)
	if err != nil || !afterCandidate.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("disabled candidate changed account version: before=%v after=%v err=%v", before.UpdatedAt, afterCandidate.UpdatedAt, err)
	}
	session.Ciphertext = "as1.confirmed-fresh-cursor"
	confirmed, savedSession, err := db.ConfirmFreshAutoAlias(ctx, session, pending.ID, before.UpdatedAt)
	if err != nil {
		t.Fatalf("confirm fresh automatic alias: %v", err)
	}
	if !confirmed.Enabled || confirmed.LastSyncStatus != domain.SyncStatusPending || confirmed.LastSyncError != "" {
		t.Fatalf("fresh confirmation did not publish the alias: %#v", confirmed)
	}
	if savedSession.Ciphertext != session.Ciphertext {
		t.Fatal("fresh confirmation did not persist the rotated session")
	}
	after, err := db.GetAccount(ctx, account.ID)
	if err != nil || !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("fresh confirmation did not advance account version: before=%v after=%v err=%v", before.UpdatedAt, after.UpdatedAt, err)
	}
	cursor, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil || cursor != cursorBefore {
		t.Fatalf("fresh confirmation changed cursor: before=%#v after=%#v err=%v", cursorBefore, cursor, err)
	}

	// Even an otherwise current alias set cannot publish with the version from
	// before confirmation. Also cover the actual pre-confirmation alias set.
	for _, aliases := range [][]domain.Alias{{existing}, {existing, confirmed}} {
		err = db.ApplyMailboxSync(ctx, account.ID, before.UpdatedAt, aliases, domain.MailboxSyncResult{
			ArchivedMessages: []domain.ArchivedMessage{{
				AccountID: account.ID, UIDValidity: 1, UID: 354, InternalDate: observed.Add(time.Minute),
				Subject: "stale result", ContentState: domain.ArchiveContentMetadata, AliasIDs: []int64{existing.ID},
			}},
			State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1, LastUID: 354, UpdatedAt: observed.Add(time.Minute)},
		}, observed.Add(time.Minute))
		if err != nil {
			t.Fatalf("discard stale mailbox result: %v", err)
		}
		cursor, err = db.GetIMAPSyncState(ctx, account.ID)
		if err != nil || cursor != cursorBefore {
			t.Fatalf("stale mailbox result overwrote preserved cursor: %#v, %v", cursor, err)
		}
	}
	stillPending, err := db.GetAlias(ctx, confirmed.ID)
	if err != nil || stillPending.LastSyncStatus != domain.SyncStatusPending {
		t.Fatalf("stale mailbox result marked new alias healthy: %#v, %v", stillPending, err)
	}

	raw := []byte("From: sender@example.test\r\nTo: new-fresh-cursor@icloud.com\r\nSubject: first new message\r\n\r\nnew body")
	err = db.ApplyMailboxSync(ctx, account.ID, after.UpdatedAt, []domain.Alias{existing, confirmed}, domain.MailboxSyncResult{
		ArchivedMessages: []domain.ArchivedMessage{{
			AccountID: account.ID, UIDValidity: 1, UID: 353, InternalDate: observed.Add(2 * time.Minute),
			Subject: "first new message", RawMIME: raw, AliasIDs: []int64{confirmed.ID},
		}},
		State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 1, LastUID: 353, UpdatedAt: observed.Add(2 * time.Minute)},
	}, observed.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("archive increment after fresh confirmation: %v", err)
	}
	cursor, err = db.GetIMAPSyncState(ctx, account.ID)
	if err != nil || cursor.UIDValidity != 1 || cursor.LastUID != 353 {
		t.Fatalf("increment cursor = %#v, err=%v", cursor, err)
	}
	newMail := oneArchivedMailboxMessage(t, ctx, db, confirmed.ID)
	if newMail.MailboxUID != 1 || newMail.Subject != "first new message" {
		t.Fatalf("new alias lost its first incremental message: %#v", newMail)
	}
	if content, err := db.ReadArchivedContent(newMail); err != nil || !bytes.Equal(content, raw) {
		t.Fatalf("new alias MIME = %q, err=%v", content, err)
	}
	oldMail := oneArchivedMailboxMessage(t, ctx, db, existing.ID)
	if oldMail.MailboxUID != 1 || oldMail.Subject != "already archived" {
		t.Fatalf("confirmation/stale sync changed existing archive: %#v", oldMail)
	}
	if content, err := db.ReadArchivedContent(oldMail); err != nil || !bytes.Equal(content, existingRaw) {
		t.Fatalf("existing alias MIME = %q, err=%v", content, err)
	}
	for aliasID, wantUID := range map[int64]uint32{existing.ID: 352, confirmed.ID: 353} {
		latest, err := db.GetLatestMessage(ctx, aliasID)
		if err != nil || latest.UIDValidity != 1 || latest.UID != wantUID {
			t.Fatalf("alias %d upstream projection = %#v, err=%v, want UID %d", aliasID, latest, err, wantUID)
		}
	}
}

func TestAutoAliasConfirmationCursorSafetyBoundaries(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name             string
		seedCursor       bool
		advanceVersion   bool
		zeroVersion      bool
		traditional      bool
		wantCursorExists bool
	}{
		{name: "fresh matching version preserves cursor", seedCursor: true, wantCursorExists: true},
		{name: "fresh missing cursor remains missing"},
		{name: "changed account version resets cursor", seedCursor: true, advanceVersion: true},
		{name: "zero expected version resets cursor", seedCursor: true, zeroVersion: true},
		{name: "traditional pending confirmation resets cursor", seedCursor: true, traditional: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := openTestStore(t)
			account := createAccount(t, ctx, db, "Confirmation boundary", "confirmation-boundary@icloud.com")
			existing := createAlias(t, ctx, db, account.ID, "existing-boundary@icloud.com", bytes.Repeat([]byte{0x72}, 32))
			if test.seedCursor {
				applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{existing}, nil, 1, 352, true)
			}
			before, err := db.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			pending, session := createPendingFreshCursorAlias(t, ctx, db, before)
			expected := before.UpdatedAt
			if test.advanceVersion {
				// A concurrent completed sync advances both the account version and
				// cursor while the disabled candidate remains unpublished.
				applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{existing}, nil, 1, 353, false)
			}
			if test.zeroVersion {
				expected = time.Time{}
			}
			beforeConfirmation, err := db.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			var confirmed domain.Alias
			if test.traditional {
				confirmed, _, err = db.ConfirmPendingAutoAlias(ctx, session, pending.ID)
			} else {
				confirmed, _, err = db.ConfirmFreshAutoAlias(ctx, session, pending.ID, expected)
			}
			if err != nil || !confirmed.Enabled {
				t.Fatalf("confirmation = %#v, err=%v", confirmed, err)
			}
			after, err := db.GetAccount(ctx, account.ID)
			if err != nil || !after.UpdatedAt.After(beforeConfirmation.UpdatedAt) {
				t.Fatalf("confirmation version = %v, before=%v err=%v", after.UpdatedAt, beforeConfirmation.UpdatedAt, err)
			}
			cursor, err := db.GetIMAPSyncState(ctx, account.ID)
			if test.wantCursorExists {
				if err != nil || cursor.UIDValidity != 1 || cursor.LastUID != 352 {
					t.Fatalf("preserved cursor = %#v, err=%v", cursor, err)
				}
			} else if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("cursor = %#v, err=%v, want ErrNotFound", cursor, err)
			}
		})
	}
}

func createPendingFreshCursorAlias(t *testing.T, ctx context.Context, db *store.Store, account domain.Account) (domain.Alias, domain.AppleWebSession) {
	t.Helper()
	session := domain.AppleWebSession{
		AccountID: account.ID, Ciphertext: "as1.pending-fresh-cursor", AppleID: account.Email,
		Region: "global", Authenticated: true,
	}
	alias, _, err := db.CreateAliasWithPendingAPIKey(ctx, session, domain.Alias{
		AccountID: account.ID, Address: "new-fresh-cursor@icloud.com", Enabled: false,
		APIKeyHash: bytes.Repeat([]byte{0x73}, 32), APIKeyPrefix: "fresh-key",
	}, "encrypted-pending-key")
	if err != nil {
		t.Fatalf("create disabled automatic alias: %v", err)
	}
	return alias, session
}
