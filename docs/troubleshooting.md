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
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"action":"active"}' http://127.0.0.1:8090/torrents | \
  jq '.[] | {title: .title[:50], speed_mbps: ((.download_speed//0)/1048576|round), peers: (.active_peers//0)}'
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

**If you are not using a VPN, forward the peer port on your router.** Tiramisu
asks the router for the mapping over UPnP on its own, but many routers ship with
UPnP disabled (and some ISP routers do not offer it at all). When that request
fails nothing is logged as an error: the port simply stays closed and peer
counts stay low.

1. Pin the port. In the Control Panel, set **GoStorm → `PeersListenPort`** to a
   fixed high port (e.g. `64407`). Left at `0` the engine picks a new random port
   on every start, which no static forward can follow.
2. Forward that same port to the machine running Tiramisu, **both TCP and UDP**.
3. Verify from outside your network that the port is open: a closed port and a
   working UPnP mapping look identical from the inside.

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
<summary><b>Scripts or tools get 401, or cannot reach <code>:8090</code></b></summary>

With sign-in on, every call to `:9080` needs an API token
(`-H "Authorization: Bearer tmr_..."`), created in **Control Panel → Access**.
The GoStorm API on `:8090` listens on `127.0.0.1` only since v1.11.15: run its
commands on the host, or set `"gostorm_listen_ip": "all"` in `config.json`.

</details>

## Key File Locations

Paths below use the defaults set by `install.sh`. All are configurable during installation.

**Runtime (install directory, default `~/Tiramisu/`)**

| Path | Purpose |
|------|---------|
| `~/Tiramisu/tiramisu` | Production binary |
| `~/Tiramisu/config.json` | Live configuration (edit → `sudo systemctl restart tiramisu`) |
| `~/Tiramisu/STATE/tiramisu.db` | SQLite state database: inode map, sync caches, episode registry, scheduler state |
| `~/Tiramisu/logs/` | All service logs (`tiramisu.log`, `movies-sync.log`, `tv-sync.log`, `watchlist-sync.log`) |
| `/mnt/tiramisu-mkv-virtual/` | FUSE mount point (served to Plex/Jellyfin -> Samba) |
| `/etc/systemd/system/tiramisu.service` | systemd service definition |

**Build (cloned repository)**

| Path | Purpose |
|------|---------|
| `~/Tiramisu/` | Install directory: clone, build and runtime |
| `~/Tiramisu/default.pgo` | PGO profile, regenerate after major code changes |
