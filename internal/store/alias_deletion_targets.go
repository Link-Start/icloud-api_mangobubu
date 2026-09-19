package store

import (
	"context"
	"fmt"
	"strings"

	"icloud-api/internal/domain"
)

// GetAliasesByIDs reads one bounded deletion selection in a single round trip.
// Missing IDs reject the whole selection, just as individual GetAlias calls do.
func (s *Store) GetAliasesByIDs(ctx context.Context, ids []int64) (map[int64]domain.Alias, error) {
	aliases := make(map[int64]domain.Alias, len(ids))
	if len(ids) == 0 {
		return aliases, nil
	}
	if len(ids) > maxAliasDeletionJobItems {
		return nil, fmt.Errorf("alias selection exceeds %d items", maxAliasDeletionJobItems)
	}
	args := make([]any, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for index, id := range ids {
		if id < 1 {
			return nil, fmt.Errorf("alias ID must be positive")
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("duplicate alias ID")
		}
		seen[id] = struct{}{}
		args[index] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := s.queryContext(ctx, "SELECT "+aliasColumns+aliasJoins+" WHERE al.id IN ("+placeholders+")", args...)
	if err != nil {
		return nil, fmt.Errorf("read selected aliases: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		alias, err := scanAlias(rows)
		if err != nil {
			return nil, err
		}
		aliases[alias.ID] = alias
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(aliases) != len(ids) {
		return nil, ErrNotFound
	}
	return aliases, nil
}
