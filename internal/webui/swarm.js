// Swarm network view: peers → torrents → piece cache → FUSE → clients, drawn as SVG.
// Every mark comes from the monitor APIs; node positions carry no data (stable per hash).
// Adaptive Shield is drawn as the one accent travelling along the pipes:
//   corruption = a wave entering the affected torrent's pipe, STRICT = pipes tinted from the
//   cache to the clients with a clean-streak ring, restore = the tint draining to the clients.
(function () {
  const NS = 'http://www.w3.org/2000/svg';
  const DEFAULT_CLEAN_S = 30;     // engine minimum; the strict log line states the real one
  const WAVE_MS = 12000;          // how long a corruption wave stays on screen
  const DRAIN_MS = 10000;         // how long the tint takes to leave after a restore
  const MAX_PEERS = 48, MAX_PENDING = 8;

  let host = null, data = null, transport = null, clients = null, engine = null, lastSig = '';
  let wasStrict = false, strictEndedAt = 0;
  // Per torrent: last peer count, plus when each index appeared or vanished, so a redraw
  // can resume the fade-in / pulse-and-fade exactly where it was.
  const life = new Map();
  const BORN_MS = 900, BIRTH_PULSE_MS = 2000, DYING_MS = 1800;

  function el(tag, attrs, parent) {
    const n = document.createElementNS(NS, tag);
    for (const k in attrs) n.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(n);
    return n;
  }
  function txt(parent, x, y, s, cls, anchor) {
    const t = el('text', { x, y, class: cls || 'sw-lab', 'text-anchor': anchor || 'start' }, parent);
    t.textContent = s;
    return t;
  }
  // Deterministic PRNG per torrent so peers don't jump on every poll.
  function rng(seed) {
    let h = 2166136261;
    for (const c of String(seed)) { h ^= c.charCodeAt(0); h = Math.imul(h, 16777619); }
    return () => { h ^= h << 13; h ^= h >>> 17; h ^= h << 5; return ((h >>> 0) % 100000) / 100000; };
  }
  const short = (s, n) => s && s.length > n ? s.slice(0, n - 1) + '…' : (s || '');

  // Shield state: the engine's own status (/metrics) decides STRICT and its clean streak; the
  // log events only add the corruption wave and which torrent it hit.
  function shieldState(events, now) {
    const ev = [...(events || [])].sort((a, b) => a.t - b.t);
    let strictAt = null, strictHash = '', restoreAt = null, lastCorr = null, need = DEFAULT_CLEAN_S;
    for (const e of ev) {
      if (e.type === 'strict') { strictAt = e.t; strictHash = e.hash || strictHash; restoreAt = null; need = e.need_s || DEFAULT_CLEAN_S; }
      else if (e.type === 'restore') { if (strictAt !== null) restoreAt = e.t; strictAt = null; }
      if (e.type === 'corruption' || e.type === 'strict') lastCorr = e;
    }
    const s = { mode: 'fast', hash: '', clean: 0 };
    if (engine) {
      if (engine.active) {
        s.mode = 'strict';
        s.hash = strictHash;
        s.need = engine.need;
        const since = engine.last ? now / 1000 - engine.last : 0;
        s.clean = Math.max(0, Math.min(1, since / engine.need));
        s.cleanS = Math.max(0, Math.min(engine.need, Math.floor(since)));
        wasStrict = true;
      } else {
        if (wasStrict) { wasStrict = false; strictEndedAt = now; }
        if (now - strictEndedAt < DRAIN_MS) s.mode = 'recover';
      }
    } else if (strictAt !== null) {
      s.mode = 'strict';
      s.hash = strictHash;
      const since = (now - (lastCorr ? lastCorr.t : strictAt)) / 1000;
      s.need = need;
      s.clean = Math.max(0, Math.min(1, since / need));
      s.cleanS = Math.min(need, Math.floor(since));
    } else if (restoreAt !== null && now - restoreAt < DRAIN_MS) {
      s.mode = 'recover';
      s.drain = (now - restoreAt) / DRAIN_MS;
    }
    if (lastCorr && now - lastCorr.t < WAVE_MS) {
      s.wave = lastCorr.hash || '';
      s.waveMs = now - lastCorr.t;
      s.piece = lastCorr.piece || 0;
    }
    return s;
  }

  // A rebuild restarts every animation, so skip it when nothing visible changed.
  function signature() {
    const t = (data.torrents || []).filter(x => x.status !== 'idle')
      .map(x => [x.hash.slice(0, 8), x.speed_mbs.toFixed(1), x.peers, x.seeders, x.half_open, x.is_priority].join(':')).join('|');
    const sh = shieldState(data.shield, Date.now());
    return [host.clientWidth, document.documentElement.dataset.theme, t, sh.mode, sh.cleanS, sh.wave, (data.health || {}).v304_banned_peers,
      (data.health || {}).cache_hit_rate_pct, Math.round((data.health || {}).fuse_active_pct || 0),
      Math.round((data.health || {}).fuse_stale_pct || 0), transport && transport.utp, clients && clients.size].join('#');
  }

  function draw(force) {
    if (!host || host.hidden || !data) return;
    const sig = signature();
    if (!force && sig === lastSig) return;
    lastSig = sig;
    const W = host.clientWidth || 600;
    const vertical = W < 560;
    const H = vertical ? 620 : 470;
    host.innerHTML = '';
    const svg = el('svg', { viewBox: `0 0 ${W} ${H}`, width: '100%', height: H, class: 'swarm', role: 'img', 'aria-label': 'Swarm network' }, host);

    const allActive = (data.torrents || []).filter(t => t.status !== 'idle')
      .sort((a, b) => b.speed_mbs - a.speed_mbs);
    const torrents = allActive.slice(0, 4);
    if (!torrents.length) {
      txt(svg, W / 2, H / 2, 'No active swarm — the graph fills when a stream starts', 'sw-empty', 'middle');
      return;
    }
    const now = Date.now();
    const sh = shieldState(data.shield, now);
    const h = data.health || {};

    // Logical coords: f = along the flow (0..1), c = across (0..1). Map to x/y per orientation.
    const pad = vertical ? { f0: 40, f1: H - 44, c0: 30, c1: W - 30 } : { f0: 18, f1: W - 30, c0: 44, c1: H - 18 };
    const P = (f, c) => {
      const ff = pad.f0 + (pad.f1 - pad.f0) * f, cc = pad.c0 + (pad.c1 - pad.c0) * c;
      return vertical ? [cc, ff] : [ff, cc];
    };
    const curve = (a, b) => {
      const m = (a[0] + b[0]) / 2, n = (a[1] + b[1]) / 2;
      return vertical ? `M${a[0]} ${a[1]} C${a[0]} ${n} ${b[0]} ${n} ${b[0]} ${b[1]}`
        : `M${a[0]} ${a[1]} C${m} ${a[1]} ${m} ${b[1]} ${b[0]} ${b[1]}`;
    };
    const F = { peers: 0, peersEnd: 0.33, torrent: 0.47, cache: 0.66, fuse: 0.80, client: 0.97 };
    const cOf = i => torrents.length === 1 ? 0.45 : 0.12 + i * (0.76 / (torrents.length - 1));

    // Column headings and guides.
    const heads = [['Peers', F.peers], ['Torrents', F.torrent], ['Piece cache', F.cache], ['FUSE', F.fuse], ['Clients', F.client]];
    heads.forEach(([name, f], i) => {
      const [x, y] = P(f, 0);
      if (i && !vertical) {
        const a = P(f, 0.02), b = P(f, 1);
        el('line', { x1: a[0], y1: a[1], x2: b[0], y2: b[1], class: 'sw-guide' }, svg);
      }
      if (vertical) { if (i === 0) txt(svg, W - 4, y + 3, name.toUpperCase(), 'sw-head', 'end'); }
      else txt(svg, x, 14, name.toUpperCase(), 'sw-head', i === 0 ? 'start' : i === heads.length - 1 ? 'end' : 'middle');
    });

    const cacheP = P(F.cache, 0.45), fuseP = P(F.fuse, 0.45);
    const tot = transport && (transport.tcp + transport.utp) > 0 ? transport : null;
    const utpShare = tot ? transport.utp / (transport.tcp + transport.utp) : 0;
    const peerLayer = el('g', {}, svg), edgeLayer = el('g', {}, svg), flowLayer = el('g', {}, svg), nodeLayer = el('g', {}, svg);
    svg.insertBefore(edgeLayer, peerLayer);

    const maxSpeed = Math.max(1, ...torrents.map(t => t.speed_mbs));
    const width = s => s > 0.01 ? Math.max(1.5, s / maxSpeed * 9) : 1;
    const pipes = [];

    torrents.forEach((t, i) => {
      const c = cOf(i), tp = P(F.torrent, c);
      const n = Math.min(t.peers || 0, MAX_PEERS), seeds = Math.min(t.seeders || 0, n);
      // pending_peers is the address pool (refilled from cached addresses): not drawn.
      const pend = Math.min(t.half_open || 0, MAX_PENDING);
      const spread = Math.min(0.34, 0.12 + n * 0.006) / Math.sqrt(Math.max(1, torrents.length - 1));
      // The API gives counts, not identities: a node is "peer slot k", stable at its own spot.
      const spot = (k, tag) => {
        const r = rng(t.hash + tag + k);
        const pf = F.peers + r() * (F.peersEnd - F.peers);
        const pc = Math.max(0.02, Math.min(0.98, c + (r() + r() - 1) * spread * 1.6));
        return P(pf, pc);
      };
      // Track arrivals and departures between polls.
      let L = life.get(t.hash);
      if (!L) { L = { n, seeds, born: {}, dying: {} }; life.set(t.hash, L); }
      else if (n !== L.n) {
        for (let k = n; k < L.n; k++) { L.dying[k] = { at: now, seed: k < (L.seeds || 0) }; delete L.born[k]; }
        for (let k = L.n; k < n; k++) { L.born[k] = now; delete L.dying[k]; }
        L.n = n;
      }
      L.seeds = seeds;
      const delay = (ms, total) => `${-Math.min(ms, total)}ms`;
      for (let k = 0; k < n + pend; k++) {
        const halfOpen = k >= n;
        const pp = halfOpen ? spot(k - n, ':h') : spot(k, ':');
        const edge = el('path', { d: curve(pp, tp), class: halfOpen ? 'sw-edge pending' : 'sw-edge' }, edgeLayer);
        let node;
        if (halfOpen) {
          node = el('circle', { cx: pp[0], cy: pp[1], r: 2.2, class: 'sw-peer pending' }, peerLayer);
        } else {
          const seed = k < seeds;
          // uTP vs TCP is known only in aggregate: the share is applied per torrent, by index.
          const utp = tot && ((k + 1) * 0.6180339887) % 1 < utpShare;
          node = utp ? el('rect', { x: pp[0] - 3, y: pp[1] - 3, width: 6, height: 6, class: seed ? 'sw-peer seed' : 'sw-peer' }, peerLayer)
            : el('circle', { cx: pp[0], cy: pp[1], r: 3.3, class: seed ? 'sw-peer seed' : 'sw-peer' }, peerLayer);
          // Data leaving the peer: short dark segments travelling to the torrent, out of phase.
          // Only while the torrent downloads: the API has no per-peer rate to show otherwise.
          const fl = t.speed_mbs > 0.01 ? el('path', { d: curve(pp, tp), class: 'sw-peer-flow', pathLength: 100 }, edgeLayer)
            : document.createElementNS(NS, 'path');
          fl.style.animationDelay = `${-((now / 1000 + rng(t.hash + ':f' + k)() * 2.6) % 2.6).toFixed(2)}s`;
          const b = L.born[k];
          if (b !== undefined) {
            const age = now - b;
            if (age < BORN_MS) {
              [node, edge, fl].forEach(x => { x.classList.add('sw-born'); x.style.animationDelay = (x === fl ? fl.style.animationDelay + ',' : '') + delay(age, BORN_MS); });
            }
            // A new peer announces itself: pulses for two seconds where it appeared.
            if (age < BIRTH_PULSE_MS) {
              const ring = el('circle', { cx: pp[0], cy: pp[1], r: 3.3, class: 'sw-birth-ring' }, peerLayer);
              ring.style.animationDelay = delay(age, BIRTH_PULSE_MS);
            } else delete L.born[k];
          }
        }
      }
      // Peers that just left: two pulses, then they fade out where they stood.
      Object.keys(L.dying).forEach(key => {
        const k = +key, age = now - L.dying[k].at;
        if (age >= DYING_MS || k < n) { delete L.dying[k]; return; }
        const pp = spot(k, ':');
        const eg = el('g', { class: 'sw-dying' }, edgeLayer);
        eg.style.animationDelay = delay(age, DYING_MS);
        el('path', { d: curve(pp, tp), class: 'sw-edge' }, eg);
        const g = el('g', { class: 'sw-dying' }, peerLayer);
        g.style.animationDelay = delay(age, DYING_MS);
        const ring = el('circle', { cx: pp[0], cy: pp[1], r: 3.3, class: 'sw-death-ring' }, g);
        ring.style.animationDelay = delay(age, DYING_MS);
        const utp = tot && ((k + 1) * 0.6180339887) % 1 < utpShare;
        const cls = L.dying[k].seed ? 'sw-peer seed' : 'sw-peer';
        if (utp) el('rect', { x: pp[0] - 3, y: pp[1] - 3, width: 6, height: 6, class: cls }, g);
        else el('circle', { cx: pp[0], cy: pp[1], r: 3.3, class: cls }, g);
      });
      // Torrent → cache pipe, width = throughput.
      const d = curve(tp, cacheP);
      const live = t.speed_mbs > 0.01;
      el('path', { d, class: live ? 'sw-pipe' : 'sw-pipe idle', 'stroke-width': width(t.speed_mbs) }, edgeLayer);
      pipes.push({ t, d, w: width(t.speed_mbs), tp, live });
    });

    const shown = new Set(torrents.map(t => t.hash));
    [...life.keys()].forEach(hk => { if (!shown.has(hk)) life.delete(hk); });

    // Banned peers sit in the peer field, cut off: no pipe reaches a torrent. The engine counts
    // bans process-wide, so they are spread over the field rather than pinned to one swarm.
    const bannedN = Math.min(h.v304_banned_peers || 0, 8);
    if (bannedN) {
      const r = rng('banned');
      for (let k = 0; k < bannedN; k++) {
        const p = P(F.peers + r() * (F.peersEnd - F.peers) * 0.85, 0.06 + r() * 0.88);
        const g = el('g', { class: 'sw-banned-g' }, peerLayer);
        el('title', {}, g).textContent = `Banned peers: ${h.v304_banned_peers} in total (corrupt pieces), not tied to a torrent`;
        el('path', { d: `M${p[0] - 3.5} ${p[1] - 3.5} l7 7 M${p[0] + 3.5} ${p[1] - 3.5} l-7 7`, class: 'sw-banned' }, g);
      }
    }

    // Cache → FUSE trunk, and FUSE → clients.
    const totalSpeed = torrents.reduce((a, t) => a + t.speed_mbs, 0);
    const trunkW = Math.max(10, width(Math.max(totalSpeed, 0.02)) + 1);
    // The trunk is the FUSE read-ahead buffer: a tube holding 3 rows of dots along its length.
    // Filled = active, grey = stale, faint = free. The dots travel from the cache to FUSE.
    const tubeW = Math.max(18, trunkW + 4);
    const gauge = el('g', { class: 'sw-gauge' }, edgeLayer);
    el('title', {}, gauge).textContent = `FUSE buffer ${Math.round(h.fuse_budget_mb || 0)} MB · active ${(h.fuse_active_pct || 0).toFixed(0)}% · stale ${(h.fuse_stale_pct || 0).toFixed(0)}%`;
    const dx = fuseP[0] - cacheP[0], dy = fuseP[1] - cacheP[1];
    const len = Math.hypot(dx, dy) || 1, ux = dx / len, uy = dy / len, nx = -uy, ny = ux;
    // Tube: rim and interior, between the two node gaps.
    const inset = 16;
    const t0 = [cacheP[0] + ux * inset, cacheP[1] + uy * inset], t1 = [fuseP[0] - ux * inset, fuseP[1] - uy * inset];
    el('path', { d: `M${t0[0]} ${t0[1]} L${t1[0]} ${t1[1]}`, class: 'sw-gauge-rim', 'stroke-width': tubeW + 2 }, gauge);
    el('path', { d: `M${t0[0]} ${t0[1]} L${t1[0]} ${t1[1]}`, class: 'sw-gauge-free', 'stroke-width': tubeW }, gauge);
    const clipId = 'sw-tube-clip';
    const defs = el('defs', {}, gauge);
    const cp = el('clipPath', { id: clipId }, defs);
    const half = tubeW / 2 - 1.5, inner = len - inset * 2;
    const corners = [[0, -half], [inner, -half], [inner, half], [0, half]]
      .map(([a2, b2]) => [t0[0] + ux * a2 + nx * b2, t0[1] + uy * a2 + ny * b2]);
    el('path', { d: 'M' + corners.map(p => p.join(' ')).join(' L') + ' Z' }, cp);
    const step = 7, cols = Math.max(1, Math.floor(inner / step)), rows = [-4.5, 0, 4.5];
    const total = cols * 3;
    const act = Math.max(0, Math.min(1, (h.fuse_active_pct || 0) / 100));
    const stale = Math.max(0, Math.min(1 - act, (h.fuse_stale_pct || 0) / 100));
    const nAct = Math.round(total * act), nStale = Math.round(total * stale);
    const dots = el('g', { 'clip-path': `url(#${clipId})` }, gauge);
    // VU meter. Left half: fixed lit dots. Right half: peaks rising and falling in a loop, their
    // height following the real fill (active + stale): an empty buffer barely moves.
    const halfCols = Math.max(1, Math.floor(cols / 2));
    const animate = !window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    const CYCLE = 3.2, N = 32, span = cols - halfCols;
    const levels = [];
    for (let i = 0; i < N; i++) {
      const x = i / N * Math.PI * 2;
      const f = 0.5 + 0.3 * Math.sin(x * 2) + 0.15 * Math.sin(x * 5 + 1.3) + 0.08 * Math.sin(x * 11 + 0.4);
      levels.push(span * Math.max(0, Math.min(1, f)) * Math.min(1, 0.25 + (act + stale) * 1.25));
    }
    const keyTimes = levels.map((_, i) => (i / N).toFixed(4)).join(';');
    const phase = ((now / 1000) % CYCLE).toFixed(2);
    for (let c = 0; c < cols; c++) {
      rows.forEach(r => {
        const along = c * step + step / 2;
        const pos = { cx: t0[0] + ux * along + nx * r, cy: t0[1] + uy * along + ny * r, r: 1.6 };
        if (c < halfCols) {
          el('circle', { ...pos, class: 'sw-dot act' }, dots);
          return;
        }
        const dot = el('circle', { ...pos, class: 'sw-dot act' }, dots);
        if (animate) {
          const values = levels.map(l => (l >= c - halfCols + 1 ? '1' : '0.15')).join(';');
          el('animate', { attributeName: 'opacity', values, keyTimes, calcMode: 'discrete', dur: `${CYCLE}s`,
            repeatCount: 'indefinite', begin: `-${phase}s` }, dot);
        }
      });
    }
    const mid = [(t0[0] + t1[0]) / 2, (t0[1] + t1[1]) / 2];
    txt(gauge, vertical ? mid[0] + tubeW / 2 + 8 : mid[0], vertical ? mid[1] + 3 : mid[1] + tubeW / 2 + 13,
      `${(h.fuse_active_pct || 0).toFixed(0)}% active · ${(h.fuse_stale_pct || 0).toFixed(0)}% stale`, 'sw-sub', vertical ? 'start' : 'middle');

    // Clients: one node per device we can name, else the stream's anonymous Samba reader.
    const byDevice = new Map();
    torrents.forEach(t => {
      const cl = clients && clients.get(t.hash);
      // A torrent nobody is known to read (preload, sync) gets no client node.
      if (!cl && !t.is_priority) return;
      const name = cl ? cl.label : 'Samba reader';
      const cur = byDevice.get(name) || { name, speed: 0, known: !!cl, hashes: [] };
      cur.speed += t.speed_mbs; cur.hashes.push(t.hash);
      byDevice.set(name, cur);
    });
    const devs = [...byDevice.values()];
    const cliPipes = [];
    devs.forEach((dv, i) => {
      const span = vertical ? [0.08, 0.84] : [0.15, 0.6];
      const cp = P(F.client, devs.length === 1 ? 0.45 : span[0] + i * (span[1] / Math.max(1, devs.length - 1)));
      const d = curve(fuseP, cp);
      el('path', { d, class: 'sw-pipe', 'stroke-width': width(dv.speed) }, edgeLayer);
      cliPipes.push({ d, w: width(dv.speed), speed: dv.speed });
      el('circle', { cx: cp[0], cy: cp[1], r: 6, class: dv.known ? 'sw-client' : 'sw-client unknown' }, nodeLayer);
      if (vertical) {
        const [dev, via] = dv.name.split(' · ');
        txt(nodeLayer, cp[0], cp[1] + 20, short(dev, devs.length > 2 ? 11 : 14), 'sw-name', i === 0 && devs.length > 1 ? 'start' : i === devs.length - 1 && devs.length > 1 ? 'end' : 'middle');
        if (via) txt(nodeLayer, cp[0], cp[1] + 32, via, 'sw-sub', i === 0 && devs.length > 1 ? 'start' : i === devs.length - 1 && devs.length > 1 ? 'end' : 'middle');
      }
      else txt(nodeLayer, cp[0] + 6, i === 0 && devs.length > 1 ? cp[1] - 14 : cp[1] + 22, short(dv.name, 26), 'sw-name', 'end');
    });

    // --- Adaptive Shield overlay ---
    const flow = (d, w, cls, dur) => {
      const p = el('path', { d, class: cls, 'stroke-width': w, pathLength: 100 }, flowLayer);
      if (dur) {
        p.style.animationDuration = dur + 's';
        // Phase from the clock, so a rebuild continues the motion instead of restarting it.
        p.style.animationDelay = `${-((now / 1000) % dur).toFixed(2)}s`;
      }
      return p;
    };
    if (sh.wave !== undefined && sh.mode !== 'strict') {
      // Corruption: a solid stretch grows from the hit torrent toward the cache with a lighter
      // tail ahead of it. Without a hash (older log lines) it enters at the cache.
      const hit = pipes.find(p => sh.wave && p.t.hash === sh.wave);
      const d = hit ? hit.d : `M${cacheP[0]} ${cacheP[1]} L${fuseP[0]} ${fuseP[1]}`;
      const w = (hit ? hit.w : trunkW) + 2;
      ['sw-wave-tail', 'sw-wave'].forEach(cls => {
        const p = el('path', { d, class: cls, 'stroke-width': w, pathLength: 100 }, flowLayer);
        p.style.animationDelay = `${-sh.waveMs}ms`;
      });
    }
    if (sh.mode === 'strict') {
      // Solid accent on every pipe from the torrents to the clients: STRICT is a state, not a flow.
      const tint = (d, w) => flow(d, w, 'sw-tint');
      pipes.filter(p => p.live).forEach(p => tint(p.d, p.w + 1));
      // The FUSE buffer gauge keeps its own colours: it is a reading, not part of the fault state.
      cliPipes.forEach(p => tint(p.d, p.w + 1));
    } else if (sh.mode === 'recover') {
      cliPipes.forEach(p => { flow(p.d, p.w + 1, 'sw-drain', 3).style.animationDelay = `${-(now - strictEndedAt)}ms`; });
    }

    // Flow pulses: dashes moving along the live pipes, faster for faster streams.
    pipes.filter(p => p.live).forEach(p => flow(p.d, Math.max(1.5, p.w * 0.45), 'sw-pulse', Math.max(0.8, 6 / Math.max(0.3, p.t.speed_mbs))));
    cliPipes.filter(p => p.speed > 0.01).forEach(p => flow(p.d, Math.max(1.5, p.w * 0.45), 'sw-pulse', Math.max(0.8, 6 / Math.max(0.3, p.speed))));

    if (allActive.length > torrents.length) {
      const p = P(F.torrent, 1);
      txt(nodeLayer, p[0], p[1] + (vertical ? 0 : 4), `+${allActive.length - torrents.length} more`, 'sw-sub', 'middle');
    }

    // Torrent nodes and labels.
    pipes.forEach((p, i) => {
      const t = p.t, rad = 7 + Math.min(8, t.speed_mbs * 0.6);
      if (t.is_priority) el('circle', { cx: p.tp[0], cy: p.tp[1], r: rad + 5, class: 'sw-prio' }, nodeLayer);
      if (sh.wave !== undefined && sh.wave && t.hash === sh.wave) {
        el('circle', { cx: p.tp[0], cy: p.tp[1], r: rad + 11, class: 'sw-hit' }, nodeLayer);
        if (sh.mode !== 'strict') {
          const lab = `hash fail${sh.piece ? ' · piece ' + sh.piece : ''}`;
          if (vertical) txt(nodeLayer, p.tp[0], p.tp[1] - rad - 16, lab, 'sw-sub accent', 'middle');
          else txt(nodeLayer, p.tp[0] - rad - 14, p.tp[1] + rad + 16, lab, 'sw-sub accent', 'end');
        }
      }
      // Downloading: a ring keeps expanding from the node. Not downloading: the node stays still.
      if (p.live) el('circle', { cx: p.tp[0], cy: p.tp[1], r: rad, class: 'sw-torrent-pulse' }, nodeLayer).style.animationDelay = `${-(now % 1600)}ms`;
      el('circle', { cx: p.tp[0], cy: p.tp[1], r: rad, class: p.live ? 'sw-torrent' : 'sw-torrent idle' }, nodeLayer);
      const name = short(t.clean_title || t.title || t.hash.slice(0, 8), vertical ? 12 : 24);
      const sub = p.live ? `${t.speed_mbs.toFixed(1)} MB/s · ${t.peers} peers` : `idle · ${t.peers} peers`;
      if (vertical) {
        txt(nodeLayer, p.tp[0], p.tp[1] + rad + 14, name, 'sw-name', 'middle');
        txt(nodeLayer, p.tp[0], p.tp[1] + rad + 26, p.live ? `${t.speed_mbs.toFixed(1)} MB/s` : 'idle', 'sw-sub', 'middle');
      } else {
        const below = i > 0 && i === pipes.length - 1;
        const y = below ? p.tp[1] + rad + 14 : p.tp[1] - rad - 18;
        txt(nodeLayer, p.tp[0] + rad + 8, y, name, 'sw-name');
        txt(nodeLayer, p.tp[0] + rad + 8, y + 12, sub, 'sw-sub');
      }
    });

    // Cache and FUSE nodes.
    const strict = sh.mode === 'strict';
    [[cacheP, 'GoStorm', strict ? `STRICT · ${vertical ? "" : "clean "}${sh.cleanS}/${sh.need} s` : sh.mode === 'recover' ? 'FAST · restored'
      : sh.wave !== undefined ? 'FAST kept' : 'FAST · piece cache'],
    [fuseP, 'raCache', `${Math.round(h.fuse_budget_mb || 0)} MB · hit ${h.cache_hit_rate_pct == null ? '—' : Math.round(h.cache_hit_rate_pct) + '%'}`]]
      .forEach(([p, name, sub], i) => {
        if (i === 0 && (strict || sh.mode === 'recover')) {
          el('circle', { cx: p[0], cy: p[1], r: 19, class: 'sw-ring' }, nodeLayer);
          const frac = strict ? sh.clean : 1, a = -Math.PI / 2 + 2 * Math.PI * Math.min(0.999, frac);
          if (frac > 0) el('path', { d: `M${p[0]} ${p[1] - 19} A19 19 0 ${frac > 0.5 ? 1 : 0} 1 ${p[0] + 19 * Math.cos(a)} ${p[1] + 19 * Math.sin(a)}`, class: strict ? 'sw-ring-arc strict' : 'sw-ring-arc' }, nodeLayer);
        }
        el('rect', { x: p[0] - 10, y: p[1] - 10, width: 20, height: 20, rx: 4, class: i === 0 && strict ? 'sw-box strict' : 'sw-box' }, nodeLayer);
        if (vertical) {
          txt(nodeLayer, p[0] + 26, p[1] - 1, name, 'sw-name');
          txt(nodeLayer, p[0] + 26, p[1] + 11, sub, i === 0 && strict ? 'sw-sub accent' : 'sw-sub');
        } else {
          txt(nodeLayer, p[0] - 24, p[1] - 40, name, 'sw-name');
          txt(nodeLayer, p[0] - 24, p[1] - 28, sub, i === 0 && strict ? 'sw-sub accent' : 'sw-sub');
        }
      });
  }

  async function fetchMetrics() {
    try {
      const r = await fetch('/metrics');
      if (r.ok) {
        const m = await r.json();
        transport = { tcp: m.peer_conns_tcp || 0, utp: m.peer_conns_utp || 0 };
        if (typeof m.shield_active === 'boolean') {
          engine = { active: m.shield_active, last: m.shield_last_corruption || 0, need: m.shield_need_s || 30 };
        }
      }
    } catch (_) { }
  }

  async function fetchExtras() {
    try {
      const r = await fetch('/api/clients');
      if (r.ok) {
        const list = await r.json();
        const via = { plex: 'Plex', jellyfin: 'Jellyfin', smb: 'SMB' };
        clients = new Map((list || []).filter(c => c.hash && c.device)
          .map(c => {
            // The player app says more than the server it went through: "Apple TV · Infuse".
            const app = (c.product || '').replace(/^Plex for .*/, 'Plex') || via[c.source] || c.source || '?';
            return [c.hash, { label: `${c.device} · ${c.source === 'smb' ? 'SMB' : app}` }];
          }));
      }
    } catch (_) { }
  }

  window.Swarm = {
    mount(node) {
      host = node;
      fetchMetrics().then(() => draw());
      fetchExtras().then(() => draw());
      setInterval(() => { if (!host.hidden) fetchMetrics().then(() => draw()); }, 5000);
      setInterval(() => { if (!host.hidden) fetchExtras().then(() => draw()); }, 15000);
    },
    update(d) { data = d; draw(); },
    redraw: () => draw(true),
  };
})();
