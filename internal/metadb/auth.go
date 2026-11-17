package metadb

import (
	"database/sql"
	"errors"
	"time"
)

const authSchema = `
CREATE TABLE IF NOT EXISTS auth_user (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    username      TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    enabled       INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS auth_sessions (
    id_hash    TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    user_agent TEXT DEFAULT '',
    ip         TEXT DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_auth_sessions_exp ON auth_sessions(expires_at);
CREATE TABLE IF NOT EXISTS auth_tokens (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    prefix     TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    last_used  INTEGER DEFAULT 0,
    last_ip    TEXT DEFAULT ''
);`

func (d *DB) execAuthSchema() error {
	if _, err := d.db.Exec(authSchema); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (12, 'add auth tables')`)
	// Additive: tokens made before scopes become MCP-only, the narrower of the two.
	if err := d.addColumn("auth_tokens", "scope",
		`ALTER TABLE auth_tokens ADD COLUMN scope TEXT NOT NULL DEFAULT 'mcp'`); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (13, 'add auth_tokens.scope')`)
	return nil
}

type AuthUser struct {
	Username     string
	PasswordHash string
	Enabled      bool
	CreatedAt    int64
	UpdatedAt    int64
}

type AuthSession struct {
	IDHash    string `json:"-"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
	ExpiresAt int64  `json:"expires_at"`
	UserAgent string `json:"user_agent"`
	IP        string `json:"ip"`
}

type AuthToken struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	CreatedAt int64  `json:"created_at"`
	LastUsed  int64  `json:"last_used"`
	LastIP    string `json:"last_ip"`
	Scope     string `json:"scope"` // "mcp" or "api"
}

var ErrAuthUserExists = errors.New("auth user already exists")

func (d *DB) GetAuthUser() (AuthUser, bool, error) {
	var u AuthUser
	var en int
	err := d.db.QueryRow(`SELECT username, password_hash, enabled, created_at, updated_at FROM auth_user WHERE id = 1`).
		Scan(&u.Username, &u.PasswordHash, &en, &u.CreatedAt, &u.UpdatedAt)
	if err == sql.ErrNoRows {
		return u, false, nil
	}
	u.Enabled = en == 1
	return u, err == nil, err
}

func (d *DB) CreateAuthUser(username, passwordHash string) error {
	now := time.Now().Unix()
	res, err := d.db.Exec(`INSERT OR IGNORE INTO auth_user (id, username, password_hash, enabled, created_at, updated_at)
		VALUES (1, ?, ?, 0, ?, ?)`, username, passwordHash, now, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAuthUserExists
	}
	return nil
}

func (d *DB) SetAuthEnabled(on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := d.db.Exec(`UPDATE auth_user SET enabled = ?, updated_at = ? WHERE id = 1`, v, time.Now().Unix())
	return err
}

func (d *DB) SetAuthPassword(passwordHash string) error {
	_, err := d.db.Exec(`UPDATE auth_user SET password_hash = ?, updated_at = ? WHERE id = 1`, passwordHash, time.Now().Unix())
	return err
}

// CreateAuthSession stores s and replaces any session of the same device (same browser
// and address): signing in again on one device must not add a row each time.
func (d *DB) CreateAuthSession(s AuthSession) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.UserAgent != "" && s.IP != "" {
		if _, err := tx.Exec(`DELETE FROM auth_sessions WHERE user_agent = ? AND ip = ?`, s.UserAgent, s.IP); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO auth_sessions (id_hash, created_at, last_seen, expires_at, user_agent, ip)
		VALUES (?, ?, ?, ?, ?, ?)`, s.IDHash, s.CreatedAt, s.LastSeen, s.ExpiresAt, s.UserAgent, s.IP); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) GetAuthSession(idHash string) (AuthSession, bool, error) {
	s := AuthSession{IDHash: idHash}
	err := d.db.QueryRow(`SELECT created_at, last_seen, expires_at, user_agent, ip FROM auth_sessions WHERE id_hash = ?`, idHash).
		Scan(&s.CreatedAt, &s.LastSeen, &s.ExpiresAt, &s.UserAgent, &s.IP)
	if err == sql.ErrNoRows {
		return s, false, nil
	}
	return s, err == nil, err
}

// TouchAuthSession slides a session; false means the session no longer exists.
func (d *DB) TouchAuthSession(idHash string, lastSeen, expiresAt int64) (bool, error) {
	res, err := d.db.Exec(`UPDATE auth_sessions SET last_seen = ?, expires_at = ? WHERE id_hash = ?`, lastSeen, expiresAt, idHash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (d *DB) DeleteAuthSession(idHash string) error {
	_, err := d.db.Exec(`DELETE FROM auth_sessions WHERE id_hash = ?`, idHash)
	return err
}

func (d *DB) DeleteAuthSessionsExcept(idHash string) error {
	_, err := d.db.Exec(`DELETE FROM auth_sessions WHERE id_hash <> ?`, idHash)
	return err
}

func (d *DB) PurgeExpiredAuthSessions(now int64) error {
	_, err := d.db.Exec(`DELETE FROM auth_sessions WHERE expires_at < ?`, now)
	return err
}

func (d *DB) ListAuthSessions() ([]AuthSession, error) {
	rows, err := d.db.Query(`SELECT id_hash, created_at, last_seen, expires_at, user_agent, ip FROM auth_sessions ORDER BY last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuthSession
	for rows.Next() {
		var s AuthSession
		if err := rows.Scan(&s.IDHash, &s.CreatedAt, &s.LastSeen, &s.ExpiresAt, &s.UserAgent, &s.IP); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) CreateAuthToken(name, prefix, tokenHash, scope string, createdAt int64) (int64, error) {
	res, err := d.db.Exec(`INSERT INTO auth_tokens (name, prefix, token_hash, scope, created_at) VALUES (?, ?, ?, ?, ?)`,
		name, prefix, tokenHash, scope, createdAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) GetAuthTokenByHash(tokenHash string) (AuthToken, bool, error) {
	var t AuthToken
	err := d.db.QueryRow(`SELECT id, name, prefix, created_at, last_used, last_ip, scope FROM auth_tokens WHERE token_hash = ?`, tokenHash).
		Scan(&t.ID, &t.Name, &t.Prefix, &t.CreatedAt, &t.LastUsed, &t.LastIP, &t.Scope)
	if err == sql.ErrNoRows {
		return t, false, nil
	}
	return t, err == nil, err
}

func (d *DB) TouchAuthToken(id, lastUsed int64, ip string) error {
	_, err := d.db.Exec(`UPDATE auth_tokens SET last_used = ?, last_ip = ? WHERE id = ?`, lastUsed, ip, id)
	return err
}

func (d *DB) DeleteAuthToken(id int64) (bool, error) {
	res, err := d.db.Exec(`DELETE FROM auth_tokens WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (d *DB) ListAuthTokens() ([]AuthToken, error) {
	rows, err := d.db.Query(`SELECT id, name, prefix, created_at, last_used, last_ip, scope FROM auth_tokens ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuthToken
	for rows.Next() {
		var t AuthToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.CreatedAt, &t.LastUsed, &t.LastIP, &t.Scope); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ResetAuth clears user, sessions and tokens in one transaction.
func (d *DB) ResetAuth() error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	for _, q := range []string{`DELETE FROM auth_user`, `DELETE FROM auth_sessions`, `DELETE FROM auth_tokens`} {
		if _, err := tx.Exec(q); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
