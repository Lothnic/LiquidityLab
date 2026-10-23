package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	d "github.com/liquiditylab/liquiditylab/internal/domain"
)

func historyApply(t *testing.T, store *Store, key Key, owner Ownership, account Account, id, kind string) Account {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := store.Apply(ctx, Change{
		Key: key, Ownership: owner,
		Command:  Command{ID: id, Kind: kind, ExpectedRevision: account.Revision},
		Snapshot: json.RawMessage(`{"history_test":true}`),
	})
	if err != nil {
		t.Fatalf("append journal entry %s: %v", id, err)
	}
	updated, err := store.GetAccount(ctx, key)
	if err != nil {
		t.Fatalf("read account after journal append: %v", err)
	}
	return updated
}

func historyAccount(t *testing.T, store *Store, key Key, tenantLabel string) (Ownership, Account) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := store.CreateAccount(ctx, key,
		[]Balance{{Currency: "USD", Total: d.One, Free: d.One, Locked: d.Zero}},
		json.RawMessage(`{"history_test":true}`))
	if err != nil {
		t.Fatalf("create account %s: %v", tenantLabel, err)
	}
	owner, err := store.ClaimOwnership(ctx, key, "history-worker-"+tenantLabel, time.Minute, RecoveryProof{})
	if err != nil {
		t.Fatalf("claim account %s: %v", tenantLabel, err)
	}
	account, err := store.GetAccount(ctx, key)
	if err != nil {
		t.Fatalf("read account %s: %v", tenantLabel, err)
	}
	return owner, account
}

func historyRead(t *testing.T, store *Store, key Key, after int64, limit int) ([]JournalEntry, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return store.Journal(ctx, key, after, limit)
}

func TestPostgresJournalPagesStrictlyByRevisionWithoutMutation(t *testing.T) {
	store := testStore(t)
	key, owner, account := initialized(t, store)
	account = historyApply(t, store, key, owner, account, "history-state-1", "state_change")
	account = historyApply(t, store, key, owner, account, "history-state-2", "fill")
	account = historyApply(t, store, key, owner, account, "history-state-3", "pause")

	before := mustAccount(t, store, key)
	first, err := historyRead(t, store, key, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Revision != 1 || first[0].Kind != "ownership" || first[1].Revision != 2 || first[1].Kind != "reconcile" {
		t.Fatalf("first page is not ascending or correctly bounded: %+v", first)
	}
	second, err := historyRead(t, store, key, first[len(first)-1].Revision, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].Revision != 3 || second[0].Kind != "state_change" || second[1].Revision != 4 || second[1].Kind != "fill" {
		t.Fatalf("second page did not continue strictly after cursor: %+v", second)
	}
	tail, err := historyRead(t, store, key, second[len(second)-1].Revision, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 1 || tail[0].Revision != 5 || tail[0].Kind != "pause" {
		t.Fatalf("third page did not return final journal entry: %+v", tail)
	}
	empty, err := historyRead(t, store, key, tail[0].Revision, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("history tail after latest revision should be empty: %+v", empty)
	}
	after := mustAccount(t, store, key)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("Journal mutated account state: before=%+v after=%+v", before, after)
	}
}

func TestPostgresJournalScopesTenantAndAccount(t *testing.T) {
	store := testStore(t)
	key, owner, account := initialized(t, store)
	account = historyApply(t, store, key, owner, account, "tenant-a-account-a-entry", "tenant_a_account_a")

	otherAccountKey := Key{TenantID: "tenant-a", AccountID: "account-b"}
	otherAccountOwner, otherAccount := historyAccount(t, store, otherAccountKey, "tenant-a-account-b")
	historyApply(t, store, otherAccountKey, otherAccountOwner, otherAccount, "tenant-a-account-b-entry", "tenant_a_account_b")

	otherTenantKey := Key{TenantID: "tenant-b", AccountID: "account-a"}
	otherTenantOwner, otherTenant := historyAccount(t, store, otherTenantKey, "tenant-b-account-a")
	historyApply(t, store, otherTenantKey, otherTenantOwner, otherTenant, "tenant-b-account-a-entry", "tenant_b_account_a")

	entries, err := historyRead(t, store, key, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[2].Kind != "tenant_a_account_a" {
		t.Fatalf("primary account journal leaked or missed metadata: %+v", entries)
	}
	for _, tc := range []struct {
		key  Key
		kind string
	}{
		{key: otherAccountKey, kind: "tenant_a_account_b"},
		{key: otherTenantKey, kind: "tenant_b_account_a"},
	} {
		values, err := historyRead(t, store, tc.key, 0, 100)
		if err != nil {
			t.Fatalf("read isolated journal for %+v: %v", tc.key, err)
		}
		if len(values) != 2 || values[1].Kind != tc.kind || values[0].Kind != "ownership" {
			t.Fatalf("journal crossed tenant/account boundary for %+v: %+v", tc.key, values)
		}
	}
	if _, err := historyRead(t, store, Key{TenantID: "tenant-missing", AccountID: key.AccountID}, 0, 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nonexistent scoped account error=%v, want ErrNotFound", err)
	}
}

func TestPostgresJournalValidatesBoundsAndKey(t *testing.T) {
	store := testStore(t)
	key, _, _ := initialized(t, store)
	for _, tc := range []struct {
		name  string
		key   Key
		after int64
		limit int
	}{
		{name: "empty tenant", key: Key{AccountID: key.AccountID}, limit: 1},
		{name: "empty account", key: Key{TenantID: key.TenantID}, limit: 1},
		{name: "negative cursor", key: key, after: -1, limit: 1},
		{name: "zero limit", key: key, limit: 0},
		{name: "limit over maximum", key: key, limit: 1001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := historyRead(t, store, tc.key, tc.after, tc.limit); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Journal error=%v, want ErrInvalid", err)
			}
		})
	}
	if _, err := historyRead(t, store, key, 0, 1000); err != nil {
		t.Fatalf("maximum valid page size rejected: %v", err)
	}
}
