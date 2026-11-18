// Restart with feedback, shared by the Control Panel and the Dashboard: a confirm modal, a
// countdown while the engine is away, a reload when the new process answers, and a banner on
// the reloaded page that says whether it really restarted (its uptime is shorter than the wait).
(function () {
  const EST_KEY = 'tiramisu-restart-s';
  const BANNER_MS = 8000;
  // systemd waits RestartSec=10 before starting again, then ~4 s to answer.
  const DEFAULT_EST = 15;

  function estimate() {
    try { const v = +localStorage.getItem(EST_KEY); if (v >= 2 && v <= 120) return Math.round(v); } catch (_) { }
    return DEFAULT_EST;
  }
  // Give up at 3x the estimate, never before 30 s.
  const giveUpAfter = est => Math.max(30, est * 3);

  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }
  function seconds(u) {
    const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)(ms|µs|s))?$/.exec(String(u || '').trim());
    if (!m) return NaN;
    const s = m[4] === 's' ? (+m[3] || 0) : 0;
    return (+m[1] || 0) * 3600 + (+m[2] || 0) * 60 + s;
  }
  // /metrics: {ok, up, version} when it answers; {useHttps} when the new process refuses plain
  // API requests (sign-in on, HTTPS on); throws when nothing answers.
  async function probe() {
    const r = await fetch('/metrics', { cache: 'no-store', signal: AbortSignal.timeout(2000) });
    if (r.status === 403) {
      const b = await r.json().catch(() => ({}));
      if (/https/i.test(b.error || '')) return { useHttps: true };
    }
    if (!r.ok) throw new Error(String(r.status));
    const m = await r.json();
    return { ok: true, up: seconds(m.uptime), uptime: m.uptime, version: m.version };
  }
  // Whether this restart moves the page: HTTPS saved on but not running yet (and we are on
  // http), or saved off while running (and we are on https).
  async function detectScheme() {
    try {
      const [c, t] = await Promise.all([
        fetch('/api/config', { cache: 'no-store' }).then(r => r.ok ? r.json() : null),
        fetch('/api/auth/tls', { cache: 'no-store' }).then(r => r.ok ? r.json() : null),
      ]);
      if (!c || !t) return '';
      const on = !!(c.https && c.https.enabled), runs = !!t.running;
      if (on && !runs && location.protocol === 'http:') return 'https:';
      if (!on && runs && location.protocol === 'https:') return 'http:';
    } catch (_) { }
    return '';
  }

  // ---------- dialog ----------
  let dlg, locked = false;
  function dialog() {
    if (dlg) return dlg;
    dlg = el('dialog', 'rs-dialog');
    dlg.setAttribute('aria-labelledby', 'rs-title');
    dlg.addEventListener('cancel', e => { if (locked) e.preventDefault(); });
    // Chrome lets a second Escape through: a restart in progress reopens itself.
    dlg.addEventListener('close', () => { if (locked) setTimeout(() => { if (locked && !dlg.open) dlg.showModal(); }, 0); });
    document.body.appendChild(dlg);
    return dlg;
  }
  function frame(title, closable, role) {
    const d = dialog();
    d.replaceChildren();
    d.setAttribute('role', role || 'dialog');
    const head = el('div', 'card-head');
    const t = el('span', 'title', title); t.id = 'rs-title';
    head.append(t, el('span', 'spacer'));
    if (closable) {
      const x = el('button', 'key', '✕'); x.type = 'button'; x.setAttribute('aria-label', 'Close');
      x.onclick = () => d.close('cancel');
      head.appendChild(x);
    }
    const body = el('div', 'card-body');
    const foot = el('div', 'rs-foot');
    d.append(head, body, foot);
    locked = !closable;
    if (!d.open) d.showModal();
    return { d, body, foot };
  }
  function button(label, kind, onclick) {
    const b = el('button', 'btn sm ' + (kind || ''), label);
    b.type = 'button';
    b.onclick = onclick;
    return b;
  }
  function list(items) {
    const l = el('div', 'rs-changes');
    items.slice(0, 8).forEach(c => l.appendChild(el('div', 'mono', c)));
    if (items.length > 8) l.appendChild(el('div', 'small faint', `and ${items.length - 8} more`));
    return l;
  }

  // confirm({title, changes:[text], unsaved, discarded:[text], ok}) → Promise<boolean>
  function confirm(opts = {}) {
    return new Promise(resolve => {
      const { d, body, foot } = frame(opts.title || 'Restart Tiramisu?', true);
      const changes = opts.changes || [], lost = opts.discarded || [];
      const one = changes.length === 1;
      body.appendChild(el('p', 'small', changes.length
        ? (opts.unsaved
          ? `${one ? 'One change is' : changes.length + ' changes are'} saved now and take${one ? 's' : ''} effect when Tiramisu starts again.`
          : `${one ? 'One saved change waits' : changes.length + ' saved changes wait'} for Tiramisu to start again.`)
        : 'The engine stops and starts again with the configuration saved now.'));
      if (changes.length) body.appendChild(list(changes));
      if (lost.length) {
        const it = lost.length === 1 ? 'it' : 'them';
        body.append(el('div', 'warn-note', `${lost.length === 1 ? 'One edit is' : lost.length + ' edits are'} not saved and will be lost: apply or discard ${it} first to keep ${it}.`), list(lost));
      }
      const w = el('div', 'rs-what');
      w.append(el('span', 'label', 'What happens'),
        el('span', 'small', `Streams pause for about ${estimate()} s; Plex and Infuse resume on their own. This page waits and reloads when Tiramisu answers again.`));
      body.appendChild(w);
      foot.append(el('span', 'spacer'));
      const cancel = button('Cancel', 'ghost', () => d.close('cancel'));
      const ok = button(opts.ok || 'Restart', 'primary', () => d.close('ok'));
      foot.append(cancel, ok);
      d.returnValue = '';
      d.addEventListener('close', () => resolve(d.returnValue === 'ok'), { once: true });
      cancel.focus(); // a stray Enter must not restart
    });
  }

  // ---------- countdown ----------
  const R = 60, C = 2 * Math.PI * R;
  function ring() {
    const wrap = el('div', 'rs-ring');
    wrap.innerHTML = `<svg viewBox="0 0 132 132" aria-hidden="true"><circle class="track" cx="66" cy="66" r="${R}"/><circle class="arc" cx="66" cy="66" r="${R}" stroke-dasharray="${C}" stroke-dashoffset="0" transform="rotate(-90 66 66)"/></svg>`;
    const num = el('div', 'num');
    const n = el('span', 'n'), u = el('span', 'label');
    num.append(n, u);
    wrap.appendChild(num);
    return {
      node: wrap,
      set(text, unit, frac, state) {
        n.textContent = text; u.textContent = unit;
        wrap.dataset.state = state;
        wrap.querySelector('.arc').setAttribute('stroke-dashoffset', String(C * (1 - Math.max(0, Math.min(1, frac)))));
      },
    };
  }
  function stepsList(rows) {
    const s = el('div', 'rs-steps');
    const items = rows.map(label => {
      const r = el('div', 'rs-step');
      const lamp = el('span', 'lamp off'), t = el('span', 'txt', label), m = el('span', 'mono faint');
      r.append(lamp, t, m);
      s.appendChild(r);
      return { r, lamp, m };
    });
    return {
      node: s,
      set(i, state, meta) {
        const it = items[i];
        if (!it) return;
        it.r.dataset.state = state;
        it.lamp.className = 'lamp ' + (state === 'run' ? 'run' : state === 'off' ? 'off' : '');
        if (meta !== undefined) it.m.textContent = meta;
      },
    };
  }

  // Lets a page skip its "unsaved changes" prompt for the reload this module does.
  let leaving = false;

  // run({changes:n, scheme}): the restart is requested here. scheme undefined = detect it.
  async function run(opts = {}) {
    const n = opts.changes || 0;
    const move = opts.scheme !== undefined && opts.scheme !== null ? opts.scheme : await detectScheme();
    const est = estimate();
    let limit = giveUpAfter(est);
    const t0 = Date.now();
    let before = NaN;
    try { const p = await probe(); if (p.ok) before = p.up; } catch (_) { }

    const centre = el('div', 'rs-centre');
    const rg = ring();
    const title = el('div', 'rs-big');
    const sub = el('div', 'small faint rs-sub');
    centre.append(rg.node, title, sub);
    const rows = [];
    if (n) rows.push('Changes saved');
    rows.push('Engine stopped', move === 'https:' ? 'Starting with HTTPS' : 'Starting',
      move ? `Moving to ${move}//${location.host}` : `Answering on :${location.port || (location.protocol === 'https:' ? 443 : 80)}`);
    const st = stepsList(rows);
    const off = n ? 1 : 0;
    if (n) st.set(0, 'ok', String(n));
    const note = el('span', 'small faint', move ? `The page opens on ${move}// when the new process answers there.`
      : `The page reloads on its own. Estimate from the last restart (${est} s).`);
    let reloadBtn = null;
    const show = () => {
      const f = frame('Restarting Tiramisu', false);
      f.body.append(centre, st.node);
      f.foot.append(note);
      if (reloadBtn) f.foot.appendChild(reloadBtn);
    };
    show();

    try {
      const r = await fetch('/api/restart', { method: 'POST' });
      // A 401/403 is a refusal; a dropped connection or a 5xx from a dying process is not.
      if (!r.ok && r.status < 500) {
        const f = frame('The restart was refused', true, 'alertdialog');
        f.body.append(el('p', 'small', `Tiramisu answered ${r.status}: sign in again and retry. Nothing was restarted.`));
        f.foot.append(el('span', 'spacer'));
        const b = button('Close', 'primary', () => f.d.close());
        f.foot.appendChild(b);
        b.focus();
        return;
      }
    } catch (_) { /* the connection may drop as the old process goes */ }

    let downAt = 0, done = false, timer = 0, busy = false;
    st.set(off, 'run');
    const went = () => {
      if (downAt) return;
      downAt = Date.now();
      st.set(off, 'ok', ((downAt - t0) / 1000).toFixed(1) + ' s');
      st.set(off + 1, 'run');
    };
    const finish = (url, measured, notRestarted) => {
      done = true;
      clearInterval(timer);
      if (measured && !notRestarted) try { localStorage.setItem(EST_KEY, String(Math.max(2, Math.round(measured)))); } catch (_) { }
      st.set(off + 2, 'ok');
      const u = new URL(url || location.href);
      u.searchParams.set('restarted', String(t0));
      u.searchParams.set('est', String(est));
      if (n) u.searchParams.set('changes', String(n));
      leaving = true;
      locked = false;
      location.replace(u.toString());
    };
    const other = scheme => scheme + '//' + location.host + location.pathname + location.search + location.hash;

    const draw = () => {
      const s = (Date.now() - t0) / 1000;
      if (s <= est) {
        const left = Math.max(1, Math.ceil(est - s));
        rg.set(String(left), 'seconds', 1 - s / est, 'run');
        title.textContent = `Back in about ${left} s`;
        sub.textContent = '';
      } else {
        rg.set('+' + Math.round(s - est), 'over', 1, 'late');
        title.textContent = 'Taking longer than usual';
        sub.textContent = 'Still waiting for an answer. A large torrent list or a slow disk can add a few seconds.';
        note.textContent = `Gives up after ${Math.round(limit)} s.`;
        if (!reloadBtn) reloadBtn = button('Reload now', '', () => location.reload());
        const foot = dlg.querySelector('.rs-foot');
        if (foot && !foot.contains(reloadBtn)) foot.appendChild(reloadBtn);
      }
    };

    const fail = () => {
      done = true;
      clearInterval(timer);
      const f = frame('Tiramisu did not come back', false, 'alertdialog');
      const c = el('div', 'rs-centre');
      const r2 = ring();
      r2.set(String(Math.round(limit)), 'seconds', 1, 'err');
      c.append(r2.node, el('div', 'rs-big', `No answer for ${Math.round(limit)} s`));
      f.body.append(c, el('p', 'small', 'The service is down or still starting. The saved changes are kept. Check it on the host:'));
      const pre = el('pre', 'rs-term');
      pre.textContent = 'sudo systemctl status tiramisu\njournalctl -u tiramisu -n 50';
      f.body.append(pre, el('span', 'small faint', 'Docker: docker logs tiramisu'));
      const reload = button('Reload', 'primary', () => location.reload());
      f.foot.append(el('span', 'spacer'),
        button('Keep waiting', 'ghost', () => { limit += giveUpAfter(est); done = false; show(); timer = setInterval(poll, 1000); }),
        reload);
      reload.focus();
    };

    const check = async () => {
      const s = (Date.now() - t0) / 1000;
      if (move === 'http:') {
        // From https:// the new plain-only process cannot be reached: wait for the old one to go,
        // then for the time systemd needs to start it again (the estimate), then go.
        try { await probe(); } catch (_) { went(); }
        if (downAt && s > est + 3) finish(other('http:'), 0);
        return;
      }
      let p;
      try { p = await probe(); } catch (_) { went(); return; }
      if (p.useHttps) {
        // The new process runs HTTPS and refuses the API in clear: it is back, move there.
        went();
        st.set(off + 1, 'ok');
        return finish(other('https:'), s);
      }
      // The old process answers until it is down: only an uptime shorter than the wait is the new one.
      const fresh = Number.isFinite(p.up) && p.up <= s + 1 && (!Number.isFinite(before) || p.up < before);
      if (!fresh) {
        // Never went down and still the old process: the restart was not carried out.
        if (!downAt && Number.isFinite(p.up) && p.up > s + 5 && s > Math.max(20, est * 2)) finish(null, 0, true);
        return;
      }
      went();
      st.set(off + 1, 'ok');
      if (move === 'https:') {
        const t = await fetch('/api/auth/tls', { cache: 'no-store', signal: AbortSignal.timeout(2000) }).catch(() => null);
        const j = t && t.ok ? await t.json().catch(() => ({})) : {};
        if (j.running) return finish(other('https:'), s);
      }
      finish(null, s);
    };

    const poll = async () => {
      if (done || busy) return;
      busy = true;
      try { await check(); } finally { busy = false; }
      if (done) return;
      draw();
      // Fail only after a check that found nothing: a tab woken from the background looks first.
      if ((Date.now() - t0) / 1000 > limit) fail();
    };

    document.addEventListener('visibilitychange', () => { if (!document.hidden) poll(); });
    draw();
    timer = setInterval(poll, 1000);
  }

  // start({changes:[text], discarded:[text], title, ok, scheme}) = confirm, then run.
  async function start(opts = {}) {
    if (!(await confirm(opts))) return false;
    await run({ changes: (opts.changes || []).length, scheme: typeof opts.scheme === 'function' ? opts.scheme() : opts.scheme });
    return true;
  }

  // ---------- after the reload ----------
  async function outcome() {
    const u = new URL(location.href);
    const t0 = +u.searchParams.get('restarted');
    if (!t0) return;
    const n = +u.searchParams.get('changes') || 0;
    const est = +u.searchParams.get('est') || estimate();
    ['restarted', 'changes', 'est'].forEach(k => u.searchParams.delete(k));
    history.replaceState(null, '', u.toString());
    const waited = (Date.now() - t0) / 1000;
    if (waited < 0 || waited > 600) return; // a stale link, not a restart just made
    let p = null;
    try { p = await probe(); } catch (_) { }
    const up = p && p.ok ? p.up : NaN;
    const savedN = n === 1 ? 'saved change is' : n + ' saved changes are';
    const inUse = n ? ` · the ${savedN} in use` : '';
    let state, title, detail, action;
    if (!Number.isFinite(up)) {
      state = 'late'; title = 'Restarted'; detail = 'Tiramisu answers again; its uptime could not be read.';
    } else if (up > waited + 2) {
      state = 'err'; title = 'Tiramisu did not restart';
      detail = `It kept running (up ${String(p.uptime).replace(/\.\d+s$/, 's')}): the restart request was not carried out.` +
        (n ? ` The ${savedN} not in use yet.` : '');
      action = ['Restart again', () => start({})];
    } else {
      const took = Math.max(1, Math.round(waited - up));
      const slow = took > est * 1.5 && took > 20;
      state = slow ? 'late' : 'ok';
      title = slow ? 'Restarted, but slowly' : `Restarted in ${took} s`;
      detail = slow ? `Back after ${took} s; the page reloaded when it answered${inUse}.` : `${p.version || ''} · up ${Math.round(up)} s${inUse}.`;
    }
    // An empty live region first, filled a moment later: screen readers announce the change.
    const b = el('div', 'rs-banner ' + state);
    b.setAttribute('role', state === 'err' ? 'alert' : 'status');
    const host = document.querySelector('.content') || document.body;
    host.insertBefore(b, host.firstChild);
    setTimeout(() => {
      const lamp = el('span', 'lamp ' + (state === 'err' ? 'err' : ''));
      const t = el('div', 'txt');
      t.append(el('div', 'title', title), el('div', 'small', detail));
      b.append(lamp, t);
      if (action) b.appendChild(button(action[0], '', action[1]));
      const x = el('button', 'key', '✕'); x.type = 'button'; x.setAttribute('aria-label', 'Dismiss');
      x.onclick = () => b.remove();
      b.appendChild(x);
      if (state !== 'err') setTimeout(() => { b.classList.add('gone'); setTimeout(() => b.remove(), 400); }, BANNER_MS);
    }, 60);
  }

  window.TiramisuRestart = { confirm, run, start, estimate, get leaving() { return leaving; } };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', outcome);
  else outcome();
})();
