package store_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/secure"
	"icloud-api/internal/store"
)

func TestRotateAllAliasCredentialsMigratesLegacyRotatesV2AndRotatesPending(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	configureRotateAllCredentialFactory(db, cipher, nil)

	account := createAccount(t, ctx, db, "Rotate all", "rotate-all@icloud.com")
	// An empty second account makes the lock-order trigger prove that every
	// account is locked before the first alias bundle is published.
	_ = createAccount(t, ctx, db, "Rotate all empty", "rotate-all-empty@icloud.com")

	legacyKey, legacyHash, legacyPrefix, err := secure.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := db.CreateAlias(ctx, domain.Alias{
		AccountID: account.ID, Address: "rotate-all-legacy@icloud.com",
		APIKeyHash: legacyHash, APIKeyPrefix: legacyPrefix,
		CredentialMode: domain.AliasCredentialModeLegacy, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx,
		`UPDATE aliases SET mailbox_uid_next = 9 WHERE id = ?`, legacy.ID,
	); err != nil {
		t.Fatal(err)
	}
	legacy, err = db.GetAlias(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldLegacyDirect, err := cipher.DirectLinkToken(legacy.ID, legacy.APIKeyHash)
	if err != nil {
		t.Fatal(err)
	}

	v2 := createAlias(t, ctx, db, account.ID, "rotate-all-v2@icloud.com", nil)
	oldV2Credentials, err := cipher.DecryptAliasCredentials(v2.ID, v2.CredentialCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	oldRecentToken, err := cipher.RecentMailToken(v2.ID, v2.APIKeyHash)
	if err != nil {
		t.Fatal(err)
	}
	oldOTPToken, err := cipher.OTPToken(v2.ID, v2.APIKeyHash)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 30, 1, 2, 3, 0, time.UTC)
	oldAccessToken, err := cipher.IssueAliasAccessToken(
		v2.ID, v2.CredentialVersion, v2.RefreshTokenHash, now.Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}

	pending := createAlias(t, ctx, db, account.ID, "rotate-all-pending@icloud.com", nil)
	oldPendingCredentials, err := cipher.DecryptAliasCredentials(
		pending.ID, pending.CredentialCiphertext,
	)
	if err != nil {
		t.Fatal(err)
	}
	oldPendingCiphertext, err := cipher.EncryptPendingAliasAPIKey(oldPendingCredentials.APIKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		UPDATE aliases SET enabled = FALSE, last_sync_error = ? WHERE id = ?`,
		domain.AppleAliasConfirmationPending, pending.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
		VALUES(?, ?, 1)`, pending.ID, oldPendingCiphertext,
	); err != nil {
		t.Fatal(err)
	}
	oldLegacyPendingCiphertext, err := cipher.EncryptPendingAliasAPIKey(legacyKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
		VALUES(?, ?, 2)`, legacy.ID, oldLegacyPendingCiphertext,
	); err != nil {
		t.Fatal(err)
	}
	pendingBefore, err := db.GetAlias(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}

	for _, statement := range []string{
		`CREATE TABLE rotate_all_lock_order(position INTEGER PRIMARY KEY AUTOINCREMENT, event TEXT NOT NULL)`,
		`CREATE TRIGGER record_rotate_all_account_lock BEFORE UPDATE ON accounts
			BEGIN INSERT INTO rotate_all_lock_order(event) VALUES('account-lock'); END`,
		`CREATE TRIGGER require_all_account_locks_before_rotation
			BEFORE UPDATE OF credential_ciphertext ON aliases
			BEGIN
				SELECT CASE WHEN (
					SELECT COUNT(*) FROM rotate_all_lock_order WHERE event = 'account-lock'
				) < 2 THEN RAISE(ABORT, 'alias rotated before all account locks') END;
				INSERT INTO rotate_all_lock_order(event) VALUES('alias-update');
			END`,
	} {
		if _, err := db.DB().ExecContext(ctx, statement); err != nil {
			t.Fatalf("install rotate-all lock-order fixture: %v", err)
		}
	}

	summary, err := db.RotateAllAliasCredentials(ctx)
	if err != nil {
		t.Fatalf("rotate all alias credentials: %v", err)
	}
	if summary != (store.RotateAllAliasCredentialsResult{
		Total: 3, Rotated: 3, MigratedLegacy: 1, RotatedV2: 2, RotatedPending: 2,
	}) {
		t.Fatalf("rotation summary = %#v", summary)
	}

	migrated, err := db.GetAlias(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.CredentialMode != domain.AliasCredentialModeV2 ||
		migrated.CredentialVersion != 1 || migrated.CredentialCiphertext == "" ||
		migrated.MailboxUIDValidity == 0 ||
		migrated.MailboxUIDValidity == legacy.MailboxUIDValidity ||
		migrated.MailboxUIDNext != 9 ||
		secure.HashEqual(migrated.APIKeyHash, legacy.APIKeyHash) {
		t.Fatalf("migrated legacy alias = %#v; before=%#v", migrated, legacy)
	}
	migratedCredentials, err := cipher.DecryptAliasCredentials(migrated.ID, migrated.CredentialCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	if migratedCredentials.APIKey == legacyKey ||
		!secure.HashEqual(secure.HashToken(migratedCredentials.APIKey), migrated.APIKeyHash) {
		t.Fatal("legacy alias did not receive a fresh complete v2 credential bundle")
	}
	if cipher.VerifyDirectLinkToken(oldLegacyDirect, migrated.ID, migrated.APIKeyHash) {
		t.Fatal("legacy direct-link token survived migration to v2")
	}
	if _, err := db.GetAliasByAPIKeyHash(ctx, legacy.APIKeyHash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old legacy API key lookup error = %v", err)
	}

	rotated, err := db.GetAlias(ctx, v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	newV2Credentials, err := cipher.DecryptAliasCredentials(rotated.ID, rotated.CredentialCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.CredentialMode != domain.AliasCredentialModeV2 ||
		rotated.CredentialVersion != v2.CredentialVersion+1 ||
		rotated.MailboxUIDValidity != v2.MailboxUIDValidity ||
		rotated.MailboxUIDNext != v2.MailboxUIDNext {
		t.Fatalf("rotated v2 alias = %#v; before=%#v", rotated, v2)
	}
	oldValues := []string{
		oldV2Credentials.APIKey, oldV2Credentials.IMAPPassword,
		oldV2Credentials.ClientID, oldV2Credentials.RefreshToken,
	}
	newValues := []string{
		newV2Credentials.APIKey, newV2Credentials.IMAPPassword,
		newV2Credentials.ClientID, newV2Credentials.RefreshToken,
	}
	for index := range oldValues {
		if oldValues[index] == newValues[index] {
			t.Fatalf("v2 credential field %d was not rotated", index)
		}
	}
	for name, lookup := range map[string]func() error{
		"API key": func() error {
			_, lookupErr := db.GetAliasByAPIKeyHash(ctx, secure.HashToken(oldV2Credentials.APIKey))
			return lookupErr
		},
		"IMAP password": func() error {
			_, lookupErr := db.GetAliasByIMAPPasswordHash(ctx, secure.HashToken(oldV2Credentials.IMAPPassword))
			return lookupErr
		},
		"OAuth client": func() error {
			_, lookupErr := db.GetAliasByOAuthClientID(ctx, oldV2Credentials.ClientID)
			return lookupErr
		},
	} {
		if err := lookup(); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("old %s lookup error = %v", name, err)
		}
	}
	if cipher.VerifyRecentMailToken(oldRecentToken, rotated.ID, rotated.APIKeyHash) ||
		cipher.VerifyOTPToken(oldOTPToken, rotated.ID, rotated.APIKeyHash) {
		t.Fatal("old recent-mail or OTP URL survived v2 rotation")
	}
	if cipher.VerifyAliasAccessToken(
		oldAccessToken, rotated.ID, rotated.CredentialVersion,
		rotated.RefreshTokenHash, now,
	) {
		t.Fatal("old OAuth access token survived v2 rotation")
	}

	pendingAfter, err := db.GetAlias(ctx, pending.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pendingAfter.CredentialVersion != pendingBefore.CredentialVersion+1 ||
		pendingAfter.Enabled != pendingBefore.Enabled ||
		pendingAfter.LastSyncError != domain.AppleAliasConfirmationPending ||
		pendingAfter.MailboxUIDValidity != pendingBefore.MailboxUIDValidity {
		t.Fatalf("confirmation-pending alias rotation = before:%#v after:%#v", pendingBefore, pendingAfter)
	}
	var pendingCiphertext string
	var pendingCreatedAt int64
	if err := db.DB().QueryRowContext(ctx,
		`SELECT api_key_ciphertext, created_at FROM pending_alias_api_keys WHERE alias_id = ?`,
		pending.ID,
	).Scan(&pendingCiphertext, &pendingCreatedAt); err != nil {
		t.Fatal(err)
	}
	newPendingKey, err := cipher.DecryptPendingAliasAPIKey(pendingCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	newPendingCredentials, err := cipher.DecryptAliasCredentials(
		pendingAfter.ID, pendingAfter.CredentialCiphertext,
	)
	if err != nil {
		t.Fatal(err)
	}
	if pendingCiphertext == oldPendingCiphertext ||
		newPendingKey == oldPendingCredentials.APIKey ||
		newPendingKey != newPendingCredentials.APIKey ||
		pendingCreatedAt != 1 {
		t.Fatalf("rotated pending key mismatch: old=%q new=%q bundle=%q created_at=%d",
			oldPendingCredentials.APIKey, newPendingKey, newPendingCredentials.APIKey, pendingCreatedAt)
	}
	if _, err := db.GetAliasByAPIKeyHash(
		ctx, secure.HashToken(oldPendingCredentials.APIKey),
	); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old pending API key lookup error = %v", err)
	}
	if current, err := db.GetAliasByAPIKeyHash(
		ctx, secure.HashToken(newPendingKey),
	); err != nil || current.ID != pending.ID {
		t.Fatalf("new pending API key lookup = %#v, err=%v", current, err)
	}

	var legacyPendingCiphertext string
	var legacyPendingCreatedAt int64
	if err := db.DB().QueryRowContext(ctx,
		`SELECT api_key_ciphertext, created_at FROM pending_alias_api_keys WHERE alias_id = ?`,
		legacy.ID,
	).Scan(&legacyPendingCiphertext, &legacyPendingCreatedAt); err != nil {
		t.Fatal(err)
	}
	legacyPendingKey, err := cipher.DecryptPendingAliasAPIKey(legacyPendingCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	if legacyPendingKey != migratedCredentials.APIKey ||
		legacyPendingKey == legacyKey || legacyPendingCreatedAt != 2 {
		t.Fatalf("migrated legacy pending key does not match new bundle")
	}
}

func TestRotateAllAliasCredentialsRollsBackEveryAliasOnIssuerFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x62}, 32))
	if err != nil {
		t.Fatal(err)
	}
	configureRotateAllCredentialFactory(db, cipher, nil)
	account := createAccount(t, ctx, db, "Rotate rollback", "rotate-rollback-all@icloud.com")
	first := createAlias(t, ctx, db, account.ID, "rotate-rollback-first@icloud.com", nil)
	second := createAlias(t, ctx, db, account.ID, "rotate-rollback-second@icloud.com", nil)
	if _, err := db.DB().ExecContext(ctx, `
		INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
		VALUES(?, 'ak1.rollback', 1)`, first.ID,
	); err != nil {
		t.Fatal(err)
	}
	firstBefore, _ := db.GetAlias(ctx, first.ID)
	secondBefore, _ := db.GetAlias(ctx, second.ID)
	admin, rawSession := createRotateAllAdminSession(t, ctx, db, "rollback-admin")

	var calls int
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		calls++
		if calls == 2 {
			return domain.AliasCredentialMaterial{}, errors.New("injected rotate-all issuer failure")
		}
		_, material, issueErr := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, issueErr
	})
	if _, err := db.RotateAllAliasCredentialsWithAudit(
		ctx, admin.ID, admin.PasswordVersion, domain.AuditLog{
			Username: "rollback-admin", Action: "rotate_all_credentials",
			ResourceType: "alias", ResourceID: "all", Result: "success",
		}); err == nil ||
		!strings.Contains(err.Error(), "injected rotate-all issuer failure") {
		t.Fatalf("rotate-all issuer error = %v", err)
	}
	firstAfter, _ := db.GetAlias(ctx, first.ID)
	secondAfter, _ := db.GetAlias(ctx, second.ID)
	if !reflect.DeepEqual(firstAfter, firstBefore) || !reflect.DeepEqual(secondAfter, secondBefore) {
		t.Fatalf("partial rotation survived rollback: first=%#v second=%#v", firstAfter, secondAfter)
	}
	var pendingCount int
	if err := db.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_alias_api_keys WHERE alias_id = ?`,
		first.ID,
	).Scan(&pendingCount); err != nil || pendingCount != 1 {
		t.Fatalf("pending key after rollback = %d, err=%v", pendingCount, err)
	}
	audits, err := db.ListAuditLogs(ctx, 10, 0)
	if err != nil || len(audits) != 0 {
		t.Fatalf("audit rows after issuer rollback = %#v, err=%v", audits, err)
	}
	adminAfter, err := db.GetAdminByID(ctx, admin.ID)
	if err != nil || adminAfter.PasswordVersion != admin.PasswordVersion {
		t.Fatalf("admin version after issuer rollback = %#v, err=%v", adminAfter, err)
	}
	if _, err := db.GetSessionByHash(ctx, secure.HashToken(rawSession)); err != nil {
		t.Fatalf("session was revoked by rolled-back issuer failure: %v", err)
	}
}

func TestRotateAllAliasCredentialsRequiresAndAtomicallyRotatesPendingKeys(t *testing.T) {
	t.Run("confirmation marker without pending row", func(t *testing.T) {
		ctx := context.Background()
		db := openTestStore(t)
		cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x64}, 32))
		if err != nil {
			t.Fatal(err)
		}
		configureRotateAllCredentialFactory(db, cipher, nil)
		account := createAccount(t, ctx, db, "Missing pending", "missing-pending@icloud.com")
		first := createAlias(t, ctx, db, account.ID, "missing-pending-first@icloud.com", nil)
		missing := createAlias(t, ctx, db, account.ID, "missing-pending-marker@icloud.com", nil)
		if _, err := db.DB().ExecContext(ctx, `
			UPDATE aliases SET enabled = FALSE, last_sync_error = ? WHERE id = ?`,
			domain.AppleAliasConfirmationPending, missing.ID,
		); err != nil {
			t.Fatal(err)
		}
		firstBefore, _ := db.GetAlias(ctx, first.ID)
		missingBefore, _ := db.GetAlias(ctx, missing.ID)

		if _, err := db.RotateAllAliasCredentials(ctx); err == nil ||
			!strings.Contains(err.Error(), "has no pending API key") {
			t.Fatalf("missing pending-key rotation error = %v", err)
		}
		firstAfter, _ := db.GetAlias(ctx, first.ID)
		missingAfter, _ := db.GetAlias(ctx, missing.ID)
		if !reflect.DeepEqual(firstAfter, firstBefore) ||
			!reflect.DeepEqual(missingAfter, missingBefore) {
			t.Fatal("missing pending row did not roll back the complete rotation")
		}
	})

	t.Run("pending key factory failure", func(t *testing.T) {
		ctx := context.Background()
		db := openTestStore(t)
		cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x65}, 32))
		if err != nil {
			t.Fatal(err)
		}
		configureRotateAllCredentialFactory(db, cipher, nil)
		account := createAccount(t, ctx, db, "Pending factory", "pending-factory@icloud.com")
		first := createAlias(t, ctx, db, account.ID, "pending-factory-first@icloud.com", nil)
		pending := createAlias(t, ctx, db, account.ID, "pending-factory-second@icloud.com", nil)
		pendingCredentials, err := cipher.DecryptAliasCredentials(
			pending.ID, pending.CredentialCiphertext,
		)
		if err != nil {
			t.Fatal(err)
		}
		pendingCiphertext, err := cipher.EncryptPendingAliasAPIKey(pendingCredentials.APIKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx, `
			INSERT INTO pending_alias_api_keys(alias_id, api_key_ciphertext, created_at)
			VALUES(?, ?, 17)`, pending.ID, pendingCiphertext,
		); err != nil {
			t.Fatal(err)
		}
		firstBefore, _ := db.GetAlias(ctx, first.ID)
		pendingBefore, _ := db.GetAlias(ctx, pending.ID)
		db.ConfigureAliasPendingKeyRotationFactory(func(int64, string) (string, error) {
			return "", errors.New("injected pending-key factory failure")
		})

		if _, err := db.RotateAllAliasCredentials(ctx); err == nil ||
			!strings.Contains(err.Error(), "injected pending-key factory failure") {
			t.Fatalf("pending-key factory error = %v", err)
		}
		firstAfter, _ := db.GetAlias(ctx, first.ID)
		pendingAfter, _ := db.GetAlias(ctx, pending.ID)
		if !reflect.DeepEqual(firstAfter, firstBefore) ||
			!reflect.DeepEqual(pendingAfter, pendingBefore) {
			t.Fatal("pending-key factory failure did not roll back every alias")
		}
		var afterCiphertext string
		var afterCreatedAt int64
		if err := db.DB().QueryRowContext(ctx, `
			SELECT api_key_ciphertext, created_at
			FROM pending_alias_api_keys WHERE alias_id = ?`, pending.ID,
		).Scan(&afterCiphertext, &afterCreatedAt); err != nil {
			t.Fatal(err)
		}
		if afterCiphertext != pendingCiphertext || afterCreatedAt != 17 {
			t.Fatal("pending-key factory failure changed the pending row")
		}
	})
}

