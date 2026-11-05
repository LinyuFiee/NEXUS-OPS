package config

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"opsagent/internal/store"
)

func TestStructuredSecretsAndPEMRedacted(t *testing.T) {
	db, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	m, _ := Load(db, t.TempDir())
	secret := "abc\"def\\ghi-123456"
	m.secrets = Secrets{ModelKey: secret}
	raw, _ := json.Marshal(map[string]any{"output": secret, "args": map[string]any{secret: "other"}})
	clean := m.CleanJSON(string(raw))
	if strings.Contains(clean, "abc") || !m.ContainsSecretJSON(string(raw)) {
		t.Fatal("structured secret leaked")
	}
	escaped := `{"command":"\u0061bc\"def\\ghi-123456"}`
	if !m.ContainsSecretJSON(escaped) {
		t.Fatal("unicode encoding bypass")
	}
	pem := "before\n-----BEGIN RSA PRIVATE KEY-----\nbase64-secret-body\n-----END RSA PRIVATE KEY-----\nafter"
	if strings.Contains(m.Redact(pem), "base64-secret-body") {
		t.Fatal("PEM body leaked")
	}
}
func TestTokenRotationAtomicAcrossManagers(t *testing.T) {
	dir := t.TempDir()
	a, e := store.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	b, e := store.Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	work := t.TempDir()
	ma, _ := Load(a, work)
	mb, _ := Load(b, work)
	old, e := ma.Initialize(Default(work), Secrets{})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for _, m := range []*Manager{ma, mb} {
		wg.Add(1)
		go func(m *Manager) {
			defer wg.Done()
			if _, e := m.RotateToken(); e != nil {
				t.Error(e)
			}
		}(m)
	}
	wg.Wait()
	actual, e := a.Get("access_token")
	if e != nil || !ma.VerifyToken(actual) {
		t.Fatal("token source and hash inconsistent")
	}
	if ma.VerifyToken(old) {
		t.Fatal("old token active")
	}
	if _, e = ma.CreateSession(old); e == nil {
		t.Fatal("old token created new session")
	}
}
