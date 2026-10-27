package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func sessionFixture(t *testing.T, s *Store, key Key, snaps ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `INSERT INTO accounts(tenant_id,account_id,revision) VALUES($1,$2,$3)`, key.TenantID, key.AccountID, len(snaps)); err != nil {
		t.Fatal(err)
	}
	created := map[string]bool{}
	for i, snap := range snaps {
		var doc any
		if err := json.Unmarshal([]byte(snap), &doc); err != nil {
			t.Fatal(err)
		}
		state := doc.(map[string]any)
		parent := state["parent"].(map[string]any)
		id := parent["id"].(string)
		kind := "update"
		if !created[id] {
			kind = "create"
			created[id] = true
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO account_journal(tenant_id,account_id,revision,kind,detail) VALUES($1,$2,$3,$4,$5)`, key.TenantID, key.AccountID, i+1, kind, map[string]any{"snapshot": doc}); err != nil {
			t.Fatal(err)
		}
	}
}

func snapshotForSession(id, note string) string {
	return fmt.Sprintf(`{"version":"account-state-v1","profile":"paper","data_source":"replay","base_currency":"USD","quote_currency":"BTC","parent":{"id":%q,"note":%q}}`, id, note)
}

func TestSessionsHistoricalSnapshotsAndExclusiveCreationPagination(t *testing.T) {
	s := testStore(t)
	key := Key{"tenant-a", "acct-a"}
	sessionFixture(t, s, key, snapshotForSession("old", "created"), snapshotForSession("old", "updated"), snapshotForSession("new", "created"), snapshotForSession("new", "current"))
	page, err := s.Sessions(context.Background(), key, 0, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.AsOfRevision != 3 || len(page.Entries) != 1 || page.Entries[0].PlanID != "new" || page.Entries[0].CreatedRevision != 3 || page.Entries[0].Revision != 3 {
		t.Fatalf("unexpected first historical page: %+v", page)
	}
	older, err := s.Sessions(context.Background(), key, page.Entries[0].CreatedRevision, 3, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Entries) != 1 || older.Entries[0].PlanID != "old" || older.Entries[0].Revision != 2 || older.Entries[0].Profile != "paper" || string(older.Entries[0].Parent) == "" {
		t.Fatalf("bad older page: %+v", older)
	}
	got, err := s.Session(context.Background(), key, "old", 3)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got.Snapshot, &decoded); err != nil {
		t.Fatal(err)
	}
	parent, _ := decoded["parent"].(map[string]any)
	if got.AsOfRevision != 3 || got.CreatedRevision != 1 || got.Revision != 2 || parent["note"] != "updated" {
		t.Fatalf("session didn't return latest snapshot at as-of revision: %+v", got)
	}
	got, err = s.Session(context.Background(), key, "old", 0)
	if err != nil || got.Revision != 2 {
		t.Fatalf("default as-of lookup: %+v %v", got, err)
	}
}

func TestSessionListMetadataAndTerminalSnapshotUseLatestSessionState(t *testing.T) {
	s := testStore(t)
	key := Key{"tenant-a", "acct-a"}
	sessionFixture(t, s, key,
		`{"profile":"paper","data_source":"replay","base_currency":"USD","quote_currency":"BTC","parent":{"id":"plan","status":"active","phase":"starting"},"book":{"mid":"100"}}`,
		`{"profile":"paper","data_source":"replay","base_currency":"USD","quote_currency":"BTC","parent":{"id":"plan","status":"completed","phase":"done"},"book":{"mid":"101"}}`,
		`{"profile":"shadow","data_source":"feed-b","base_currency":"EUR","quote_currency":"ETH","parent":{"id":"plan","status":"completed","phase":"done"},"book":{"mid":"102"}}`,
	)
	page, err := s.Sessions(context.Background(), key, 0, 3, 5)
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("session list: %+v %v", page, err)
	}
	meta := page.Entries[0]
	var parent map[string]any
	if err = json.Unmarshal(meta.Parent, &parent); err != nil {
		t.Fatal(err)
	}
	if meta.Revision != 3 || meta.Profile != "shadow" || meta.DataSource != "feed-b" || meta.BaseCurrency != "EUR" || meta.QuoteCurrency != "ETH" || parent["status"] != "completed" {
		t.Fatalf("list metadata came from creation snapshot instead of latest: %+v parent=%+v", meta, parent)
	}
	beforeFinal, err := s.Session(context.Background(), key, "plan", 1)
	if err != nil || beforeFinal.TerminalRevision != 0 || len(beforeFinal.TerminalSnapshot) != 0 {
		t.Fatalf("terminal state appeared before final status: %+v %v", beforeFinal, err)
	}
	ended, err := s.Session(context.Background(), key, "plan", 3)
	if err != nil {
		t.Fatalf("read terminal session: %v", err)
	}
	var terminal map[string]any
	if err = json.Unmarshal(ended.TerminalSnapshot, &terminal); err != nil {
		t.Fatal(err)
	}
	terminalBook, _ := terminal["book"].(map[string]any)
	if ended.TerminalRevision != 2 || ended.TerminalAt.IsZero() || terminalBook["mid"] != "101" {
		t.Fatalf("terminal snapshot wasn't frozen at first final transition: %+v terminal=%+v", ended, terminal)
	}
}

func TestSessionsScopeBoundsAndReadOnly(t *testing.T) {
	s := testStore(t)
	key := Key{"tenant-a", "acct-a"}
	snap := snapshotForSession("plan", "x")
	sessionFixture(t, s, key, snap)
	other := Key{"tenant-b", "acct-a"}
	sessionFixture(t, s, other, snapshotForSession("secret", "x"))
	before := mustAccount(t, s, key)
	p, err := s.Sessions(context.Background(), key, 0, 0, 50)
	if err != nil || len(p.Entries) != 1 || p.Entries[0].PlanID != "plan" {
		t.Fatalf("scoped session list %+v %v", p, err)
	}
	if _, err = s.Session(context.Background(), key, "secret", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant plan lookup err=%v", err)
	}
	if _, err = s.Session(context.Background(), Key{"missing", "acct-a"}, "plan", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account err=%v", err)
	}
	after := mustAccount(t, s, key)
	if before.Revision != after.Revision || string(before.Snapshot) != string(after.Snapshot) {
		t.Fatal("read-only session query mutated account")
	}
	for _, tc := range []struct {
		before, asof int64
		limit        int
	}{{-1, 0, 1}, {0, -1, 1}, {0, 99, 1}, {0, 0, 0}, {0, 0, 51}} {
		if _, err = s.Sessions(context.Background(), key, tc.before, tc.asof, tc.limit); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Sessions(%+v) err=%v", tc, err)
		}
	}
	for _, asof := range []int64{-1, 99} {
		if _, err = s.Session(context.Background(), key, "plan", asof); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Session as-of %d err=%v", asof, err)
		}
	}
	if _, err = s.Session(context.Background(), key, "absent", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing plan err=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.Sessions(ctx, key, 0, 0, 5); err == nil {
		t.Fatal("canceled list context succeeded")
	}
	if _, err = s.Session(ctx, key, "plan", 0); err == nil {
		t.Fatal("canceled detail context succeeded")
	}
}

func TestSessionRejectsOversizedSnapshotAndParent(t *testing.T) {
	s := testStore(t)
	key := Key{"tenant", "acct"}
	large := `{"parent":{"id":"large"},"padding":"` + strings.Repeat("x", (1<<20)+1) + `"}`
	sessionFixture(t, s, key, large)
	if _, err := s.Session(context.Background(), key, "large", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized snapshot err=%v", err)
	}
	if _, err := s.Sessions(context.Background(), key, 0, 0, 5); err != nil {
		t.Fatalf("list should not load/reject oversized snapshot body: %v", err)
	}
	s2 := testStore(t)
	key2 := Key{"tenant", "acct"}
	longID := strings.Repeat("p", (16<<10)+1)
	sessionFixture(t, s2, key2, snapshotForSession(longID, "x"))
	if _, err := s2.Sessions(context.Background(), key2, 0, 0, 5); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized parent err=%v", err)
	}
}

func TestSessionMetadataDoesNotRequireDataSource(t *testing.T) {
	s := testStore(t)
	key := Key{"tenant", "acct"}
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO accounts(tenant_id,account_id,revision) VALUES($1,$2,1)`, key.TenantID, key.AccountID); err != nil {
		t.Fatal(err)
	}
	_, err := s.pool.Exec(context.Background(), `INSERT INTO account_journal(tenant_id,account_id,revision,kind,detail,created_at) VALUES($1,$2,1,'create',$3,$4)`, key.TenantID, key.AccountID, `{"snapshot":{"parent":{"id":"legacy"},"profile":"paper"}}`, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Sessions(context.Background(), key, 0, 0, 5)
	if err != nil || len(p.Entries) != 1 || p.Entries[0].DataSource != "" {
		t.Fatalf("legacy empty source: %+v %v", p, err)
	}
}

func TestSessionJournalPagesOnlyScopedMetadata(t *testing.T) {
	s := testStore(t)
	key := Key{"tenant-a", "acct-a"}
	sessionFixture(t, s, key, snapshotForSession("old", "one"), snapshotForSession("old", "two"), snapshotForSession("new", "three"))
	page, err := s.SessionJournal(context.Background(), key, "old", 0, 3, 1)
	if err != nil || len(page) != 1 || page[0].Revision != 1 || page[0].Kind != "create" {
		t.Fatalf("first scoped event page: %+v %v", page, err)
	}
	page, err = s.SessionJournal(context.Background(), key, "old", page[0].Revision, 3, 200)
	if err != nil || len(page) != 1 || page[0].Revision != 2 || page[0].Kind != "update" {
		t.Fatalf("second scoped event page: %+v %v", page, err)
	}
	empty, err := s.SessionJournal(context.Background(), key, "old", 3, 3, 2)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty page for existing session: %+v %v", empty, err)
	}
	if _, err = s.SessionJournal(context.Background(), key, "missing", 3, 3, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session on empty page: %v", err)
	}
	for _, tc := range []struct {
		after, asof int64
		limit       int
	}{{-1, 0, 1}, {0, -1, 1}, {0, 4, 1}, {0, 0, 0}, {0, 0, 201}} {
		if _, err = s.SessionJournal(context.Background(), key, "old", tc.after, tc.asof, tc.limit); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid SessionJournal bounds %+v: %v", tc, err)
		}
	}
}
