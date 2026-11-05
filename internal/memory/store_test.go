package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

var testNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func testStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return testNow }
	return s
}

func putItem(t *testing.T, s *Store, text, scope string) Item {
	t.Helper()
	item, err := s.Put(context.Background(), Item{Text: text, Scope: scope, Source: "user-confirmed", Importance: 0.6})
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func searchItems(t *testing.T, s *Store, query string, scopes []string, limit, budget int) []Hit {
	t.Helper()
	hits, err := s.Search(context.Background(), query, scopes, limit, budget)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func TestTokensAndChineseSearch(t *testing.T) {
	want := []string{"服务", "务器", "磁盘", "nginx", "42", "a", "中"}
	if got := tokens("服务器，磁盘 NGINX-42 A中"); !reflect.DeepEqual(got, want) {
		t.Fatalf("tokens=%q, want %q", got, want)
	}
	s := testStore(t)
	target := putItem(t, s, "生产服务器磁盘空间不足时，先检查日志占用。NGINX 使用独立日志目录。", "owner")
	putItem(t, s, "备份应保存到远程存储。", "owner")
	for _, query := range []string{"磁盘空间", "服务器磁盘", "nGiNx"} {
		hits := searchItems(t, s, query, []string{"owner"}, 5, 6000)
		if len(hits) == 0 || hits[0].Item.ID != target.ID || hits[0].Score <= 0 {
			t.Fatalf("query %q: %#v", query, hits)
		}
		if !strings.Contains(hits[0].Why[0], "不代表正确性") {
			t.Fatalf("missing relevance warning: %#v", hits[0].Why)
		}
	}
	metadata, err := s.Put(context.Background(), Item{Text: "每周维护", Subject: "PostgreSQL", Scope: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if hits := searchItems(t, s, "postgresql", []string{"owner"}, 5, 6000); len(hits) != 1 || hits[0].Item.ID != metadata.ID {
		t.Fatalf("subject should be indexed: %#v", hits)
	}
	if got := searchItems(t, s, "没有索引匹配", []string{"owner"}, 5, 6000); len(got) != 0 {
		t.Fatalf("unexpected unrelated hits: %#v", got)
	}
}

func TestScopeFilteredBeforeBM25AndLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	group := "group:onebot:123:456"
	target := putItem(t, s, "本群的磁盘日志处理经验", group)
	putItem(t, s, "本群的备份恢复经验", group)
	before := searchItems(t, s, "磁盘日志", []string{group}, 1, 6000)
	if len(before) != 1 || before[0].Item.ID != target.ID {
		t.Fatalf("missing group hit: %#v", before)
	}
	// These exact/short matches would crowd out the group if a global LIMIT or
	// corpus statistics were used before authorization filtering.
	for i := 0; i < 16; i++ {
		putItem(t, s, "磁盘日志", "owner")
		putItem(t, s, "磁盘日志", "group:onebot:123:other")
	}
	for _, scope := range []string{"group:onebot:999:456", "group:official:123:456"} {
		putItem(t, s, "磁盘日志", scope)
	}
	archived := putItem(t, s, "磁盘日志", group)
	if err := s.SetStatus(ctx, archived.ID, StatusArchived, ""); err != nil {
		t.Fatal(err)
	}
	expired := testNow.Add(-time.Second)
	if _, err := s.Put(ctx, Item{Text: "磁盘日志", Scope: group, ExpiresAt: &expired}); err != nil {
		t.Fatal(err)
	}
	after := searchItems(t, s, "磁盘日志", []string{group}, 1, 6000)
	if len(after) != 1 || after[0].Item.ID != target.ID {
		t.Fatalf("out-of-scope items affected selection: %#v", after)
	}
	if math.Abs(before[0].Score-after[0].Score) > 1e-12 {
		t.Fatalf("out-of-scope/inactive records affected BM25: %.15f -> %.15f", before[0].Score, after[0].Score)
	}
	duplicateScopes := searchItems(t, s, "磁盘日志", []string{group, group}, 1, 6000)
	if duplicateScopes[0].Score != after[0].Score {
		t.Fatal("duplicate scope changed score")
	}
	if got := searchItems(t, s, "磁盘日志", nil, 5, 6000); len(got) != 0 {
		t.Fatal("empty scopes must fail closed")
	}
	for _, scopes := range [][]string{{""}, {"*"}, {"owner' OR 1=1 --"}, {"group:onebot::456"}, {"group:onebot:123:456:extra"}} {
		if _, err := s.Search(ctx, "磁盘日志", scopes, 5, 6000); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid scopes %q accepted: %v", scopes, err)
		}
	}
}

func TestSearchBudgetsAndReadOnly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 14; i++ {
		putItem(t, s, "磁盘日志", "owner")
	}
	before, err := s.List(ctx, "", "", MaxItems)
	if err != nil {
		t.Fatal(err)
	}
	versionsBefore := countRows(t, s.db, "SELECT COUNT(*) FROM mem_versions")
	hits := searchItems(t, s, "磁盘", []string{"owner"}, 999, 999999)
	if len(hits) != MaxResults {
		t.Fatalf("result cap: got %d, want %d", len(hits), MaxResults)
	}
	body, _ := json.Marshal(hits)
	if utf8.RuneCount(body) > MaxSearchChars {
		t.Fatal("maximum character budget exceeded")
	}
	one := searchItems(t, s, "磁盘", []string{"owner"}, 1, 6000)
	oneBody, _ := json.Marshal(one)
	exact := utf8.RuneCount(oneBody)
	if got := searchItems(t, s, "磁盘", []string{"owner"}, 1, exact); len(got) != 1 {
		t.Fatalf("exact Unicode JSON budget should fit: %d", exact)
	}
	if got := searchItems(t, s, "磁盘", []string{"owner"}, 1, exact-1); len(got) != 0 {
		t.Fatal("one-character-short budget should not return a partial hit")
	}
	for _, budget := range []int{-1, 0, 1, 50, 400, 700, 1800, 6000} {
		got := searchItems(t, s, "磁盘", []string{"owner"}, 10, budget)
		encoded, _ := json.Marshal(got)
		if len(got) > 0 && utf8.RuneCount(encoded) > budget {
			t.Fatalf("budget %d exceeded by %d", budget, utf8.RuneCount(encoded))
		}
	}
	if got := searchItems(t, s, "磁盘", []string{"owner"}, 0, 6000); len(got) != 0 {
		t.Fatal("zero result limit must return nothing")
	}
	if got := searchItems(t, s, " \n?!", []string{"owner"}, 5, 6000); len(got) != 0 {
		t.Fatal("tokenless query must return nothing")
	}
	if _, err := s.Search(ctx, strings.Repeat("中", MaxQueryRunes+1), []string{"owner"}, 5, 6000); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long query accepted: %v", err)
	}
	after, err := s.List(ctx, "", "", MaxItems)
	if err != nil || !reflect.DeepEqual(before, after) || countRows(t, s.db, "SELECT COUNT(*) FROM mem_versions") != versionsBefore {
		t.Fatalf("search mutated memory or versions: %v", err)
	}
}

