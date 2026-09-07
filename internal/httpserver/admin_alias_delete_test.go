package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"icloud-api/internal/domain"
	"icloud-api/internal/hmesync"
	"icloud-api/internal/store"
)

func TestAdminAPIBatchAliasDeleteErrorPreservesAppleCause(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "login required wrapping missing session",
			err:        fmt.Errorf("load Apple session: %w: %w", hmesync.ErrLoginRequired, store.ErrNotFound),
			wantStatus: http.StatusConflict, wantCode: hmesync.CodeLoginRequired,
		},
		{
			name:       "expired session joined with missing row",
			err:        errors.Join(hmesync.ErrSessionExpired, store.ErrNotFound),
			wantStatus: http.StatusConflict, wantCode: hmesync.CodeSessionExpired,
		},
		{
			name:       "upstream error joined with missing row",
			err:        errors.Join(hmesync.ErrUpstream, store.ErrNotFound),
			wantStatus: http.StatusBadGateway, wantCode: hmesync.CodeUpstreamError,
		},
		{
			name:       "rate limit joined with missing row",
			err:        errors.Join(hmesync.ErrRateLimited, store.ErrNotFound),
			wantStatus: http.StatusTooManyRequests, wantCode: hmesync.CodeRateLimited,
		},
		{
			name:       "deadline joined with missing row",
			err:        errors.Join(context.DeadlineExceeded, store.ErrNotFound),
			wantStatus: http.StatusGatewayTimeout, wantCode: hmesync.CodeUpstreamError,
		},
		{
			name: "pending confirmation", err: store.ErrAliasConfirmationPending,
			wantStatus: http.StatusConflict, wantCode: hmesync.CodeAliasConfirmationPending,
		},
		{
			name: "missing alias", err: store.ErrNotFound,
			wantStatus: http.StatusNotFound, wantCode: "NOT_FOUND",
		},
		{
			name: "wrapped missing alias", err: fmt.Errorf("load alias: %w", store.ErrNotFound),
			wantStatus: http.StatusNotFound, wantCode: "NOT_FOUND",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := adminAPIBatchAliasDeleteError(test.err)
			if got.Status != test.wantStatus || got.Code != test.wantCode {
				t.Fatalf("batch deletion error = %#v, want %d %s", got, test.wantStatus, test.wantCode)
			}
			if test.wantCode == "NOT_FOUND" && got.Message != "隐私邮箱不存在" {
				t.Fatalf("missing alias message = %q", got.Message)
			}
		})
	}
}

func TestAdminAPIBatchDeleteAliasesSessionLossKeepsLocalRecords(t *testing.T) {
	env := newAdminAPITestEnv(t)
	var logs strings.Builder
	env.server.logger = slog.New(slog.NewTextHandler(&logs, nil))
	sessionCookie, csrf, _ := env.createSession(t, "batch-session-loss-admin", "unused-password")
	account := adminAPITestCreateAccount(t, env, "batch-session-loss@icloud.com")
	first := adminAPITestCreateDeleteAlias(t, env, account.ID, "batch-session-loss-first@icloud.com")
	second := adminAPITestCreateDeleteAlias(t, env, account.ID, "batch-session-loss-second@icloud.com")
	const secret = "private-upstream-session-data"
	deleteCalls := 0
	env.server.SetHMESyncService(&fakeHMESyncService{
		getSession: func(context.Context, int64) (hmesync.SessionInfo, error) {
			return hmesync.SessionInfo{Status: hmesync.StatusAuthenticated}, nil
		},
		deleteAliases: func(_ context.Context, aliasIDs []int64) ([]hmesync.AliasDeletionOutcome, error) {
			deleteCalls++
			if len(aliasIDs) != 2 || aliasIDs[0] != first.ID || aliasIDs[1] != second.ID {
				t.Fatalf("batch deletion IDs = %v", aliasIDs)
			}
			return []hmesync.AliasDeletionOutcome{
				{AliasID: first.ID, Err: hmesync.ErrSessionExpired},
				{AliasID: second.ID, Err: fmt.Errorf("%s: %w: %w", secret, hmesync.ErrLoginRequired, store.ErrNotFound)},
			}, nil
		},
	})

	response := env.request(t, http.MethodDelete, "/admin/api/v1/aliases/batch", adminAPITestJSON(t, map[string]any{
		"alias_ids": []int64{first.ID, second.ID},
	}), "application/json", []*http.Cookie{sessionCookie}, csrf)
	if response.Code != http.StatusOK || deleteCalls != 1 {
		t.Fatalf("batch session loss response = %d; calls=%d; body=%s", response.Code, deleteCalls, response.Body.String())
	}
	var payload struct {
		Data adminAPIAliasBatchDeleteDTO `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode batch deletion: %v", err)
	}
	if payload.Data.Requested != 2 || payload.Data.Deleted != 0 || payload.Data.Failed != 2 || len(payload.Data.Results) != 2 {
		t.Fatalf("batch session loss payload = %#v", payload.Data)
	}
	wantCodes := []string{hmesync.CodeSessionExpired, hmesync.CodeLoginRequired}
	for i, alias := range []domain.Alias{first, second} {
		item := payload.Data.Results[i]
		if item.ID != alias.ID || item.Deleted || item.Code != wantCodes[i] || !item.LocalRetained ||
			!strings.Contains(item.Message, "本地记录已保留") {
			t.Fatalf("batch session loss result = %#v, want %s", item, wantCodes[i])
		}
		if _, err := env.store.GetAlias(context.Background(), alias.ID); err != nil {
			t.Fatalf("session loss removed alias %d: %v", alias.ID, err)
		}
		assertAdminAliasDeleteAudit(t, env.store, alias.ID, "failed", wantCodes[i])
		if !strings.Contains(logs.String(), fmt.Sprintf("alias_id=%d", alias.ID)) {
			t.Errorf("deletion log omitted alias ID %d: %s", alias.ID, logs.String())
		}
	}
	if strings.Contains(logs.String(), "NOT_FOUND") || !strings.Contains(logs.String(), hmesync.CodeLoginRequired) {
		t.Errorf("session loss log has incorrect error code: %s", logs.String())
	}
	if strings.Contains(response.Body.String(), secret) || strings.Contains(logs.String(), secret) {
		t.Error("batch deletion exposed the wrapped error cause")
	}
}
