package config

import (
	"net"
	"net/url"
)

// GoStormBindIP is the address the GoStorm API (:8090) listens on. Only the engine
// itself calls it, so it stays on loopback unless gostorm_listen_ip opens it
// ("all" = every interface, as before 2026-11-17, or a specific IP). warn is set
// when gostorm_url points elsewhere: restricting then would cut the engine off.
func (c *Config) GoStormBindIP() (ip string, warn string) {
	switch c.GoStormListenIP {
	case "all", "*", "0.0.0.0", "::":
		return "", ""
	case "":
	default:
		return c.GoStormListenIP, ""
	}
	if u, err := url.Parse(c.GoStormBaseURL); err == nil && u.Hostname() != "" {
		h := u.Hostname()
		if h != "localhost" {
			if a := net.ParseIP(h); a == nil || !a.IsLoopback() {
				return "", "gostorm_url (" + c.GoStormBaseURL + ") is not a loopback address: :8090 stays open on every interface; set it to http://127.0.0.1:8090 to close it"
			}
		}
	}
	return "127.0.0.1", ""
}

// ApplyGoStormBind logs where :8090 will listen and returns the address for the GoStorm args.
func (c *Config) ApplyGoStormBind(logf func(string, ...any)) string {
	ip, warn := c.GoStormBindIP()
	switch {
	case warn != "":
		logf("[GoStorm] WARNING: %s", warn)
	case ip == "":
		logf("[GoStorm] API :8090 open on every interface (gostorm_listen_ip)")
	default:
		logf("[GoStorm] API :8090 listening on %s only", ip)
	}
	return ip
}
