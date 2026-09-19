package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestAppleDirectoryReconciliationDeletesMissingAndPreservesSharedArchive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := openArchiveV2Store(t, 1<<20)
	configureStrictImportCredentials(t, db)
	account := createAccount(t, ctx, db, "Directory reconciliation", "directory@icloud.com")
	initial, _, err := db.ImportAliasesWithCredentials(ctx, account.ID, []domain.AliasImportCandidate{
		{Address: "missing-directory@icloud.com", Label: "Local label", Active: true},
		{Address: "inactive-directory@icloud.com", Active: true},
		{Address: "active-directory@icloud.com", Active: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	missing, inactive, active := initial.Created[0], initial.Created[1], initial.Created[2]
	group, err := db.CreateMailGroup(ctx, "Retained group")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetAliasGroup(ctx, missing.ID, &group.ID); err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC)
	raw := []byte("To: missing-directory@icloud.com, active-directory@icloud.com\r\nSubject: shared message\r\n\r\narchive retained")
	applyArchiveV2Batch(t, ctx, db, account.ID, initial.Created, []domain.ArchivedMessage{
		{AccountID: account.ID, UIDValidity: 8, UID: 11, InternalDate: observed, Subject: "shared message", RawMIME: raw, AliasIDs: []int64{missing.ID, active.ID}},
		{AccountID: account.ID, UIDValidity: 8, UID: 12, InternalDate: observed.Add(time.Minute), Subject: "missing-only message", ContentState: domain.ArchiveContentMetadata, AliasIDs: []int64{missing.ID}},
	}, 8, 12, true)
	otherAccount := createAccount(t, ctx, db, "Unrelated archive", "unrelated-directory@icloud.com")
	otherAlias := createAlias(t, ctx, db, otherAccount.ID, "unrelated-directory-alias@icloud.com", []byte("unrelated-directory-key"))
	applyArchiveV2Batch(t, ctx, db, otherAccount.ID, []domain.Alias{otherAlias}, []domain.ArchivedMessage{
		{AccountID: otherAccount.ID, UIDValidity: 8, UID: 11, InternalDate: observed, Subject: "unrelated message", RawMIME: raw, AliasIDs: []int64{otherAlias.ID}},
	}, 8, 11, true)
	otherArchiveBefore, err := db.ListArchivedMailboxMessages(ctx, otherAlias.ID)
	if err != nil || len(otherArchiveBefore) != 1 {
		t.Fatalf("unrelated archive before reconciliation: %#v, err=%v", otherArchiveBefore, err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at) VALUES(?, 8, 11, 1)`, missing.ID); err != nil {
		t.Fatal(err)
	}
	accountBefore := requireDirectoryAccount(t, ctx, db, account.ID)
	cursorBefore, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	archiveBefore, err := db.ListArchivedMailboxMessages(ctx, active.ID)
	if err != nil || len(archiveBefore) != 1 {
		t.Fatalf("shared archive before reconciliation = %d messages, err=%v", len(archiveBefore), err)
	}
	directory := []domain.AliasImportCandidate{
		{Address: active.Address, Active: true},
		{Address: inactive.Address, Active: false},
		{Address: "new-inactive-directory@icloud.com", Active: false},
	}
	result, issued, err := db.ReconcileAppleAliasesWithCredentials(ctx, account.ID, directory, directoryCandidateAddresses(directory))
	if err != nil || result.MissingCount != 1 || result.RemovedCount != 1 || result.InactiveUpdatedCount != 1 || result.RestoredCount != 0 ||
		len(result.Created) != 1 || len(issued) != 1 || len(result.Existing) != 2 {
		t.Fatalf("directory reconciliation result=%#v, issued=%d, err=%v", result, len(issued), err)
	}
	if got := result.Created[0]; got.Enabled || got.LastSyncStatus != domain.SyncStatusError || got.LastSyncError != domain.AppleAliasInactive {
		t.Fatalf("new inactive directory entry state = %#v", got)
	}
	if _, err := db.GetAlias(ctx, missing.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing alias was not deleted: %v", err)
	}
	if _, err := db.GetAliasByAPIKeyHash(ctx, missing.APIKeyHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("removed alias credential remained usable: %v", err)
	}
	logs, err := db.ListAuditLogsFiltered(ctx, store.AuditLogFilter{Action: "sync_remove", ResourceType: "alias"})
	if err != nil || len(logs) != 1 || logs[0].ResourceID != fmt.Sprint(missing.ID) || logs[0].Username != "system" || logs[0].Detail != domain.AppleAliasNotFound || logs[0].Result != "success" {
		t.Fatalf("directory removal audit=%#v, err=%v", logs, err)
	}
	for _, table := range []string{"latest_messages", "alias_messages", "consumed_messages"} {
		var count int
		if err := db.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE alias_id = ?", missing.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("removed alias retained %s records: count=%d, err=%v", table, count, err)
		}
	}
	var archiveCount int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM archived_messages WHERE account_id = ?`, account.ID).Scan(&archiveCount); err != nil || archiveCount != 2 {
		t.Fatalf("account archive records changed: count=%d, err=%v", archiveCount, err)
	}
	var groupCount int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_groups WHERE id = ?`, group.ID).Scan(&groupCount); err != nil || groupCount != 1 {
		t.Fatalf("alias removal deleted its group: count=%d, err=%v", groupCount, err)
	}
	if got := requireDirectoryAlias(t, ctx, db, inactive.ID); got.Enabled || got.LastSyncError != domain.AppleAliasInactive {
		t.Fatalf("inactive Apple entry remained routable: %#v", got)
	}
	if result.Existing[1].Enabled || result.Existing[1].LastSyncError != domain.AppleAliasInactive {
		t.Fatalf("existing result contains stale state: %#v", result.Existing)
	}
	accountAfter := requireDirectoryAccount(t, ctx, db, account.ID)
	if !accountAfter.UpdatedAt.After(accountBefore.UpdatedAt) {
		t.Fatal("routing removal did not advance account version")
	}
	if cursor, err := db.GetIMAPSyncState(ctx, account.ID); err != nil || cursor != cursorBefore {
		t.Fatalf("removal reset cursor: before=%#v after=%#v err=%v", cursorBefore, cursor, err)
	}
	archiveAfter, err := db.ListArchivedMailboxMessages(ctx, active.ID)
	if err != nil || !reflect.DeepEqual(archiveAfter, archiveBefore) {
		t.Fatalf("reconciliation changed shared archived mail: before=%#v after=%#v err=%v", archiveBefore, archiveAfter, err)
	}
	if content, err := db.ReadArchivedContent(archiveAfter[0]); err != nil || !bytes.Equal(content, raw) {
		t.Fatalf("reconciliation lost shared archived content: %q, err=%v", content, err)
	}
	otherArchiveAfter, err := db.ListArchivedMailboxMessages(ctx, otherAlias.ID)
	if err != nil || !reflect.DeepEqual(otherArchiveAfter, otherArchiveBefore) {
		t.Fatalf("reconciliation changed another account's archive: %#v, err=%v", otherArchiveAfter, err)
	}
	if content, err := db.ReadArchivedContent(otherArchiveAfter[0]); err != nil || !bytes.Equal(content, raw) {
		t.Fatalf("reconciliation lost another account's raw content: %q, err=%v", content, err)
	}
	// A sync started with the old routing set must not restore deleted aliases
	// or overwrite the new directory status.
	if err := db.ApplyMailboxSync(ctx, account.ID, accountBefore.UpdatedAt, initial.Created, domain.MailboxSyncResult{
		State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 8, LastUID: 13, UpdatedAt: observed.Add(2 * time.Minute)},
	}, observed.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAlias(ctx, missing.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale sync restored removed alias: %v", err)
	}
	if got := requireDirectoryAlias(t, ctx, db, inactive.ID); got.LastSyncError != domain.AppleAliasInactive {
		t.Fatal("stale mailbox sync overwrote directory state")
	}
	repeated, issued, err := db.ReconcileAppleAliasesWithCredentials(ctx, account.ID, directory, directoryCandidateAddresses(directory))
	if err != nil || repeated.MissingCount != 0 || repeated.RemovedCount != 0 || repeated.InactiveUpdatedCount != 0 || repeated.RestoredCount != 0 || len(issued) != 0 {
		t.Fatalf("repeated reconciliation = %#v, issued=%d, err=%v", repeated, len(issued), err)
	}
	if got := requireDirectoryAccount(t, ctx, db, account.ID); !reflect.DeepEqual(got, accountAfter) {
		t.Fatal("same directory snapshot advanced account version")
	}
	if logs, err := db.ListAuditLogsFiltered(ctx, store.AuditLogFilter{Action: "sync_remove"}); err != nil || len(logs) != 1 {
		t.Fatalf("repeated reconciliation duplicated removal audit: %#v, err=%v", logs, err)
	}
	directory[1].Active, directory[2].Active = true, true
	restored, issued, err := db.ReconcileAppleAliasesWithCredentials(ctx, account.ID, directory, directoryCandidateAddresses(directory))
	if err != nil || restored.RestoredCount != 2 || restored.RemovedCount != 0 || len(issued) != 0 {
		t.Fatalf("restored reconciliation=%#v, issued=%d, err=%v", restored, len(issued), err)
	}
	for _, aliasID := range []int64{inactive.ID, result.Created[0].ID} {
		got := requireDirectoryAlias(t, ctx, db, aliasID)
		if got.Enabled || got.LastSyncError != "" || got.LastSyncStatus != domain.SyncStatusPending {
			t.Fatalf("verified availability changed local enabled state: %#v", got)
		}
	}
	if cursor, err := db.GetIMAPSyncState(ctx, account.ID); err != nil || cursor != cursorBefore {
		t.Fatalf("availability restoration changed cursor: %#v, err=%v", cursor, err)
	}
	if got := requireDirectoryAccount(t, ctx, db, account.ID); !reflect.DeepEqual(got, accountAfter) {
		t.Fatal("availability restoration changed account routing version")
	}
	if err := db.SetAliasEnabled(ctx, inactive.ID, true); err != nil {
		t.Fatalf("enable alias after verified availability restoration: %v", err)
	}
	if _, err := db.GetIMAPSyncState(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("explicit re-enable did not reset cursor: %v", err)
	}
}

func TestAppleDirectoryReconciliationRemovesMissingPendingAndDisabledAliases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	configureStrictImportCredentials(t, db)
	account := createAccount(t, ctx, db, "Pending aliases", "pending-directory@icloud.com")
	pending, _ := createPendingFreshCursorAlias(t, ctx, db, account)
	var disabled []domain.Alias
	for _, name := range []string{"admin-active", "admin-inactive", "admin-missing", "previously-missing"} {
		alias := createAlias(t, ctx, db, account.ID, name+"@icloud.com", []byte(name+"-hash"))
		if err := db.SetAliasEnabled(ctx, alias.ID, false); err != nil {
			t.Fatal(err)
		}
		if name == "previously-missing" {
			if err := db.UpdateAliasSyncStatus(ctx, alias.ID, domain.SyncStatusError, domain.AppleAliasNotFound, nil); err != nil {
				t.Fatal(err)
			}
		}
		disabled = append(disabled, requireDirectoryAlias(t, ctx, db, alias.ID))
	}
	accountBefore := requireDirectoryAccount(t, ctx, db, account.ID)
	candidates := []domain.AliasImportCandidate{
		{Address: disabled[0].Address, Active: true},
		{Address: disabled[1].Address, Active: false},
		{Address: pending.Address, Active: true},
	}
	result, _, err := db.ReconcileAppleAliasesWithCredentials(ctx, account.ID, candidates, directoryCandidateAddresses(candidates))
	if err != nil || result.RemovedCount != 2 || result.MissingCount != 2 || result.InactiveUpdatedCount != 1 {
		t.Fatalf("disabled cleanup result=%#v, err=%v", result, err)
	}
	for _, alias := range disabled[2:] {
		if _, err := db.GetAlias(ctx, alias.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("disabled/previously missing alias remained: %v", err)
		}
	}
	if got := requireDirectoryAlias(t, ctx, db, disabled[0].ID); !reflect.DeepEqual(got, disabled[0]) {
		t.Fatal("active directory entry enabled administrator-disabled alias")
	}
	for _, active := range []bool{true, false} {
		candidates[2].Active = active
		if _, _, err := db.ReconcileAppleAliasesWithCredentials(ctx, account.ID, candidates, directoryCandidateAddresses(candidates)); err != nil {
			t.Fatal(err)
		}
		if got := requireDirectoryAlias(t, ctx, db, pending.ID); !reflect.DeepEqual(got, pending) {
			t.Fatalf("present confirmation-pending alias changed: %#v", got)
		}
	}
	candidates = candidates[:2]
	result, _, err = db.ReconcileAppleAliasesWithCredentials(ctx, account.ID, candidates, directoryCandidateAddresses(candidates))
	if err != nil || result.RemovedCount != 1 || result.MissingCount != 1 {
		t.Fatalf("pending cleanup result=%#v, err=%v", result, err)
	}
	if _, err := db.GetAlias(ctx, pending.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent pending alias remained: %v", err)
	}
	var keyCount int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_alias_api_keys WHERE alias_id = ?`, pending.ID).Scan(&keyCount); err != nil || keyCount != 0 {
		t.Fatalf("removed pending alias retained one-time key: count=%d, err=%v", keyCount, err)
	}
	accountBefore.AliasCount -= 3
	if got := requireDirectoryAccount(t, ctx, db, account.ID); !reflect.DeepEqual(got, accountBefore) {
		t.Fatal("deleting non-routable aliases changed account version")
	}
	empty, _, err := db.ReconcileAppleAliasesWithCredentials(ctx, account.ID, nil, nil)
	if err != nil || empty.RemovedCount != 2 || empty.MissingCount != 2 {
		t.Fatalf("complete empty directory result=%#v, err=%v", empty, err)
	}
	if aliases, err := db.ListAliasesByAccount(ctx, account.ID); err != nil || len(aliases) != 0 {
		t.Fatalf("empty directory retained aliases: count=%d, err=%v", len(aliases), err)
	}
}

