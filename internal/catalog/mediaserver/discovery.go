package mediaserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// Product names returned by Identify and Discover.
const (
	ProductPlex     = "Plex"
	ProductJellyfin = "Jellyfin"
)

// Discovery ports and probes. Both servers answer an unauthenticated UDP
// broadcast: Plex speaks GDM, Jellyfin its own one-line protocol.
const (
	plexGDMPort      = 32414
	jellyfinDiscPort = 7359
)

var (
	plexGDMProbe      = []byte("M-SEARCH * HTTP/1.0\r\n\r\n")
	jellyfinDiscProbe = []byte("who is JellyfinServer?")
)

// Discovered is one media server that answered a broadcast.
type Discovered struct {
	Product string `json:"product"`
	Name    string `json:"name"`
	Address string `json:"address"` // http://host:port
}

// Discover broadcasts both probes and collects replies until wait elapses.
// A broadcast does not cross subnets and Jellyfin lets users switch its
// discovery off, so an empty result does not prove no server is running.
func Discover(ctx context.Context, wait time.Duration) []Discovered {
	type reply struct{ found []Discovered }
	ch := make(chan reply, 2)

	go func() { ch <- reply{probe(ctx, plexGDMPort, plexGDMProbe, wait, parsePlexGDM)} }()
	go func() { ch <- reply{probe(ctx, jellyfinDiscPort, jellyfinDiscProbe, wait, parseJellyfin)} }()

	var out []Discovered
	for i := 0; i < 2; i++ {
		out = append(out, (<-ch).found...)
	}
	return out
}

// probe sends one broadcast datagram and parses every reply that arrives
// before the deadline. Each responder is reported once.
func probe(ctx context.Context, port int, payload []byte, wait time.Duration,
	parse func(body []byte, from net.IP) (Discovered, bool)) []Discovered {

	pc, err := broadcastConn(ctx)
	if err != nil {
		return nil
	}
	defer pc.Close()

	dst := &net.UDPAddr{IP: net.IPv4bcast, Port: port}
	if _, err := pc.WriteTo(payload, dst); err != nil {
		return nil
	}

	deadline := time.Now().Add(wait)
	_ = pc.SetReadDeadline(deadline)

	seen := make(map[string]bool)
	var out []Discovered
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			break
		}
		udp, ok := from.(*net.UDPAddr)
		if !ok || seen[udp.IP.String()] {
			continue
		}
		seen[udp.IP.String()] = true
		if d, ok := parse(buf[:n], udp.IP); ok {
			out = append(out, d)
		}
	}
	return out
}

// broadcastConn opens an ephemeral UDP socket with SO_BROADCAST set; without
// that option the kernel refuses a send to 255.255.255.255.
func broadcastConn(ctx context.Context) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var setErr error
		if err := c.Control(func(fd uintptr) {
			setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
		}); err != nil {
			return err
		}
		return setErr
	}}
	return lc.ListenPacket(ctx, "udp4", ":0")
}

// parsePlexGDM reads a GDM reply, which is HTTP-shaped but not HTTP:
// "HTTP/1.0 200 OK" followed by "Key: value" lines carrying Name and Port.
func parsePlexGDM(body []byte, from net.IP) (Discovered, bool) {
	if !strings.Contains(string(body), "plex/media-server") {
		return Discovered{}, false
	}
	d := Discovered{Product: ProductPlex}
	port := "32400"
	for _, line := range strings.Split(string(body), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "name":
			d.Name = v
		case "port":
			if v != "" {
				port = v
			}
		}
	}
	d.Address = fmt.Sprintf("http://%s:%s", from.String(), port)
	return d, true
}

// parseJellyfin reads the JSON reply. Address is the server's own view of its
// URL and can be empty or unreachable, so the sender IP is the fallback.
func parseJellyfin(body []byte, from net.IP) (Discovered, bool) {
	var r struct {
		Address string `json:"Address"`
		Name    string `json:"Name"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Discovered{}, false
	}
	d := Discovered{Product: ProductJellyfin, Name: r.Name, Address: r.Address}
	if d.Address == "" {
		d.Address = fmt.Sprintf("http://%s:8096", from.String())
	}
	return d, true
}

// Identify probes a base URL and reports which product answers. Both endpoints
// are unauthenticated, so this works before a token is configured. Returns ""
// when the host answers with neither.
func Identify(ctx context.Context, url string, timeout time.Duration) string {
	url = strings.TrimRight(url, "/")
	if url == "" {
		return ""
	}
	if body, ok := getBody(ctx, url+"/identity", timeout); ok &&
		strings.Contains(string(body), "machineIdentifier") {
		return ProductPlex
	}
	if body, ok := getBody(ctx, url+"/System/Info/Public", timeout); ok {
		var r struct{ ProductName string }
		if json.Unmarshal(body, &r) == nil && strings.Contains(r.ProductName, "Jellyfin") {
			return ProductJellyfin
		}
	}
	return ""
}

func getBody(ctx context.Context, url string, timeout time.Duration) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return body, err == nil
}
