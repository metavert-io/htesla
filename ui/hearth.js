/* The page-side half of Hearthloom's sandbox handshake (after
   hpresence/ui/hearth.js). The shell posts {type:"hearthloom", token,
   base, palette} into this iframe on load; `window.hearth` resolves
   with a tiny client bound to that token — the ONLY authority this
   page has. The token can set the ember's secrets but never read them
   back, and it cannot reach Tesla: the sidecar does that. Messages
   count only when they come from the window that embeds us (the
   dashboard shell): any other window that can reach this page (one that
   opened it, say) could otherwise hand it a `base` of its own and
   receive the secrets typed into it. */
// Two checks: the message comes from the window that embeds us, and that
// window is the dashboard's own origin (the page and the dashboard are
// served by the same node). The source check alone would accept a site
// that frames this page itself.
const fromShell = (ev) =>
  window.parent !== window && ev.source === window.parent && ev.origin === location.origin;

window.hearth = new Promise((resolve) => {
  addEventListener("message", (ev) => {
    if (!fromShell(ev)) return;
    const d = ev.data;
    if (!d || d.type !== "hearthloom") return;
    for (const [k, v] of Object.entries(d.palette || {})) {
      if (v) document.documentElement.style.setProperty(k, v);
    }
    document.documentElement.dataset.framed = "1";
    const headers = { authorization: `Bearer ${d.token}` };
    const url = (p) => `${d.base}${p}`;
    const check = async (r) => {
      if (!r.ok) {
        let msg = `${r.status}`;
        try { msg = (await r.text()) || msg; } catch { /* keep status */ }
        const e = new Error(msg);
        e.status = r.status;
        throw e;
      }
      return r;
    };
    resolve({
      base: d.base,
      hasToken: !!d.token,
      async kv(key) {
        const r = await fetch(url(`/v1/plugins/self/kv/${encodeURIComponent(key)}`), { headers, cache: "no-store" });
        if (r.status === 404) return null;
        await check(r);
        const v = (await r.json()).value;
        try { return JSON.parse(v); } catch { return v; }
      },
      async kvPut(key, value) {
        await check(await fetch(url(`/v1/plugins/self/kv/${encodeURIComponent(key)}`), {
          method: "PUT", headers, body: JSON.stringify(value),
        }));
      },
      async secrets() {
        const r = await check(await fetch(url("/v1/plugins/self/secrets"), { headers, cache: "no-store" }));
        return r.json();
      },
      async secretPut(key, text) {
        await check(await fetch(url(`/v1/plugins/self/secrets/${encodeURIComponent(key)}`), {
          method: "PUT", headers, body: text,
        }));
      },
      async file(path) {
        const r = await fetch(url(`/v1/plugins/self/files/${path.split("/").map(encodeURIComponent).join("/")}`), { headers, cache: "no-store" });
        if (r.status === 404) return null;
        await check(r);
        const t = await r.text();
        try { return JSON.parse(t); } catch { return t; }
      },
      async oauthPending(state, authorizeUrl) {
        const r = await check(await fetch(url("/v1/plugins/self/oauth/pending"), {
          method: "POST",
          headers: { ...headers, "content-type": "application/json" },
          body: JSON.stringify({ state, authorize_url: authorizeUrl, ttl_secs: 900 }),
        }));
        return r.json();
      },
    });
  });
});

// Report content height so the dashboard sizes our iframe to fit (no
// inner scrollbar — the page flows in the dashboard's own scroll).
{
  const report = () => {
    try {
      parent.postMessage({ type: "hearthloom-height", px: Math.ceil(document.documentElement.scrollHeight) }, "*");
    } catch { /* not framed */ }
  };
  addEventListener("load", report);
  if (window.ResizeObserver) {
    const ro = new ResizeObserver(report);
    ro.observe(document.documentElement);
    addEventListener("DOMContentLoaded", () => ro.observe(document.body)); // content growth
  }
  setInterval(report, 1000);
}
