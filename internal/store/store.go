package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB  *sql.DB
	Dir string
}

type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Scope     string `json:"scope"`
	Channel   string `json:"channel"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type Message struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	Raw       json.RawMessage `json:"-"`
	CreatedAt string          `json:"created_at"`
}

type Task struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Input     string `json:"input"`
	Status    string `json:"status"`
	Output    string `json:"output"`
	Error     string `json:"error"`
	DryRun    bool   `json:"dry_run"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type Event struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	TaskID    string          `json:"task_id"`
	SessionID string          `json:"session_id"`
	Data      json.RawMessage `json:"data"`
	CreatedAt string          `json:"created_at"`
}

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("secure random unavailable")
	}
	return hex.EncodeToString(b[:])
}
func Now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func Open(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	if err = os.Chmod(abs, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(abs, "ops.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, Dir: abs}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS kv(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS auth_sessions(hash TEXT PRIMARY KEY,kind TEXT NOT NULL,expires INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(id TEXT PRIMARY KEY,title TEXT NOT NULL,scope TEXT NOT NULL,channel TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS messages(id TEXT PRIMARY KEY,session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,role TEXT NOT NULL,content TEXT NOT NULL,raw TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS messages_session ON messages(session_id,created_at);
CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY,session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,input TEXT NOT NULL,status TEXT NOT NULL,output TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',dry_run INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS tasks_session ON tasks(session_id,created_at);
CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY AUTOINCREMENT,kind TEXT NOT NULL,task_id TEXT NOT NULL,session_id TEXT NOT NULL,data TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS events_task ON events(task_id,id);
CREATE TABLE IF NOT EXISTS bindings(id TEXT PRIMARY KEY,channel TEXT NOT NULL,bot_id TEXT NOT NULL,user_id TEXT NOT NULL,created_at TEXT NOT NULL,UNIQUE(channel,bot_id,user_id));
CREATE TABLE IF NOT EXISTS pairings(id TEXT PRIMARY KEY,channel TEXT NOT NULL,code TEXT NOT NULL,user_id TEXT NOT NULL DEFAULT '',bot_id TEXT NOT NULL DEFAULT '',expires_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS channel_seen(key TEXT PRIMARY KEY,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS channel_sessions(key TEXT PRIMARY KEY,session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS notifications(id TEXT PRIMARY KEY,metric TEXT NOT NULL,state TEXT NOT NULL,message TEXT NOT NULL,value REAL NOT NULL,threshold REAL NOT NULL,created_at TEXT NOT NULL);
PRAGMA user_version=1;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(filepath.Join(abs, "ops.db"), 0600); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) Get(key string) (string, error) {
	var v string
	err := s.DB.QueryRow("SELECT value FROM kv WHERE key=?", key).Scan(&v)
	return v, err
}
func (s *Store) Set(key, value string) error {
	_, e := s.DB.Exec("INSERT INTO kv(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return e
}
func (s *Store) Initialized() bool { v, e := s.Get("initialized"); return e == nil && v == "true" }

func (s *Store) CreateSession(title, scope, channel string) (Session, error) {
	if len([]rune(title)) > 70 {
		title = string([]rune(title)[:70])
	}
	if strings.TrimSpace(title) == "" {
		title = "新的运维对话"
	}
	x := Session{ID: ID(), Title: title, Scope: scope, Channel: channel, CreatedAt: Now(), UpdatedAt: Now()}
	_, e := s.DB.Exec("INSERT INTO sessions VALUES(?,?,?,?,?,?)", x.ID, x.Title, x.Scope, x.Channel, x.CreatedAt, x.UpdatedAt)
	return x, e
}
func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var x Session
	e := row.Scan(&x.ID, &x.Title, &x.Scope, &x.Channel, &x.CreatedAt, &x.UpdatedAt)
	return x, e
}
func (s *Store) Session(id string) (Session, error) {
	return scanSession(s.DB.QueryRow("SELECT id,title,scope,channel,created_at,updated_at FROM sessions WHERE id=?", id))
}
func (s *Store) Sessions() ([]Session, error) {
	r, e := s.DB.Query("SELECT id,title,scope,channel,created_at,updated_at FROM sessions ORDER BY updated_at DESC LIMIT 100")
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := []Session{}
	for r.Next() {
		x, e := scanSession(r)
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, r.Err()
}
func (s *Store) DeleteSession(id string) error {
	var n int
	e := s.DB.QueryRow("SELECT count(*) FROM tasks WHERE session_id=? AND status IN ('queued','running','waiting_approval')", id).Scan(&n)
	if e != nil {
		return e
	}
	if n > 0 {
		return errors.New("请先停止此会话中的任务")
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("DELETE FROM events WHERE session_id=?", id); e != nil {
		return e
	}
	if _, e = tx.Exec("DELETE FROM sessions WHERE id=?", id); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) AddMessage(sessionID, role, content string, raw any) error {
	j, e := json.Marshal(raw)
	if e != nil {
		return e
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	_, e = tx.Exec("INSERT INTO messages VALUES(?,?,?,?,?,?)", ID(), sessionID, role, content, string(j), Now())
	if e != nil {
		return e
	}
	_, e = tx.Exec("UPDATE sessions SET updated_at=? WHERE id=?", Now(), sessionID)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Messages(id string, limit int) ([]Message, error) {
	if limit < 1 || limit > 200 {
		limit = 100
	}
	r, e := s.DB.Query("SELECT id,session_id,role,content,raw,created_at FROM (SELECT rowid,id,session_id,role,content,raw,created_at FROM messages WHERE session_id=? ORDER BY rowid DESC LIMIT ?) ORDER BY rowid", id, limit)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := []Message{}
	for r.Next() {
		var x Message
		var raw string
		if e = r.Scan(&x.ID, &x.SessionID, &x.Role, &x.Content, &raw, &x.CreatedAt); e != nil {
			return nil, e
		}
		x.Raw = json.RawMessage(raw)
		out = append(out, x)
	}
	return out, r.Err()
}
func (s *Store) CreateTask(sessionID, input string, dry bool) (Task, error) {
	x := Task{ID: ID(), SessionID: sessionID, Input: input, Status: "queued", DryRun: dry, CreatedAt: Now(), UpdatedAt: Now()}
	_, e := s.DB.Exec("INSERT INTO tasks(id,session_id,input,status,dry_run,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", x.ID, x.SessionID, x.Input, x.Status, x.DryRun, x.CreatedAt, x.UpdatedAt)
	return x, e
}
func scanTask(r interface{ Scan(...any) error }) (Task, error) {
	var x Task
	e := r.Scan(&x.ID, &x.SessionID, &x.Input, &x.Status, &x.Output, &x.Error, &x.DryRun, &x.CreatedAt, &x.UpdatedAt)
	return x, e
}

const taskCols = "id,session_id,input,status,output,error,dry_run,created_at,updated_at"

func (s *Store) Task(id string) (Task, error) {
	return scanTask(s.DB.QueryRow("SELECT "+taskCols+" FROM tasks WHERE id=?", id))
}
func (s *Store) Tasks(sessionID string) ([]Task, error) {
	q := "SELECT " + taskCols + " FROM tasks"
	args := []any{}
	if sessionID != "" {
		q += " WHERE session_id=?"
		args = append(args, sessionID)
	}
	q += " ORDER BY created_at DESC LIMIT 100"
	r, e := s.DB.Query(q, args...)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := []Task{}
	for r.Next() {
		x, e := scanTask(r)
		if e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, r.Err()
}
func (s *Store) UpdateTask(id, status, output, errText string) error {
	_, e := s.DB.Exec("UPDATE tasks SET status=?,output=?,error=?,updated_at=? WHERE id=?", status, output, errText, Now(), id)
	return e
}
func (s *Store) Emit(kind, taskID, sessionID string, data any) (Event, error) {
	j, e := json.Marshal(data)
	if e != nil {
		return Event{}, e
	}
	x := Event{Kind: kind, TaskID: taskID, SessionID: sessionID, Data: j, CreatedAt: Now()}
	res, e := s.DB.Exec("INSERT INTO events(kind,task_id,session_id,data,created_at) VALUES(?,?,?,?,?)", kind, taskID, sessionID, string(j), x.CreatedAt)
	if e != nil {
		return x, e
	}
	x.ID, e = res.LastInsertId()
	return x, e
}
func (s *Store) Events(after int64, latest bool) ([]Event, error) {
	q := "SELECT id,kind,task_id,session_id,data,created_at FROM events WHERE id>? ORDER BY id LIMIT 100"
	if latest {
		q = "SELECT id,kind,task_id,session_id,data,created_at FROM events WHERE id>? ORDER BY id DESC LIMIT 100"
	}
	r, e := s.DB.Query(q, after)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	out := []Event{}
	for r.Next() {
		var x Event
		var j string
		if e = r.Scan(&x.ID, &x.Kind, &x.TaskID, &x.SessionID, &j, &x.CreatedAt); e != nil {
			return nil, e
		}
		x.Data = json.RawMessage(j)
		out = append(out, x)
	}
	return out, r.Err()
}
func (s *Store) Seen(key string) (bool, error) {
	res, e := s.DB.Exec("INSERT OR IGNORE INTO channel_seen(key,created_at) VALUES(?,?)", key, Now())
	if e != nil {
		return false, e
	}
	n, e := res.RowsAffected()
	return n == 0, e
}
func (s *Store) ClearModelHistory() error {
	_, e := s.DB.Exec("UPDATE messages SET raw='null' WHERE role!='user'")
	return e
}

func (s *Store) Cleanup(ctx context.Context, days int) error {
	cut := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	for _, q := range []string{"DELETE FROM events WHERE created_at < ?", "DELETE FROM notifications WHERE created_at < ?", "DELETE FROM channel_seen WHERE created_at < ?"} {
		if _, e := s.DB.ExecContext(ctx, q, cut); e != nil {
			return e
		}
	}
	_, e := s.DB.ExecContext(ctx, "DELETE FROM auth_sessions WHERE expires < ?", time.Now().Unix())
	if e != nil {
		return e
	}
	_, e = s.DB.ExecContext(ctx, "DELETE FROM sessions WHERE updated_at < ? AND NOT EXISTS(SELECT 1 FROM tasks WHERE tasks.session_id=sessions.id AND status IN ('queued','running','waiting_approval'))", cut)
	if e != nil {
		return e
	}
	for _, table := range []string{"events", "notifications", "channel_seen"} {
		col := "id"
		if table == "channel_seen" {
			col = "key"
		}
		q := fmt.Sprintf("DELETE FROM %s WHERE %s NOT IN (SELECT %s FROM %s ORDER BY created_at DESC LIMIT 20000)", table, col, col, table)
		if _, e = s.DB.ExecContext(ctx, q); e != nil {
			return e
		}
	}
	return nil
}
