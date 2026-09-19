package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"icloud-api/internal/domain"
)

// ImportAliases atomically creates aliases that are missing from an account.
// Existing aliases owned by the account are preserved exactly as stored. If
// any address belongs to another account, the full batch is rejected before
// any new alias is inserted.
func (s *Store) ImportAliases(
	ctx context.Context,
	accountID int64,
	candidates []domain.AliasImportCandidate,
) (domain.AliasImportResult, error) {
	result, _, err := s.importAliases(ctx, accountID, candidates, false, false, domain.MailboxTypeICloud, false, nil)
	return result, err
}

// ImportAliasesWithCredentials preserves the original one-time sync response
// while keeping v2 credential generation inside the store transaction.
func (s *Store) ImportAliasesWithCredentials(
	ctx context.Context,
	accountID int64,
	candidates []domain.AliasImportCandidate,
) (domain.AliasImportResult, []domain.AliasImportCredential, error) {
	return s.importAliases(ctx, accountID, candidates, true, false, domain.MailboxTypeICloud, false, nil)
}

// ReconcileAppleAliasesWithCredentials imports a complete, freshly fetched
// Apple directory and reconciles its availability with existing local aliases.
// The caller must validate the directory's completeness and account identity
// before calling, including when the complete directory is empty. Candidates
// contain entries forwarding to this account; directoryAddresses must contain
// every address, including entries forwarding elsewhere. Missing local aliases
// and their dependent credentials/mailbox mappings are deleted atomically;
// inactive aliases remain disabled. Account-level archived messages are retained.
// Original ImportAliases methods deliberately keep their additive contract.
func (s *Store) ReconcileAppleAliasesWithCredentials(
	ctx context.Context,
	accountID int64,
	candidates []domain.AliasImportCandidate,
	directoryAddresses []string,
) (domain.AliasImportResult, []domain.AliasImportCredential, error) {
	return s.importAliases(ctx, accountID, candidates, true, false, domain.MailboxTypeICloud, true, directoryAddresses)
}

// ImportAliasesWithCredentialsStrict is the all-or-nothing variant used by
// generated address batches. Any address that already exists causes the
// transaction to roll back, so a retry can never leave a partially committed
// batch behind.
func (s *Store) ImportAliasesWithCredentialsStrict(
	ctx context.Context,
	accountID int64,
	candidates []domain.AliasImportCandidate,
) (domain.AliasImportResult, []domain.AliasImportCredential, error) {
	return s.importAliases(ctx, accountID, candidates, true, true, "", false, nil)
}

// ImportCustomAliasesWithCredentialsStrict adds the account state and suffix
// checks required by the local random generator. Those checks run after the
// account row has been locked, so a concurrent mode/suffix/enable change is
// serialized with the all-or-nothing batch insert.
func (s *Store) ImportCustomAliasesWithCredentialsStrict(
	ctx context.Context,
	accountID int64,
	candidates []domain.AliasImportCandidate,
) (domain.AliasImportResult, []domain.AliasImportCredential, error) {
	return s.importAliases(ctx, accountID, candidates, true, true, domain.MailboxTypeCustom, false, nil)
}