func TestExpirationAndLifecycleFiltering(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	past, exact, future := testNow.Add(-time.Nanosecond), testNow, testNow.Add(time.Nanosecond)
	for _, expires := range []*time.Time{&past, &exact, &future, nil} {
		if _, err := s.Put(ctx, Item{Text: "过期验证", Scope: "owner", ExpiresAt: expires}); err != nil {
			t.Fatal(err)
		}
	}
	archived := putItem(t, s, "过期验证", "owner")
	if err := s.SetStatus(ctx, archived.ID, "archive", ""); err != nil {
		t.Fatal(err)
	}
	state, _ := s.Get(ctx, archived.ID)
	if state.Status != "archived" {
		t.Fatalf("archive input must normalize: %q", state.Status)
	}
	superseded := putItem(t, s, "过期验证", "owner")
	if err := s.SetStatus(ctx, superseded.ID, StatusSuperseded, ""); err != nil {
		t.Fatal(err)
	}
	hits := searchItems(t, s, "过期验证", []string{"owner"}, 10, 6000)
	if len(hits) != 2 {
		t.Fatalf("want future + no expiry, got %#v", hits)
	}
	all, err := s.List(ctx, "", "", 100)
	if err != nil || len(all) != 6 {
		t.Fatalf("management must retain inactive records: %d, %v", len(all), err)
	}
	s.now = func() time.Time { return testNow.Add(time.Nanosecond) }
	if hits = searchItems(t, s, "过期验证", []string{"owner"}, 10, 6000); len(hits) != 1 || hits[0].Item.ExpiresAt != nil {
		t.Fatalf("expiry boundary not excluded: %#v", hits)
	}
}

