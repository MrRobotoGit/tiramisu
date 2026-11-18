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

  let host = null, data = null, transport = null, clients = null, engine = null, scan = null, lastSig = '';
  let ui = null, focusKey = null, recent = [], curKey = null;
  let wasStrict = false, strictEndedAt = 0;
  // Per torrent: last peer count, plus when each index appeared or vanished, so a redraw
  // can resume the fade-in / pulse-and-fade exactly where it was.
  const life = new Map();
  const BORN_MS = 900, BIRTH_PULSE_MS = 2000, DYING_MS = 1800;

  function el(tag, attrs, parent) {
    const n = document.createElementNS(NS, tag);
    for (const k in attrs) n.setAttribute(k, attrs[k]);
    if (curKey) n.setAttribute('data-k', curKey);
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
  const hms = s => [Math.floor(s / 3600), Math.floor(s / 60) % 60, s % 60].map(v => String(v).padStart(2, '0')).join(':');
  const esc = s => String(s).replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
  const phone = () => matchMedia('(hover: none)').matches;

  // Details: a card next to the mark on desktop (hover), a bottom sheet on phones (tap).
  // rows = [[label, value], …]; action = { text, run }.
  let anchor = null;
  // Peer country from its ISO code (peer_geoip_enabled); the browser supplies the name.
  let regionNames = null;
  function countryName(cc) {
    if (!cc) return '';
    try {
      regionNames = regionNames || new Intl.DisplayNames(['en'], { type: 'region' });
      return regionNames.of(cc) || cc;
    } catch (_) { return cc; }
  }

  function details(title, rows, at, action) {
    closeDetails();
    anchor = at && at.node;
    // title may carry a subtitle: { title, sub } (a peer's network under its country).
    const head = typeof title === 'object' ? title : { title };
    const body = `<div class="sw-d-title">${esc(head.title)}</div>` +
      (head.sub ? `<div class="sw-d-sub">${esc(head.sub)}</div>` : '') + rows.map(([a, b]) =>
      `<div class="sw-d-row"><span>${esc(a)}</span><span class="mono">${esc(b)}</span></div>`).join('') +
      (action ? `<button type="button" class="btn sm sw-d-act">${esc(action.text)}</button>` : '');
    if (phone()) {
      const scrim = document.createElement('div'); scrim.className = 'sw-scrim';
      const sh = document.createElement('div'); sh.className = 'sw-sheet'; sh.setAttribute('role', 'dialog');
      sh.innerHTML = '<div class="sw-grab"></div>' + body;
      scrim.onclick = closeDetails;
      document.body.append(scrim, sh);
      if (action) sh.querySelector('.sw-d-act').onclick = () => { closeDetails(); action.run(); };
      return;
    }
    const card = document.createElement('div'); card.className = 'sw-card'; card.innerHTML = body;
    const parent = at.host || host, box = parent.getBoundingClientRect(), a = at.rect;
    parent.appendChild(card);
    // Above the anchor (below when there is no room), touching it, never past the parent's edges.
    const cw = card.offsetWidth, ch = card.offsetHeight;
    const left = Math.max(0, Math.min(box.width - cw, a.left + a.width / 2 - box.left - cw / 2));
    let top = a.top - box.top - ch - 4;
    if (a.top - ch - 4 < 0) top = Math.min(a.bottom - box.top + 4, Math.max(0, box.height - ch));
    card.style.left = `${left}px`; card.style.top = `${top}px`;
    card.addEventListener('mouseenter', () => clearTimeout(closeTimer));
    card.addEventListener('mouseleave', () => closeSoon());
    if (action) card.querySelector('.sw-d-act').onclick = () => { closeDetails(); action.run(); };
  }
  let closeTimer = 0;
  function closeSoon() { clearTimeout(closeTimer); closeTimer = setTimeout(closeDetails, 250); }
  function closeDetails() { clearTimeout(closeTimer); anchor = null; document.querySelectorAll('.sw-card, .sw-sheet, .sw-scrim').forEach(n => n.remove()); }
  // A card whose mark was redrawn away has nothing left to describe.
  // One speed scale everywhere: megabits per second, like the header and the chart.
  function mbps(mbs) {
    const v = mbs * 8;
    return (v >= 100 ? Math.round(v) : v >= 1 ? v.toFixed(1) : v >= 0.05 ? v.toFixed(2) : '<0.05') + ' Mbps';
  }
  function closeOrphan() {
    if (!anchor || anchor.isConnected) return;
    // A peer card follows the same peer into the new drawing; anything else closes.
    const k = anchor.getAttribute && anchor.getAttribute('data-peer');
    const again = k && host && host.querySelector(`[data-peer="${CSS.escape(k)}"]`);
    if (again) anchor = again; else closeDetails();
  }
  document.addEventListener('keydown', e => { if (e.key === 'Escape') closeDetails(); });
  document.addEventListener('click', e => { if (!e.target.closest || !e.target.closest('.sw-card, .sw-sheet')) closeDetails(); });
  // Hover on desktop (with a short grace so the pointer can reach the card), tap on touch screens.
  function hoverable(node, open) {
    node.style.cursor = 'pointer';
    node.addEventListener('mouseenter', e => { if (!phone()) { clearTimeout(closeTimer); open(e); } });
    node.addEventListener('mouseleave', () => { if (!phone()) closeSoon(); });
    node.addEventListener('click', e => { e.stopPropagation(); open(e); });
  }
  function toggleFocus(key) { focusKey = focusKey === key ? null : key; closeDetails(); draw(true); }

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
  const motionQuery = window.matchMedia('(prefers-reduced-motion: reduce)');
  const reducedMotion = () => motionQuery.matches;
  motionQuery.addEventListener('change', () => { if (host) draw(true); });

  function signature() {
    const t = (data.torrents || []).filter(x => x.status !== 'idle')
      .map(x => [x.hash.slice(0, 8), x.speed_mbs.toFixed(1), (x.up_mbs || 0).toFixed(2), x.peers, x.seeders, x.half_open, x.is_priority,
        (x.peer_rates || []).map(p => p.id + Math.round(Math.log2(1 + p.kbps)) + '/' + Math.round(Math.log2(1 + (p.up_kbps || 0)))).sort().join(',')].join(':')).join('|');
    const sh = shieldState(data.shield, Date.now());
    return [host.clientWidth, host.clientWidth < 560 ? document.documentElement.clientHeight : 0, document.documentElement.dataset.theme, t, sh.mode, sh.cleanS, sh.wave, (data.health || {}).v304_banned_peers,
      (data.health || {}).cache_hit_rate_pct, Math.round((data.health || {}).fuse_active_pct || 0),
      Math.round((data.health || {}).fuse_stale_pct || 0), transport && transport.utp, clients && JSON.stringify([...clients].map(([k, c]) => [k, c.label, c.state, c.state === 'paused' ? c.pos : 0])), reducedMotion(),
      focusKey, recent.length && recent[0].started_at,
      scan && scan.active ? [scan.label, scan.files_per_min, scan.files_read,
        Object.entries(scan.torrents || {}).map(([h, a]) => h.slice(0, 8) + Math.floor(a / 10)).sort().join(',')].join(':') : ''].join('#');
  }

  // On phones the vertical graph is sized so the whole panel (graph, shield strip, legend) fits the
  // first screen below the top bar, instead of a fixed 620px that needed scrolling.
  function fitHeight() {
    const card = host.closest('.card') || host;
    const hr = host.getBoundingClientRect(), cr = card.getBoundingClientRect();
    const top = hr.top + window.scrollY;
    const below = Math.max(0, cr.bottom - hr.bottom) + 16;
    const avail = document.documentElement.clientHeight - top - below;
    return Math.round(Math.max(400, Math.min(620, avail)));
  }

  function draw(force) {
    if (!host || host.hidden || !data) return;
    curKey = null;
    setTimeout(closeOrphan, 0);
    const sig = signature();
    if (!force && sig === lastSig) return;
    lastSig = sig;
    const W = host.clientWidth || 600;
    const vertical = W < 560;
    const H = vertical ? fitHeight() : 470;
    // Cross-fade: the previous drawing fades out on top while the new one fades in, instead of a cut.
    // Only data updates fade; forced redraws (resize, theme, motion setting) and height changes cut.
    const prevSvg = [...host.children].find(n => !n.classList.contains('sw-old'));
    const fade = !force && !reducedMotion() && prevSvg && prevSvg.getAttribute('height') == H;
    [...host.children].filter(n => n.tagName.toLowerCase() === 'svg').forEach(n => {
      if (!fade || n.classList.contains('sw-old')) { n.remove(); return; }
      n.classList.remove('sw-new');
      n.classList.add('sw-old');
      requestAnimationFrame(() => { n.style.opacity = '0'; });
      setTimeout(() => n.remove(), 700);
    });
    const allActive = (data.torrents || []).filter(t => t.status !== 'idle')
      .sort((a, b) => b.speed_mbs - a.speed_mbs);
    const torrents = allActive.slice(0, 4);
    if (!ui) { ui = document.createElement('div'); host.appendChild(ui); }
    ui.className = vertical ? 'sw-ui v' : 'sw-ui';
    ui.innerHTML = '';
    if (!torrents.length && vertical) { drawIdlePhone(); return; }
    const svg = el('svg', { viewBox: `0 0 ${W} ${H}`, width: '100%', height: H, class: fade ? 'swarm sw-new' : 'swarm', role: 'img', 'aria-label': 'Swarm network' }, host);
    host.insertBefore(svg, ui);
    if (focusKey && !torrents.some(t => t.hash === focusKey) && !String(focusKey).startsWith('dev:')) focusKey = null;
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
    // Vertical: the cache → FUSE tube keeps at least ~70px whatever the height.
    const tubeF = vertical ? Math.max(0.14, 92 / (pad.f1 - pad.f0)) : 0.14;
    const F = { peers: 0, peersEnd: 0.33, torrent: 0.47, cache: 0.66, fuse: 0.66 + tubeF, client: 0.97 };
    if (vertical) { F.cache = Math.min(0.66, 0.82 - tubeF); F.fuse = F.cache + tubeF; F.torrent = Math.min(0.47, F.cache - 0.17); F.peersEnd = F.torrent - 0.14; }
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
      curKey = t.hash;
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
      // Peers. With per-peer rates from the engine each node is a real peer (stable id, real
      // seeder flag, its own rate); without them (older engine) nodes are numbered slots.
      const rates = Array.isArray(t.peer_rates) ? t.peer_rates.slice(0, MAX_PEERS) : null;
      const list = rates ? rates.map(p => ({ key: p.id, seed: p.seed, kbps: p.kbps, utp: !!p.utp, up: p.up_kbps || 0, cc: p.cc || '', asn: p.asn || 0, org: p.org || '' }))
        : Array.from({ length: n }, (_, k) => ({ key: String(k), seed: k < seeds, kbps: null, utp: null }));
      // Track arrivals and departures between polls, by key.
      let L = life.get(t.hash);
      const keys = new Set(list.map(p => p.key));
      if (!L || L.byId !== !!rates) { L = { keys, byId: !!rates, born: {}, dying: {} }; life.set(t.hash, L); }
      else {
        L.keys.forEach(kk => { if (!keys.has(kk)) { L.dying[kk] = { at: now, seed: !!(L.seedOf && L.seedOf[kk]), utp: L.utpOf ? L.utpOf[kk] : null }; delete L.born[kk]; } });
        keys.forEach(kk => { if (!L.keys.has(kk)) { L.born[kk] = now; delete L.dying[kk]; } });
        L.keys = keys;
      }
      L.seedOf = Object.fromEntries(list.map(p => [p.key, p.seed]));
      L.utpOf = Object.fromEntries(list.map(p => [p.key, p.utp]));
      const delay = (ms, total) => `${-Math.min(ms, total)}ms`;
      // uTP vs TCP: real per peer when the engine reports it; an older engine only gives the
      // aggregate share, applied per key as an approximation.
      const isUtp = (key, utp) => utp != null ? utp : !!(tot && rng(t.hash + ':u' + key)() < utpShare);
      // Dash speed follows the peer's own rate: ~3 s along the path at 100 KB/s, faster above.
      const flowDur = kbps => Math.max(0.5, Math.min(6, 30 / Math.sqrt(kbps)));
      list.forEach(p => {
        const pp = spot(p.key, ':');
        const edge = el('path', { d: curve(pp, tp), class: 'sw-edge' }, edgeLayer);
        // uTP vs TCP is known only in aggregate: the share is applied per peer, by its key.
        const node = isUtp(p.key, p.utp) ? el('rect', { x: pp[0] - 3, y: pp[1] - 3, width: 6, height: 6, class: p.seed ? 'sw-peer seed' : 'sw-peer' }, peerLayer)
          : el('circle', { cx: pp[0], cy: pp[1], r: 3.3, class: p.seed ? 'sw-peer seed' : 'sw-peer' }, peerLayer);
        const live = p.kbps == null ? t.speed_mbs > 0.01 : p.kbps >= 0.5;
        const fl = live ? el('path', { d: curve(pp, tp), class: 'sw-peer-flow', pathLength: 100 }, edgeLayer)
          : document.createElementNS(NS, 'path');
        if (live) {
          const dur = p.kbps == null ? 2.6 : flowDur(p.kbps);
          // Two animations may run on a newborn flow (dash + fade-in): keep the fade-in at 0.9 s.
          fl.style.animationDuration = `${dur.toFixed(2)}s, ${BORN_MS / 1000}s`;
          fl.style.animationDelay = `${-((now / 1000 + rng(t.hash + ':f' + p.key)() * dur) % dur).toFixed(2)}s`;
        }
        // Upload: blue segments running back along the same line, from the torrent to the peer.
        if (p.up >= 0.5) {
          const ue = el('path', { d: curve(pp, tp), class: 'sw-up-edge' }, edgeLayer);
          const uf = el('path', { d: curve(pp, tp), class: 'sw-up-flow', pathLength: 100 }, edgeLayer);
          const ud = flowDur(p.up);
          const run = `${-((now / 1000 + rng(t.hash + ':up' + p.key)() * ud) % ud).toFixed(2)}s`;
          uf.style.animationDuration = `${ud.toFixed(2)}s`;
          uf.style.animationDelay = run;
          // A newborn peer's upload fades in with it, like its download line.
          const b = L.born[p.key];
          if (b !== undefined && now - b < BORN_MS) {
            ue.classList.add('sw-born'); ue.style.animationDelay = delay(now - b, BORN_MS);
            uf.classList.add('sw-born');
            uf.style.animationDuration = `${ud.toFixed(2)}s, ${BORN_MS / 1000}s`;
            uf.style.animationDelay = `${run}, ${delay(now - b, BORN_MS)}`;
          }
        }
        // A 6px node is too small to hover and is redrawn every few seconds, which resets a native
        // tooltip: a larger invisible target opens the details card, re-anchored across redraws.
        if (p.kbps != null) {
          const hit = el('circle', { cx: pp[0], cy: pp[1], r: 9, class: 'sw-peer-hit', 'data-peer': t.hash + ':' + p.key }, nodeLayer);
          hoverable(hit, () => details({
            title: countryName(p.cc) || 'Peer',
            sub: p.org ? p.org + (p.asn ? ` · AS${p.asn}` : '') : '',
          }, [
            ['Download', mbps(p.kbps / 1024)],
            ['Upload', p.up > 0 ? mbps(p.up / 1024) : '—'],
            ['Role', p.seed ? 'seeder' : 'leecher'],
            ['Transport', p.utp ? 'uTP' : 'TCP'],
          ], { rect: hit.getBoundingClientRect(), node: hit }));
        }
        const b = L.born[p.key];
        if (b !== undefined) {
          const age = now - b;
          if (age < BORN_MS) {
            [node, edge, fl].forEach(x => { x.classList.add('sw-born'); x.style.animationDelay = (x === fl && live ? fl.style.animationDelay + ',' : '') + delay(age, BORN_MS); });
          }
          // A new peer announces itself: pulses for two seconds where it appeared.
          if (age < BIRTH_PULSE_MS) {
            const ring = el('circle', { cx: pp[0], cy: pp[1], r: 3.3, class: 'sw-birth-ring' }, peerLayer);
            ring.style.animationDelay = delay(age, BIRTH_PULSE_MS);
          } else delete L.born[p.key];
        }
      });
      for (let k = 0; k < pend; k++) {
        const pp = spot(k, ':h');
        el('path', { d: curve(pp, tp), class: 'sw-edge pending' }, edgeLayer);
        el('circle', { cx: pp[0], cy: pp[1], r: 2.2, class: 'sw-peer pending' }, peerLayer);
      }
      // Peers that just left: two pulses, then they fade out where they stood.
      Object.keys(L.dying).forEach(key => {
        const age = now - L.dying[key].at;
        if (age >= DYING_MS || keys.has(key)) { delete L.dying[key]; return; }
        const pp = spot(key, ':');
        const eg = el('g', { class: 'sw-dying' }, edgeLayer);
        eg.style.animationDelay = delay(age, DYING_MS);
        el('path', { d: curve(pp, tp), class: 'sw-edge' }, eg);
        const g = el('g', { class: 'sw-dying' }, peerLayer);
        g.style.animationDelay = delay(age, DYING_MS);
        const ring = el('circle', { cx: pp[0], cy: pp[1], r: 3.3, class: 'sw-death-ring' }, g);
        ring.style.animationDelay = delay(age, DYING_MS);
        const cls = L.dying[key].seed ? 'sw-peer seed' : 'sw-peer';
        if (isUtp(key, L.dying[key].utp)) el('rect', { x: pp[0] - 3, y: pp[1] - 3, width: 6, height: 6, class: cls }, g);
        else el('circle', { cx: pp[0], cy: pp[1], r: 3.3, class: cls }, g);
      });
      // Torrent → cache pipe, width = throughput.
      const d = curve(tp, cacheP);
      const live = t.speed_mbs > 0.01;
      el('path', { d, class: live ? 'sw-pipe' : 'sw-pipe idle', 'stroke-width': width(t.speed_mbs) }, edgeLayer);
      // What the graph leaves out, so the labels can say so: peers past MAX_PEERS, half-open past MAX_PENDING.
      pipes.push({ t, d, w: width(t.speed_mbs), tp, live, shown: list.length, pendShown: pend });
      curKey = null;
    });

    const shown = new Set(torrents.map(t => t.hash));
    [...life.keys()].forEach(hk => { if (!shown.has(hk)) life.delete(hk); });

    // Banned peers sit in the peer field, cut off: no pipe reaches a torrent. The engine counts
    // bans process-wide, so they are spread over the field rather than pinned to one swarm.
    const bannedN = torrents.length ? Math.min(h.v304_banned_peers || 0, 8) : 0;
    if (bannedN) {
      const r = rng('banned');
      for (let k = 0; k < bannedN; k++) {
        const p = P(F.peers + r() * (F.peersEnd - F.peers) * 0.85, 0.06 + r() * 0.88);
        const g = el('g', { class: 'sw-banned-g', 'data-k': 'banned' }, peerLayer);
        el('title', {}, g).textContent = `Banned peers: ${h.v304_banned_peers} in total (corrupt pieces), not tied to a torrent`;
        el('path', { d: `M${p[0] - 3.5} ${p[1] - 3.5} l7 7 M${p[0] + 3.5} ${p[1] - 3.5} l-7 7`, class: 'sw-banned' }, g);
      }
    }

    // Cache → FUSE trunk, and FUSE → clients.
    const totalSpeed = torrents.reduce((a, t) => a + t.speed_mbs, 0);
    const trunkW = Math.max(10, width(Math.max(totalSpeed, 0.02)) + 1);
    // The trunk is the FUSE read-ahead buffer as a level meter: a tube with 3 rows of dots, lit
    // from the cache end in proportion to the real fill (active dark, stale grey). Only the top of
    // the level twitches by about one column, like a VU meter; its average is the real value.
    const tubeW = Math.max(18, trunkW + 4);
    const gauge = el('g', { class: 'sw-gauge' }, edgeLayer);
    el('title', {}, gauge).textContent = `FUSE buffer ${Math.round(h.fuse_budget_mb || 0)} MB · active ${(h.fuse_active_pct || 0).toFixed(0)}% · stale ${(h.fuse_stale_pct || 0).toFixed(0)}%`;
    const dx = fuseP[0] - cacheP[0], dy = fuseP[1] - cacheP[1];
    const len = Math.hypot(dx, dy) || 1, ux = dx / len, uy = dy / len, nx = -uy, ny = ux;
    // The tube is one piece of pipe from node to node, like every other pipe: its ends run under
    // the boxes. The dots stay clear of the boxes (and of GoStorm's ring in STRICT).
    const inA = sh.mode === 'strict' ? 25 : 18, inB = 18;
    const t0 = [cacheP[0] + ux * inA, cacheP[1] + uy * inA];
    el('path', { d: `M${cacheP[0]} ${cacheP[1]} L${fuseP[0]} ${fuseP[1]}`, class: 'sw-gauge-rim', 'stroke-width': tubeW + 2 }, gauge);
    el('path', { d: `M${cacheP[0]} ${cacheP[1]} L${fuseP[0]} ${fuseP[1]}`, class: 'sw-gauge-free', 'stroke-width': tubeW }, gauge);
    const inner = Math.max(0, len - inA - inB);
    const step = vertical ? 6 : 7, cols = Math.max(1, Math.floor(inner / step)), rows = [-4.5, 0, 4.5];
    const act = Math.max(0, Math.min(1, (h.fuse_active_pct || 0) / 100));
    const stale = Math.max(0, Math.min(1 - act, (h.fuse_stale_pct || 0) / 100));
    const fill = act + stale;
    const level = cols * fill, actLevel = cols * act;   // in columns
    const animate = fill >= 0.01 && !reducedMotion();
    const CYCLE = 2.4, N = 24;
    const levels = [];
    for (let i = 0; i < N; i++) {
      const x = i / N * Math.PI * 2;
      // Mean-zero twitch, at most ±1 column and never pushed past either end (keeps the average true).
      const amp = Math.min(1, cols * 0.08, level, cols - level);
      levels.push(level + amp * (0.7 * Math.sin(x * 3) + 0.3 * Math.sin(x * 7 + 1.1)));
    }
    const keyTimes = levels.map((_, i) => (i / N).toFixed(4)).join(';');
    const phase = ((now / 1000) % CYCLE).toFixed(2);
    for (let c = 0; c < cols; c++) {
      // A column is lit when the level covers most of it; it is stale past the active share.
      const cls = c + 0.5 <= actLevel ? 'sw-dot act' : 'sw-dot stale';
      const steady = c + 0.5 <= level - 1;
      const near = !steady && c + 0.5 <= level + 1;
      rows.forEach(r => {
        const along = c * step + step / 2;
        const pos = { cx: t0[0] + ux * along + nx * r, cy: t0[1] + uy * along + ny * r, r: 1.6 };
        if (steady) { el('circle', { ...pos, class: cls }, gauge); return; }
        if (!near || fill < 0.01) { el('circle', { ...pos, class: 'sw-dot off' }, gauge); return; }
        if (!animate) { el('circle', { ...pos, class: c + 0.5 <= level ? cls : 'sw-dot off' }, gauge); return; }
        const dot = el('circle', { ...pos, class: cls }, gauge);
        el('animate', { attributeName: 'opacity', values: levels.map(l => (l >= c + 0.5 ? '1' : '0.15')).join(';'), keyTimes,
          calcMode: 'discrete', dur: `${CYCLE}s`, repeatCount: 'indefinite', begin: `-${phase}s` }, dot);
      });
    }
    const mid = [(cacheP[0] + fuseP[0]) / 2, (cacheP[1] + fuseP[1]) / 2];
    txt(gauge, vertical ? mid[0] + tubeW / 2 + 8 : mid[0], vertical ? mid[1] + 3 : mid[1] + tubeW / 2 + 13,
      `${(h.fuse_active_pct || 0).toFixed(0)}% active · ${(h.fuse_stale_pct || 0).toFixed(0)}% stale`, 'sw-sub', vertical ? 'start' : 'middle');

    // Clients: one node per device we can name, else the stream's anonymous Samba reader.
    const byDevice = new Map();
    torrents.forEach(t => {
      const cl = clients && clients.get(t.hash);
      // A torrent nobody is known to read (preload, sync) gets no client node.
      if (!cl && !t.is_priority) return;
      const name = cl ? cl.label : 'Samba reader';
      const cur = byDevice.get(name) || { name, speed: 0, known: !!cl, hashes: [], paused: true, pos: 0 };
      cur.speed += t.speed_mbs; cur.hashes.push(t.hash);
      // Paused only when every stream it reads is paused, as the media server reports it.
      cur.paused = cur.paused && !!cl && cl.state === 'paused';
      if (cl && cl.pos) cur.pos = cl.pos;
      byDevice.set(name, cur);
    });
    const devs = [...byDevice.values()];
    // A library scan is a client of its own: the media server reading many files, nobody watching.
    if (scan && scan.active) devs.push({ name: scan.label, scan: true, speed: 0, known: true, hashes: [], paused: false, pos: 0 });
    const cliPipes = [];
    devs.forEach((dv, i) => {
      curKey = 'dev:' + dv.name;
      dv.key = curKey;
      const span = vertical ? [0.08, 0.84] : [0.15, 0.48];
      const cp = P(F.client, devs.length === 1 ? 0.45 : span[0] + i * (span[1] / Math.max(1, devs.length - 1)));
      const d = curve(fuseP, cp);
      if (dv.scan) {
        // Short scattered reads, not a stream: a dotted line, no flow.
        el('path', { d, class: 'sw-scan-edge' }, edgeLayer);
        const hit = el('g', { class: 'sw-hitbox' }, nodeLayer);
        el('circle', { cx: cp[0], cy: cp[1], r: 6, class: 'sw-client scan' }, hit);
        const open = torrents.filter(t => scan.torrents && scan.torrents[t.hash] !== undefined && !(clients && clients.get(t.hash))).length;
        const rate = `${scan.files_per_min} file${scan.files_per_min === 1 ? '' : 's'}/min`;
        if (vertical) {
          const an = devs.length > 1 && i === devs.length - 1 ? 'end' : 'middle';
          txt(hit, cp[0], cp[1] + 20, dv.name, 'sw-name', an);
          txt(hit, cp[0], cp[1] + 32, rate, 'sw-sub', an);
        } else {
          txt(hit, cp[0] + 6, cp[1] + 22, dv.name, 'sw-name', 'end');
          txt(hit, cp[0] + 6, cp[1] + 34, `${rate} · ${open} open`, 'sw-sub', 'end');
        }
        const mins = Math.max(1, Math.round((scan.since_s || 0) / 60));
        hoverable(hit, () => details({ title: dv.name, sub: (scan.reader ? scan.reader + ' · ' : '') + 'no playback' }, [
          ['Files read', `${scan.files_read} in ${mins} min`],
          ['Rate', rate],
          ['Open now', `${open} torrent${open === 1 ? '' : 's'}`],
        ], { rect: hit.getBoundingClientRect(), node: hit }));
        curKey = null;
        return;
      }
      el('path', { d, class: dv.paused ? 'sw-pipe paused' : 'sw-pipe', 'stroke-width': dv.paused ? 2 : width(dv.speed) }, edgeLayer);
      if (!dv.paused) cliPipes.push({ d, w: width(dv.speed), speed: dv.speed, key: curKey });
      const hit = el('g', { class: 'sw-hitbox' }, nodeLayer);
      el('circle', { cx: cp[0], cy: cp[1], r: 6, class: dv.paused ? 'sw-client paused' : dv.known ? 'sw-client' : 'sw-client unknown' }, hit);
      const pausedTxt = dv.paused ? `paused${dv.pos ? ' · ' + hms(dv.pos) : ''}` : '';
      const lab = cls => dv.paused ? cls + ' dim' : cls;
      if (vertical) {
        const [dev, via] = dv.name.split(' · ');
        const an = i === 0 && devs.length > 1 ? 'start' : i === devs.length - 1 && devs.length > 1 ? 'end' : 'middle';
        txt(hit, cp[0], cp[1] + 20, short(dev, devs.length > 2 ? 11 : 14), lab('sw-name'), an);
        if (via || pausedTxt) txt(hit, cp[0], cp[1] + 32, pausedTxt || via, 'sw-sub', an);
      } else {
        const y = i === 0 && devs.length > 1 ? cp[1] - (pausedTxt ? 26 : 14) : cp[1] + 22;
        txt(hit, cp[0] + 6, y, short(dv.name, 26), lab('sw-name'), 'end');
        if (pausedTxt) txt(hit, cp[0] + 6, y + 12, pausedTxt, 'sw-sub', 'end');
      }
      hit.addEventListener('click', e => { e.stopPropagation(); toggleFocus(dv.key); });
      hit.style.cursor = 'pointer';
      curKey = null;
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
      curKey = hit ? hit.t.hash : null;
      const d = hit ? hit.d : `M${cacheP[0]} ${cacheP[1]} L${fuseP[0]} ${fuseP[1]}`;
      const w = (hit ? hit.w : trunkW) + 2;
      ['sw-wave-tail', 'sw-wave'].forEach(cls => {
        const p = el('path', { d, class: cls, 'stroke-width': w, pathLength: 100 }, flowLayer);
        p.style.animationDelay = `${-sh.waveMs}ms`;
      });
      curKey = null;
    }
    if (sh.mode === 'strict') {
      // Solid accent on every pipe from the torrents to the clients: STRICT is a state, not a flow.
      const tint = (d, w) => flow(d, w, 'sw-tint');
      pipes.filter(p => p.live).forEach(p => { curKey = p.t.hash; tint(p.d, p.w + 1); });
      // The FUSE buffer gauge keeps its own colours: it is a reading, not part of the fault state.
      cliPipes.forEach(p => { curKey = p.key; tint(p.d, p.w + 1); });
    } else if (sh.mode === 'recover') {
      cliPipes.forEach(p => { curKey = p.key; flow(p.d, p.w + 1, 'sw-drain', 3).style.animationDelay = `${-(now - strictEndedAt)}ms`; });
    }
    curKey = null;

    // Flow pulses: dashes moving along the live pipes, faster for faster streams.
    pipes.filter(p => p.live).forEach(p => { curKey = p.t.hash; flow(p.d, Math.max(1.5, p.w * 0.45), 'sw-pulse', Math.max(0.8, 6 / Math.max(0.3, p.t.speed_mbs))); });
    cliPipes.filter(p => p.speed > 0.01).forEach(p => { curKey = p.key; flow(p.d, Math.max(1.5, p.w * 0.45), 'sw-pulse', Math.max(0.8, 6 / Math.max(0.3, p.speed))); });
    curKey = null;

    // Torrents beyond the first four: a node that lists them, instead of letting them vanish.
    const hidden = allActive.slice(torrents.length);
    let moreG = null;
    if (hidden.length) {
      const g = moreG = el('g', { class: 'sw-more' }, nodeLayer);
      const label = `+${hidden.length} more${vertical ? '' : ' torrent' + (hidden.length > 1 ? 's' : '')}`;
      if (vertical) {
        // A pill in the free space left of GoStorm, clear of the torrent labels.
        const p = P(F.cache, 0.14), w = 64;
        el('rect', { x: p[0] - w / 2, y: p[1] - 10, width: w, height: 20, rx: 10, class: 'sw-more-ring' }, g);
        txt(g, p[0], p[1] + 4, label, 'sw-sub', 'middle');
      } else {
        // Left of the torrent column, at its foot, clear of the last torrent's label.
        const p = P(F.torrent, 1); p[0] -= 18;
        el('circle', { cx: p[0], cy: p[1], r: 9, class: 'sw-more-ring' }, g);
        txt(g, p[0], p[1] + 3.5, `+${hidden.length}`, 'sw-sub', 'middle');
        txt(g, p[0] - 15, p[1] + 4, label, 'sw-sub', 'end');
      }
      hoverable(g, () => {
        const r = g.querySelector('circle, rect').getBoundingClientRect();
        details(`${hidden.length} more active torrent${hidden.length > 1 ? 's' : ''}`,
          hidden.map(t => [short(t.clean_title || t.title || t.hash.slice(0, 8), 30),
            t.speed_mbs > 0.01 ? `${mbps(t.speed_mbs)} · ${t.peers} peer${t.peers === 1 ? '' : 's'}` : 'warming up']),
          { rect: r, node: g });
      });
    }

    // Torrent nodes and labels.
    pipes.forEach((p, i) => {
      curKey = p.t.hash;
      const t = p.t, rad = 7 + Math.min(8, t.speed_mbs * 0.6);
      if (focusKey === t.hash) el('circle', { cx: p.tp[0], cy: p.tp[1], r: rad + 7, class: 'sw-focus' }, nodeLayer);
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
      const node = el('circle', { cx: p.tp[0], cy: p.tp[1], r: rad + 6, class: 'sw-tap' }, nodeLayer);
      node.addEventListener('click', e => { e.stopPropagation(); toggleFocus(t.hash); });
      el('circle', { cx: p.tp[0], cy: p.tp[1], r: rad, class: p.live ? 'sw-torrent' : 'sw-torrent idle' }, nodeLayer).style.pointerEvents = 'none';
      const name = short(t.clean_title || t.title || t.hash.slice(0, 8), vertical ? (torrents.length > 3 ? 9 : 12) : 24);
      const hiddenPeers = Math.max(0, (t.peers || 0) - p.shown), hiddenPend = Math.max(0, (t.half_open || 0) - p.pendShown);
      const sub = (p.live ? `↓ ${mbps(t.speed_mbs)} · ${t.peers} peers` : `idle · ${t.peers} peers`);
      // Upload line, only while this torrent sends: rate and how many peers receive it.
      const upPeers = (t.peer_rates || []).filter(x => (x.up_kbps || 0) >= 0.5).length;
      const upLine = (t.up_mbs || 0) * 8 >= 0.05 ? `↑ ${mbps(t.up_mbs)} · ${upPeers} peer${upPeers === 1 ? '' : 's'}` : '';
      // Third line, only when something is left out: the graph never passes a sample for the whole.
      // Woken by a library scan, not by a player: a dashed ring and a "scan" note.
      const scanAgo = scan && scan.active && !(clients && clients.get(t.hash)) && scan.torrents ? scan.torrents[t.hash] : undefined;
      if (scanAgo !== undefined) el('circle', { cx: p.tp[0], cy: p.tp[1], r: rad + 6, class: 'sw-scan-ring' }, nodeLayer);
      const trunc = [scanAgo !== undefined ? `scan · ${scanAgo < 5 ? 'now' : scanAgo + ' s ago'}` : '',
        hiddenPeers ? `${p.shown} of ${t.peers} peers drawn` : '', hiddenPend ? `${p.pendShown} of ${t.half_open} connecting` : '']
        .filter(Boolean).join(' · ');
      if (vertical) {
        txt(nodeLayer, p.tp[0], p.tp[1] + rad + 14, name, 'sw-name', 'middle');
        txt(nodeLayer, p.tp[0], p.tp[1] + rad + 26, (p.live ? '↓ ' + mbps(t.speed_mbs) : 'idle') + (hiddenPeers ? ` · ${p.shown}/${t.peers}` : ''), 'sw-sub', 'middle');
        if (upLine) txt(nodeLayer, p.tp[0], p.tp[1] + rad + 38, '↑ ' + mbps(t.up_mbs), 'sw-sub up', 'middle');
        else if (scanAgo !== undefined) txt(nodeLayer, p.tp[0], p.tp[1] + rad + 38, 'scan', 'sw-sub dim', 'middle');
      } else {
        const below = i > 0 && i === pipes.length - 1;
        const y = below ? p.tp[1] + rad + 14 : p.tp[1] - rad - 18;
        const extra = (trunc ? 1 : 0) + (upLine ? 1 : 0);
        const yy = below ? y : y - 12 * extra;
        txt(nodeLayer, p.tp[0] + rad + 8, yy, name, 'sw-name');
        txt(nodeLayer, p.tp[0] + rad + 8, yy + 12, sub, 'sw-sub');
        if (upLine) txt(nodeLayer, p.tp[0] + rad + 8, yy + 24, upLine, 'sw-sub up');
        // The lowest label has room for three lines only: the truncation note then goes on the upload line.
        if (trunc && upLine && below) nodeLayer.lastChild.textContent = upLine + ' · ' + trunc;
        else if (trunc) txt(nodeLayer, p.tp[0] + rad + 8, yy + (upLine ? 36 : 24), trunc, 'sw-sub dim');
      }
      curKey = null;
    });

    if (moreG) nodeLayer.appendChild(moreG);   // above the torrent labels

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

    // Follow one stream: everything outside the chosen path fades. With a single path there is
    // nothing to fade, so the gesture is off (and an old focus is dropped).
    const multi = torrents.length > 1 || devs.length > 1;
    if (!multi) focusKey = null;
    svg.querySelectorAll('.sw-tap, .sw-hitbox').forEach(n => { n.style.cursor = multi ? 'pointer' : 'default'; if (!multi) n.style.pointerEvents = 'none'; });
    if (focusKey) {
      const dev = devs.find(d => d.key === focusKey) || devs.find(d => d.hashes.includes(focusKey));
      const isDev = String(focusKey).startsWith('dev:');
      const t = torrents.find(x => x.hash === focusKey) || (dev && torrents.find(x => dev.hashes.includes(x.hash)));
      if (!t && !dev) focusKey = null;
      else {
        const keep = new Set(isDev ? [dev.key, ...dev.hashes] : [t.hash, ...(dev ? [dev.key] : [])]);
        svg.querySelectorAll('[data-k]').forEach(n => { if (!keep.has(n.getAttribute('data-k'))) n.classList.add('sw-dim'); });
        const label = [t && short(t.clean_title || t.title, 24), dev && dev.name.split(' · ')[0]].filter(Boolean);
        const bar = document.createElement('div');
        bar.className = vertical ? 'sw-focusbar' : 'sw-chip';
        bar.innerHTML = vertical
          ? `<span>${esc(label.join(' → '))}${t && t.speed_mbs > 0.01 ? ' · ' + mbps(t.speed_mbs) : ''}</span><button type="button" aria-label="Show all streams">✕</button>`
          : `<b>${esc(label[0] || '')}</b><span>${label[1] ? ' · ' + esc(label[1]) : ''}</span><button type="button" class="sw-showall">Show all ✕</button>`;
        bar.onclick = () => toggleFocus(focusKey);
        ui.appendChild(bar);
      }
    }
    svg.addEventListener('click', () => { if (focusKey) toggleFocus(focusKey); });

    if (!torrents.length) drawIdleCard();
  }

  function recentRows() {
    const day = new Date().toDateString();
    return recent.slice(0, 3).map(r => {
      const d = new Date(r.started_at * 1000);
      const yday = new Date(Date.now() - 86400000).toDateString();
      const when = d.toDateString() === day ? d.toTimeString().slice(0, 5)
        : d.toDateString() === yday ? 'yesterday' : d.toLocaleDateString([], { weekday: 'short', day: 'numeric' });
      const dur = r.duration_s >= 3600 ? `${Math.floor(r.duration_s / 3600)} h ${Math.floor(r.duration_s / 60) % 60} min` : `${Math.max(1, Math.round(r.duration_s / 60))} min`;
      const t = (data.torrents || []).find(x => x.hash === r.hash);
      return [short(t && (t.clean_title || t.title) || r.name.replace(/[._][0-9a-f]{8}$/i, '').replace(/[._]/g, ' '), 28), `${when} · ${dur}`];
    });
  }
  // Idle (desktop): the engine stays drawn; a card lists the last streams.
  function drawIdleCard() {
    const c = document.createElement('div'); c.className = 'sw-idle';
    const rows = recentRows();
    c.innerHTML = `<div class="sw-d-title">No active stream</div><div class="small faint">The engine is idle. The graph fills when a player opens a file.</div>`
      + (rows.length ? `<hr><div class="label">Recent</div>` + rows.map(([a, b]) => `<div class="sw-d-row"><span>${esc(a)}</span><span class="mono">${esc(b)}</span></div>`).join('') : '');
    ui.appendChild(c);
  }
  // Idle (phone): one engine row and the last streams, instead of an empty tall graph.
  function drawIdlePhone() {
    const h = data.health || {};
    const act = Math.max(0, Math.min(1, (h.fuse_active_pct || 0) / 100)), stale = Math.max(0, Math.min(1 - act, (h.fuse_stale_pct || 0) / 100));
    const cols = 16, lit = Math.round(cols * (act + stale)), actC = Math.round(cols * act);
    let dots = '';
    for (let c = 0; c < cols; c++) for (const r of [-4.5, 0, 4.5]) {
      const cls = c < actC ? 'sw-dot act' : c < lit ? 'sw-dot stale' : 'sw-dot off';
      dots += `<circle cx="${70 + c * 9}" cy="${23 + r}" r="1.6" class="${cls}"/>`;
    }
    const rows = recentRows();
    ui.innerHTML = `<div class="sw-idle v"><div class="sw-d-title">No active stream</div>`
      + `<svg viewBox="0 0 300 64" width="100%" height="64" aria-hidden="true"><g class="sw-gauge"><path d="M40 23 L260 23" class="sw-gauge-rim" stroke-width="20"/><path d="M40 23 L260 23" class="sw-gauge-free" stroke-width="18"/>${dots}</g>`
      + `<rect x="30" y="13" width="20" height="20" rx="4" class="sw-box"/><rect x="250" y="13" width="20" height="20" rx="4" class="sw-box"/>`
      + `<text x="40" y="54" class="sw-name" text-anchor="middle">GoStorm</text><text x="260" y="54" class="sw-name" text-anchor="middle">raCache</text></svg>`
      + (rows.length ? `<hr><div class="label">Recent</div>` + rows.map(([a, b]) => `<div class="sw-d-row col"><span>${esc(a)}</span><span class="mono">${esc(b)}</span></div>`).join('') : '')
      + `</div>`;
    [...host.children].filter(n => n.tagName.toLowerCase() === 'svg').forEach(n => n.remove());
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
            return [c.hash, { label: `${c.device} · ${c.source === 'smb' ? 'SMB' : app}`, state: c.state || '', pos: c.position_s || 0 }];
          }));
      }
    } catch (_) { }
    try {
      const rs = await fetch('/api/scan');
      scan = rs.ok ? await rs.json() : null;   // a failed answer must not keep an old scan on screen
    } catch (_) { }
    try {
      const rr = await fetch('/api/recent-streams');
      if (rr.ok) recent = (await rr.json()) || [];
    } catch (_) { }
  }

  window.Swarm = {
    mount(node) {
      host = node;
      host.classList.add('sw-host');
      fetchMetrics().then(() => draw());
      fetchExtras().then(() => draw());
      setInterval(() => { if (!host.hidden) fetchMetrics().then(() => draw()); }, 5000);
      setInterval(() => { if (!host.hidden) fetchExtras().then(() => draw()); }, 15000);
    },
    update(d) { data = d; draw(); },
    redraw: () => draw(true),
    details, closeDetails, closeOrphan, hoverable, mbps,
  };
})();
