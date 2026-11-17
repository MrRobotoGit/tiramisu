// Sign-in on :9080: a 401 from the Tiramisu middleware sends the page to /login, and the
// signed-in user gets a name and "Sign out" in the top bar. Inert while sign-in is off.
(function () {
  const native = window.fetch.bind(window);
  let redirecting = false;

  function toLogin() {
    if (redirecting) return;
    redirecting = true;
    location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search + location.hash);
  }

  window.fetch = async function (input, init) {
    const res = await native(input, init);
    if (res.status === 401) {
      const url = new URL(input instanceof Request ? input.url : String(input), location.href);
      // Only our own middleware answers with this realm; GoStorm's Basic Auth 401 must not loop to /login.
      const ours = (res.headers.get('WWW-Authenticate') || '').includes('realm="tiramisu"');
      if (ours && url.origin === location.origin && !url.pathname.startsWith('/api/auth/')) toLogin();
    }
    return res;
  };

  async function status() {
    const r = await native('/api/auth/status', { cache: 'no-store' });
    return r.ok ? r.json() : null;
  }

  function slot(st) {
    const bar = document.querySelector('.topbar');
    if (!bar || !st || !st.enabled || !st.authenticated || !st.username) return;
    const wrap = document.createElement('span');
    wrap.className = 'auth-user';
    const av = document.createElement('button');
    av.type = 'button';
    av.className = 'avatar';
    av.textContent = st.username.charAt(0).toUpperCase();
    av.setAttribute('aria-label', 'Sign out ' + st.username);
    const name = document.createElement('span');
    name.className = 'small hide-sm';
    name.style.fontWeight = '500';
    name.textContent = st.username;
    const out = document.createElement('button');
    out.type = 'button';
    out.className = 'btn ghost sm';
    out.textContent = 'Sign out';
    const signOut = async () => {
      await native('/api/auth/logout', { method: 'POST' }).catch(() => {});
      location.href = '/login';
    };
    out.addEventListener('click', signOut);
    av.addEventListener('click', () => { if (confirm('Sign out of Tiramisu?')) signOut(); });
    wrap.append(av, name, out);
    bar.appendChild(wrap);
  }

  window.TiramisuAuth = { status };
  const ready = () => status().then(slot).catch(() => {});
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', ready);
  else ready();
})();
