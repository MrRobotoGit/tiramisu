package metadb

import "database/sql"

const friendsSchema = `
CREATE TABLE IF NOT EXISTS friends (
    name       TEXT PRIMARY KEY,
    pair_key   BLOB NOT NULL,
    role       TEXT NOT NULL,
    addr       TEXT NOT NULL DEFAULT '',
    addr_seq   INTEGER NOT NULL DEFAULT 0,
    my_seq     INTEGER NOT NULL DEFAULT 0,
    last_seen  INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS friend_invites (
    invite_hash BLOB PRIMARY KEY,
    secret      BLOB NOT NULL,
    expires_at  INTEGER NOT NULL,
    state       TEXT NOT NULL DEFAULT 'open',
    created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS friend_requests (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    invite_hash  BLOB NOT NULL,
    direction    TEXT NOT NULL,
    name         TEXT NOT NULL,
    addr         TEXT NOT NULL,
    nonce_mine   BLOB NOT NULL,
    nonce_theirs BLOB NOT NULL DEFAULT x'',
    secret       BLOB NOT NULL DEFAULT x'',
    state        TEXT NOT NULL DEFAULT 'pending',
    pair_key     BLOB NOT NULL DEFAULT x'',
    expires_at   INTEGER NOT NULL,
    created_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS friend_settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);`