func TestAppleDirectoryReconciliationUsesCompletePresenceBeforeForwardingFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	configureStrictImportCredentials(t, db)
	account := createAccount(t, ctx, db, "Mixed forwarding", "mixed-directory@icloud.com")
	matched := createAlias(t, ctx, db, account.ID, "matched-directory@icloud.com", []byte("matched-directory-key"))
	forwardedElsewhere := createAlias(t, ctx, db, account.ID, "forwarded-elsewhere@icloud.com", []byte("forwarded-elsewhere-key"))
	missing := createAlias(t, ctx, db, account.ID, "missing-mixed-directory@icloud.com", []byte("missing-mixed-key"))
	otherAccount := createAccount(t, ctx, db, "Other destination", "other-mixed-directory@icloud.com")
	otherOwned := createAlias(t, ctx, db, otherAccount.ID, "other-owned-directory@icloud.com", []byte("other-owned-directory-key"))
	candidates := []domain.AliasImportCandidate{{Address: matched.Address, Active: true}}
	directory := []string{
		" MATCHED-DIRECTORY@ICLOUD.COM ", forwardedElsewhere.Address,
		otherOwned.Address, "foreign-not-imported@icloud.com",
	}
	result, issued, err := db.ReconcileAppleAliasesWithCredentials(ctx, account.ID, candidates, directory)
	if err != nil || result.RemovedCount != 1 || result.MissingCount != 1 || len(result.Existing) != 1 || len(result.Created) != 0 || len(issued) != 0 {
		t.Fatalf("mixed forwarding reconciliation=%#v, issued=%d, err=%v", result, len(issued), err)
	}
	if _, err := db.GetAlias(ctx, missing.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent alias survived complete directory reconciliation: %v", err)
	}
	for _, before := range []domain.Alias{matched, forwardedElsewhere, otherOwned} {
		if got := requireDirectoryAlias(t, ctx, db, before.ID); !reflect.DeepEqual(got, before) {
			t.Fatalf("present address changed after forwarding filter: before=%#v after=%#v", before, got)
		}
	}
	if _, err := db.GetAliasByAddress(ctx, "foreign-not-imported@icloud.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("presence-only address was imported: %v", err)
	}
}

