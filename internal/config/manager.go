package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"opsagent/internal/store"
)

type Manager struct {
	mu      sync.RWMutex
	db      *store.Store
	cfg     Config
	secrets Secrets
}

func Load(db *store.Store, work string) (*Manager, error) {
	m := &Manager{db: db, cfg: Default(work)}
	if raw, e := db.Get("config"); e == nil {
		if e = json.Unmarshal([]byte(raw), &m.cfg); e != nil {
			return nil, errors.New("配置损坏，请从备份恢复")
		}
	}
	b, e := os.ReadFile(filepath.Join(db.Dir, "credentials.json"))
	if e == nil {
		if e = json.Unmarshal(b, &m.secrets); e != nil {
			return nil, errors.New("凭据文件损坏")
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	return m, nil
}
func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, _ := json.Marshal(m.cfg)
	var c Config
	_ = json.Unmarshal(b, &c)
	return c
}
func (m *Manager) ModelCredentials() (Model, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Model, m.secrets.ModelKey
}
func (m *Manager) Secrets() Secrets { m.mu.RLock(); defer m.mu.RUnlock(); return m.secrets }
func RandomToken() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("secure random unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func Hash(s string) string { x := sha256.Sum256([]byte(s)); return hex.EncodeToString(x[:]) }
func EqualHash(hash, value string) bool {
	return subtle.ConstantTimeCompare([]byte(hash), []byte(Hash(value))) == 1
}
func (m *Manager) VerifyToken(token string) bool {
	if len(token) < 32 || len(token) > 128 {
		return false
	}
	hash, e := m.db.Get("token_hash")
	return e == nil && EqualHash(hash, token)
}

func (m *Manager) CreateSession(accessToken string) (string, error) {
	if len(accessToken) < 32 || len(accessToken) > 128 {
		return "", errors.New("访问令牌无效")
	}
	token := RandomToken()
	// One SQL statement atomically checks the current token and creates the session.
	res, err := m.db.DB.Exec("INSERT INTO auth_sessions(hash,kind,expires) SELECT ?, 'login', ? WHERE EXISTS (SELECT 1 FROM kv WHERE key='token_hash' AND value=?) AND EXISTS (SELECT 1 FROM kv WHERE key='initialized' AND value='true')", Hash(token), time.Now().Add(24*time.Hour).Unix(), Hash(accessToken))
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", errors.New("访问令牌无效")
	}
	return token, nil
}
func writePrivate(path string, value []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".pending-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(value)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}
func (m *Manager) saveLocked(c Config, s Secrets) error {
	if e := c.Validate(); e != nil {
		return e
	}
	if _, e := os.Stat(c.WorkDir); e != nil {
		return errors.New("工作目录不存在或不可访问")
	}
	for _, root := range append(append([]string{}, c.ReadRoots...), c.WriteRoots...) {
		st, e := os.Stat(root)
		if e != nil || !st.IsDir() {
			return errors.New("允许目录不存在或不是目录：" + root)
		}
	}
	if len(s.ModelKey) > 4096 || len(s.OfficialSecret) > 4096 || len(s.OneBotToken) > 4096 {
		return errors.New("凭据长度超限")
	}
	personaText := strings.Join([]string{c.Persona.Name, c.Persona.Role, c.Persona.Address, c.Persona.Tone, c.Persona.Detail, c.Persona.OwnerProfile, c.Persona.HostProfile}, "\n")
	if privateKeyPattern.MatchString(personaText) || secretPattern.MatchString(personaText) {
		return errors.New("人设与档案不能包含凭据")
	}
	for _, value := range []string{s.ModelKey, s.OfficialSecret, s.OneBotToken} {
		if len(value) >= 4 && strings.Contains(personaText, value) {
			return errors.New("人设与档案不能包含凭据")
		}
	}
	b, _ := json.Marshal(s)
	if e := writePrivate(filepath.Join(m.db.Dir, "credentials.json"), b); e != nil {
		return e
	}
	b, _ = json.Marshal(c)
	if e := m.db.Set("config", string(b)); e != nil {
		return e
	}
	m.cfg = c
	m.secrets = s
	return nil
}
func (m *Manager) Save(c Config, patch Secrets) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.secrets
	if patch.ModelKey != "" {
		s.ModelKey = patch.ModelKey
	}
	if patch.OfficialSecret != "" {
		s.OfficialSecret = patch.OfficialSecret
	}
	if patch.OneBotToken != "" {
		s.OneBotToken = patch.OneBotToken
	}
	return m.saveLocked(c, s)
}
func (m *Manager) Initialize(c Config, s Secrets) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.db.Initialized() {
		return "", errors.New("已完成初始化")
	}
	if c.Channels.Official.Enabled || c.Channels.OneBot.Enabled || c.Watch.Enabled {
		return "", errors.New("初始化阶段请保持 QQ 和值守关闭，完成后可在设置中启用")
	}
	if e := m.saveLocked(c, s); e != nil {
		return "", e
	}
	token := RandomToken()
	tx, e := m.db.DB.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	for k, v := range map[string]string{"initialized": "true", "token_hash": Hash(token), "access_token": token, "setup_code_hash": "", "setup_code_expires": "0"} {
		if _, e = tx.Exec("INSERT INTO kv VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", k, v); e != nil {
			return "", e
		}
	}
	if _, e = tx.Exec("DELETE FROM auth_sessions"); e != nil {
		return "", e
	}
	if e = tx.Commit(); e != nil {
		return "", e
	}
	return token, nil
}
func (m *Manager) RotateToken() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.db.Initialized() {
		return "", errors.New("尚未初始化")
	}
	token := RandomToken()
	tx, e := m.db.DB.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("UPDATE kv SET value=? WHERE key='token_hash'", Hash(token)); e != nil {
		return "", e
	}
	if _, e = tx.Exec("INSERT INTO kv(key,value) VALUES('access_token',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", token); e != nil {
		return "", e
	}
	if _, e = tx.Exec("DELETE FROM auth_sessions"); e != nil {
		return "", e
	}
	return token, tx.Commit()
}

