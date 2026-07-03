# AI Tiramisu Pilot Overview [Experimental]

> ### Author's Note – Why This Is Interesting
>
> This project explores a non-traditional application of AI: using a Large Language Model (LLM) as a real-time policy engine to dynamically optimize BitTorrent runtime parameters during media streaming.
>
> Traditional torrent clients rely on static configuration or deterministic heuristics to manage peer connections, timeouts, and bandwidth behavior. In contrast, Tiramisu's AI Pilot periodically analyzes live system metrics (CPU load, buffer state, peer count, throughput, contextual usage scenario) and adapts operational parameters in real time.
>
> What makes this approach interesting is not the use of AI for content generation, but the use of a lightweight LLM as a decision layer inside a constrained edge environment (e.g., Raspberry Pi), controlling a distributed P2P network workload under streaming conditions (including 4K media). The model operates with a reduced context window and low resource footprint, demonstrating that adaptive AI-driven control loops can function on minimal hardware without cloud dependency.
>
> This experiment reframes an LLM from a conversational tool into a runtime optimization component — effectively acting as a soft, contextual control system layered on top of a torrent engine.
>
> While experimental, this approach opens discussion around AI-assisted network tuning, adaptive peer management, and lightweight autonomous optimization in decentralized systems.
>
> Author: **Matteo Rancilio**

---

**AI Tiramisu Pilot** is an **optional** autonomous optimization engine designed for Tiramisu. It leverages a LLM — either a tiny quantized model running **locally** on the Raspberry Pi or a **cloud provider** (e.g., OpenRouter) — to dynamically tune BitTorrent parameters with two goals:

1. **4K Stabilization**: Protects the system from CPU spikes and thermal stress by scaling down resources when performance is optimal.
2. **Discovery Boost**: Actively improves connectivity for low-peer torrents by experimenting with higher connection limits and tracker re-announces to find faster seeders.

## Optional Activation

The system is entirely decoupled from the streaming pipeline:

- **Auto-Detection**: Tiramisu activates the AI Pilot only if `ai_url` is set in `config.json`.
- **Auto-Disable**: If the server is unreachable (`connection refused`), the AI Pilot disables itself after the first failed attempt and logs a single message. Subsequent cycles are silent. Re-enable by restarting Tiramisu.
- **Silent Fallback**: Non-connection errors (timeouts, malformed responses) log a `Communication Delay` and maintain current settings without affecting playback.
- **Zero Impact**: The streaming pipeline never waits for AI responses.

---

## Core Architecture

### Dual-Provider Design

The AI Tuner supports two backend modes, selected automatically at startup:

| Mode | Trigger | Endpoint | Auth |
|------|---------|----------|------|
| **Local** (`llama.cpp`) | `ai_url` contains `127.0.0.1` or `localhost` | `POST /completion` | None |
| **Cloud** (OpenRouter / OpenAI-compatible) | Any other URL | `POST /chat/completions` | `Bearer {ai_api_key}` |

The `AIProvider` struct carries the full configuration:

```go
type AIProvider struct {
    URL     string // e.g. "http://127.0.0.1:8085" or "https://openrouter.ai/api/v1"
    APIKey  string // cloud only
    Model   string // cloud only, e.g. "mistralai/mistral-7b-instruct:free"
    IsLocal bool   // auto-detected from URL
}
```

### Components

1. **AI Server** *(local mode only)*: A background service (`ai-server.service`) running `llama.cpp`. Hosts the quantized model, provides a local REST API on port `8085`. Configured with a 512-token context window and `Nice=15`.

2. **AI Tuner**: A background goroutine within Tiramisu that samples system metrics every **5 seconds** and invokes the LLM on an adaptive schedule:
   - **Normal mode**: every **180 seconds** (36 samples × 5s)
   - **Crisis mode**: every **60 seconds** (12 samples × 5s) — activated when the 3-minute average speed drops below **2.0 MB/s**

3. **HTTP Clients**: Two separate clients with different timeouts:
   - Local: 60s timeout (llama.cpp is fast when warm)
   - Cloud: 120s timeout (accounts for network latency and rate-limited free tiers)

---

## Configuration (`config.json`)

```json
{
  "ai_url":      "http://127.0.0.1:8085",
  "ai_provider": "local",
  "ai_model":    "",
  "ai_api_key":  ""
}
```

