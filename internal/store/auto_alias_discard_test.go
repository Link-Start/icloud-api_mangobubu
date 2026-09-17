package store_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

func TestDiscardPendingAutoAliasCleansCandidateAndPreservesAccountState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Discard candidate", "discard-candidate@icloud.com")
	sibling := createAlias(t, ctx, db, account.ID, "keep-alias@icloud.com", []byte("keep-alias-hash"))
	pending := createDiscardPendingAlias(t, ctx, db, account)
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO imap_sync_states(account_id, uid_validity, last_uid, updated_at)
		VALUES(?, 1, 352, 1000)`, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO consumed_messages(alias_id, uid_validity, uid, consumed_at)
		VALUES(?, 1, 352, 1000)`, pending.ID); err != nil {
		t.Fatal(err)
	}
	beforeAccount, err := db.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeCursor, err := db.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeSession, err := db.GetAppleWebSession(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	installAliasMutationOrderLog(t, ctx, db)
	if _, err := db.DB().ExecContext(ctx, `
		CREATE TRIGGER require_account_lock_before_candidate_discard
		BEFORE DELETE ON aliases
		BEGIN
			SELECT CASE WHEN NOT EXISTS(
				SELECT 1 FROM alias_mutation_order WHERE event = 'account-lock'
			) THEN RAISE(ABORT, 'candidate discarded before account lock') END;
			INSERT INTO alias_mutation_order(event) VALUES('candidate-discard');
		END`); err != nil {
		t.Fatal(err)
	}
	if err := db.DiscardPendingAutoAlias(ctx, account.ID, pending.ID); err != nil {
		t.Fatalf("discard candidate: %v", err)
	}
	assertMutationOrder(t, ctx, db, "account-lock,candidate-discard")
	for name, lookup := range map[string]func() (domain.Alias, error){
		"alias":         func() (domain.Alias, error) { return db.GetAlias(ctx, pending.ID) },
		"API key":       func() (domain.Alias, error) { return db.GetAliasByAPIKeyHash(ctx, pending.APIKeyHash) },
		"IMAP password": func() (domain.Alias, error) { return db.GetAliasByIMAPPasswordHash(ctx, pending.IMAPPasswordHash) },
		"OAuth client":  func() (domain.Alias, error) { return db.GetAliasByOAuthClientID(ctx, pending.OAuthClientID) },
	} {
		if _, err := lookup(); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s remains after discard: %v", name, err)
		}
	}
	for _, table := range []string{"pending_alias_api_keys", "consumed_messages"} {
		var count int
		if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE alias_id = ?`, pending.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows after discard = %d, err=%v", table, count, err)
		}
	}
	if _, err := db.GetPendingAutoAliasConfirmation(ctx, account.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("candidate still blocks next creation: %v", err)
	}
	if got, err := db.GetAlias(ctx, sibling.ID); err != nil || !reflect.DeepEqual(got, sibling) {
		t.Fatalf("discard changed unrelated alias: got=%#v err=%v", got, err)
	}
	beforeAccount.AliasCount-- // GetAccount includes the current local alias count.
	if got, err := db.GetAccount(ctx, account.ID); err != nil || !reflect.DeepEqual(got, beforeAccount) {
		t.Fatalf("discard changed account/version: got=%#v err=%v", got, err)
	}
	if got, err := db.GetIMAPSyncState(ctx, account.ID); err != nil || got != beforeCursor {
		t.Fatalf("discard changed IMAP cursor: got=%#v err=%v", got, err)
	}
	if got, err := db.GetAppleWebSession(ctx, account.ID); err != nil || !reflect.DeepEqual(got, beforeSession) {
		t.Fatalf("discard changed Apple session: got=%#v err=%v", got, err)
	}
	if err := db.DiscardPendingAutoAlias(ctx, account.ID, pending.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("repeated discard = %v, want ErrNotFound", err)
	}
}

func TestDiscardPendingAutoAliasProtectsNonCandidates(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"different account", "missing account", "missing alias", "ordinary disabled alias",
		"confirmed alias", "enabled pending marker", "cleared marker", "missing pending key",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := openTestStore(t)
			account := createAccount(t, ctx, db, "Protected candidate", "protected-candidate@icloud.com")
			pending := createDiscardPendingAlias(t, ctx, db, account)
			targetAccountID, targetAliasID := account.ID, pending.ID
			switch name {
			case "different account":
				other := createAccount(t, ctx, db, "Other account", "other-discard@icloud.com")
				targetAccountID = other.ID
			case "missing account":
				targetAccountID += 1000
			case "missing alias":
				targetAliasID += 1000
			case "ordinary disabled alias":
				ordinary, err := db.CreateAlias(ctx, domain.Alias{
					AccountID: account.ID, Address: "ordinary-disabled@icloud.com",
					APIKeyHash: []byte("ordinary-disabled-hash"), Enabled: false,
				})
				if err != nil {
					t.Fatal(err)
				}
				targetAliasID = ordinary.ID
			case "confirmed alias":
				session, err := db.GetAppleWebSession(ctx, account.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := db.ConfirmPendingAutoAlias(ctx, session, pending.ID); err != nil {
					t.Fatal(err)
				}
			case "enabled pending marker":
				if _, err := db.DB().ExecContext(ctx, `UPDATE aliases SET enabled = TRUE WHERE id = ?`, pending.ID); err != nil {
					t.Fatal(err)
				}
			case "cleared marker":
				if _, err := db.DB().ExecContext(ctx, `UPDATE aliases SET last_sync_error = '' WHERE id = ?`, pending.ID); err != nil {
					t.Fatal(err)
				}
			case "missing pending key":
				if _, err := db.DB().ExecContext(ctx, `DELETE FROM pending_alias_api_keys WHERE alias_id = ?`, pending.ID); err != nil {
					t.Fatal(err)
				}
			}
			beforeAliases, err := db.ListAliasesByAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeKeys, err := db.CountPendingAliasAPIKeysByAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeAccount, err := db.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.DiscardPendingAutoAlias(ctx, targetAccountID, targetAliasID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("discard protected alias = %v, want ErrNotFound", err)
			}
			if got, err := db.ListAliasesByAccount(ctx, account.ID); err != nil || !reflect.DeepEqual(got, beforeAliases) {
				t.Fatalf("protected aliases changed: got=%#v err=%v", got, err)
			}
			if got, err := db.CountPendingAliasAPIKeysByAccount(ctx, account.ID); err != nil || got != beforeKeys {
				t.Fatalf("protected pending keys changed: got=%d want=%d err=%v", got, beforeKeys, err)
			}
			if got, err := db.GetAccount(ctx, account.ID); err != nil || !reflect.DeepEqual(got, beforeAccount) {
				t.Fatalf("protected account/version changed: got=%#v err=%v", got, err)
			}
		})
	}
}

func TestDiscardPendingAutoAliasRollsBackCascadeFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Discard rollback", "discard-rollback@icloud.com")
	pending := createDiscardPendingAlias(t, ctx, db, account)
	beforeKey, err := db.GetPendingAutoAliasConfirmation(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_candidate_key_discard
		BEFORE DELETE ON pending_alias_api_keys
		BEGIN SELECT RAISE(ABORT, 'injected pending key discard failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.DiscardPendingAutoAlias(ctx, account.ID, pending.ID); err == nil || !strings.Contains(err.Error(), "injected pending key discard failure") {
		t.Fatalf("discard with failing cascade = %v", err)
	}
	if got, err := db.GetAlias(ctx, pending.ID); err != nil || !reflect.DeepEqual(got, pending) {
		t.Fatalf("failed discard lost candidate/credentials: got=%#v err=%v", got, err)
	}
	if got, err := db.GetPendingAutoAliasConfirmation(ctx, account.ID); err != nil || !reflect.DeepEqual(got, beforeKey) {
		t.Fatalf("failed discard changed pending key: got=%#v err=%v", got, err)
	}
	if _, err := db.DB().ExecContext(ctx, `DROP TRIGGER reject_candidate_key_discard`); err != nil {
		t.Fatal(err)
	}
	if err := db.DiscardPendingAutoAlias(ctx, account.ID, pending.ID); err != nil {
		t.Fatalf("retry discard after rollback: %v", err)
	}
}

func createDiscardPendingAlias(t *testing.T, ctx context.Context, db *store.Store, account domain.Account) domain.Alias {
	t.Helper()
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x76}, 32))
	if err != nil {
		t.Fatal(err)
	}
	db.ConfigureAliasCredentialReuseFactory(func(aliasID, version int64, ciphertext string) (domain.AliasCredentialMaterial, error) {
		apiKey, err := cipher.DecryptPendingAliasAPIKey(ciphertext)
		if err != nil {
			return domain.AliasCredentialMaterial{}, err
		}
		_, material, err := secure.NewAliasCredentialMaterialWithAPIKey(cipher, aliasID, version, apiKey)
		return material, err
	})
	key, hash, prefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := cipher.EncryptPendingAliasAPIKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pending, _, err := db.CreateAliasWithPendingAPIKey(ctx, domain.AppleWebSession{
		AccountID: account.ID, Ciphertext: "as1.discard-candidate", AppleID: account.Email,
		Region: "global", Authenticated: true,
	}, domain.Alias{
		AccountID: account.ID, Address: "discard-pending-alias@icloud.com", Enabled: false,
		APIKeyHash: hash, APIKeyPrefix: prefix,
	}, ciphertext)
	if err != nil {
		t.Fatalf("create discard candidate: %v", err)
	}
	if pending.Enabled || pending.LastSyncError != domain.AppleAliasConfirmationPending ||
		pending.CredentialCiphertext == "" || len(pending.IMAPPasswordHash) == 0 || pending.OAuthClientID == "" {
		t.Fatalf("invalid discard candidate fixture: %#v", pending)
	}
	return pending
}