func (d *DB) execFriendsSchema() error {
	if _, err := d.db.Exec(friendsSchema); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (18, 'add friends')`)
	// Schema 19, additive: ephemeral X25519 keys of the invite exchange.
	for _, c := range []string{"eph_priv", "pub_mine", "pub_theirs"} {
		if err := d.addColumn("friend_requests", c,
			`ALTER TABLE friend_requests ADD COLUMN `+c+` BLOB NOT NULL DEFAULT x''`); err != nil {
			return err
		}
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (19, 'add friend_requests ephemeral keys')`)
	if err := d.execFriendLibSchema(); err != nil {
		return err
	}
	// Schema 21, additive: a friend seen on the encrypted control channel is never spoken to in clear.
	if err := d.addColumn("friends", "secure", `ALTER TABLE friends ADD COLUMN secure INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	_, _ = d.db.Exec(`INSERT OR IGNORE INTO schema_version (version, description) VALUES (21, 'add friends.secure')`)
	return nil
}

// Friend is a paired node; Role is our side of the pairing ("inviter" or "joiner").
type Friend struct {
	Name                string
	PairKey             []byte
	Role                string
	Addr                string
	AddrSeq, MySeq      int64
	LastSeen, CreatedAt int64
	Secure              bool // seen on the encrypted control channel: plaintext refused
}

// FriendInvite is an invite code we issued; Hash is the infohash the joiner dials.
type FriendInvite struct {
	Hash, Secret []byte
	ExpiresAt    int64
	State        string
	CreatedAt    int64
}

// FriendRequest is a pairing request, Direction "in" (they used our code) or "out" (we used theirs).
type FriendRequest struct {
	ID                     int64
	InviteHash             []byte
	Direction, Name, Addr  string
	NonceMine, NonceTheirs []byte
	Secret                 []byte
	State                  string
	PairKey                []byte
	EphPriv                []byte // our ephemeral private key, out requests only, until the decision
	PubMine, PubTheirs     []byte
	ExpiresAt, CreatedAt   int64
}

// ListFriends returns all friends ordered by name.
func (d *DB) ListFriends() ([]Friend, error) {
	rows, err := d.db.Query(`SELECT name, pair_key, role, addr, addr_seq, my_seq, last_seen, created_at, secure FROM friends ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Friend
	for rows.Next() {
		var f Friend
		if err := rows.Scan(&f.Name, &f.PairKey, &f.Role, &f.Addr, &f.AddrSeq, &f.MySeq, &f.LastSeen, &f.CreatedAt, &f.Secure); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// PutFriend is a plain INSERT: a duplicate name is an error, never an overwrite of another friend's key.
func (d *DB) PutFriend(f Friend) error {
	_, err := d.db.Exec(`INSERT INTO friends (name, pair_key, role, addr, addr_seq, my_seq, last_seen, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		f.Name, f.PairKey, f.Role, f.Addr, f.AddrSeq, f.MySeq, f.LastSeen, f.CreatedAt)
	return err
}

func (d *DB) DeleteFriend(name string) error {
	_, err := d.db.Exec(`DELETE FROM friends WHERE name = ?`, name)
	return err
}

func (d *DB) UpdateFriendAddr(name, addr string, seen int64) error {
	_, err := d.db.Exec(`UPDATE friends SET addr = ?, last_seen = ? WHERE name = ?`, addr, seen, name)
	return err
}

func (d *DB) SetFriendAddrSeq(name string, addrSeq, mySeq int64) error {
	_, err := d.db.Exec(`UPDATE friends SET addr_seq = ?, my_seq = ? WHERE name = ?`, addrSeq, mySeq, name)
	return err
}

// SetFriendSecure records that the friend with this pairing speaks the encrypted control channel.
func (d *DB) SetFriendSecure(name string, pairKey []byte) error {
	_, err := d.db.Exec(`UPDATE friends SET secure = 1 WHERE name = ? AND pair_key = ?`, name, pairKey)
	return err
}

// ClearFriendSecure forgets it, on the user's request (the friend went back to an older version).
func (d *DB) ClearFriendSecure(name string, pairKey []byte) error {
	_, err := d.db.Exec(`UPDATE friends SET secure = 0 WHERE name = ? AND pair_key = ?`, name, pairKey)
	return err
}

func (d *DB) TouchFriend(name string, seen int64) error {
	_, err := d.db.Exec(`UPDATE friends SET last_seen = ? WHERE name = ?`, seen, name)
	return err
}

func (d *DB) PutInvite(in FriendInvite) error {
	_, err := d.db.Exec(`INSERT INTO friend_invites (invite_hash, secret, expires_at, state, created_at) VALUES (?, ?, ?, ?, ?)`,
		in.Hash, in.Secret, in.ExpiresAt, in.State, in.CreatedAt)
	return err
}

// GetInvite returns sql.ErrNoRows when the hash is unknown.
func (d *DB) GetInvite(hash []byte) (FriendInvite, error) {
	var in FriendInvite
	err := d.db.QueryRow(`SELECT invite_hash, secret, expires_at, state, created_at FROM friend_invites WHERE invite_hash = ?`, hash).
		Scan(&in.Hash, &in.Secret, &in.ExpiresAt, &in.State, &in.CreatedAt)
	return in, err
}

// ListInvites returns all invites, oldest first.
func (d *DB) ListInvites() ([]FriendInvite, error) {
	rows, err := d.db.Query(`SELECT invite_hash, secret, expires_at, state, created_at FROM friend_invites ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FriendInvite
	for rows.Next() {
		var in FriendInvite
		if err := rows.Scan(&in.Hash, &in.Secret, &in.ExpiresAt, &in.State, &in.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (d *DB) SetInviteState(hash []byte, state string) error {
	_, err := d.db.Exec(`UPDATE friend_invites SET state = ? WHERE invite_hash = ?`, state, hash)
	return err
}

// PurgeInvites deletes expired, used and cancelled invites (and their secrets).
func (d *DB) PurgeInvites(now int64) error {
	_, err := d.db.Exec(`DELETE FROM friend_invites WHERE expires_at <= ? OR state IN ('used', 'cancelled')`, now)
	return err
}

const friendRequestCols = `id, invite_hash, direction, name, addr, nonce_mine, nonce_theirs, secret, state, pair_key, eph_priv, pub_mine, pub_theirs, expires_at, created_at`

func scanFriendRequest(row interface{ Scan(...any) error }) (FriendRequest, error) {
	var r FriendRequest
	err := row.Scan(&r.ID, &r.InviteHash, &r.Direction, &r.Name, &r.Addr, &r.NonceMine, &r.NonceTheirs,
		&r.Secret, &r.State, &r.PairKey, &r.EphPriv, &r.PubMine, &r.PubTheirs, &r.ExpiresAt, &r.CreatedAt)
	return r, err
}

// PutRequest inserts a request and returns its id.
func (d *DB) PutRequest(r FriendRequest) (int64, error) {
	res, err := d.db.Exec(`INSERT INTO friend_requests (invite_hash, direction, name, addr, nonce_mine, nonce_theirs, secret, state, pair_key, eph_priv, pub_mine, pub_theirs, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.InviteHash, r.Direction, r.Name, r.Addr, nonEmpty(r.NonceMine), nonEmpty(r.NonceTheirs), nonEmpty(r.Secret),
		r.State, nonEmpty(r.PairKey), nonEmpty(r.EphPriv), nonEmpty(r.PubMine), nonEmpty(r.PubTheirs), r.ExpiresAt, r.CreatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// nonEmpty turns a nil slice into an empty one: a nil []byte is stored as NULL, which NOT NULL columns reject.
func nonEmpty(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// GetRequest returns sql.ErrNoRows when the id is unknown.
func (d *DB) GetRequest(id int64) (FriendRequest, error) {
	return scanFriendRequest(d.db.QueryRow(`SELECT `+friendRequestCols+` FROM friend_requests WHERE id = ?`, id))
}

func (d *DB) ListRequests() ([]FriendRequest, error) {
	rows, err := d.db.Query(`SELECT ` + friendRequestCols + ` FROM friend_requests ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FriendRequest
	for rows.Next() {
		r, err := scanFriendRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRequestState records a decision and erases the ephemeral private key.
func (d *DB) SetRequestState(id int64, state string, pairKey []byte, expiresAt int64) error {
	_, err := d.db.Exec(`UPDATE friend_requests SET state = ?, pair_key = ?, eph_priv = x'', expires_at = ? WHERE id = ?`,
		state, nonEmpty(pairKey), expiresAt, id)
	return err
}

// SetRequestApproved stores the derived pair key with the exchange it came from and erases the
// ephemeral private key.
func (d *DB) SetRequestApproved(id int64, pairKey, nonceTheirs, pubMine, pubTheirs []byte, expiresAt int64) error {
	_, err := d.db.Exec(`UPDATE friend_requests SET state = 'approved', pair_key = ?, nonce_theirs = ?, pub_mine = ?,
		pub_theirs = ?, eph_priv = x'', expires_at = ? WHERE id = ?`,
		nonEmpty(pairKey), nonEmpty(nonceTheirs), nonEmpty(pubMine), nonEmpty(pubTheirs), expiresAt, id)
	return err
}

func (d *DB) DeleteRequest(id int64) error {
	_, err := d.db.Exec(`DELETE FROM friend_requests WHERE id = ?`, id)
	return err
}

func (d *DB) PurgeRequests(now int64) error {
	_, err := d.db.Exec(`DELETE FROM friend_requests WHERE expires_at <= ?`, now)
	return err
}

// GetFriendSetting returns "" with a nil error when the key is not set.
func (d *DB) GetFriendSetting(key string) (string, error) {
	var v string
	err := d.db.QueryRow(`SELECT value FROM friend_settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (d *DB) SetFriendSetting(key, value string) error {
	_, err := d.db.Exec(`INSERT INTO friend_settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