func (s *Store) importAliases(
	ctx context.Context,
	accountID int64,
	candidates []domain.AliasImportCandidate,
	includeCredentials bool,
	strict bool,
	requiredMailboxType string,
	reconcileAppleDirectory bool,
	directoryAddresses []string,
) (domain.AliasImportResult, []domain.AliasImportCredential, error) {
	if includeCredentials && s.credentialFactory == nil {
		return domain.AliasImportResult{}, nil, fmt.Errorf("import aliases: v2 credential factory is not configured")
	}
	if includeCredentials && s.credentialRevealFactory == nil {
		return domain.AliasImportResult{}, nil, fmt.Errorf("import aliases: credential reveal factory is not configured")
	}
	normalized, err := normalizeAliasImportCandidates(candidates)
	if err != nil {
		return domain.AliasImportResult{}, nil, err
	}
	var directoryPresence map[string]struct{}
	if reconcileAppleDirectory {
		directoryPresence, err = normalizeAppleDirectoryAddresses(directoryAddresses)
		if err != nil {
			return domain.AliasImportResult{}, nil, err
		}
		for _, candidate := range normalized {
			if _, present := directoryPresence[candidate.Address]; !present {
				return domain.AliasImportResult{}, nil, fmt.Errorf("reconcile Apple aliases: candidate %q is absent from directory addresses", candidate.Address)
			}
		}
	}
	if !includeCredentials {
		for _, candidate := range normalized {
			if len(candidate.APIKeyHash) == 0 {
				return domain.AliasImportResult{}, nil, fmt.Errorf("import aliases: candidate %q has empty api key hash", candidate.Address)
			}
			if candidate.APIKeyPrefix == "" {
				return domain.AliasImportResult{}, nil, fmt.Errorf("import aliases: candidate %q has empty api key prefix", candidate.Address)
			}
		}
	}

	var tx *sql.Tx
	if requiredMailboxType == domain.MailboxTypeCustom {
		tx, err = s.beginAddressNamespaceTx(ctx)
	} else {
		tx, err = s.db.BeginTx(ctx, &sql.TxOptions{})
	}
	if err != nil {
		return domain.AliasImportResult{}, nil, fmt.Errorf("begin alias import: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	accountVersion, err := s.lockAccountVersionForUpdate(ctx, tx, accountID)
	if err != nil {
		return domain.AliasImportResult{}, nil, fmt.Errorf("lock account for alias import: %w", err)
	}
	state, stateErr := s.readAccountMailboxStateTx(ctx, tx, accountID)
	if stateErr != nil {
		return domain.AliasImportResult{}, nil, fmt.Errorf("read account mailbox before alias import: %w", stateErr)
	}
	if requiredMailboxType != "" {
		if state.MailboxType != requiredMailboxType {
			if requiredMailboxType == domain.MailboxTypeCustom {
				return domain.AliasImportResult{}, nil, ErrCustomMailboxRequired
			}
			return domain.AliasImportResult{}, nil, ErrICloudMailboxRequired
		}
		if requiredMailboxType == domain.MailboxTypeCustom {
			if !state.Enabled {
				return domain.AliasImportResult{}, nil, ErrAccountDisabled
			}
			identityConflict, conflictErr := s.aliasImportAccountEmailConflictTx(ctx, tx, normalized)
			if conflictErr != nil {
				return domain.AliasImportResult{}, nil, fmt.Errorf("check account email before alias import: %w", conflictErr)
			}
			if identityConflict {
				return domain.AliasImportResult{}, nil, ErrAliasIdentityConflict
			}
			for _, candidate := range normalized {
				if candidate.Address == domain.NormalizeEmail(state.IMAPUsername) {
					return domain.AliasImportResult{}, nil, ErrAliasIdentityConflict
				}
				at := strings.LastIndexByte(candidate.Address, '@')
				if at <= 0 || candidate.Address[at+1:] != state.EmailSuffix {
					return domain.AliasImportResult{}, nil, ErrAliasSuffixMismatch
				}
			}
		}
	}

	result := domain.AliasImportResult{
		Created:   make([]domain.Alias, 0, len(normalized)),
		Existing:  make([]domain.Alias, 0, len(normalized)),
		Conflicts: make([]domain.AliasImportConflict, 0),
	}
	pending := make([]domain.AliasImportCandidate, 0, len(normalized))
	for _, candidate := range normalized {
		existing, findErr := s.getAliasByAddressTx(ctx, tx, candidate.Address)
		switch {
		case findErr == nil && existing.AccountID == accountID:
			if strict {
				return result, nil, fmt.Errorf("import aliases: %w", ErrAliasOwnershipConflict)
			}
			result.Existing = append(result.Existing, existing)
			continue
		case findErr == nil:
			result.Conflicts = append(result.Conflicts, domain.AliasImportConflict{
				Address:              candidate.Address,
				ExistingAliasID:      existing.ID,
				ExistingAccountID:    existing.AccountID,
				ExistingAccountEmail: existing.AccountEmail,
			})
			continue
		case findErr != ErrNotFound:
			return domain.AliasImportResult{}, nil, fmt.Errorf("find alias %q during import: %w", candidate.Address, findErr)
		}
		pending = append(pending, candidate)
	}
	if len(result.Conflicts) > 0 {
		return result, nil, fmt.Errorf("import aliases: %w", ErrAliasOwnershipConflict)
	}

	now := s.now()
	routingChanged := false
	var existingBeforeReconciliation []domain.Alias
	if reconcileAppleDirectory {
		existingBeforeReconciliation = append([]domain.Alias(nil), result.Existing...)
		// Delete missing and disable inactive addresses before allocating capacity to new
		// addresses. The account lock covers every operation, and any later
		// import failure rolls these back too.
		routingChanged, err = s.reconcileAppleAliasesTx(ctx, tx, accountID, normalized, directoryPresence, &result, now)
		if err != nil {
			return domain.AliasImportResult{}, nil, err
		}
	}

	limitEnabledAliases := mailboxHasEnabledAliasLimit(state.MailboxType)
	var enabledCount int
	if limitEnabledAliases {
		if err := s.txQueryRowContext(ctx, tx, `
			SELECT COUNT(*) FROM aliases
			WHERE account_id = ?
			  AND (enabled = TRUE OR (enabled = FALSE AND last_sync_error = ?))`,
			accountID, domain.AppleAliasConfirmationPending,
		).Scan(&enabledCount); err != nil {
			return domain.AliasImportResult{}, nil, fmt.Errorf("count enabled and confirmation-pending aliases before import: %w", err)
		}
	}
	if strict && limitEnabledAliases {
		requestedEnabled := 0
		for _, candidate := range pending {
			if candidate.Active {
				requestedEnabled++
			}
		}
		if requestedEnabled > domain.MaxEnabledAliasesPerAccount-enabledCount {
			return domain.AliasImportResult{}, nil, ErrAliasLimit
		}
	}

	type insertion struct {
		candidate domain.AliasImportCandidate
		enabled   bool
	}
	insertions := make([]insertion, 0, len(pending))
	for _, candidate := range pending {
		enabled := candidate.Active && (!limitEnabledAliases || enabledCount < domain.MaxEnabledAliasesPerAccount)
		if enabled {
			enabledCount++
		}
		insertions = append(insertions, insertion{candidate: candidate, enabled: enabled})
	}
	// PostgreSQL takes speculative unique-index locks as rows are inserted.
	// Every batch uses the same address order so overlapping cross-account
	// imports cannot acquire those locks in opposite order and deadlock.
	sort.Slice(insertions, func(left, right int) bool {
		return insertions[left].candidate.Address < insertions[right].candidate.Address
	})

	createdEnabled := false
	createdByAddress := make(map[string]domain.Alias, len(insertions))
	credentialsByAddress := make(map[string]domain.AliasImportCredential, len(insertions))
	for _, item := range insertions {
		candidate := item.candidate
		enabled := item.enabled
		credentialMode := domain.AliasCredentialModeLegacy
		initialHash := append([]byte(nil), candidate.APIKeyHash...)
		apiKeyPrefix := candidate.APIKeyPrefix
		if includeCredentials {
			credentialMode = domain.AliasCredentialModeV2
			apiKeyPrefix = ""
			initialHash, err = provisionalAliasHash(nil)
			if err != nil {
				return domain.AliasImportResult{}, nil, fmt.Errorf("create imported alias provisional credential: %w", err)
			}
		}
		var id int64
		initialStatus, initialSyncError := domain.SyncStatusPending, ""
		if reconcileAppleDirectory && !candidate.Active {
			initialStatus, initialSyncError = domain.SyncStatusError, domain.AppleAliasInactive
		}
		err = s.txQueryRowContext(ctx, tx, `
			INSERT INTO aliases(
				account_id, address, label, api_key_hash, api_key_prefix, credential_mode, enabled,
				last_sync_status, last_sync_error, last_synced_at,
				last_accessed_at, created_at, updated_at
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?)
			ON CONFLICT(address) DO NOTHING
			RETURNING id`,
			accountID, candidate.Address, candidate.Label, initialHash, apiKeyPrefix,
			credentialMode, enabled, initialStatus, initialSyncError,
			timestamp(now), timestamp(now),
		).Scan(&id)
		if err == sql.ErrNoRows {
			existing, findErr := s.getAliasByAddressTx(ctx, tx, candidate.Address)
			if findErr != nil {
				return domain.AliasImportResult{}, nil, fmt.Errorf(
					"read concurrent owner of alias %q: %w", candidate.Address, findErr,
				)
			}
			if existing.AccountID == accountID {
				if strict {
					return result, nil, fmt.Errorf("import aliases: %w", ErrAliasOwnershipConflict)
				}
				result.Existing = append(result.Existing, existing)
				continue
			}
			result.Created = result.Created[:0]
			result.ImportedDisabledCount = 0
			result.MissingCount = 0
			result.RemovedCount = 0
			result.InactiveUpdatedCount = 0
			result.RestoredCount = 0
			if reconcileAppleDirectory {
				result.Existing = existingBeforeReconciliation
			}
			result.Conflicts = append(result.Conflicts, domain.AliasImportConflict{
				Address:              candidate.Address,
				ExistingAliasID:      existing.ID,
				ExistingAccountID:    existing.AccountID,
				ExistingAccountEmail: existing.AccountEmail,
			})
			return result, nil, fmt.Errorf("import aliases: %w", ErrAliasOwnershipConflict)
		}
		if err != nil {
			return domain.AliasImportResult{}, nil, fmt.Errorf("import alias %q: %w", candidate.Address, err)
		}
		var material domain.AliasCredentialMaterial
		if includeCredentials {
			material, err = s.installGeneratedAliasCredentialsTx(ctx, tx, id, 1, true)
			if err != nil {
				return domain.AliasImportResult{}, nil, fmt.Errorf("create imported alias %q credentials: %w", candidate.Address, err)
			}
		}
		created, err := s.getAliasByIDTx(ctx, tx, id)
		if err != nil {
			return domain.AliasImportResult{}, nil, fmt.Errorf("read imported alias %q: %w", candidate.Address, err)
		}
		createdByAddress[candidate.Address] = created
		if includeCredentials {
			rawKey, revealErr := s.credentialRevealFactory(id, material.Ciphertext)
			if revealErr != nil {
				return domain.AliasImportResult{}, nil, fmt.Errorf("reveal imported alias %q API key: %w", candidate.Address, revealErr)
			}
			credentialsByAddress[candidate.Address] = domain.AliasImportCredential{Alias: created, APIKey: rawKey}
		}
		if enabled {
			createdEnabled = true
		} else if candidate.Active {
			result.ImportedDisabledCount++
		}
	}
	// Keep the public result in candidate order even though the writes use the
	// deterministic address order above.
	for _, candidate := range pending {
		if created, ok := createdByAddress[candidate.Address]; ok {
			result.Created = append(result.Created, created)
		}
	}
	issued := make([]domain.AliasImportCredential, 0, len(result.Created))
	for _, created := range result.Created {
		if credential, ok := credentialsByAddress[domain.NormalizeEmail(created.Address)]; ok {
			issued = append(issued, credential)
		}
	}
	if createdEnabled {
		if _, err := s.txExecContext(ctx, tx,
			`DELETE FROM imap_sync_states WHERE account_id = ?`, accountID,
		); err != nil {
			return domain.AliasImportResult{}, nil, fmt.Errorf("reset IMAP cursor after alias import: %w", err)
		}
	}
	if createdEnabled || routingChanged {
		if _, err := s.bumpAccountVersionTx(ctx, tx, accountID, accountVersion); err != nil {
			return domain.AliasImportResult{}, nil, fmt.Errorf("advance account version after alias import: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.AliasImportResult{}, nil, fmt.Errorf("commit alias import: %w", err)
	}
	return result, issued, nil
}

func isAppleDirectoryUnavailable(syncError string) bool {
	return syncError == domain.AppleAliasNotFound || syncError == domain.AppleAliasInactive
}

// reconcileAppleAliasesTx runs after the account lock and ownership preflight.
// All aliases absent from the complete directory are removed. Present
// confirmation-pending aliases remain managed by the creation confirmation
// workflow. Active entries clear availability markers
// without enabling aliases: a separate administrator action preserves their
// local enable/disable decision without requiring another persisted state.
func (s *Store) reconcileAppleAliasesTx(
	ctx context.Context,
	tx *sql.Tx,
	accountID int64,
	candidates []domain.AliasImportCandidate,
	directoryPresence map[string]struct{},
	result *domain.AliasImportResult,
	now time.Time,
) (bool, error) {
	remote := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		remote[candidate.Address] = candidate.Active
	}
	rows, err := s.txQueryContext(ctx, tx, `
		SELECT id, address, enabled, last_sync_status, last_sync_error
		FROM aliases WHERE account_id = ? ORDER BY address, id`, accountID)
	if err != nil {
		return false, fmt.Errorf("read aliases before Apple directory reconciliation: %w", err)
	}
	var locals []domain.Alias
	for rows.Next() {
		var alias domain.Alias
		if err := rows.Scan(&alias.ID, &alias.Address, &alias.Enabled, &alias.LastSyncStatus, &alias.LastSyncError); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("scan aliases before Apple directory reconciliation: %w", err)
		}
		locals = append(locals, alias)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, fmt.Errorf("iterate aliases before Apple directory reconciliation: %w", err)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("close aliases before Apple directory reconciliation: %w", err)
	}

	routingChanged := false
	changed := make(map[int64]struct{})
	for _, alias := range locals {
		address := domain.NormalizeEmail(alias.Address)
		if _, present := directoryPresence[address]; !present {
			deleted, err := s.txExecContext(ctx, tx,
				`DELETE FROM aliases WHERE id = ? AND account_id = ?`, alias.ID, accountID,
			)
			if err != nil {
				return false, fmt.Errorf("remove alias absent from Apple directory: %w", err)
			}
			if err := requireAffected(deleted, "alias"); err != nil {
				return false, fmt.Errorf("remove alias absent from Apple directory: %w", err)
			}
			if _, err := s.createAuditLogTx(ctx, tx, domain.AuditLog{
				Username: "system", Action: "sync_remove", ResourceType: "alias",
				ResourceID: strconv.FormatInt(alias.ID, 10), Result: "success",
				Detail: domain.AppleAliasNotFound, CreatedAt: now,
			}); err != nil {
				return false, fmt.Errorf("audit alias removal after Apple directory reconciliation: %w", err)
			}
			// Foreign keys remove alias-scoped state. Shared archived messages
			// and raw files belong to the account, not to an individual alias.
			result.MissingCount++
			result.RemovedCount++
			routingChanged = routingChanged || alias.Enabled
			continue
		}
		if alias.LastSyncError == domain.AppleAliasConfirmationPending {
			continue
		}
		active, applies := remote[address]
		if !applies {
			// The address exists at Apple but forwards to another destination.
			// Filtering import candidates must never make it look deleted.
			continue
		}
		if active {
			if !isAppleDirectoryUnavailable(alias.LastSyncError) {
				continue
			}
			if _, err := s.txExecContext(ctx, tx, `
				UPDATE aliases SET last_sync_status = ?, last_sync_error = '', updated_at = ?
				WHERE id = ? AND account_id = ?`, domain.SyncStatusPending, timestamp(now), alias.ID, accountID,
			); err != nil {
				return false, fmt.Errorf("clear active alias Apple directory marker: %w", err)
			}
			changed[alias.ID] = struct{}{}
			result.RestoredCount++
			continue
		}
		marker := domain.AppleAliasInactive
		if !alias.Enabled && alias.LastSyncStatus == domain.SyncStatusError && alias.LastSyncError == marker {
			continue
		}
		if _, err := s.txExecContext(ctx, tx, `
			UPDATE aliases SET enabled = FALSE, last_sync_status = ?, last_sync_error = ?, updated_at = ?
			WHERE id = ? AND account_id = ?`, domain.SyncStatusError, marker, timestamp(now), alias.ID, accountID,
		); err != nil {
			return false, fmt.Errorf("mark alias unavailable in Apple directory: %w", err)
		}
		changed[alias.ID] = struct{}{}
		result.InactiveUpdatedCount++
		if alias.Enabled {
			routingChanged = true
		}
	}
	// Return the final committed state for entries that were already local.
	for index, alias := range result.Existing {
		if _, ok := changed[alias.ID]; !ok {
			continue
		}
		updated, err := s.getAliasByIDTx(ctx, tx, alias.ID)
		if err != nil {
			return false, fmt.Errorf("read reconciled Apple alias: %w", err)
		}
		result.Existing[index] = updated
	}
	return routingChanged, nil
}

func normalizeAppleDirectoryAddresses(addresses []string) (map[string]struct{}, error) {
	normalized := make(map[string]struct{}, len(addresses))
	for index, address := range addresses {
		address = domain.NormalizeEmail(address)
		parsed, err := mail.ParseAddress(address)
		if address == "" || err != nil || domain.NormalizeEmail(parsed.Address) != address {
			return nil, fmt.Errorf("reconcile Apple aliases: directory address %d is invalid", index)
		}
		if _, duplicate := normalized[address]; duplicate {
			return nil, fmt.Errorf("reconcile Apple aliases: duplicate directory address %q", address)
		}
		normalized[address] = struct{}{}
	}
	return normalized, nil
}

func (s *Store) aliasImportAccountEmailConflictTx(
	ctx context.Context,
	tx *sql.Tx,
	candidates []domain.AliasImportCandidate,
) (bool, error) {
	if len(candidates) == 0 {
		return false, nil
	}
	args := make([]any, 0, len(candidates))
	placeholders := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		args = append(args, candidate.Address)
		placeholders = append(placeholders, "?")
	}
	var conflict bool
	err := s.txQueryRowContext(ctx, tx, `
		SELECT EXISTS(
			SELECT 1 FROM accounts
			WHERE email IN (`+strings.Join(placeholders, ", ")+`)
		)`, args...,
	).Scan(&conflict)
	return conflict, err
}

func normalizeAliasImportCandidates(candidates []domain.AliasImportCandidate) ([]domain.AliasImportCandidate, error) {
	normalized := make([]domain.AliasImportCandidate, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for index, candidate := range candidates {
		candidate.Address = domain.NormalizeEmail(candidate.Address)
		parsed, err := mail.ParseAddress(candidate.Address)
		if candidate.Address == "" || err != nil || domain.NormalizeEmail(parsed.Address) != candidate.Address {
			return nil, fmt.Errorf("import aliases: candidate %d has invalid address", index)
		}
		if _, exists := seen[candidate.Address]; exists {
			return nil, fmt.Errorf("import aliases: duplicate candidate address %q", candidate.Address)
		}
		candidate.Label = strings.TrimSpace(sanitizePostgresText(candidate.Label))
		candidate.APIKeyPrefix = strings.TrimSpace(sanitizePostgresText(candidate.APIKeyPrefix))
		candidate.APIKeyHash = append([]byte(nil), candidate.APIKeyHash...)
		seen[candidate.Address] = struct{}{}
		normalized = append(normalized, candidate)
	}
	return normalized, nil
}

func (s *Store) getAliasByAddressTx(ctx context.Context, tx *sql.Tx, address string) (domain.Alias, error) {
	return scanAlias(s.txQueryRowContext(ctx, tx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.address = ?`, address,
	))
}

func (s *Store) getAliasByIDTx(ctx context.Context, tx *sql.Tx, id int64) (domain.Alias, error) {
	return scanAlias(s.txQueryRowContext(ctx, tx,
		`SELECT `+aliasColumns+aliasJoins+` WHERE al.id = ?`, id,
	))
}