func TestPutRevisionsAndDeletionForgetVersions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	item := putItem(t, s, "旧记录甲乙", "owner")
	if item.ID == "" || item.CreatedAt.IsZero() || item.Status != StatusActive {
		t.Fatalf("missing server metadata: %#v", item)
	}
	duplicate := putItem(t, s, "旧记录甲乙", "owner")
	if duplicate.ID == item.ID {
		t.Fatal("duplicates must not automatically overwrite")
	}
	if err := s.Delete(ctx, duplicate.ID); err != nil {
		t.Fatal(err)
	}
	old := item
	item.Text = "新记录丙丁"
	item.CreatedAt = testNow.Add(-24 * time.Hour) // Client cannot rewrite creation time.
	item, err := s.Put(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	if !item.CreatedAt.Equal(old.CreatedAt) || !item.UpdatedAt.After(old.UpdatedAt) {
		t.Fatal("revision timestamps are not server-owned/monotonic")
	}
	if _, err := s.Put(ctx, old); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	if got := searchItems(t, s, "甲乙", []string{"owner"}, 5, 6000); len(got) != 0 {
		t.Fatal("old text survived in live index")
	}
	if got := searchItems(t, s, "丙丁", []string{"owner"}, 5, 6000); len(got) != 1 {
		t.Fatal("new text missing from live index")
	}
	if err := s.SetStatus(ctx, item.ID, StatusArchived, ""); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s.db, "SELECT COUNT(*) FROM mem_versions WHERE item_id=?", item.ID); got != 3 {
		t.Fatalf("initial/revision/status versions = %d, want 3", got)
	}
	var snapshot string
	if err := s.db.QueryRow("SELECT snapshot FROM mem_versions WHERE item_id=? AND version=1", item.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	var initial Item
	if err := json.Unmarshal([]byte(snapshot), &initial); err != nil || initial.Text != old.Text {
		t.Fatalf("initial history not retained: %v", err)
	}
	if err := s.SetStatus(ctx, item.ID, StatusArchived, ""); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s.db, "SELECT COUNT(*) FROM mem_versions WHERE item_id=?", item.ID); got != 3 {
		t.Fatal("no-op status created a spurious version")
	}
	current, _ := s.Get(ctx, item.ID)
	current.Text, current.Status = "修订戊己", ""
	current, err = s.Put(ctx, current)
	if err != nil || current.Status != StatusArchived {
		t.Fatalf("editing archived item silently restored it: %v, %#v", err, current)
	}
	if got := searchItems(t, s, "戊己", []string{"owner"}, 5, 6000); len(got) != 0 {
		t.Fatal("archived revision visible in search")
	}
	// New does not rebuild from history or silently reactivate anything.
	reopened, err := New(s.db)
	if err != nil {
		t.Fatal(err)
	}
	reopened.now = s.now
	if got := searchItems(t, reopened, "戊己", []string{"owner"}, 5, 6000); len(got) != 0 {
		t.Fatal("initialization resurrected archived content")
	}
	if err := s.Delete(ctx, item.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"mem_items", "mem_terms", "mem_versions"} {
		if countRows(t, s.db, "SELECT COUNT(*) FROM "+table) != 0 {
			t.Fatalf("Delete left records in %s", table)
		}
	}
	if _, err := s.Get(ctx, item.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted Get: %v", err)
	}
	if _, err := s.Put(ctx, current); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale Put resurrected deleted item: %v", err)
	}
	if err := s.SetStatus(ctx, item.ID, StatusActive, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("status operation resurrected deleted item: %v", err)
	}
	if got := searchItems(t, reopened, "戊己 甲乙 丙丁", []string{"owner"}, 5, 6000); len(got) != 0 {
		t.Fatal("deleted memory remains retrievable")
	}
}

func TestReplacementScopeAndAtomicValidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	original := putItem(t, s, "旧备份路径", "owner")
	replacement := putItem(t, s, "新备份路径", "owner")
	foreign := putItem(t, s, "其他群的备份路径", "group:official:bot:room")
	for _, change := range []struct{ status, replacement string }{
		{StatusSuperseded, foreign.ID}, {StatusSuperseded, original.ID},
		{StatusSuperseded, "missing"}, {StatusActive, replacement.ID}, {"deleted", ""},
	} {
		if err := s.SetStatus(ctx, original.ID, change.status, change.replacement); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid lifecycle transition accepted: %#v, %v", change, err)
		}
	}
	if got := countRows(t, s.db, "SELECT COUNT(*) FROM mem_versions WHERE item_id=?", original.ID); got != 1 {
		t.Fatal("invalid changes wrote history")
	}
	if err := s.SetStatus(ctx, original.ID, StatusSuperseded, replacement.ID); err != nil {
		t.Fatal(err)
	}
	state, _ := s.Get(ctx, original.ID)
	if state.Status != StatusSuperseded || state.SupersededBy != replacement.ID {
		t.Fatalf("replacement metadata missing: %#v", state)
	}
	if err := s.Delete(ctx, replacement.ID); err != nil {
		t.Fatal(err)
	}
	state, _ = s.Get(ctx, original.ID)
	if state.Status != StatusSuperseded {
		t.Fatal("deleting replacement automatically revived the old memory")
	}
	if err := s.SetStatus(ctx, original.ID, StatusActive, ""); err != nil {
		t.Fatal(err)
	}
	state, _ = s.Get(ctx, original.ID)
	if state.Status != StatusActive || state.SupersededBy != "" {
		t.Fatal("explicit restoration failed")
	}
}

