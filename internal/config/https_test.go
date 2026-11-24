package config

import (
	"encoding/json"
	"testing"
)

func TestHTTPSDefaultOffAndPatch(t *testing.T) {
	var c Config
	if c.HTTPS.Enabled {
		t.Fatal("HTTPS must be off by default")
	}
	next, err := c.Patch([]byte(`{"https":{"enabled":true,"extra_hostnames":["10.0.0.5"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !next.HTTPS.Enabled || len(next.HTTPS.ExtraHostnames) != 1 || next.HTTPS.CertFile != "" {
		t.Fatalf("wrong patch: %+v", next.HTTPS)
	}
	b, _ := json.Marshal(next)
	var back Config
	if err := json.Unmarshal(b, &back); err != nil || !back.HTTPS.Equal(next.HTTPS) {
		t.Fatalf("round-trip: %v %+v", err, back.HTTPS)
	}
	if next.HTTPS.Equal(HTTPSConfig{Enabled: true}) {
		t.Fatal("Equal must compare extra_hostnames too")
	}
	if k := SignInOnlyChange(c, next); k != "https" {
		t.Fatalf("SignInOnlyChange = %q", k)
	}
	p := c
	p.AuthTrustedProxies = []string{"192.168.1.250"}
	if k := SignInOnlyChange(c, p); k != "auth_trusted_proxies" {
		t.Fatalf("SignInOnlyChange = %q", k)
	}
	if k := SignInOnlyChange(c, c); k != "" {
		t.Fatalf("no change: %q", k)
	}
}