func TestAppleDirectoryReconciliationAllocatesFreedCapacityBeforeImport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	configureStrictImportCredentials(t, db)
	account := createAccount(t, ctx, db, "Capacity reconciliation", "directory-capacity@icloud.com")
	fill := make([]domain.AliasImportCandidate, 0, domain.MaxEnabledAliasesPerAccount-1)
	for index := 0; index < domain.MaxEnabledAliasesPerAccount-1; index++ {
		fill = append(fill, domain.AliasImportCandidate{
			Address: fmt.Sprintf("capacity-directory-%04d@icloud.com", index), APIKeyHash: []byte(fmt.Sprintf("capacity-directory-hash-%04d", index)),
			APIKeyPrefix: "capacity", Active: true,
		})
	}
	if _, err := db.ImportAliases(ctx, account.ID, fill); err != nil {
		t.Fatal(err)
	}
	pending, _ := createPendingFreshCursorAlias(t, ctx, db, account)
	for _, name := range []string{"a-restore", "b-restore"} {
		if _, err := db.CreateAlias(ctx, domain.Alias{
			AccountID: account.ID, Address: name + "@icloud.com", Enabled: false,
			LastSyncStatus: domain.SyncStatusError, LastSyncError: domain.AppleAliasNotFound,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// One disappeared active address frees one slot. The pending reservation
	// occupies the last slot even though it is not yet enabled.
	candidates := append([]domain.AliasImportCandidate{}, fill[1:]...)
	candidates = append(candidates,
		domain.AliasImportCandidate{Address: "a-restore@icloud.com", Active: true},
		domain.AliasImportCandidate{Address: "b-restore@icloud.com", Active: true},
		domain.AliasImportCandidate{Address: "new-at-capacity@icloud.com", Active: true},
		domain.AliasImportCandidate{Address: "new-over-capacity@icloud.com", Active: true},
		domain.AliasImportCandidate{Address: pending.Address, Active: true},
	)
	result, _, err := db.ReconcileAppleAliasesWithCredentials(ctx, account.ID, candidates, directoryCandidateAddresses(candidates))
	if err != nil || result.MissingCount != 1 || result.RemovedCount != 1 || result.RestoredCount != 2 || result.ImportedDisabledCount != 1 || len(result.Created) != 2 || !result.Created[0].Enabled || result.Created[1].Enabled {
		t.Fatalf("reconciliation at capacity=%#v, err=%v", result, err)
	}
	if count, err := db.CountEnabledAliasesByAccount(ctx, account.ID); err != nil || count != domain.MaxEnabledAliasesPerAccount-1 {
		t.Fatalf("enabled count at capacity=%d, err=%v", count, err)
	}
	if got := requireDirectoryAlias(t, ctx, db, pending.ID); !reflect.DeepEqual(got, pending) {
		t.Fatal("capacity allocation changed pending reservation")
	}
	second, err := db.GetAliasByAddress(ctx, "b-restore@icloud.com")
	if err != nil || second.Enabled || second.LastSyncError != "" {
		t.Fatalf("availability restoration enabled an alias without an administrator action: %#v, err=%v", second, err)
	}
	// Releasing another existing slot lets a later full directory import a new
	// address while existing disabled addresses retain their local state.
	candidates = candidates[1:]
	candidates = append(candidates, domain.AliasImportCandidate{Address: "new-after-capacity@icloud.com", Active: true})
	result, _, err = db.ReconcileAppleAliasesWithCredentials(ctx, account.ID, candidates, directoryCandidateAddresses(candidates))
	if err != nil || result.MissingCount != 1 || result.RemovedCount != 1 || result.RestoredCount != 0 || len(result.Created) != 1 || !result.Created[0].Enabled {
		t.Fatalf("later capacity reconciliation=%#v, err=%v", result, err)
	}
	if got := requireDirectoryAlias(t, ctx, db, second.ID); !reflect.DeepEqual(got, second) {
		t.Fatalf("existing disabled alias changed after capacity was freed: %#v", got)
	}
}

func TestAppleDirectoryReconciliationAcceptsCompleteEmptyDirectory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	configureStrictImportCredentials(t, db)
	account := createAccount(t, ctx, db, "Empty directory", "empty-directory@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "last-directory-alias@icloud.com", []byte("last-directory-hash"))
	applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{alias}, nil, 7, 21, true)
	before := requireDirectoryAccount(t, ctx, db, account.ID)
	cursorBefore, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, issued, err := db.ReconcileAppleAliasesWithCredentials(ctx, account.ID, nil, nil)
	if err != nil || result.MissingCount != 1 || result.RemovedCount != 1 || len(result.Created) != 0 || len(issued) != 0 {
		t.Fatalf("complete empty directory=%#v, issued=%d, err=%v", result, len(issued), err)
	}
	if _, err := db.GetAlias(ctx, alias.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("complete empty directory did not remove last alias: %v", err)
	}
	if got := requireDirectoryAccount(t, ctx, db, account.ID); !got.UpdatedAt.After(before.UpdatedAt) {
		t.Fatal("complete empty directory did not advance routing version")
	}
	if cursor, err := db.GetIMAPSyncState(ctx, account.ID); err != nil || cursor != cursorBefore {
		t.Fatalf("complete empty directory changed preserved cursor: %#v, err=%v", cursor, err)
	}
}

func TestAppleDirectoryReconciliationRollsBackWithImportFailure(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"ownership", "credential", "audit", "duplicate", "custom", "invalid directory", "duplicate directory", "candidate absent from directory"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db, _ := openArchiveV2Store(t, 1<<20)
			configureStrictImportCredentials(t, db)
			account := createAccount(t, ctx, db, "Atomic reconciliation", "atomic-directory@icloud.com")
			if failure == "custom" {
				account = createCustomImportAccount(t, ctx, db, "directory.test", true)
			}
			missing := createAlias(t, ctx, db, account.ID, "atomic-missing@icloud.com", []byte("atomic-missing-hash"))
			restore, err := db.CreateAlias(ctx, domain.Alias{
				AccountID: account.ID, Address: "atomic-restore@icloud.com", Enabled: false,
				LastSyncStatus: domain.SyncStatusError, LastSyncError: domain.AppleAliasInactive,
			})
			if err != nil {
				t.Fatal(err)
			}
			applyArchiveV2Batch(t, ctx, db, account.ID, []domain.Alias{missing}, []domain.ArchivedMessage{
				{AccountID: account.ID, UIDValidity: 4, UID: 31, InternalDate: time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC), ContentState: domain.ArchiveContentMetadata, AliasIDs: []int64{missing.ID}},
			}, 4, 31, true)
			archiveBefore, err := db.ListArchivedMailboxMessages(ctx, missing.ID)
			if err != nil || len(archiveBefore) != 1 {
				t.Fatalf("archive before failed reconciliation: %#v, err=%v", archiveBefore, err)
			}
			missing = requireDirectoryAlias(t, ctx, db, missing.ID)
			before := requireDirectoryAccount(t, ctx, db, account.ID)
			cursorBefore, err := db.GetIMAPSyncState(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			candidates := []domain.AliasImportCandidate{
				{Address: restore.Address, Active: true}, {Address: "atomic-new@icloud.com", Active: true},
			}
			switch failure {
			case "ownership":
				other := createAccount(t, ctx, db, "Other owner", "other-directory@icloud.com")
				owned := createAlias(t, ctx, db, other.ID, "atomic-owned@icloud.com", []byte("owned-hash"))
				candidates = append(candidates, domain.AliasImportCandidate{Address: owned.Address, Active: true})
			case "credential":
				db.ConfigureAliasCredentialFactory(func(int64, int64) (domain.AliasCredentialMaterial, error) {
					return domain.AliasCredentialMaterial{}, errors.New("injected import credential failure")
				})
			case "audit":
				if _, err := db.DB().ExecContext(ctx, `CREATE TRIGGER fail_directory_removal_audit
					BEFORE INSERT ON audit_logs WHEN NEW.action = 'sync_remove'
					BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				candidates = append(candidates, candidates[1])
			}
			directory := directoryCandidateAddresses(candidates)
			switch failure {
			case "invalid directory":
				directory = append(directory, "not an email")
			case "duplicate directory":
				directory = append(directory, "ATOMIC-NEW@ICLOUD.COM")
			case "candidate absent from directory":
				directory = directory[:1]
			}
			result, issued, err := db.ReconcileAppleAliasesWithCredentials(ctx, account.ID, candidates, directory)
			if err == nil || len(result.Created) != 0 || len(issued) != 0 || result.MissingCount != 0 || result.RemovedCount != 0 || result.RestoredCount != 0 {
				t.Fatalf("failed reconciliation=%#v, issued=%d, err=%v", result, len(issued), err)
			}
			if failure == "ownership" && (!errors.Is(err, store.ErrAliasOwnershipConflict) || len(result.Conflicts) != 1) {
				t.Fatalf("ownership failure=%#v, err=%v", result, err)
			}
			if failure == "custom" && !errors.Is(err, store.ErrICloudMailboxRequired) {
				t.Fatalf("custom mailbox reconciliation error=%v", err)
			}
			for _, alias := range []domain.Alias{missing, restore} {
				if got := requireDirectoryAlias(t, ctx, db, alias.ID); !reflect.DeepEqual(got, alias) {
					t.Fatalf("failed reconciliation changed alias: before=%#v after=%#v", alias, got)
				}
			}
			if archiveAfter, err := db.ListArchivedMailboxMessages(ctx, missing.ID); err != nil || !reflect.DeepEqual(archiveAfter, archiveBefore) {
				t.Fatalf("failed reconciliation removed archived mail mappings: %#v, err=%v", archiveAfter, err)
			}
			if logs, err := db.ListAuditLogsFiltered(ctx, store.AuditLogFilter{Action: "sync_remove"}); err != nil || len(logs) != 0 {
				t.Fatalf("failed reconciliation committed removal audit: %#v, err=%v", logs, err)
			}
			if got := requireDirectoryAccount(t, ctx, db, account.ID); !reflect.DeepEqual(got, before) {
				t.Fatal("failed reconciliation advanced account version")
			}
			if cursor, err := db.GetIMAPSyncState(ctx, account.ID); err != nil || cursor != cursorBefore {
				t.Fatalf("failed reconciliation changed cursor: %#v, err=%v", cursor, err)
			}
			if _, err := db.GetAliasByAddress(ctx, "atomic-new@icloud.com"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("failed reconciliation retained new alias: %v", err)
			}
		})
	}
}

func TestAppleDirectoryMarkersRequireVerificationBeforeManualEnable(t *testing.T) {
	t.Parallel()
	for _, marker := range []string{domain.AppleAliasNotFound, domain.AppleAliasInactive} {
		t.Run(marker, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := openTestStore(t)
			account := createAccount(t, ctx, db, "Manual state", "manual-directory@icloud.com")
			alias, err := db.CreateAlias(ctx, domain.Alias{
				AccountID: account.ID, Address: "manual-marker@icloud.com", APIKeyHash: []byte("manual-marker-hash"),
				Enabled: false, LastSyncStatus: domain.SyncStatusError, LastSyncError: marker,
			})
			if err != nil {
				t.Fatal(err)
			}
			group, err := db.CreateMailGroup(ctx, "Directory metadata")
			if err != nil {
				t.Fatal(err)
			}
			before := requireDirectoryAccount(t, ctx, db, account.ID)
			if err := db.SetAliasEnabled(ctx, alias.ID, true); !errors.Is(err, store.ErrAppleAliasUnavailable) {
				t.Fatalf("manual enable error=%v", err)
			}
			enabled := true
			if _, err := db.UpdateAliasAdminState(ctx, alias.ID, store.AliasAdminStateUpdate{
				Enabled: &enabled, GroupIDPresent: true, GroupID: &group.ID,
			}); !errors.Is(err, store.ErrAppleAliasUnavailable) {
				t.Fatalf("combined manual enable error=%v", err)
			}
			if got := requireDirectoryAlias(t, ctx, db, alias.ID); !reflect.DeepEqual(got, alias) {
				t.Fatal("rejected manual enable partially changed alias/group")
			}
			alias.Label = "Edited local label"
			alias, err = db.UpdateAlias(ctx, alias)
			if err != nil || alias.Label != "Edited local label" || alias.LastSyncError != marker {
				t.Fatalf("metadata edit on unavailable alias=%#v, err=%v", alias, err)
			}
			if updated, err := db.UpdateAliasAdminState(ctx, alias.ID, store.AliasAdminStateUpdate{GroupIDPresent: true, GroupID: &group.ID}); err != nil || updated.GroupID == nil || *updated.GroupID != group.ID || updated.LastSyncError != marker || updated.Enabled {
				t.Fatalf("group edit on unavailable alias=%#v, err=%v", updated, err)
			}
			if got := requireDirectoryAccount(t, ctx, db, account.ID); !reflect.DeepEqual(got, before) {
				t.Fatal("metadata edits or rejected enable changed account routing version")
			}
		})
	}
}

func TestCredentialAliasImportPreservesAdditiveContract(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	configureStrictImportCredentials(t, db)
	account := createAccount(t, ctx, db, "Additive import", "additive-directory@icloud.com")
	missing := createAlias(t, ctx, db, account.ID, "additive-missing@icloud.com", []byte("additive-missing-hash"))
	inactive := createAlias(t, ctx, db, account.ID, "additive-inactive@icloud.com", []byte("additive-inactive-hash"))
	before := requireDirectoryAccount(t, ctx, db, account.ID)
	result, _, err := db.ImportAliasesWithCredentials(ctx, account.ID, []domain.AliasImportCandidate{{Address: inactive.Address, Active: false}})
	if err != nil || result.MissingCount != 0 || result.InactiveUpdatedCount != 0 || result.RestoredCount != 0 {
		t.Fatalf("historical additive import=%#v, err=%v", result, err)
	}
	for _, alias := range []domain.Alias{missing, inactive} {
		if got := requireDirectoryAlias(t, ctx, db, alias.ID); !reflect.DeepEqual(got, alias) {
			t.Fatal("historical additive import reconciled existing alias")
		}
	}
	if got := requireDirectoryAccount(t, ctx, db, account.ID); !reflect.DeepEqual(got, before) {
		t.Fatal("historical additive import changed account version")
	}
}

func requireDirectoryAlias(t *testing.T, ctx context.Context, db *store.Store, aliasID int64) domain.Alias {
	t.Helper()
	alias, err := db.GetAlias(ctx, aliasID)
	if err != nil {
		t.Fatal(err)
	}
	return alias
}

func directoryCandidateAddresses(candidates []domain.AliasImportCandidate) []string {
	addresses := make([]string, len(candidates))
	for index, candidate := range candidates {
		addresses[index] = candidate.Address
	}
	return addresses
}

func requireDirectoryAccount(t *testing.T, ctx context.Context, db *store.Store, accountID int64) domain.Account {
	t.Helper()
	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	return account
}