func TestRotateAllAliasCredentialsRollsBackWhenAuditInsertFails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestStore(t)
	cipher, err := secure.NewCipher(bytes.Repeat([]byte{0x63}, 32))
	if err != nil {
		t.Fatal(err)
	}
	configureRotateAllCredentialFactory(db, cipher, nil)
	account := createAccount(t, ctx, db, "Rotate audit rollback", "rotate-audit-rollback@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "rotate-audit-rollback-alias@icloud.com", nil)
	before, _ := db.GetAlias(ctx, alias.ID)
	admin, rawSession := createRotateAllAdminSession(t, ctx, db, "audit-admin")
	if _, err := db.DB().ExecContext(ctx, `
		CREATE TRIGGER reject_rotate_all_audit
		BEFORE INSERT ON audit_logs
		WHEN NEW.action = 'rotate_all_credentials'
		BEGIN SELECT RAISE(ABORT, 'injected rotate-all audit failure'); END`,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := db.RotateAllAliasCredentialsWithAudit(
		ctx, admin.ID, admin.PasswordVersion, domain.AuditLog{
			Username: "audit-admin", Action: "rotate_all_credentials",
			ResourceType: "alias", ResourceID: "all", Result: "success",
		}); err == nil || !strings.Contains(err.Error(), "injected rotate-all audit failure") {
		t.Fatalf("rotate-all audit error = %v", err)
	}
	after, _ := db.GetAlias(ctx, alias.ID)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("credential rotation survived failed atomic audit: before=%#v after=%#v", before, after)
	}
	audits, err := db.ListAuditLogs(ctx, 10, 0)
	if err != nil || len(audits) != 0 {
		t.Fatalf("audit rows after failed atomic operation = %#v, err=%v", audits, err)
	}
	adminAfter, err := db.GetAdminByID(ctx, admin.ID)
	if err != nil || adminAfter.PasswordVersion != admin.PasswordVersion {
		t.Fatalf("admin version after audit rollback = %#v, err=%v", adminAfter, err)
	}
	if _, err := db.GetSessionByHash(ctx, secure.HashToken(rawSession)); err != nil {
		t.Fatalf("session was revoked by rolled-back audit failure: %v", err)
	}
}