func TestSecretRejectionAndBounds(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	secrets := []string{
		"sk-test-only-placeholder", "-----BEGIN PRIVATE KEY-----",
		"-----BEGIN RSA PRIVATE KEY-----", "-----BEGIN OPENSSH PRIVATE KEY-----",
		"Authorization: Bearer example-placeholder", "Authorization Bearer example-placeholder",
		"authorization = Basic example-placeholder", `{"password":"example-placeholder"}`,
		"密码：示例口令", "密码是示例口令", "api_key = example-placeholder",
		"client_secret: example-placeholder", "token: example-placeholder",
		"https://example:placeholder@example.invalid/",
	}
	for _, secret := range secrets {
		for _, field := range []string{"text", "subject", "source"} {
			item := Item{Text: "安全运维经验", Scope: "owner", Importance: 0.5}
			switch field {
			case "text":
				item.Text = secret
			case "subject":
				item.Subject = secret
			case "source":
				item.Source = secret
			}
			if _, err := s.Put(ctx, item); !errors.Is(err, ErrSecret) {
				t.Fatalf("%s secret not rejected: %v", field, err)
			} else if strings.Contains(err.Error(), secret) {
				t.Fatal("error leaked rejected content")
			}
		}
	}
	if countRows(t, s.db, "SELECT COUNT(*) FROM mem_items") != 0 || countRows(t, s.db, "SELECT COUNT(*) FROM mem_versions") != 0 {
		t.Fatal("rejected secret persisted")
	}
	good := putItem(t, s, "不要在记忆中保存密码，使用安全配置入口。", "owner")
	badRevision := good
	badRevision.Text = "密码：示例口令"
	if _, err := s.Put(ctx, badRevision); !errors.Is(err, ErrSecret) {
		t.Fatal("secret revision accepted")
	}
	current, _ := s.Get(ctx, good.ID)
	if current.Text != good.Text || countRows(t, s.db, "SELECT COUNT(*) FROM mem_versions WHERE item_id=?", good.ID) != 1 {
		t.Fatal("failed revision was not atomic")
	}
	if _, err := s.Put(ctx, Item{Text: strings.Repeat("中", 1000), Scope: "owner"}); err != nil {
		t.Fatalf("1000 Chinese runes rejected: %v", err)
	}
	for _, bad := range []Item{
		{Text: strings.Repeat("中", 1001), Scope: "owner"}, {Text: " ", Scope: "owner"},
		{Text: "正文"}, {Text: "正文", Scope: "all"}, {Text: "正文", Scope: "owner", Importance: -0.1},
		{Text: "正文", Scope: "owner", Importance: 1.01}, {Text: "正文", Scope: "owner", Importance: math.NaN()},
		{Text: "正文", Scope: "owner", Importance: math.Inf(1)}, {Text: "正文", Scope: "owner", Status: "deleted"},
		{Text: string([]byte{0xff}), Scope: "owner"}, {Text: "正文\x00", Scope: "owner"},
	} {
		if _, err := s.Put(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid item accepted: %#v, %v", bad, err)
		}
	}
	if _, err := s.Put(ctx, Item{ID: "nonexistent-explicit-id", Text: "新内容", Scope: "owner"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("explicit unknown ID must not create a new record: %v", err)
	}
}

func TestCapacityAndOwnerList(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := putItem(t, s, "拥有者记忆", "owner")
	group := putItem(t, s, "本群只读资料", "group:onebot:bot:group")
	if err := s.SetStatus(ctx, group.ID, StatusArchived, ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		scope, status string
		limit, want   int
	}{
		{"", "", 100, 2}, {"owner", "", 100, 1}, {"", "archived", 100, 1},
		{"", "archive", 100, 1}, {"", "active", 100, 1}, {"", "", 1, 1},
	} {
		items, err := s.List(ctx, tc.scope, tc.status, tc.limit)
		if err != nil || len(items) != tc.want {
			t.Fatalf("List %#v: %d, %v", tc, len(items), err)
		}
	}
	// Fill remaining capacity in a single local SQL statement; token_count=0
	// fixtures are not searchable and do not require 2998 separate transactions.
	_, err := s.db.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?)
INSERT INTO mem_items(id,text,category,subject,scope,source,status,superseded_by,importance,created_at,updated_at,token_count)
SELECT 'capacity-'||x,'占位资料','fact','','owner','user','active','',0,?,?,0 FROM n`, MaxItems-2, testNow.Format(timeLayout), testNow.Format(timeLayout))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Put(ctx, Item{Text: "超额条目", Scope: "owner"}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity cap not enforced: %v", err)
	}
	owner.Text = "拥有者明确修订"
	if _, err = s.Put(ctx, owner); err != nil {
		t.Fatalf("revisions should still work at capacity: %v", err)
	}
	if err = s.Delete(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
	putItem(t, s, "删除后可新增", "owner")
	items, err := s.List(ctx, "", "", MaxItems+1000)
	if err != nil || len(items) != MaxItems {
		t.Fatalf("list/capacity bounds: %d, %v", len(items), err)
	}
}

func TestDiskIndexPersistsAndNoForeignSchemaChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { db.Close() })
		return db
	}
	db := open()
	if _, err := db.Exec("PRAGMA user_version=71; CREATE TABLE unrelated(value TEXT); INSERT INTO unrelated VALUES('keep')"); err != nil {
		t.Fatal(err)
	}
	s, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	target := putItem(t, s, "磁盘索引应持久保存", "owner")
	if countRows(t, db, "PRAGMA user_version") != 71 {
		t.Fatal("memory.New changed the shared schema version")
	}
	if countRows(t, db, "SELECT COUNT(*) FROM unrelated") != 1 {
		t.Fatal("memory.New changed unrelated data")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open()
	s, err = New(db)
	if err != nil {
		t.Fatal(err)
	}
	if got := searchItems(t, s, "磁盘索引", []string{"owner"}, 5, 6000); len(got) != 1 || got[0].Item.ID != target.ID {
		t.Fatalf("disk index not persistent: %#v", got)
	}
	if countRows(t, db, "SELECT COUNT(*) FROM mem_terms") == 0 {
		t.Fatal("no persistent postings")
	}
}

func TestJSONSnakeCaseAndCancelledContext(t *testing.T) {
	s := testStore(t)
	item := putItem(t, s, "JSON 字段验证", "owner")
	body, err := json.Marshal(Hit{Item: item, Score: 1, Why: []string{"related"}})
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"item", "score", "why"} {
		if _, ok := root[field]; !ok {
			t.Fatalf("missing JSON field %s", field)
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(root["item"], &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "text", "category", "subject", "scope", "source", "status", "superseded_by", "importance", "created_at", "updated_at", "expires_at"} {
		if _, ok := fields[field]; !ok {
			t.Fatalf("missing JSON field %s", field)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Search(ctx, "磁盘", []string{"owner"}, 5, 6000); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled search: %v", err)
	}
	if _, err := s.Put(ctx, Item{Text: "正文", Scope: "owner"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled put: %v", err)
	}
	if _, err := New(nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil db: %v", err)
	}
}

func countRows(t *testing.T, db *sql.DB, statement string, args ...any) int {
	t.Helper()
	var count int
	if err := db.QueryRow(statement, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
