# Installation

Docker in detail, Plex/Jellyfin and Samba setup, and building from source. The short version is in the [README](../README.md#quick-install).

[← Back to the README](../README.md) · [Documentation index](README.md)

---

## Docker

> [!IMPORTANT]
> Tiramisu mounts a FUSE filesystem at startup. Docker blocks this syscall by default. The container requires elevated privileges to run.

Pre-built images for `linux/amd64` and `linux/arm64` are published automatically on every release to Docker Hub and GitHub Container Registry:

| Registry | Image |
|----------|-------|
| **Docker Hub** | `mrrobotogit/tiramisu:latest` |
| **GHCR** | `ghcr.io/mrrobotogit/tiramisu:latest` |

**Pull:**

```bash
docker pull mrrobotogit/tiramisu:latest
# or
docker pull ghcr.io/mrrobotogit/tiramisu:latest
```

**Run:**

```bash
docker run -d \
  --name tiramisu \
  --restart unless-stopped \
  --device /dev/fuse \
  --cap-add SYS_ADMIN \
  --cap-add NET_ADMIN \
  -v /path/to/config.json:/config.json \
  -v /path/to/state:/state \
  -v /mnt/tiramisu-mkv-real:/mnt/tiramisu-mkv-real \
  -v /mnt/tiramisu-mkv-virtual:/mnt/tiramisu-mkv-virtual:rshared \
  -p 9080:9080 \
  mrrobotogit/tiramisu:latest
```

Or use `--privileged` as a simpler alternative to the individual capabilities (e.g. on a Raspberry Pi where the container is fully trusted). In practice `--cap-add SYS_ADMIN`/`NET_ADMIN` alone have been reported insufficient on some Ubuntu hosts — if the container starts cleanly but the virtual directory stays empty, use `--privileged`.

`config.json` must be volume-mounted at `/config.json` (the default `MKV_PROXY_CONFIG_PATH`). Use `config.json.example` as the starting point. Do not mount it `:ro`: the Control Panel writes settings back to this file, and a read-only mount makes every save fail.

**Mount a volume on `/state`.** Everything that must outlive the container lives there:

| Path | Holds |
|------|-------|
| `/state/config.db` | Torrent list, and GoStorm settings when `StoreSettingsInJson` is off |
| `/state/settings.json` | GoStorm settings: peer port, cache size, connection limit |
| `/state/STATE/` | Inode map and sync caches |
| `/state/logs/` | Log files the Control Panel tails |
| `/state/blocklist` | The downloaded IP blocklist |

Mount a host directory there and everything survives `docker rm`. Without that volume the peer port, the torrent list and every other setting are discarded when the container is removed, and the next `docker run` starts from the defaults; the container prints a warning on startup when it detects this. `/state` is the default, so `TIRAMISU_ROOT_PATH` only needs setting to move the whole directory elsewhere, and `TIRAMISU_STATE_DIR` / `TIRAMISU_LOG_DIR` default to `STATE/` and `logs/` underneath it.

Do not mount a named or anonymous Docker volume expecting the warning to catch a mistake: an anonymous volume looks mounted, but a fresh `docker run` attaches a new empty one. Use a host path.

**Updating the container.** Unmount the virtual directory between removing the old container and starting the new one: Docker refuses to bind-mount over the FUSE mount the previous container left behind.

```bash
docker pull mrrobotogit/tiramisu:latest
docker rm -f tiramisu
sudo fusermount3 -uz /mnt/tiramisu-mkv-virtual
docker run -d ...   # same flags as above
```

> [!TIP]
> **Troubleshooting: real directory fills up, virtual stays empty.** If `docker logs` shows `FUSE mounted at ... all systems active` and the InodeMap saving files, but `/mnt/tiramisu-mkv-virtual` is empty on the host, the FUSE mount succeeded *inside* the container but never propagated out — Docker bind mounts default to private propagation, so a mount created inside the container isn't visible outside it. Fix: add `:rshared` to the virtual volume's `-v` flag (as above). If Docker then refuses to start with an error like *"must be shared or slave"*, the host mountpoint itself isn't shared yet - run `sudo mount --make-rshared /mnt` (or whichever parent directory holds it) once, then restart the container.

**Build from source** (from the repository root):

```bash
docker build -f docker/Dockerfile -t tiramisu .
```

### Windows Docker Installer

For Windows users, a dedicated installer generates a ready-to-use [Dockge](https://github.com/louislam/dockge) stack with Tiramisu + Plex **or** Jellyfin in a single container:

**[docker-windows/](https://github.com/MrRobotoGit/tiramisu/tree/main/docker-windows)**

- Run `docker-windows\install-rebuild.bat` for interactive setup (flavor, paths, ports)
- Auto port conflict resolution
- Idempotent rebuild: never deletes existing media or config data

> **Requires:** Docker Desktop with WSL2 (Linux containers) and FUSE support (`/dev/fuse` available inside containers).

---

## Plex/Jellyfin and Samba Setup

### Connecting a Media Server

Point the **Media Server** card in the Control Panel at your server, and Tiramisu
identifies which product answers, showing it as verified with its latency. It
also broadcasts on the LAN, Plex GDM on UDP 32414 and Jellyfin on UDP 7359, and
lists whatever replies, so a server on the same network appears without
configuration. A broadcast does not cross a Docker bridge network, and Jellyfin
lets you switch its discovery off, so setting the URL is the reliable way.

| Setting | Plex | Jellyfin |
|---------|------|----------|
| `media_server_type` | `plex` | `jellyfin` |
| `plex.url` | `http://host:32400` | `http://host:8096` |
| `plex.token` | [X-Plex-Token](https://support.plex.tv/articles/204059436-finding-an-authentication-token-x-plex-token/) | API key: Dashboard → Advanced → API Keys → **+** |
| `plex.library_id` / `plex.tv_library_id` | Section IDs to refresh after a sync | Leave at `0` |

The token drives the library refresh Tiramisu issues when the movies, TV and
watchlist syncs finish, and the field names keep the `plex.` prefix for backward
compatibility: they hold whichever server `media_server_type` names. Plex
refreshes the section named by `library_id` and does nothing without one;
Jellyfin refreshes every library, which is why its section IDs stay at `0`.
Without a token the media server finds new files on its own scan schedule
instead. The playback sessions and posters on the dashboard are Plex-only.

### Samba Configuration

Critical parameters in `/etc/samba/smb.conf` to prevent FUSE deadlocks during Plex/Jellyfin library scans:

```ini
[tiramisu-mkv-virtual]
   path = /mnt/tiramisu-mkv-virtual
   browseable = yes
   read only = yes
   oplocks = no           # CRITICAL: prevents kernel exclusive locks on FUSE files
   aio read size = 1      # CRITICAL: forces async I/O, prevents smbd D-state
   deadtime = 15          # cleans inactive SMB connections every 15 minutes
   vfs objects = fileid   # CRITICAL: transmits 64-bit inodes to Synology/Plex/Jellyfin
```

> [!WARNING]
> **`oplocks = no` is non-negotiable.** With oplocks enabled, the kernel requests exclusive locks on FUSE-backed files, causing `smbd` threads to enter D-state indefinitely during concurrent Plex/Jellyfin scans.

> [!IMPORTANT]
> **`vfs objects = fileid`** ensures 64-bit inode transmission. Without it, Synology receives truncated 32-bit inodes, causing Plex to misidentify files.

### Synology CIFS Mount

```
Source:  //pi-ip/tiramisu-mkv-virtual
Target:  /volume1/Tiramisu
Options: serverino,vers=3.0,uid=1024,gid=100,file_mode=0777,dir_mode=0777
```

`serverino` must remain active. Synology may silently drop it after network timeouts. Schedule a Task Scheduler job (every 5 min) to verify and remount if needed.

---

## Build from Source

> [!IMPORTANT]
> Compile natively on the target machine. Do not cross-compile: the PGO profile must match the target CPU architecture.

```bash
ssh pi@<tiramisu-host>
cd ~/Tiramisu

/usr/local/go/bin/go clean -cache
/usr/local/go/bin/go mod tidy
GOARCH=arm64 CGO_ENABLED=1 /usr/local/go/bin/go build -pgo=auto -o tiramisu .

# Deploy: the install directory is ~/Tiramisu by default; if you built
# elsewhere, copy the binary there first
sudo systemctl stop tiramisu
sudo systemctl start tiramisu
```

**Verify the toolchain is 64-bit:**
```bash
/usr/local/go/bin/go version
# Required: go version go1.26.x linux/arm64
# Wrong:    go version go1.26.x linux/arm   <-- 32-bit
```

**Install Go 1.26 if needed:**
```bash
wget https://go.dev/dl/go1.26.8.linux-arm64.tar.gz
sudo tar -C /usr/local -xzf go1.26.8.linux-arm64.tar.gz
```