func createRotateAllAdminSession(
	t *testing.T,
	ctx context.Context,
	db *store.Store,
	username string,
) (domain.Admin, string) {
	t.Helper()
	admin, err := db.CreateAdmin(ctx, username, "bcrypt-test-hash")
	if err != nil {
		t.Fatal(err)
	}
	rawSession := "rotate-all-session-" + username
	if err := db.CreateSession(ctx, secure.HashToken(rawSession), domain.Session{
		AdminID: admin.ID, Username: admin.Username,
		PasswordVersion: admin.PasswordVersion, CSRF: "rotate-all-csrf",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	return admin, rawSession
}

func configureRotateAllCredentialFactory(
	db *store.Store,
	cipher *secure.Cipher,
	calls *int,
) {
	db.ConfigureAliasCredentialFactory(func(aliasID, version int64) (domain.AliasCredentialMaterial, error) {
		if calls != nil {
			*calls++
		}
		_, material, err := secure.NewAliasCredentialMaterial(cipher, aliasID, version)
		return material, err
	})
	db.ConfigureAliasPendingKeyRotationFactory(func(aliasID int64, credentialCiphertext string) (string, error) {
		credentials, err := cipher.DecryptAliasCredentials(aliasID, credentialCiphertext)
		if err != nil {
			return "", err
		}
		return cipher.EncryptPendingAliasAPIKey(credentials.APIKey)
	})
}
