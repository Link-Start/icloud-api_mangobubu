package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestDeleteAliasAllowsStaleConfirmationMarker(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	account := createAccount(t, ctx, db, "Stale confirmation", "stale-confirmation@icloud.com")
	alias := createAlias(t, ctx, db, account.ID, "stale-pending@icloud.com", bytes.Repeat([]byte{0x51}, 32))
	if err := db.SetAliasEnabled(ctx, alias.ID, false); err != nil {
		t.Fatalf("disable alias: %v", err)
	}
	if _, err := db.DB().ExecContext(ctx,
		`UPDATE aliases SET last_sync_status = ?, last_sync_error = ? WHERE id = ?`,
		domain.SyncStatusPending, domain.AppleAliasConfirmationPending, alias.ID,
	); err != nil {
		t.Fatalf("mark confirmation pending: %v", err)
	}

	if err := db.DeleteAlias(ctx, alias.ID); err != nil {
		t.Fatalf("delete stale confirmation alias: %v", err)
	}
	if _, err := db.GetAlias(ctx, alias.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale confirmation alias remains: err=%v", err)
	}
}
