// Package memory stores owner-managed reference material. It does not grant
// authorization: callers must authenticate all management methods and derive
// Search scopes from the server session, never from model-supplied arguments.
package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxItems       = 3000
	MaxItemRunes   = 1000
	MaxResults     = 10
	MaxSearchChars = 6000
	MaxQueryRunes  = 2000

	StatusActive     = "active"
	StatusArchived   = "archived"
	StatusSuperseded = "superseded"

	// Fixed precision makes SQLite string comparisons chronological, including
	// timestamps that fall exactly on a second boundary.
	timeLayout = "2006-01-02T15:04:05.000000000Z"
)

var (
	ErrNotFound = sql.ErrNoRows
	ErrInvalid  = errors.New("记忆字段无效")
	ErrSecret   = errors.New("记忆疑似包含凭据，不能保存；请使用安全配置入口")
	ErrCapacity = errors.New("记忆数量已达到 3000 条上限，请先管理或删除旧条目")
	ErrConflict = errors.New("记忆已变更，请重新读取后明确提交修改")
)

type Item struct {
	ID           string     `json:"id"`
	Text         string     `json:"text"`
	Category     string     `json:"category"`
	Subject      string     `json:"subject"`
	Scope        string     `json:"scope"`
	Source       string     `json:"source"`
	Status       string     `json:"status"`
	SupersededBy string     `json:"superseded_by"`
	Importance   float64    `json:"importance"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	ExpiresAt    *time.Time `json:"expires_at"`
}

type Hit struct {
	Item  Item     `json:"item"`
	Score float64  `json:"score"`
	Why   []string `json:"why"`
}

type Store struct {
	db  *sql.DB
	mu  sync.Mutex // Serializes changes made through this Store, not searches.
	now func() time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS mem_items (
 id TEXT PRIMARY KEY,
 text TEXT NOT NULL CHECK(length(text) BETWEEN 1 AND 1000),
 category TEXT NOT NULL, subject TEXT NOT NULL, scope TEXT NOT NULL,
 source TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('active','archived','superseded')),
 superseded_by TEXT NOT NULL DEFAULT '',
 importance REAL NOT NULL CHECK(importance >= 0 AND importance <= 1),
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL, expires_at TEXT,
 token_count INTEGER NOT NULL CHECK(token_count >= 0)
);
CREATE INDEX IF NOT EXISTS mem_items_scope_status ON mem_items(scope,status,expires_at);
CREATE TABLE IF NOT EXISTS mem_terms (
 term TEXT NOT NULL, item_id TEXT NOT NULL REFERENCES mem_items(id) ON DELETE CASCADE,
 tf INTEGER NOT NULL CHECK(tf > 0),
 PRIMARY KEY(term,item_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS mem_terms_item ON mem_terms(item_id);
CREATE TABLE IF NOT EXISTS mem_versions (
 item_id TEXT NOT NULL REFERENCES mem_items(id) ON DELETE CASCADE,
 version INTEGER NOT NULL CHECK(version > 0), snapshot TEXT NOT NULL, changed_at TEXT NOT NULL,
 PRIMARY KEY(item_id,version)
) WITHOUT ROWID;`

// New creates only mem_-prefixed objects on an existing SQLite connection. It
// neither owns/closes db nor changes connection limits, PRAGMAs or user_version.
// The application registers its SQLite driver. In-memory tests must set
// db.SetMaxOpenConns(1), since separate :memory: connections are separate DBs.
func New(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: 数据库不能为空", ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("初始化记忆表: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &Store{db: db, now: time.Now}, nil
}

const itemColumns = "id,text,category,subject,scope,source,status,superseded_by,importance,created_at,updated_at,expires_at"

type scanner interface{ Scan(...any) error }

func scanItem(row scanner) (Item, error) {
	var item Item
	var created, updated string
	var expires sql.NullString
	if err := row.Scan(&item.ID, &item.Text, &item.Category, &item.Subject, &item.Scope, &item.Source, &item.Status, &item.SupersededBy, &item.Importance, &created, &updated, &expires); err != nil {
		return Item{}, err
	}
	var err error
	if item.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
		return Item{}, fmt.Errorf("读取记忆创建时间: %w", err)
	}
	if item.UpdatedAt, err = time.Parse(timeLayout, updated); err != nil {
		return Item{}, fmt.Errorf("读取记忆修改时间: %w", err)
	}
	if expires.Valid {
		t, e := time.Parse(timeLayout, expires.String)
		if e != nil {
			return Item{}, fmt.Errorf("读取记忆有效期: %w", e)
		}
		item.ExpiresAt = &t
	}
	return item, nil
}