var privateKeyPattern = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`)
var secretPattern = regexp.MustCompile(`(?i)(?:sk-[a-z0-9_-]{8,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|(?:authorization\s*[:=]\s*bearer|api[_-]?key|client[_-]?secret|password|密码)\s*[:=]?\s*[^\s,;]{4,})`)

func (m *Manager) Redact(text string) string {
	m.mu.RLock()
	s := m.secrets
	m.mu.RUnlock()
	values := []string{s.ModelKey, s.OfficialSecret, s.OneBotToken}
	if token, err := m.db.Get("access_token"); err == nil {
		values = append(values, token)
	}
	for _, value := range values {
		if len(value) >= 4 {
			text = strings.ReplaceAll(text, value, "[已隐藏凭据]")
		}
	}
	text = privateKeyPattern.ReplaceAllString(text, "[已隐藏私钥]")
	return secretPattern.ReplaceAllString(text, "[已隐藏敏感内容]")
}
func (m *Manager) CleanValue(value any) any {
	switch x := value.(type) {
	case string:
		return m.Redact(x)
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = m.CleanValue(v)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[m.Redact(k)] = m.CleanValue(v)
		}
		return out
	default:
		return value
	}
}
func (m *Manager) CleanJSON(raw string) string {
	var value any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return m.Redact(raw)
	}
	b, err := json.Marshal(m.CleanValue(value))
	if err != nil {
		return "null"
	}
	return string(b)
}
func (m *Manager) ContainsSecretJSON(raw string) bool {
	var value any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return m.ContainsSecret(raw)
	}
	original, _ := json.Marshal(value)
	cleaned, _ := json.Marshal(m.CleanValue(value))
	return string(original) != string(cleaned)
}
func (m *Manager) ContainsSecret(text string) bool { return m.Redact(text) != text }
