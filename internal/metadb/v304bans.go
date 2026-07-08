package metadb

import "time"

// SaveV304Ban upserts a banned peer IP with the current timestamp.
func (d *DB) SaveV304Ban(ip string) error {
	_, err := d.db.Exec(`INSERT INTO v304_bans (ip, banned_at) VALUES (?, strftime('%s','now'))
		ON CONFLICT(ip) DO UPDATE SET banned_at = excluded.banned_at`, ip)
	return err
}

// LoadV304Bans purges bans older than ttl and returns the remaining banned IPs.
func (d *DB) LoadV304Bans(ttl time.Duration) ([]string, error) {
	cutoff := time.Now().Add(-ttl).Unix()
	_, _ = d.db.Exec(`DELETE FROM v304_bans WHERE banned_at < ?`, cutoff)

	rows, err := d.db.Query(`SELECT ip FROM v304_bans`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ips []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		ips = append(ips, ip)
	}
	return ips, rows.Err()
}