// Get is an owner-management operation; unlike Search it includes inactive and
// expired items. It is not an authorization check.
func (s *Store) Get(ctx context.Context, id string) (Item, error) {
	return scanItem(s.db.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM mem_items WHERE id=?", id))
}

// List is for authenticated owner management. An empty scope/status means all
// scopes/statuses, including expired entries. Group/model callers must not use
// this method. limit defaults to 100 and is capped at MaxItems.
func (s *Store) List(ctx context.Context, scope, status string, limit int) ([]Item, error) {
	if scope != "" && !validScope(scope) {
		return nil, fmt.Errorf("%w: scope", ErrInvalid)
	}
	if status != "" {
		var err error
		status, err = normalizeStatus(status)
		if err != nil {
			return nil, err
		}
	}
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, MaxItems)
	where := []string{"1=1"}
	args := []any{}
	if scope != "" {
		where = append(where, "scope=?")
		args = append(args, scope)
	}
	if status != "" {
		where = append(where, "status=?")
		args = append(args, status)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, "SELECT "+itemColumns+" FROM mem_items WHERE "+strings.Join(where, " AND ")+" ORDER BY updated_at DESC,id LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Item{}
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// Put creates an entry only when ID is empty. Supplying an existing ID is an
// explicit full revision (never an implicit duplicate merge); an unknown or
// deleted ID returns ErrNotFound, so stale clients cannot silently revive it.
// If UpdatedAt is supplied for a revision it must match the current version.
// CreatedAt/UpdatedAt are server-owned. Empty status preserves the old status
// on revision; a newly created entry defaults to active.
func (s *Store) Put(ctx context.Context, item Item) (Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Item{}, err
	}
	defer tx.Rollback()
	fresh := item.ID == ""
	var previous Item
	if !fresh {
		previous, err = scanItem(tx.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM mem_items WHERE id=?", item.ID))
		if err != nil {
			return Item{}, err
		}
		if !item.UpdatedAt.IsZero() && !item.UpdatedAt.Equal(previous.UpdatedAt) {
			return Item{}, ErrConflict
		}
		if item.Status == "" {
			item.Status = previous.Status
			if item.SupersededBy == "" {
				item.SupersededBy = previous.SupersededBy
			}
		}
	}
	if err = normalizeItem(&item); err != nil {
		return Item{}, err
	}
	now := s.now().UTC()
	if fresh {
		var entropy [16]byte
		if _, err = rand.Read(entropy[:]); err != nil {
			return Item{}, err
		}
		item.ID = hex.EncodeToString(entropy[:])
		item.CreatedAt = now
	} else {
		item.CreatedAt = previous.CreatedAt
		if !now.After(previous.UpdatedAt) {
			now = previous.UpdatedAt.Add(time.Nanosecond)
		}
	}
	item.UpdatedAt = now
	if err = validateReplacement(ctx, tx, item); err != nil {
		return Item{}, err
	}
	terms, count := termCounts(item.Text + "\n" + item.Subject + "\n" + item.Category)
	var expiry any
	if item.ExpiresAt != nil {
		expiry = item.ExpiresAt.Format(timeLayout)
	}
	values := []any{item.Text, item.Category, item.Subject, item.Scope, item.Source, item.Status, item.SupersededBy, item.Importance, item.CreatedAt.Format(timeLayout), item.UpdatedAt.Format(timeLayout), expiry, count}
	if fresh {
		args := append([]any{item.ID}, values...)
		args = append(args, MaxItems)
		result, e := tx.ExecContext(ctx, "INSERT INTO mem_items ("+itemColumns+",token_count) SELECT ?,?,?,?,?,?,?,?,?,?,?,?,? WHERE (SELECT COUNT(*) FROM mem_items) < ?", args...)
		if e != nil {
			return Item{}, e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return Item{}, e
		}
		if n == 0 {
			return Item{}, ErrCapacity
		}
	} else {
		values = append(values, item.ID)
		if _, err = tx.ExecContext(ctx, "UPDATE mem_items SET text=?,category=?,subject=?,scope=?,source=?,status=?,superseded_by=?,importance=?,created_at=?,updated_at=?,expires_at=?,token_count=? WHERE id=?", values...); err != nil {
			return Item{}, err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM mem_terms WHERE item_id=?", item.ID); err != nil {
			return Item{}, err
		}
	}
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO mem_terms(term,item_id,tf) VALUES(?,?,?)")
	if err != nil {
		return Item{}, err
	}
	for term, frequency := range terms {
		if _, err = stmt.ExecContext(ctx, term, item.ID, frequency); err != nil {
			stmt.Close()
			return Item{}, err
		}
	}
	if err = stmt.Close(); err != nil {
		return Item{}, err
	}
	if err = saveVersion(ctx, tx, item); err != nil {
		return Item{}, err
	}
	if err = tx.Commit(); err != nil {
		return Item{}, err
	}
	return item, nil
}

// SetStatus changes only lifecycle metadata and records the resulting version.
// "archive" is accepted as a compatibility input and stored as "archived".
// Restoring archived/superseded records is always an explicit caller operation.
func (s *Store) SetStatus(ctx context.Context, id, status, supersededBy string) error {
	status, err := normalizeStatus(status)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	item, err := scanItem(tx.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM mem_items WHERE id=?", id))
	if err != nil {
		return err
	}
	if item.Status == status && item.SupersededBy == supersededBy {
		return nil // A no-op is not a new version.
	}
	item.Status, item.SupersededBy = status, supersededBy
	if err = validateReplacement(ctx, tx, item); err != nil {
		return err
	}
	now := s.now().UTC()
	if !now.After(item.UpdatedAt) {
		now = item.UpdatedAt.Add(time.Nanosecond)
	}
	item.UpdatedAt = now
	if _, err = tx.ExecContext(ctx, "UPDATE mem_items SET status=?,superseded_by=?,updated_at=? WHERE id=?", item.Status, item.SupersededBy, now.Format(timeLayout), item.ID); err != nil {
		return err
	}
	if err = saveVersion(ctx, tx, item); err != nil {
		return err
	}
	return tx.Commit()
}

// Delete removes the item, its postings and all its version snapshots in one
// transaction, even if the connection has foreign_keys disabled. It provides
// logical deletion, NOT secure disk erasure or deletion of external backups.
func (s *Store) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"mem_terms", "mem_versions"} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE item_id=?", id); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM mem_items WHERE id=?", id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func saveVersion(ctx context.Context, tx *sql.Tx, item Item) error {
	body, err := json.Marshal(item)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mem_versions(item_id,version,snapshot,changed_at)
SELECT ?,COALESCE(MAX(version),0)+1,?,? FROM mem_versions WHERE item_id=?`, item.ID, string(body), item.UpdatedAt.Format(timeLayout), item.ID)
	return err
}

func normalizeStatus(status string) (string, error) {
	if status == "archive" {
		status = StatusArchived
	}
	switch status {
	case StatusActive, StatusArchived, StatusSuperseded:
		return status, nil
	default:
		return "", fmt.Errorf("%w: status 仅允许 active/archived/superseded", ErrInvalid)
	}
}

func validScope(scope string) bool {
	if scope == "owner" {
		return true
	}
	if len(scope) > 256 {
		return false
	}
	parts := strings.Split(scope, ":")
	if len(parts) != 4 || parts[0] != "group" {
		return false
	}
	for _, part := range parts[1:] {
		if part == "" {
			return false
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
				return false
			}
		}
	}
	return true
}

func normalizeItem(item *Item) error {
	item.Text = strings.TrimSpace(item.Text)
	item.Category = strings.TrimSpace(item.Category)
	item.Subject = strings.TrimSpace(item.Subject)
	item.Source = strings.TrimSpace(item.Source)
	if item.Category == "" {
		item.Category = "fact"
	}
	if item.Source == "" {
		item.Source = "user"
	}
	if item.Status == "" {
		item.Status = StatusActive
	}
	var err error
	item.Status, err = normalizeStatus(item.Status)
	if err != nil {
		return err
	}
	if item.Text == "" || utf8.RuneCountInString(item.Text) > MaxItemRunes || !validScope(item.Scope) {
		return fmt.Errorf("%w: 正文应为 1–1000 个字符，scope 必须为 owner 或 group:<channel>:<bot>:<group>", ErrInvalid)
	}
	if math.IsNaN(item.Importance) || math.IsInf(item.Importance, 0) || item.Importance < 0 || item.Importance > 1 {
		return fmt.Errorf("%w: importance 必须在 0–1 之间", ErrInvalid)
	}
	for _, field := range []struct {
		value string
		limit int
	}{
		{item.ID, 64}, {item.Text, MaxItemRunes}, {item.Category, 64}, {item.Subject, 160},
		{item.Scope, 256}, {item.Source, 64}, {item.SupersededBy, 64},
	} {
		if !utf8.ValidString(field.value) || utf8.RuneCountInString(field.value) > field.limit || strings.IndexByte(field.value, 0) >= 0 {
			return fmt.Errorf("%w: 字段过长或编码无效", ErrInvalid)
		}
		if containsSecret(field.value) {
			return ErrSecret
		}
	}
	if item.ExpiresAt != nil {
		t := item.ExpiresAt.UTC()
		if t.Year() < 1 || t.Year() > 9999 {
			return fmt.Errorf("%w: 有效期无效", ErrInvalid)
		}
		item.ExpiresAt = &t
	}
	return nil
}

func validateReplacement(ctx context.Context, tx *sql.Tx, item Item) error {
	if item.SupersededBy == "" {
		return nil // A replacement is optional when merely marking obsolete data.
	}
	if item.Status != StatusSuperseded || item.SupersededBy == item.ID || len(item.SupersededBy) > 64 {
		return fmt.Errorf("%w: superseded_by 只允许指向同一 scope 的其他条目", ErrInvalid)
	}
	var scope string
	if err := tx.QueryRowContext(ctx, "SELECT scope FROM mem_items WHERE id=?", item.SupersededBy).Scan(&scope); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 替代条目不存在", ErrInvalid)
		}
		return err
	}
	if scope != item.Scope {
		return fmt.Errorf("%w: 不能跨 scope 指定替代条目", ErrInvalid)
	}
	return nil
}

// These deliberately conservative heuristics are NOT comprehensive secret
// detection. The application must additionally block its actual configured
// secrets and apply appropriate access controls. Error messages never echo data.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bsk-`),
	regexp.MustCompile(`(?i)BEGIN\s+(?:RSA\s+|EC\s+|DSA\s+|OPENSSH\s+|ENCRYPTED\s+)?PRIVATE\s+KEY`),
	regexp.MustCompile(`(?i)authorization["'\s]*[:=]?["'\s]*(?:bearer|basic)\b`),
	regexp.MustCompile(`(?i)(?:password|passwd|pwd|api[ _-]?key|access[ _-]?token|client[ _-]?secret|secret[ _-]?key|token|密码|口令|密钥)["'\s]*[:=：]["'\s]*\S+`),
	regexp.MustCompile(`(?:密码|口令|密钥)\s*(?:是|为)\s*\S+`),
	regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^/\s:@]+:[^/\s@]+@`),
}

func containsSecret(text string) bool {
	for _, pattern := range secretPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}

// tokens uses overlapping Han bigrams and case-folded letter/digit words.
// A one-character Han run is retained as a single token. Punctuation separates
// runs, so pairs are never fabricated across sentence/field boundaries.
func tokens(text string) []string {
	out := []string{}
	run := []rune{}
	kind := 0
	flush := func() {
		if len(run) == 0 {
			return
		}
		if kind == 1 && len(run) > 1 {
			for i := 0; i+1 < len(run); i++ {
				out = append(out, string(run[i:i+2]))
			}
		} else {
			out = append(out, string(run))
		}
		run = run[:0]
	}
	for _, r := range text {
		next := 0
		if unicode.Is(unicode.Han, r) {
			next = 1
		} else if unicode.IsLetter(r) || unicode.IsDigit(r) {
			next = 2
		}
		if next != kind || next == 0 {
			flush()
		}
		kind = next
		if next != 0 {
			run = append(run, unicode.ToLower(r))
		}
	}
	flush()
	return out
}

func termCounts(text string) (map[string]int, int) {
	terms := tokens(text)
	counts := make(map[string]int, len(terms))
	for _, term := range terms {
		counts[term]++
	}
	return counts, len(terms)
}
