// Package geoip maps peer addresses to a country and a network (AS number and owner) with the
// DB-IP Lite Country and ASN databases (CC BY 4.0, "IP geolocation by DB-IP"), downloaded into
// the state dir and refreshed monthly. Off by default (peer_geoip_enabled). No address leaves
// the host.
package geoip

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

const (
	maxAge = 32 * 24 * time.Hour
	check  = 24 * time.Hour
)

// Dataset is one DB-IP Lite file; URLFormat is a var field so tests can point it at a fake server.
type Dataset struct {
	File      string
	URLFormat string
}

var (
	CountryDB = &Dataset{File: "dbip-country-lite.mmdb", URLFormat: "https://download.db-ip.com/free/dbip-country-lite-%04d-%02d.mmdb.gz"}
	ASNDB     = &Dataset{File: "dbip-asn-lite.mmdb", URLFormat: "https://download.db-ip.com/free/dbip-asn-lite-%04d-%02d.mmdb.gz"}
)

// Info is what a peer address resolves to; empty fields are unknown.
type Info struct {
	CC  string // ISO country code
	ASN uint   // autonomous system number
	Org string // network owner, e.g. "Hetzner Online GmbH"
}

// DB answers lookups; the zero value answers an empty Info until files are loaded.
type DB struct {
	mu      sync.RWMutex
	country *maxminddb.Reader
	asn     *maxminddb.Reader
}

type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

type asnRecord struct {
	Number uint   `maxminddb:"autonomous_system_number"`
	Org    string `maxminddb:"autonomous_system_organization"`
}

// Load opens (memory-maps) path as ds and swaps it in, closing the previous file.
func (d *DB) Load(ds *Dataset, path string) error {
	r, err := maxminddb.Open(path)
	if err != nil {
		return err
	}
	d.mu.Lock()
	slot := &d.country
	if ds == ASNDB {
		slot = &d.asn
	}
	old := *slot
	*slot = r
	d.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

func parse(addr string) (net.IP, bool) {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		ap, err2 := netip.ParseAddrPort(addr)
		if err2 != nil {
			return nil, false
		}
		ip = ap.Addr()
	}
	return net.IP(ip.Unmap().AsSlice()), true
}

// Lookup resolves "ip" or "ip:port".
func (d *DB) Lookup(addr string) Info {
	var out Info
	ip, ok := parse(addr)
	if !ok {
		return out
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.country != nil {
		var rec countryRecord
		if d.country.Lookup(ip, &rec) == nil {
			out.CC = rec.Country.ISOCode
		}
	}
	if d.asn != nil {
		var rec asnRecord
		if d.asn.Lookup(ip, &rec) == nil {
			out.ASN, out.Org = rec.Number, rec.Org
		}
	}
	return out
}

// Start loads the cached files if any, then keeps them fresh until ctx ends.
func Start(ctx context.Context, stateDir string, logf func(string, ...any)) *DB {
	db := &DB{}
	dir := filepath.Join(stateDir, "geoip")
	sets := []*Dataset{CountryDB, ASNDB}
	for _, ds := range sets {
		if err := db.Load(ds, filepath.Join(dir, ds.File)); err == nil {
			logf("[GeoIP] loaded %s", ds.File)
		}
	}
	go func() {
		t := time.NewTicker(check)
		defer t.Stop()
		for {
			for _, ds := range sets {
				path := filepath.Join(dir, ds.File)
				if fresh(path, time.Now()) {
					continue
				}
				if err := Download(ctx, http.DefaultClient, ds, dir, time.Now()); err != nil {
					logf("[GeoIP] %s update failed (keeping the current file): %v", ds.File, err)
				} else if err := db.Load(ds, path); err != nil {
					logf("[GeoIP] %s load failed: %v", ds.File, err)
				} else {
					logf("[GeoIP] %s updated (IP geolocation by DB-IP)", ds.File)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return db
}

func fresh(path string, now time.Time) bool {
	fi, err := os.Stat(path)
	return err == nil && now.Sub(fi.ModTime()) < maxAge
}

// Download fetches this month's file (last month's early in the month, before DB-IP
// publishes) and writes it atomically into dir.
func Download(ctx context.Context, c *http.Client, ds *Dataset, dir string, now time.Time) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	var lastErr error
	for _, m := range []time.Time{now, now.AddDate(0, -1, 0)} {
		url := fmt.Sprintf(ds.URLFormat, m.Year(), int(m.Month()))
		if lastErr = fetch(ctx, c, url, dir, ds.File); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func fetch(ctx context.Context, c *http.Client, url, dir, file string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, file+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, io.LimitReader(zr, 256<<20)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// A truncated or foreign file must not replace a good one.
	r, err := maxminddb.Open(tmp.Name())
	if err != nil {
		return fmt.Errorf("downloaded file is not a valid database: %w", err)
	}
	r.Close()
	return os.Rename(tmp.Name(), filepath.Join(dir, file))
}

// PeerInfo is Lookup in the shape torr.PeerInfo takes.
func (d *DB) PeerInfo(addr string) (string, uint, string) {
	i := d.Lookup(addr)
	return i.CC, i.ASN, i.Org
}
