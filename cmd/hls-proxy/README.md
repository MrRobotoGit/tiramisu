# hls-proxy

Sidecar service for `tiramisu` HLS fallback. Resolves m3u8 stream URLs from
`vidsrc.me` and proxies HLS as byte-range seekable HTTP, so tiramisu's
`raCache` can serve the stream when a torrent has zero seeders.

**100% HTTP-only.** No Chromium, no Xvfb, no JavaScript execution.

## TL;DR for agents

If you arrive here and want to know "should I add a browser?" — **no**. The
extraction is fully static-server-rendered. See
[docs/changelog/2026-04-26-hls-proxy-pure-http.md](../../docs/changelog/2026-04-26-hls-proxy-pure-http.md)
for the full design rationale and the list of approaches that *did not* work
(yt-dlp, TLS fingerprinting, go-rod, FlareSolverr, REST APIs). Don't redo those.

## Endpoints

```
GET  /extract/{imdbID}   → JSON {m3u8_url, total_size}
GET  /stream/{imdbID}    → byte-range seekable stream (Content-Type: video/mp4)
GET  /health             → 200 ok
```

## Extraction chain (4 sequential HTTPS GETs, ~2-4s)

```
vidsrc.me/embed/movie?imdb={id}
  ↓ 301 redirect, HTML contains: <iframe src="//cloudnestra.com/rcp/{token1}">
cloudnestra.com/rcp/{token1}
  ↓ HTML contains: <iframe src="prorcp/{token2}">
cloudnestra.com/prorcp/{token2}
  ↓ HTML contains: file: "https://tmstr3.{v1}/pl/{token3}/master.m3u8"
substitute {v1} → cloudnestra.com (or fallback domains)
  ↓
master m3u8 with 360p/720p/1080p variants → pick highest BANDWIDTH (1080p)
```

The `{v1}` placeholder is replaced from a fallback list in
`vReplacements` (cloudnestra.com, neonhorizonworkshops.com,
orchidpixelgardens.com, wanderlynest.com). Each is HEAD-validated for
`Content-Type: application/vnd.apple.mpegurl`.

## Build

```bash
cd cmd/hls-proxy
GOOS=linux GOARCH=arm64 go build -o hls-proxy .
scp hls-proxy pi@192.168.1.2:/home/pi/Tiramisu/hls-proxy
ssh pi@192.168.1.2 'sudo systemctl restart hls-proxy'
```

Pure Go, no CGO, ~9 MB binary.

## Run

```bash
./hls-proxy -addr :8086 -timeout 15s -cache 90m
```

Flags:
- `-addr` listen address (default `:8086`)
- `-timeout` per-step HTTP timeout for extraction (default 15s; full chain has 4 steps)
- `-cache` m3u8 URL cache TTL (default 90m — embed token validity is 1-6h, 90m is safe)
- `-debug` verbose logging of extraction steps

## systemd service

`hls-proxy.service` is committed in this directory. Install on Pi:

```bash
sudo cp hls-proxy.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now hls-proxy
```

Logs: `/home/pi/Tiramisu/logs/hls-proxy.log`.

## Test

```bash
# health
curl http://localhost:8086/health
# → ok

# extract
curl -s http://localhost:8086/extract/tt0816692
# → {"m3u8_url":"https://tmstr3.cloudnestra.com/.../master.m3u8","total_size":0}

# stream HEAD
curl -s -I http://localhost:8086/stream/tt0816692
# → HTTP/1.1 206, Content-Length: ~6.34 GB for Interstellar 1080p

# stream byte range — verify MPEG-TS sync byte
curl -s -H "Range: bytes=0-1023" http://localhost:8086/stream/tt0816692 -o /tmp/t.bin
od -An -tx1 -N4 /tmp/t.bin
# → 47 40 00 30  (TS sync byte 0x47)
```

## Integration with tiramisu

`tiramisu/internal/hlsfallback/` is the client-side package. In
`tiramisu/main.go`:

1. Startup: probes `localhost:8086/health` → enables fallback if reachable
2. `Open()` handler: if no warmup hit + IMDB id known + proxy ready, spawns
   goroutine that calls `waitForTorrentActivity(hash, 5*time.Second)`
3. If no peer activity after 5s → swap `NativeReader` with `HTTPRangeReader`
   that proxies range requests to `/stream/{imdbID}`

See `tiramisu/main.go:880-905`.

## Known limitations

- **Single provider**: only `vidsrc.me`. If it goes down, no HLS fallback.
- **Movies only**: hardcoded `/embed/movie?imdb=`. Series need
  `/embed/tv?imdb=...&season=N&episode=N` (not implemented).
- **Max 1080p**: embed providers don't have 4K. For 4K HDR, torrents are
  superior.
- **Segment size estimation**: `duration × bandwidth / 8`, not HEAD-probed
  (would be 2000+ requests). Players tolerate small offsets between
  declared and actual sizes.
- **DNS**: segment domains (e.g. `noosphere-nectar.site`) may be blocked by
  consumer DNS filters (Pi-hole). Production Pi resolves via Cloudflare/Quad9.

## Files

- `main.go` — extractor + segment map + byte-range stream handler
- `go.mod` — single dep: stdlib (no go-rod, no third-party HTTP libraries)
- `hls-proxy.service` — systemd unit for the Pi
