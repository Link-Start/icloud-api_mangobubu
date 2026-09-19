package store

import (
	"errors"
	"fmt"
	"testing"

	"icloud-api/internal/domain"
)

func TestDeletionTargetsReadsThousandAndRejectsIncompleteSelection(t *testing.T) {
	s := openAliasDeletionJobTestStore(t, ":memory:")
	account, err := s.CreateAccount(t.Context(), domain.Account{
		Name: "selection", Email: "selection@icloud.com", IMAPHost: "imap.mail.me.com",
		IMAPPort: 993, IMAPUsername: "selection@icloud.com", PasswordCiphertext: "test", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 1000)
	for index := range ids {
		alias, err := s.CreateAlias(t.Context(), domain.Alias{
			AccountID: account.ID, Address: fmt.Sprintf("selected-%d@icloud.com", index),
			APIKeyHash: []byte(fmt.Sprintf("selection-key-%d", index)), Enabled: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[index] = alias.ID
	}
	selected, err := s.GetAliasesByIDs(t.Context(), ids)
	if err != nil || len(selected) != 1000 {
		t.Fatalf("selection size=%d, err=%v", len(selected), err)
	}
	for _, id := range ids {
		if selected[id].AccountID != account.ID {
			t.Fatalf("incorrect owner for alias %d", id)
		}
	}
	if _, err := s.GetAliasesByIDs(t.Context(), []int64{ids[0], ids[999] + 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("incomplete selection = %v", err)
	}
	for _, invalid := range [][]int64{{0}, {ids[0], ids[0]}, make([]int64, 1001)} {
		if _, err := s.GetAliasesByIDs(t.Context(), invalid); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
	if empty, err := s.GetAliasesByIDs(t.Context(), nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty selection = %v, %v", empty, err)
	}
}
