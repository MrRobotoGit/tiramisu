# Troubleshooting

Common problems, their causes and fixes, and where Tiramisu keeps its files.

[← Back to the README](../README.md) · [Documentation index](README.md)

---

## Common Problems

<details>
<summary><b>Plex shows buffering or "Playback Error"</b></summary>

Check warmup cache:
```bash
curl -s http://127.0.0.1:9080/metrics | jq '.read_ahead_active_bytes'
```

If empty, force a Plex/Jellyfin library scan. The scan reads the first MB of each file and populates the SSD head warmup.

Check active torrent status:
```bash
curl -s http://127.0.0.1:9080/api/torrents | \
  jq '.[] | {title: .title[:50], speed_mbs, peers, status}'
```

If peer count < 3 and you are routing through WireGuard, check NAT-PMP configuration.

</details>

<details>
<summary><b>smbd D-state or Samba hangs during Plex/Jellyfin scan</b></summary>

Almost always one of three causes:

1. **`oplocks = no` missing** from `smb.conf`: kernel acquires exclusive locks on FUSE files, smbd blocks indefinitely
2. **`vfs objects = fileid` missing**: Synology receives truncated 32-bit inodes
3. **`serverino` dropped** on Synology CIFS mount: check with `mount | grep tiramisu`

Check for D-state processes:
```bash
ps aux | grep -E 'smbd|tiramisu|fuse' | awk '$8 == "D"'
```

</details>

<details>
<summary><b>Few seeders or slow downloads</b></summary>

If you are routing BitTorrent traffic through WireGuard, enable NAT-PMP to restore inbound port reachability through the VPN gateway:
```json
"natpmp": {
  "enabled": true,
  "gateway": "10.2.0.1",
  "vpn_interface": "wg0"
}
```

Without an open inbound port, peers cannot initiate connections: the engine
relies solely on outbound ones.

