package auth

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestLimiterPerIP(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < 5; i++ {
		if !l.allow("1.1.1.1") {
			t.Fatalf("attempt %d blocked", i)
		}
	}
	if l.allow("1.1.1.1") {
		t.Fatal("sixth attempt allowed")
	}
	if !l.allow("2.2.2.2") {
		t.Fatal("different IP blocked")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 7.7.7.7")
	if ip := clientIP(r, nil); ip != "10.0.0.5" {
		t.Fatalf("without trusted proxies: %s", ip)
	}
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	if ip := clientIP(r, trusted); ip != "7.7.7.7" {
		t.Fatalf("with trusted proxy: %s", ip)
	}
}

func TestLimiterGroupsIPv6By64(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < 5; i++ {
		l.allow("2001:db8:1:2::" + string(rune('a'+i)))
	}
	if l.allow("2001:db8:1:2::ffff") {
		t.Fatal("same /64 with different addresses not limited")
	}
	if !l.allow("2001:db8:1:3::1") {
		t.Fatal("different /64 blocked")
	}
}
