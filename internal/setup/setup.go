package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"opsagent/internal/config"
	"opsagent/internal/store"
)

func IssueCode(s *store.Store) (string, error) {
	if s.Initialized() {
		return "", errors.New("初始化已完成；如忘记访问令牌请使用 token reset")
	}
	code := config.RandomToken()
	tx, e := s.DB.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	for k, v := range map[string]string{"setup_code_hash": config.Hash(code), "setup_code_expires": strconv.FormatInt(time.Now().Add(15*time.Minute).Unix(), 10)} {
		if _, e = tx.Exec("INSERT INTO kv VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", k, v); e != nil {
			return "", e
		}
	}
	if _, e = tx.Exec("DELETE FROM auth_sessions WHERE kind='setup'"); e != nil {
		return "", e
	}
	return code, tx.Commit()
}
func Unlock(s *store.Store, code string) (string, error) {
	if len(code) < 32 || len(code) > 128 || s.Initialized() {
		return "", errors.New("设置码无效或初始化已完成")
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	var hash, expires string
	if e = tx.QueryRow("SELECT value FROM kv WHERE key='setup_code_hash'").Scan(&hash); e != nil {
		return "", errors.New("请先在本机生成设置码")
	}
	if e = tx.QueryRow("SELECT value FROM kv WHERE key='setup_code_expires'").Scan(&expires); e != nil {
		return "", e
	}
	at, _ := strconv.ParseInt(expires, 10, 64)
	if !config.EqualHash(hash, code) || at < time.Now().Unix() {
		return "", errors.New("设置码无效或已过期")
	}
	res, e := tx.Exec("UPDATE kv SET value='' WHERE key='setup_code_hash' AND value=?", hash)
	if e != nil {
		return "", e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return "", errors.New("设置码已使用")
	}
	token := config.RandomToken()
	h := sha256.Sum256([]byte(token))
	_, e = tx.Exec("INSERT INTO auth_sessions VALUES(?,?,?)", hex.EncodeToString(h[:]), "setup", time.Now().Add(30*time.Minute).Unix())
	if e != nil {
		return "", e
	}
	return token, tx.Commit()
}
