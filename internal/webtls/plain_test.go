package webtls

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func req(method, path, remote, accept, bearer string) *http.Request {
	r := httptest.NewRequest(method, "http://192.168.1.20:9080"+path, nil)
	r.RemoteAddr = remote
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

func noHostAddrs(t *testing.T, addrs ...string) {
	orig := hostAddrs
	hostAddrs = func() []netip.Addr {
		var out []netip.Addr
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out
	}
	local.at = time.Time{}
	t.Cleanup(func() { hostAddrs = orig; local.at = time.Time{} })
}

func TestPlainPolicy(t *testing.T) {
	noHostAddrs(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	var noted []string
	note := func(tok, ip string) { noted = append(noted, tok+"@"+ip) }
	lan, lo, proxy := "192.168.1.31:5000", "127.0.0.1:5000", "192.168.1.250:5000"
	trusted := []netip.Prefix{netip.MustParsePrefix("192.168.1.250/32")}
	cases := []struct {
		name     string
		accessOn bool
		r        *http.Request
		code     int
	}{
		{"loopback api, access on", true, req("GET", "/metrics", lo, "", ""), 200},
		{"loopback v6", true, req("GET", "/metrics", "[::1]:5000", "", ""), 200},
		{"webhook lan, access on", true, req("POST", "/plex/webhook", lan, "", ""), 200},
		{"webhook non-canonical path", true, req("POST", "/x/../webhook", lan, "", ""), 200},
		{"lan page", true, req("GET", "/control", lan, "text/html", ""), 308},
		{"lan page, access off", false, req("GET", "/control", lan, "text/html", ""), 308},
		{"api lan, access on", true, req("GET", "/api/health", lan, "", ""), 403},
		{"api lan, access off", false, req("GET", "/api/health", lan, "", ""), 200},
		{"POST html lan, access on", true, req("POST", "/api/config", lan, "text/html", ""), 403},
		{"trusted proxy, page", true, req("GET", "/control", proxy, "text/html", ""), 200},
		{"webhook with trailing slash is not the webhook", true, req("POST", "/webhook/", lan, "", ""), 403},
		{"HEAD page", true, req("HEAD", "/control", lan, "text/html", ""), 308},
		{"loopback v4-mapped", true, req("GET", "/metrics", "[::ffff:127.0.0.1]:5000", "", ""), 200},
		{"trusted proxy, api", true, req("GET", "/api/health", proxy, "", ""), 200},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		PlainPolicy(ok, c.accessOn, trusted, note).ServeHTTP(w, c.r)
		if w.Code != c.code {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.code)
		}
	}
	w := httptest.NewRecorder()
	PlainPolicy(ok, true, trusted, note).ServeHTTP(w, req("GET", "/control?x=1", lan, "text/html", ""))
	if loc := w.Header().Get("Location"); loc != "https://192.168.1.20:9080/control?x=1" {
		t.Fatalf("Location %q", loc)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("the 308 must not be cached")
	}
	w = httptest.NewRecorder()
	PlainPolicy(ok, true, trusted, note).ServeHTTP(w, req("GET", "/api/health", lan, "", ""))
	if w.Body.String() != `{"error":"use https"}` || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("403: %q %v", w.Body.String(), w.Header())
	}
	r := req("GET", "/control", lan, "text/html", "")
	r.Host = ""
	w = httptest.NewRecorder()
	PlainPolicy(ok, false, trusted, note).ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("without Host, no broken redirect: %d", w.Code)
	}
	PlainPolicy(ok, true, trusted, note).ServeHTTP(httptest.NewRecorder(), req("GET", "/api/health", lan, "", "tmr_abc"))
	PlainPolicy(ok, false, trusted, note).ServeHTTP(httptest.NewRecorder(), req("GET", "/api/health", lan, "", "tmr_def"))
	PlainPolicy(ok, true, trusted, note).ServeHTTP(httptest.NewRecorder(), req("GET", "/metrics", lo, "", "tmr_ghi"))
	if len(noted) != 2 || noted[0] != "tmr_abc@192.168.1.31" || noted[1] != "tmr_def@192.168.1.31" {
		t.Fatalf("cleartext uses: %v (loopback does not count)", noted)
	}
}

// On the Pi a loopback connection can arrive from the host's own LAN address (VPN NAT).
func TestPlainPolicyOwnAddressIsLocal(t *testing.T) {
	noHostAddrs(t, "192.168.1.20")
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	w := httptest.NewRecorder()
	PlainPolicy(ok, true, nil, nil).ServeHTTP(w, req("GET", "/metrics", "192.168.1.20:40306", "", ""))
	if w.Code != 200 {
		t.Fatalf("own address must count as this machine: %d", w.Code)
	}
	w = httptest.NewRecorder()
	PlainPolicy(ok, true, nil, nil).ServeHTTP(w, req("GET", "/metrics", "192.168.1.31:40306", "", ""))
	if w.Code != 403 {
		t.Fatalf("another host must still get 403: %d", w.Code)
	}
}
