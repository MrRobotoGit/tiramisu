// Shared shell behaviour for the Control Panel and the dashboard: theme (light / dark / auto,
// same localStorage key as the old panel), the phone drawer, the update hint in the rail.
(function () {
  const KEY = 'gs-theme';
  const media = window.matchMedia('(prefers-color-scheme: dark)');

  function stored() {
    try { return localStorage.getItem(KEY) || 'auto'; } catch (_) { return 'auto'; }
  }

  function apply(choice) {
    const dark = choice === 'dark' || (choice === 'auto' && media.matches);
    document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light');
    document.querySelectorAll('[data-theme-choice]').forEach(b => {
      b.classList.toggle('active', b.dataset.themeChoice === choice);
      b.setAttribute('aria-pressed', b.dataset.themeChoice === choice);
    });
    document.dispatchEvent(new CustomEvent('themechange'));
  }

  let version = '', uptime = '';
  // One uptime format in both panels, the Dashboard's: a Go duration rounded to the second
  // ("12m30s", "1h2m3s"). /metrics sends it unrounded ("12m30.17s").
  function fmtUptime(u) {
    if (!u) return '';
    const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:([\d.]+)s)?$/.exec(String(u).trim());
    if (!m) return u;
    const t = (+m[1] || 0) * 3600 + (+m[2] || 0) * 60 + Math.round(+m[3] || 0);
    const h = Math.floor(t / 3600), mi = Math.floor(t % 3600 / 60), sec = t % 60;
    return h ? `${h}h${mi}m${sec}s` : mi ? `${mi}m${sec}s` : `${sec}s`;
  }
  window.TiramisuUI = {
    // The dashboard polls uptime every 5 s; keep the rail line in step with it.
    setUptime(u) {
      uptime = fmtUptime(u);
      const ver = document.getElementById('rail-version');
      if (ver && version) ver.textContent = version + ' · up ' + u;
    },
    setTheme(choice) {
      try { localStorage.setItem(KEY, choice); } catch (_) { }
      apply(choice);
    },
    toggleRail(open) {
      const rail = document.querySelector('.rail');
      if (rail) rail.classList.toggle('open', open);
    },
    // Fills the rail footer from /metrics: version and, for a release build, the newer tag.
    async checkUpdate() {
      try {
        const r = await fetch('/metrics');
        if (!r.ok) return;
        const d = await r.json();
        const ver = document.getElementById('rail-version');
        version = d.version || 'dev';
        if (ver) ver.textContent = version + ' · up ' + (uptime || fmtUptime(d.uptime) || '—');
        const star = document.getElementById('rail-stars');
        if (star && d.github_stars > 0) {
          star.textContent = d.github_stars >= 1000 ? (d.github_stars / 1000).toFixed(1) + 'k' : String(d.github_stars);
          star.hidden = false;
        }
        const box = document.getElementById('rail-update');
        if (!box || !d.update_available || !d.latest_version) return;
        const isDev = !d.version || d.version === 'dev';
        // A dev build cannot tell whether it already contains the latest tag: state it, don't alarm.
        const chip = box.querySelector('.chip-tag');
        chip.textContent = isDev ? 'Stable' : 'Update';
        // Phones keep the rail off-screen: repeat a real update in the top bar.
        box.querySelector('.txt').textContent = d.latest_version + (isDev ? '' : ' available');
        box.title = isDev ? 'Dev build — latest stable: ' + d.latest_version : 'Current: ' + d.version + ' → ' + d.latest_version;
        const top = document.getElementById('top-update');
        if (top && !isDev) { top.textContent = 'Update ' + d.latest_version; top.title = box.title; top.hidden = false; }
        box.hidden = false;
      } catch (_) { }
    },
  };

  apply(stored());
  media.addEventListener('change', () => { if (stored() === 'auto') apply('auto'); });
  document.addEventListener('DOMContentLoaded', () => {
    apply(stored());
    document.addEventListener('click', e => {
      const rail = document.querySelector('.rail.open');
      if (rail && !rail.contains(e.target) && !e.target.closest('.menu-btn')) rail.classList.remove('open');
    });
  });
})();
