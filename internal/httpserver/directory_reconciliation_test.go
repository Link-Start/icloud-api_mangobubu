package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"icloud-api/internal/domain"
	"icloud-api/internal/store"
)

func TestAppleDirectorySyncRemovesMissingAliasesAndPreservesCursor(t *testing.T) {
	env := newAdminAPITestEnv(t)
	ctx := context.Background()
	account := adminAPITestCreateAccount(t, env, "directory-owner@icloud.com")
	present := adminAPITestCreateDeleteAlias(t, env, account.ID, "present@icloud.com")
	missing := adminAPITestCreateDeleteAlias(t, env, account.ID, "missing@icloud.com")
	account, err := env.store.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.ApplyMailboxSync(ctx, account.ID, account.UpdatedAt, []domain.Alias{present, missing}, domain.MailboxSyncResult{
		State: domain.IMAPSyncState{AccountID: account.ID, UIDValidity: 91, LastUID: 1200}, Reset: true,
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	cursor, err := env.store.GetIMAPSyncState(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	connectManualAliasDirectory(t, env, account.ID, present.Address)
	cookie, csrf, _ := env.createSession(t, "reconcile-admin", "unused-password")
	path := fmt.Sprintf("/admin/api/v1/accounts/%d/aliases/sync", account.ID)
	for attempt := 0; attempt < 2; attempt++ {
		response := env.request(t, http.MethodPost, path, nil, "", []*http.Cookie{cookie}, csrf)
		if response.Code != http.StatusOK {
			t.Fatalf("directory response = %d %s", response.Code, response.Body.String())
		}
		var payload struct {
			Data adminAPIAppleSyncResultDTO `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		wantRemoved := 1 - attempt
		if payload.Data.Account.AliasCount != 1 || payload.Data.Summary.Total != 1 || payload.Data.Summary.MissingCount != wantRemoved || payload.Data.Summary.RemovedCount != wantRemoved {
			t.Fatalf("local and Apple counts lost: %+v / %+v", payload.Data.Account, payload.Data.Summary)
		}
	}
	if _, err := env.store.GetAlias(ctx, missing.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing alias was not removed: %v", err)
	}
	currentCursor, err := env.store.GetIMAPSyncState(ctx, account.ID)
	if err != nil || currentCursor != cursor {
		t.Fatalf("retiring missing alias reset cursor: %+v %v", currentCursor, err)
	}
	response := env.request(t, http.MethodPatch, fmt.Sprintf("/admin/api/v1/aliases/%d", missing.ID),
		adminAPITestJSON(t, map[string]bool{"enabled": true}), "application/json", []*http.Cookie{cookie}, csrf)
	if response.Code != http.StatusNotFound || adminAPITestErrorCode(t, response) != "NOT_FOUND" {
		t.Fatalf("removed alias still exposed = %d %s", response.Code, response.Body.String())
	}
}