**If you are not using a VPN, make sure the peer port is open on your router.**
The engine asks the router for a mapping over UPnP at start (unless
`DisableUPNP` is set, or Tiramisu's NAT-PMP manages the port). If your router
has UPnP turned off, forward the port yourself. Without a mapping the port
simply stays closed and peer counts stay low; nothing is logged as an error.

1. Pin the port. In the Control Panel, set **Tentacle → Listen Port** to a
   fixed high port (e.g. `64407`). Left at `0` the engine picks a new random port
   on every start, which no static forward can follow.
2. Forward that same port to the machine running Tiramisu, **both TCP and UDP**.
3. Verify from outside your network that the port is open: from the inside, a
   closed port and an open one look the same.

A router that drops the unforwarded traffic *and* logs every drop can also wear
out its own flash storage over time; if your router has a firewall logging
option, check it is not writing a line per dropped packet.

</details>

<details>
<summary><b>Service fails to start</b></summary>

```bash
sudo systemctl status tiramisu
tail -30 ~/Tiramisu/logs/tiramisu.log
```

If the FUSE mount is stale:
```bash
fusermount3 -uz /mnt/tiramisu-mkv-virtual
sudo systemctl start tiramisu
```

Ensure mount point exists:
```bash
sudo mkdir -p /mnt/tiramisu-mkv-virtual
sudo chown pi:pi /mnt/tiramisu-mkv-virtual
```

</details>

<details>
<summary><b>Webhook not triggering Priority Mode (Plex or Jellyfin)</b></summary>

Verify connectivity:
```bash
curl -v http://127.0.0.1:9080/plex/webhook \
  -X POST -H 'Content-Type: application/json' \
  -d '{"event":"media.play","Metadata":{"librarySectionType":"movie","guid":"imdb://tt1234567","Guid":[{"id":"imdb://tt1234567"}]}}'
```

Check logs:
```bash
grep -i webhook ~/Tiramisu/logs/tiramisu.log | tail -20
```

If the connection appears in logs but IMDB matching fails, verify the raw payload contains `imdb://tt\d+`. Tiramisu uses regex on the raw JSON string, so it works even with localized titles.

**Jellyfin-specific**: if you see no log entry at all, confirm the `Content-Type` header is set correctly (Key: `Content-Type`, Value: `application/json`). Jellyfin does not send the request when the Content-Type override is malformed.

</details>

<details>
<summary><b>High CPU usage</b></summary>

Profile the live binary:
```bash
go tool pprof -top "http://127.0.0.1:9080/debug/pprof/profile?seconds=30"
```

Expected hot paths: `sha1.blockGeneric` (no crypto extensions on Pi 4 A72), `io.ReadFull`, `sync.(*Mutex).Lock`. Regenerating the PGO profile typically reduces CPU 5–7%.

</details>

---

<details>
<summary><b>Forgot the sign-in password, or <code>:9080</code> answers 503</b></summary>

Stop the service, reset sign-in, start it again:
```bash
sudo systemctl stop tiramisu
~/Tiramisu/tiramisu --reset-auth
sudo systemctl start tiramisu
```
This clears the user, the sessions and the API tokens and turns sign-in off. With
Docker, start the container once with `-e TIRAMISU_RESET_AUTH=1`, then remove the
variable.

A `503` with "authentication state unavailable" means sign-in was on but
`STATE/tiramisu.db` could not be read (or was replaced by an empty one): Tiramisu
stays closed instead of opening up. Restore the database, or reset as above.

</details>

<details>
<summary><b>Tiramisu stops at start with "config.db (BBolt) was never imported"</b></summary>

Up to v1.11.17 the torrent list and the engine settings were in `config.db` (BBolt),
next to the binary (in Docker, in `/state`). Later versions up to v1.12 import it into
`STATE/tiramisu.db` on their first start; v1.13.0 no longer reads it, and stops rather
than start with an empty torrent list and default settings. `config.db` is left
untouched, so nothing is lost. Start v1.12.03 once (Docker: the
`mrrobotogit/tiramisu:v1.12.03` image with the same volumes) to import it, then
v1.13.0 again. A `config.db` left behind after an import is ignored (the log says
so) and can be deleted.

</details>

<details>
<summary><b>Scripts or tools get 401, or cannot reach <code>:8090</code></b></summary>

With sign-in on, every call to `:9080` needs an API token
(`-H "Authorization: Bearer tmr_..."`), created in **Control Panel → Access**.
Since v1.13.0 nothing listens on `:8090`: the engine runs in-process and is reached
through `:9080` only. Move scripts to the Library API, `/api/torrents` and
`/api/gostorm/settings` (see [API Quick Reference](api.md#api-quick-reference)).

</details>

<details>
<summary><b>HTTPS: the browser says the connection is not private, or a client gets <code>403 use https</code></b></summary>

With HTTPS on (**Control Panel → Access → HTTPS**), each device has to trust
Tiramisu's local CA once:

- **Browser warning** ("not private", `NET::ERR_CERT_AUTHORITY_INVALID`): the device
  does not trust the CA yet. Accept the warning once, download `tiramisu-ca.crt`
  from the card, check its SHA-256 against the card, and trust it (macOS: Keychain
  Access → *Always Trust*; iOS: install the profile, then enable it in
  *Settings → General → About → Certificate Trust Settings*).
- **Still "not secure" with the CA trusted**: the address you use is not in the
  certificate (a Tailscale IP or `*.ts.net` name, another hostname). Add it to
  **Extra hostnames** in the card (Advanced), then Apply & restart: the CA stays the
  same, nothing to reinstall.
- **`403 {"error":"use https"}`** on an API call: the client still uses `http://`
  from another machine while sign-in is on. Switch it to `https://`.
- **`CERTIFICATE_VERIFY_FAILED`** in a Python client although the CA is in the
  system store: `httpx` and `requests` use `certifi`. Set `SSL_CERT_FILE` and
  `REQUESTS_CA_BUNDLE` to the CA file or to the system bundle
  (`/etc/ssl/certs/ca-certificates.crt` on Debian/Ubuntu). Node clients need
  `NODE_EXTRA_CA_CERTS`.
- **The card says the certificate files could not be read**: the paths in
  `https.cert_file` / `https.key_file` are wrong or not readable by the service
  user; Tiramisu is serving the local CA meanwhile (`[TLS] ERROR` in the log).

</details>

## Key File Locations

Paths below use the defaults set by `install.sh`. All are configurable during installation.

**Runtime (install directory, default `~/Tiramisu/`)**

| Path | Purpose |
|------|---------|
| `~/Tiramisu/tiramisu` | Production binary |
| `~/Tiramisu/STATE/tiramisu.db` | SQLite state database: the configuration and its history, the torrent list and Tentacle settings, inode map, sync caches, episode registry, scheduler state, sign-in |
| `~/Tiramisu/STATE/config.last.json` | Copy of the configuration, rewritten at every save: the way back if the database cannot be opened |
| `~/Tiramisu/config.json.imported-*` | A `config.json` already imported, kept as it was |
| `~/Tiramisu/logs/` | All service logs (`tiramisu.log`, `movies-sync.log`, `tv-sync.log`, `watchlist-sync.log`) |
| `/mnt/tiramisu-mkv-virtual/` | FUSE mount point (served to Plex/Jellyfin -> Samba) |
| `/etc/systemd/system/tiramisu.service` | systemd service definition |

**Build (cloned repository)**

| Path | Purpose |
|------|---------|
| `~/Tiramisu/` | Install directory: clone, build and runtime |
| `~/Tiramisu/default.pgo` | PGO profile, regenerate after major code changes |
