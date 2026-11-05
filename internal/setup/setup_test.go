package setup_test

import (
	"sync"
	"testing"

	"opsagent/internal/config"
	"opsagent/internal/setup"
	"opsagent/internal/store"
)

func TestOneTimeCodeAndInitialize(t *testing.T) {
	db, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	work := t.TempDir()
	cfg, e := config.Load(db, work)
	if e != nil {
		t.Fatal(e)
	}
	code, e := setup.IssueCode(db)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = setup.Unlock(db, "wrong"); e == nil {
		t.Fatal("bad code accepted")
	}
	token, e := setup.Unlock(db, code)
	if e != nil || token == "" {
		t.Fatal(e)
	}
	if _, e = setup.Unlock(db, code); e == nil {
		t.Fatal("code replay accepted")
	}
	access, e := cfg.Initialize(config.Default(work), config.Secrets{})
	if e != nil {
		t.Fatal(e)
	}
	if !cfg.VerifyToken(access) || cfg.VerifyToken("not-a-token") {
		t.Fatal("token check failed")
	}
	if _, e = setup.IssueCode(db); e == nil {
		t.Fatal("setup reopened")
	}
	if _, e = cfg.RotateToken(); e != nil {
		t.Fatal(e)
	}
	if cfg.VerifyToken(access) {
		t.Fatal("old token still valid")
	}
}
func TestConcurrentSetupOnlyOneWinner(t *testing.T) {
	db, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	work := t.TempDir()
	cfg, _ := config.Load(db, work)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := cfg.Initialize(config.Default(work), config.Secrets{}); results <- e }()
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("got %d setup winners", success)
	}
}
func TestRejectUnsafeConfiguration(t *testing.T) {
	c := config.Default(t.TempDir())
	c.Model.BaseURL = "http://remote.example"
	if c.Validate() == nil {
		t.Fatal("remote plaintext API accepted")
	}
	c = config.Default(t.TempDir())
	c.Limits.MaxQueue = 0
	if c.Validate() == nil {
		t.Fatal("unbounded queue accepted")
	}
}