| Field | Description | Example |
|-------|-------------|---------|
| `ai_url` | LLM endpoint base URL. If empty, AI Pilot is disabled. | `https://openrouter.ai/api/v1` |
| `ai_provider` | `"local"` or `""` → llama.cpp. Any other value → cloud mode. | `"openrouter"` |
| `ai_model` | Model ID for cloud providers. Ignored in local mode. | `"mistralai/mistral-7b-instruct:free"` |
| `ai_api_key` | API key for cloud providers. Can also be set via `AI_API_KEY` env var. | `"sk-or-..."` |

> **OpenRouter free models**: Several models on [openrouter.ai](https://openrouter.ai) are available for free (rate-limited). Models with reasoning capability (Mistral, Qwen, DeepSeek) work well for this task.

---

## Local Mode — Model

| Parameter | Value |
|-----------|-------|
| Model | `Qwen_Qwen3-0.6B-Q4_K_M.gguf` |
| Context window | 512 tokens (`-c 512`) |
| Threads | 3 (`-t 3`) |
| Inference latency (cold) | ~13s on Pi 4 Cortex-A72 |
| Inference latency (warm) | **~1.6–5s** (KV cache active) |
| Inference latency (under load) | 30–45s (CPU >90%, Pi 4 throttled) |
| RAM usage | ~545 MB |
| Prompt template | ChatML (`<\|im_start\|>`) |

Grammar-constrained output via GBNF forces valid JSON. Long hallucinated numbers (e.g., `130000000`) are truncated to 2 digits before parsing.

### Model Selection Notes

Qwen3-0.6B was chosen over Llama-3.2-1B-Instruct-Q3_K_M after field testing:

| | Qwen3-0.6B Q4_K_M | Llama-3.2-1B Q3_K_M |
|---|---|---|
| File on disk | 462 MB | 659 MB |
| RAM in use | 545 MB | 735 MB |
| Latency (cold) | ~13s | ~15–20s |
| Latency (warm) | **~1.6s** | 9–20s |
| Token/s | ~9 tok/s | ~1–2 tok/s |
| Strategy | Proactive (preemptive peer rebuild) | Reactive (conservative) |

---

## Operational Logic

### Cycle Lifecycle

Each 5-second sample:
1. Collect metrics (CPU, speed, peers, buffer)
2. Detect context changes (new torrent, fresh start)
3. Run swarm health checks
4. On AI cycle: build prompt → call LLM → apply parameters

### Key Behaviors

- **Context Change Detection**: Automatically detects when a new torrent is played (via InfoHash) and resets all history, averages, and baseline values.
- **History Management**: Maintains **4 snapshots** of previous AI cycle metrics to provide temporal context ("speed declining for 3 cycles" vs "just dropped").
- **Baseline from Config**: `connections_limit` and `peer_timeout` baselines are read from `config.json` at startup. Context change resets to these defaults.
- **No Anchoring**: Current parameter values are intentionally excluded from the prompt. Providing current values causes small models to statistically echo them (anchor effect).
- **Player State Signal**: Distinguishes `streaming` (download active), `consuming` (player consuming buffer, no download), and `paused` (no download, stable buffer) based on buffer delta between cycles.
- **Idle Guard**: If buffer > 95% and speed = 0, restores defaults silently instead of calling the LLM.
- **Multi-Stream Safety**: If more than one torrent is active:
  - Exactly **one `IsPriority=true`** torrent → AI tunes that torrent only.
  - Zero or multiple priority torrents → safety reset to defaults.

### Discovery Boost

When the swarm is weak (`ConnectedSeeders < 2` AND `speed < fileSize_GB × 15% MB/s`), Tiramisu automatically triggers a tracker **re-announce** (at most once every 120 seconds) to refresh the peer list.

```text
[AI-Pilot] DiscoveryBoost: weak swarm (seeds=1 speed=0.3MB/s threshold=2.1MB/s) → tracker re-announce triggered
```

### Circuit Breaker

After **3 consecutive timeouts**, the AI Pilot enters a **2-minute cooldown** and skips all AI calls. It resumes automatically and logs recovery.

```text
[AI-Pilot] Circuit breaker OPEN: 3 consecutive timeouts — cooldown until 15:42:00
[AI-Pilot] Circuit breaker CLOSED: AI recovered successfully
```

---

## Prompt Design

Both local and cloud modes use the same prompt content. The system message includes **few-shot examples** to guide the model without explicit range anchoring:

```
system: BitTorrent Optimizer. Examples:
- Peers:2,  Total:10,  Size:2GB,  Speed:0,  CPU:25 -> {"connections_limit":5,"peer_timeout_seconds":60}
- Peers:50, Total:150, Size:40GB, Speed:15, CPU:30 -> {"connections_limit":50,"peer_timeout_seconds":15}
- Peers:40, Total:80,  Size:15GB, Speed:10, CPU:90 -> {"connections_limit":12,"peer_timeout_seconds":20}
Output ONLY JSON. Use 2-digit seconds for timeout.

user: Peers:13, Total:45, Size:22.4GB, Speed:3.6MB/s, CPU:70%, Buf:99%,
      History:[CPU:54% (Peak:85%), Buf:99%, Peers:12/44, Speed:9.6MB/s (DOWN)],
      Trend:DOWN (-6.0MB/s)
```

Key design decisions:

- **Few-Shot Examples** — teaches the model the expected output format and the relationship between inputs and outputs without explicit range constraints
- **`Total` peers** — provides swarm headroom context (e.g., 13 active / 45 total means many unchoked peers available)
- **History** — up to 4 snapshots of previous cycles; each includes `Peers:active/total` for trend visibility
- **No current-value anchoring** — connections_limit and timeout are not in the prompt; the model reasons from metrics alone
- **`Trend:`** field — directional speed change (UP/DOWN/STABLE) with delta for quick trend reading

### Cloud Mode Differences

Cloud responses may wrap JSON in markdown fences (` ```json ... ``` `). `fetchCloudJSON` automatically strips the fencing before parsing. GBNF grammar is not applied (cloud APIs don't support it); `Sanitize()` clamps final values to `[5–60]` as a safety net.

---

## Real-Time Adjustments

| Parameter | Range | AI Strategy |
|-----------|-------|-------------|
| `connections_limit` | 5–60 | Reduce when CPU high or streaming smooth; increase when peers scarce |
| `peer_timeout_seconds` | 10–60 | Low = aggressive peer churn (crisis); High = keep working peers (stable) |

- **Hysteresis**: Changes only applied and logged when parameters actually change.
- **Pulse**: Every 5 stable cycles, a confirmation log is emitted to verify the optimizer is running.

---

## Installation & Setup

### Local Mode (Raspberry Pi)

1. **Deploy AI Directory**:
   ```bash
   rsync -avz Tiramisu/ai/ pi@192.168.1.2:/home/pi/Tiramisu/ai/
   ```

2. **Run Setup Script**:
   ```bash
   ssh pi@192.168.1.2 "cd /home/pi/Tiramisu/ai && chmod +x setup_pi.sh && ./setup_pi.sh"
   ```

3. **Service Management**:
   ```bash
   sudo systemctl start ai-server
   sudo systemctl enable ai-server   # to auto-start on boot
   ```

4. **config.json**:
   ```json
   { "ai_url": "http://127.0.0.1:8085", "ai_provider": "local" }
   ```

### Cloud Mode (OpenRouter or compatible)

No local AI server required. Just add to `config.json`:

```json
{
  "ai_url":      "https://openrouter.ai/api/v1",
  "ai_provider": "openrouter",
  "ai_model":    "mistralai/mistral-7b-instruct:free",
  "ai_api_key":  "sk-or-your-key-here"
}
```

Or via environment variable: `AI_API_KEY=sk-or-... sudo systemctl restart tiramisu`

> **Free-tier considerations**: Cloud free tiers have rate limits (typically 20–200 req/day). At the default 180s cycle, Tiramisu makes ~480 requests/day during continuous streaming. Consider using a paid tier or limiting streaming hours, or increasing the cycle interval.

---

## Fail-Safe Design

| Failure | Behavior |
|---------|----------|
| Server unreachable (connection refused) | Auto-disable after first failure, single log entry |
| Timeout (≤ 2 consecutive) | Log `Communication Delay`, maintain last settings |
| 3 consecutive timeouts | Circuit breaker OPEN: 2-minute cooldown |
| Malformed JSON from cloud | Strip markdown fences and retry parse; log error if still invalid |
| Values out of range | `Sanitize()` clamps to `[5–60]` before applying |

---

## Key Files

| File | Purpose |
|------|---------|
| `Tiramisu/ai/ai_tuner.go` | Core tuning logic, provider dispatch, cycle management |
| `Tiramisu/ai/ai-server.service` | systemd unit for local llama.cpp server |
| `Tiramisu/ai/models/Qwen_Qwen3-0.6B-Q4_K_M.gguf` | Quantized model (local mode) |
| `Tiramisu/config.go` | `ai_url`, `ai_provider`, `ai_model`, `ai_api_key` fields |
| `:9080/metrics` | Exposes `ai_current_limit` |

---

## Real-World Activity Logs

### 1. Startup — Local Mode
```text
2026/03/09 22:22:22 [AI-Pilot] Initializing llama.cpp Bridge (http://127.0.0.1:8085)... waiting for system settings.
2026/03/09 22:22:22 [AI-Pilot] Neural optimizer starting... (Stats: 5s, AI: 180s) baseline conns=25 timeout=15
```

### 2. Startup — Cloud Mode
```text
2026/04/05 10:00:00 [AI-Pilot] Initializing Cloud LLM Provider (https://openrouter.ai/api/v1, model: mistralai/mistral-7b-instruct:free)... waiting for system settings.
2026/04/05 10:00:00 [AI-Pilot] Neural optimizer starting... (Stats: 5s, AI: 180s) baseline conns=25 timeout=15
```

### 3. New Torrent Detection (History Reset)
```text
2026/03/09 22:22:25 [AI-Pilot] Fresh Start Detected: Applying defaults (Conns:25 Timeout:15s) for new torrent.
```

### 4. Dynamic Optimization — Normal conditions (Qwen3-0.6B)
```text
// Speed declining, CPU high → reduce connections (cold request, 12.7s)
2026/03/09 22:27:55 [AI-Pilot] Optimizer applying change: Conns(25->15) Timeout(15s->15s) [Metrics: [CPU:54% (Peak:85%), Buf:98%, Peers:22/60, Speed:9.6MB/s (DOWN (-2.2MB/s))]]

// Player paused (speed=0), peer pool collapsed to 2 → preemptive rebuild + max timeout (4.4s)
2026/03/09 22:37:52 [AI-Pilot] Optimizer applying change: Conns(15->20) Timeout(18s->60s) [Metrics: [CPU:23% (Peak:62%), Buf:93%, Peers:2/40, Speed:0.0MB/s (DOWN (-9.9MB/s))]]
```

### 5. Dynamic Optimization — Under load (Plex scan + 4K stream, CPU >90%)
```text
// Speed crashed from 27MB/s to 3.6MB/s → reduce conns, aggressive timeout (33.7s latency)
2026/03/10 13:30:43 [AI-Pilot] RAW: "{\"connections_limit\":13,\"peer_timeout_seconds\":10}" | Latency: 33.720322922s
2026/03/10 13:30:43 [AI-Pilot] Optimizer applying change: Conns(25->13) Timeout(15s->10s) [Metrics: [CPU:70% (Peak:91%), Buf:99%, Peers:13/45, Speed:3.6MB/s (DOWN (-24.0MB/s))]]

// Speed recovered → stabilize working peers, extend timeout (44.6s latency)
2026/03/10 13:36:19 [AI-Pilot] RAW: "{\"connections_limit\":13,\"peer_timeout_seconds\":60}" | Latency: 44.647535885s
2026/03/10 13:36:19 [AI-Pilot] Optimizer applying change: Conns(13->13) Timeout(10s->60s) [Metrics: [CPU:67% (Peak:89%), Buf:98%, Peers:13/45, Speed:15.2MB/s (UP (+8.0MB/s))]]
```

### 6. Discovery Boost
```text
2026/04/05 11:15:33 [AI-Pilot] DiscoveryBoost: weak swarm (seeds=1 speed=0.3MB/s threshold=2.1MB/s) → tracker re-announce triggered
```

### 7. Auto-Disable
```text
2026/03/09 20:37:03 [AI-Pilot] LLM not reachable (http://127.0.0.1:8085) — auto-disabled. Restart tiramisu to re-enable.
```

### 8. Stability Confirmation (Pulse)
```text
2026/03/04 11:18:28 [AI-Pilot] Pulse: Optimizer active, values stable at Conns(25) Timeout(48s). Metrics: [CPU:49%, Buf:102%, Peers:15/50, Speed:16.5MB/s]
```

### 9. Circuit Breaker
```text
2026/04/05 14:22:01 [AI-Pilot] Communication Delay (3/3): context deadline exceeded
2026/04/05 14:22:01 [AI-Pilot] Circuit breaker OPEN: 3 consecutive timeouts — cooldown until 14:24:01
2026/04/05 14:24:15 [AI-Pilot] Circuit breaker CLOSED: AI recovered successfully
```
