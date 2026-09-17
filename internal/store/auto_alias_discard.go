package store

import (
	"context"
	"database/sql"
	"fmt"

	"icloud-api/internal/domain"
)

// DiscardPendingAutoAlias removes only an unpublished automatic candidate and
// its cascading local records. The caller must first establish that the address
// is absent from Apple's directory; this method makes no remote changes.
// ErrNotFound means the account/alias does not identify a disabled candidate
// with both the pending-confirmation marker and a pending API key. In particular,
// a candidate published concurrently is protected by the same account lock and
// the DELETE predicate. Unpublished candidates do not affect the IMAP cursor or
// account version, so discarding one preserves both.
func (s *Store) DiscardPendingAutoAlias(ctx context.Context, accountID, aliasID int64) error {
	if accountID < 1 || aliasID < 1 {
		return fmt.Errorf("discard pending automatic alias: identity is invalid")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin pending automatic alias discard: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockAccountForUpdate(ctx, tx, accountID); err != nil {
		return fmt.Errorf("lock account for pending automatic alias discard: %w", err)
	}
	result, err := s.txExecContext(ctx, tx, `
		DELETE FROM aliases
		WHERE id = ? AND account_id = ?
		  AND enabled = FALSE AND last_sync_error = ?
		  AND EXISTS (
			SELECT 1 FROM pending_alias_api_keys p WHERE p.alias_id = aliases.id
		  )`, aliasID, accountID, domain.AppleAliasConfirmationPending)
	if err != nil {
		return fmt.Errorf("discard pending automatic alias: %w", err)
	}
	if err := requireAffected(result, "pending automatic alias"); err != nil {
		return fmt.Errorf("discard pending automatic alias: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pending automatic alias discard: %w", err)
	}
	return nil
}
