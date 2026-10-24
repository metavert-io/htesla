/* The Tesla setup wizard. It never talks to Tesla and never reads a
   secret: it shows the sidecar's KV "status", and each button queues a
   command in KV "cmd" that the sidecar picks up within seconds. Two
   things it does itself: save the client secret (write-only), and
   register the sign-in with the daemon's OAuth relay. */
(() => {
  "use strict";
  const $ = (sel, root = document) => root.querySelector(sel);
  const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const iso = () => new Date().toISOString();
  const rid = () => {
    const b = crypto.getRandomValues(new Uint8Array(24));
    return btoa(String.fromCharCode(...b)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  };
  const ago = (t) => {
    if (!t) return "";
    const s = Math.round((Date.now() - Date.parse(t)) / 1000);
    if (!isFinite(s)) return "";
    if (s < 45) return "just now";
    if (s < 3600) return `${Math.round(s / 60)} min ago`;
    if (s < 86400) return `${Math.round(s / 3600)} h ago`;
    return new Date(t).toLocaleDateString();
  };
  const when = (t) => (t ? new Date(t).toLocaleString() : "");

  let h = null;          // the hearth client
  let st = null;         // last status
  let busy = {};         // action → started-at (ms), until status moves
  let confirmOpen = null; // which inline confirm is open
  let lastStatusAt = 0;

  /* ———— commands ———— */
  async function queue(action, args) {
    let doc = null;
    try { doc = await h.kv("cmd"); } catch { /* start fresh */ }
    if (!doc || typeof doc !== "object") doc = {};
    const cutoff = Date.now() - 15 * 60e3;
    const q = (Array.isArray(doc.queue) ? doc.queue : []).filter((c) => Date.parse(c.at) > cutoff).slice(-19);
    q.push({ id: rid(), action, at: iso(), ...(args ? { args } : {}) });
    await h.kvPut("cmd", { seen: iso(), queue: q });
    busy[action] = Date.now();
    render();
    setTimeout(poll, 1500);
  }
  async function heartbeat() {
    if (!h || document.hidden) return;
    try {
      const doc = (await h.kv("cmd")) || {};
      await h.kvPut("cmd", { seen: iso(), queue: Array.isArray(doc.queue) ? doc.queue : [] });
    } catch { /* next time */ }
  }
  const isBusy = (action) => busy[action] && Date.now() - busy[action] < 45e3;

  /* ———— copy (the sandbox may refuse the clipboard: fall back to selecting) ———— */
  async function copyText(text, btn) {
    let ok = false;
    try { await navigator.clipboard.writeText(text); ok = true; } catch { /* sandboxed */ }
    if (!ok) {
      const ta = document.createElement("textarea");
      ta.value = text; ta.setAttribute("readonly", ""); ta.style.position = "fixed"; ta.style.opacity = "0";
      document.body.append(ta); ta.select();
      try { ok = document.execCommand("copy"); } catch { ok = false; }
      ta.remove();
    }
    if (!ok) {
      const code = btn.previousElementSibling;
      const r = document.createRange(); r.selectNodeContents(code);
      const sel = getSelection(); sel.removeAllRanges(); sel.addRange(r);
    }
    const old = btn.textContent;
    btn.textContent = ok ? "copied" : "selected — press ⌘C";
    setTimeout(() => (btn.textContent = old), 1800);
  }
  const copyRow = (value) => value
    ? `<div class="copy"><code>${esc(value)}</code><button type="button" class="small" data-copy="${esc(value)}">copy</button></div>`
    : `<span class="faint small">not set yet</span>`;

  /* ———— step states ———— */
  function stepStates(s) {
    const c = s.config || {};
    const problems = c.problems || [];
    const k = s.keys || {};
    const vehicles = s.vehicles || [];
    const paired = vehicles.filter((v) => v.paired === true);
    const per = (s.streaming && s.streaming.per_vin) || {};
    const synced = Object.values(per).length > 0 && Object.values(per).every((v) => v.synced);
    const st1 = k.state === "ready" && k.served === true ? "done" : k.state === "lost" || k.state === "error" ? "bad" : k.state === "ready" ? "attn" : "wait";
    const st2 = problems.length === 0 && s.app && s.app.client_secret_set ? "done" : "todo";
    const reg = (s.register || {}).state;
    const st3 = reg === "done" ? "done" : reg === "error" ? "bad" : reg === "working" ? "wait" : "todo";
    const si = (s.signin || {}).state;
    const st4 = si === "signed_in" ? "done" : si === "waiting" ? "wait" : si === "sign_in_again" || si === "error" ? "bad" : "todo";
    const st5 = paired.length > 0 ? "done" : vehicles.length > 0 ? "attn" : "todo";
    const ss = (s.streaming || {}).state;
    const st6 = ss === "configured" ? (s.streaming.stale ? "attn" : synced ? "done" : "wait") : ss === "paused" ? "paused"
      : ss === "error" ? "bad" : ss === "working" ? "wait" : "todo";
    return [st1, st2, st3, st4, st5, st6];
  }
  const BADGE = {
    done: ["done", "done"], todo: ["", "to do"], next: ["next", "next"], wait: ["wait", "in progress"],
    attn: ["wait", "needs you"], bad: ["bad", "needs attention"], paused: ["wait", "paused"],
  };

  /* ———— render ———— */
  function banners(s) {
    const out = [];
    const c = s.config || {};
    if (!h.hasToken) {
      out.push(`<div class="notice bad">Open this page from Hearthloom's menu — it needs the token the dashboard hands it.</div>`);
    }
    if ((c.problems || []).length) {
      out.push(`<div class="notice"><b>Settings needed.</b> Open <b>☰ → Integrations</b>, find the <b>htesla</b> card, and fill in its settings:
        <ul>${c.problems.map((p) => `<li>${esc(p)}</li>`).join("")}</ul></div>`);
    }
    if (s.sealing === "node-key") {
      out.push(`<div class="notice">Your Tesla secrets (the app key, client secret and sign-in) are sealed with a key kept on this node's own disk —
        no master key is set up here, so anyone who can read this machine's disk could unseal them.</div>`);
    } else if (s.sealing === "master-key") {
      out.push(`<div class="notice ok">Your Tesla secrets are sealed with this node's master key.</div>`);
    }
    const stale = Date.now() - Date.parse(s.updated_at || 0);
    if (stale > 3 * 60e3 && s.receiver && s.receiver.listening === false && s.updated_at) {
      out.push(`<div class="notice bad">The Tesla program last reported ${esc(ago(s.updated_at))}. It may have stopped — see the ember's log.</div>`);
    }
    return out.join("");
  }

  function body1(s) {
    const k = s.keys || {}, c = s.config || {};
    if (k.state === "lost") {
      return `<p class="err">${esc(k.error)}</p>
        ${confirmOpen === "new_key" ? `<div class="confirm"><p>A new key means Tesla must be told again (step 3) and every car paired again (step 5). Make a new key?</p>
          <div class="actions"><button class="primary" data-act="new_key" data-confirmed="1">Make a new key</button><button data-cancel>Keep things as they are</button></div></div>`
        : `<div class="actions"><button data-confirm="new_key">Make a new key…</button></div>`}`;
    }
    if (k.state !== "ready") {
      return k.error ? `<p class="err">${esc(k.error)}</p>` : `<p class="faint"><span class="spinner"></span>Making your app's key…</p>`;
    }
    const served = k.served === true ? `<span class="okc">Tesla can read it there.</span>`
      : k.served === false ? `<span class="err">Not reachable yet.</span> <span class="faint small">${esc(k.check_note || "")}</span>`
      : `<span class="faint">Not checked yet.</span>`;
    return `<p>This node made a private key for your Tesla app (P-256) and sealed it; the public half is published where Tesla looks for it.</p>
      <dl class="kv"><dt>Public key</dt><dd>${copyRow(c.public_key_url)}</dd>
      <dt>Reachable</dt><dd>${served} ${k.checked_at ? `<span class="faint small">· checked ${esc(ago(k.checked_at))}</span>` : ""}</dd></dl>
      <div class="actions"><button data-act="check_key" ${isBusy("check_key") ? "disabled" : ""}>${isBusy("check_key") ? "checking…" : "Check again"}</button></div>`;
  }

  function body2(s) {
    const c = s.config || {}, a = s.app || {};
    return `<p>At <span class="mono">developer.tesla.com</span>, create (or edit) your app with these values:</p>
      <dl class="kv">
        <dt>Allowed Origin</dt><dd>${copyRow(c.allowed_origin)}</dd>
        <dt>Redirect URI</dt><dd>${copyRow(c.redirect_uri)}</dd>
        <dt>Scopes</dt><dd class="small">Tick <b>Vehicle Information</b> and <b>Vehicle Location</b> only — not Vehicle Commands or Charging Management. This ember never asks for them.</dd>
        <dt>Client ID</dt><dd>${c.client_id ? `<span class="mono">${esc(c.client_id)}</span>` : `<span class="faint small">Paste it into the htesla card's settings (☰ → Integrations).</span>`}</dd>
        <dt>Client secret</dt><dd>${a.client_secret_set ? `<span class="okc">saved</span> <span class="faint small">${esc(ago(a.client_secret_at))}</span>` : `<span class="faint small">not saved yet — paste it below</span>`}</dd>
      </dl>`;
  }

  function body3(s) {
    const r = s.register || {}, c = s.config || {}, k = s.keys || {};
    const ready = (c.problems || []).length === 0 && (s.app || {}).client_secret_set && k.state === "ready";
    let state = "";
    if (r.state === "done") state = `<p class="okc">Registered <span class="mono">${esc(r.domain)}</span> ${esc(ago(r.at))}.</p>`;
    else if (r.state === "error") state = `<p class="err">${esc(r.error)}</p>`;
    else if (r.state === "working") state = `<p class="faint"><span class="spinner"></span>Registering…</p>`;
    return `<p>Tells Tesla that <span class="mono">${esc(c.domain || "your node")}</span> belongs to your app. Tesla checks the public key from step 1 while doing it.</p>
      ${state}
      <div class="actions"><button ${r.state === "done" ? "" : 'class="primary"'} data-act="register" ${!ready || isBusy("register") || r.state === "working" ? "disabled" : ""}>
        ${r.state === "done" ? "Register again" : "Register"}</button>
        ${ready ? "" : `<span class="faint small">Finish steps 1–2 first.</span>`}</div>`;
  }

  function body4(s) {
    const si = s.signin || {}, c = s.config || {};
    const ready = (c.problems || []).length === 0 && (s.app || {}).client_secret_set;
    const parts = [];
    if (si.state === "signed_in") {
      parts.push(`<p class="okc">Signed in${si.at ? ` ${esc(ago(si.at))}` : ""}.</p>`);
      if (si.error) parts.push(`<p class="err">${esc(si.error)}</p>`);
      parts.push(confirmOpen === "sign_out"
        ? `<div class="confirm"><p>Sign out? Your car's data stops flowing until you sign in again.</p>
            <div class="actions"><button class="primary" data-act="sign_out" data-confirmed="1">Sign out</button><button data-cancel>Stay signed in</button></div></div>`
        : `<div class="actions"><button data-confirm="sign_out">Sign out…</button></div>`);
      return parts.join("");
    }
    parts.push(`<p>Sign in with your Tesla account and approve <b>vehicle information</b> and <b>location</b> — read-only access.
      A Tesla tab opens; when it says you're signed in, come back here.</p>`);
    if (si.state === "waiting") {
      parts.push(`<p class="faint"><span class="spinner"></span>Waiting for Tesla to send you back… If no tab opened, use the link the dashboard shows above this page.</p>`);
    } else if (si.state === "sign_in_again") {
      parts.push(`<p class="err">Tesla needs you to sign in again (sign-ins last about three months, or yours was revoked).</p>`);
    } else if (si.state === "error" && si.error) {
      parts.push(`<p class="err">${esc(si.error)}</p>`);
    }
    parts.push(`<div class="actions"><button class="primary" data-signin ${!ready || !c.client_id || !c.redirect_uri ? "disabled" : ""}>Sign in with Tesla</button>
      ${si.state === "waiting" ? `<button data-act="signin_cancel">Cancel</button>` : ""}
      ${ready ? "" : `<span class="faint small">Finish step 2 first.</span>`}
      <span class="small err" id="signin-note"></span></div>`);
    return parts.join("");
  }

  function body5(s) {
    const vs = s.vehicles || [], si = s.signin || {}, c = s.config || {};
    if (si.state !== "signed_in" && !vs.length) return `<p class="faint">Sign in first; your cars appear here.</p>`;
    const cars = vs.map((v) => `
      <div class="car">
        <div class="car-head"><span class="car-name">${esc(v.name || "Your Tesla")} <span class="mono faint">…${esc(v.last4)}</span></span>
          <span class="small">${v.paired === true ? `<span class="okc">key paired</span>` : v.paired === false ? `<span class="err">not paired yet</span>` : `<span class="faint">pairing unknown</span>`}
          ${v.firmware ? ` · <span class="mono faint">${esc(v.firmware)}</span>` : ""}</span></div>
        ${v.paired === true ? "" : `<p class="small">On the phone with the Tesla app, open this link and approve the key (the car may ask you to tap your key card):</p>${copyRow(v.pair_url)}`}
      </div>`).join("");
    return `<p>Your car must trust this app's key before it will stream. Pairing only lets the app <i>receive</i> data — it grants no control.</p>
      ${s.vehicles_error ? `<p class="err">${esc(s.vehicles_error)}</p>` : ""}
      <div class="cars">${cars || `<p class="faint">No cars on this Tesla account.</p>`}</div>
      <div class="actions"><button data-act="refresh_vehicles" ${isBusy("refresh_vehicles") || si.state !== "signed_in" ? "disabled" : ""}>${isBusy("refresh_vehicles") ? "checking…" : "Check again"}</button>
        ${s.vehicles_at ? `<span class="faint small">checked ${esc(ago(s.vehicles_at))}</span>` : ""}</div>`;
  }

  function body6(s) {
    const str = s.streaming || {}, rc = s.receiver || {}, c = s.config || {};
    const paired = (s.vehicles || []).filter((v) => v.paired === true).length;
    const ready = (s.signin || {}).state === "signed_in" && paired > 0 && rc.listening;
    const out = [];
    out.push(`<p>Tells your car to send its data straight to this node — signed with your app's key. Tesla bills about $1 per 150,000 signals
      (your developer account gets $10 a month); this ember's lean field set costs well under a cent per hour of driving.</p>`);
    const cost = s.cost;
    if (cost) {
      const pctP = Math.round(cost.pct_projected || 0);
      const cls = pctP >= 100 ? "err" : pctP >= 80 ? "warnc" : "";
      out.push(`<div class="costbox"><div class="costhead"><b>This month</b><span class="faint small">${esc(cost.month)} · Tesla's $${esc(cost.credit_usd)} credit</span></div>
        <div class="meter"><i style="width:${Math.min(100, cost.pct_so_far || 0).toFixed(1)}%"></i><u style="left:${Math.min(100, pctP)}%"></u></div>
        <p class="small">$${(cost.cost_so_far_usd || 0).toFixed(2)} so far (${esc((cost.signals || 0).toLocaleString())} signals${cost.billed_calls ? `, ${esc(cost.billed_calls)} billed calls` : ""}) ·
          <span class="${cls}">projected $${(cost.projected_usd || 0).toFixed(2)} (${pctP}% of the credit)</span></p>
        <p class="small faint">${esc((cost.free_calls || 0).toLocaleString())} Fleet API calls this month, all on Tesla's free endpoints. You'll get a notification at 80% and 100% of the credit.</p></div>`);
    }
    out.push(`<dl class="kv"><dt>Receiver</dt><dd>${rc.listening
      ? `<span class="okc">listening</span> on <span class="mono">${esc(c.domain)}:${esc(c.telemetry_port)}</span>`
      : `<span class="err">not listening</span>`}${rc.error ? ` <span class="err small">${esc(rc.error)}</span>` : ""}</dd>
      ${rc.cert_host ? `<dt>Certificate</dt><dd class="small">for <span class="mono">${esc(rc.cert_host)}</span> from this node's private CA · until ${esc(new Date(rc.cert_expires).toLocaleDateString())}</dd>` : ""}
      <dt>Today</dt><dd class="small">${esc((s.today || {}).signals || 0)} signals · ≈ $${((s.today || {}).est_cost_usd || 0).toFixed(4)}</dd></dl>`);
    if (str.state === "error") out.push(`<p class="err">${esc(str.error)}</p>`);
    if (str.state === "paused") out.push(`<p class="warnc">Streaming is paused${str.paused_at ? ` (since ${esc(ago(str.paused_at))})` : ""} — your car isn't sending data and nothing is billed. Resume to send a fresh configuration.</p>`);
    if (str.state !== "error" && str.error) out.push(`<p class="err small">${esc(str.error)}</p>`);
    if (str.state === "working") out.push(`<p class="faint"><span class="spinner"></span>Sending the configuration…</p>`);
    if (str.stale) out.push(`<p class="err">${esc(str.stale)}</p>`);
    if (str.state === "configured") {
      const per = str.per_vin || {};
      const skipped = Object.entries(str.skipped || {}).filter(([, v]) => v && v.length);
      out.push(`<p class="okc">Configuration sent ${esc(ago(str.at))} to ${esc(str.updated)} car${str.updated === 1 ? "" : "s"}.</p>`);
      if (skipped.length) out.push(`<p class="err small">Tesla skipped: ${skipped.map(([k, v]) => `${esc(k.replace(/_/g, " "))} (${v.map((x) => "…" + esc(String(x).slice(-4))).join(", ")})`).join("; ")}</p>`);
      const rows = Object.entries(per).map(([vin, v]) => {
        const car = (s.cars || {})[vin] || {};
        return `<div class="car"><div class="car-head"><span class="car-name"><span class="dot ${car.online ? "on" : ""}"></span>…${esc(vin.slice(-4))}</span>
          <span class="small">${v.synced ? `<span class="okc">car has the config</span>` : `<span class="faint">waiting for the car to pick it up</span>`}
          ${v.limit_reached ? ` · <span class="err">this car already streams to its maximum number of apps</span>` : ""}</span></div>
          <p class="small faint">${car.online ? `streaming now · connected ${esc(ago(car.connected_at))}` : car.last_seen ? `last seen ${esc(ago(car.last_seen))}` : "hasn't connected yet"}${car.network ? ` · ${esc(car.network)}` : ""}</p>
          ${v.error ? `<p class="small err">${esc(v.error)}</p>` : ""}
          ${(v.errors || []).length ? `<p class="small err">Car reports: ${v.errors.map(esc).join("; ")}</p>` : ""}</div>`;
      }).join("");
      out.push(`<div class="cars">${rows}</div>`);
      if (str.fields && str.fields.length) out.push(`<details><summary>${str.fields.length} signals streamed</summary><p class="small mono faint">${str.fields.map(esc).join(" · ")}</p></details>`);
    }
    const rec = s.records || {};
    if (str.state === "configured" || str.state === "paused" || rec.posted || rec.cursor) {
      out.push(`<dl class="kv"><dt>Records</dt><dd class="small">${esc(rec.posted || 0)} posted${rec.outbox ? ` · ${esc(rec.outbox)} waiting to post` : ""}${rec.open_drives ? ` · <span class="okc">a drive in progress</span>` : ""}${rec.open_charges ? ` · <span class="okc">charging</span>` : ""}
        ${rec.last_posted ? `<br><span class="faint">last: ${esc(rec.last_posted.split(":")[0])} ${esc(ago(rec.last_at))}</span>` : ""}
        ${rec.error ? `<br><span class="err">${esc(rec.error)}</span>` : ""}</dd>
        <dt>Time zone</dt><dd class="small">${esc(c.timezone || "")}${(c.timezone || "").startsWith("Local") || !c.timezone ? ` <span class="faint">— set <b>timezone</b> on the Integrations card so drives are dated in your zone</span>` : ""}</dd></dl>`);
    }
    if (str.state === "paused") {
      out.push(`<div class="actions"><button class="primary" data-act="resume_streaming" ${!ready || isBusy("resume_streaming") ? "disabled" : ""}>Resume streaming</button>
        ${ready ? "" : `<span class="faint small">Needs steps 4–5 and a running receiver.</span>`}</div>`);
      return out.join("");
    }
    out.push(`<div class="actions"><button ${str.state === "configured" ? "" : 'class="primary"'} data-act="start_streaming" ${!ready || isBusy("start_streaming") || str.state === "working" ? "disabled" : ""}>
        ${str.state === "configured" ? "Send the configuration again" : "Start streaming"}</button>
      ${str.state === "configured" ? `<button data-act="check_streaming" ${isBusy("check_streaming") ? "disabled" : ""}>Check status</button>
        <button data-confirm="pause_streaming">Pause streaming…</button>` : ""}
      ${ready ? "" : `<span class="faint small">Needs steps 4–5 and a running receiver.</span>`}</div>`);
    if (confirmOpen === "pause_streaming") {
      out.push(`<div class="confirm"><p>Pause streaming? Tesla removes the streaming setup from your car, so nothing more is sent or billed until you resume.</p>
        <div class="actions"><button class="primary" data-act="pause_streaming" data-confirmed="1">Pause streaming</button><button data-cancel>Keep streaming</button></div></div>`);
    }
    return out.join("");
  }

  const BODIES = [body1, body2, body3, body4, body5, body6];

  function render() {
    if (!st) return;
    $("#banners").innerHTML = banners(st);
    const states = stepStates(st);
    const current = states.findIndex((x) => x !== "done");
    states.forEach((state, i) => {
      const el = $(`#s${i + 1}`);
      const shown = i === current && state === "todo" ? "next" : state;
      const [cls, label] = BADGE[shown];
      const badge = $("[data-badge]", el);
      badge.className = `badge ${cls}`;
      badge.textContent = label;
      el.classList.toggle("current", i === current);
      const body = $("[data-body]", el);
      const html = BODIES[i](st);
      if (body.__html !== html) { body.innerHTML = html; body.__html = html; }
    });
    $("#foot").textContent = `htesla ${st.version || ""} · status ${ago(st.updated_at)}`;
  }

  /* ———— polling ———— */
  async function poll() {
    if (!h) return;
    try {
      const s = await h.kv("status");
      if (s && typeof s === "object") {
        if (st && JSON.stringify(s) !== JSON.stringify(st)) {
          // anything moved: let the buttons go
          for (const k of Object.keys(busy)) if (Date.now() - busy[k] > 2500) delete busy[k];
        }
        st = s;
        lastStatusAt = Date.now();
        render();
      } else if (!st) {
        $("#banners").innerHTML = `<p class="faint"><span class="spinner"></span>Waiting for the Tesla program to start — it writes its status here within a few seconds.</p>`;
      }
    } catch (e) {
      if (!st) $("#banners").innerHTML = `<div class="notice bad">Couldn't reach Hearthloom: ${esc(e.message)}</div>`;
    }
  }

  /* ———— actions ———— */
  async function signIn(btn) {
    const c = st.config;
    const note = $("#signin-note");
    btn.disabled = true;
    try {
      const state = rid();
      const p = new URLSearchParams({ response_type: "code", client_id: c.client_id, redirect_uri: c.redirect_uri, scope: c.scopes, state });
      const authorizeUrl = `https://auth.tesla.com/oauth2/v3/authorize?${p.toString()}`;
      await h.oauthPending(state, authorizeUrl);
      // ask the shell to open exactly the address the daemon now holds
      parent.postMessage({ type: "hearthloom-oauth" }, "*");
      await queue("signin_started");
    } catch (e) {
      if (note) note.textContent = `Couldn't start sign-in: ${e.message}`;
      btn.disabled = false;
    }
  }

  document.addEventListener("click", async (ev) => {
    const t = ev.target.closest("button");
    if (!t || !h) return;
    if (t.dataset.copy !== undefined) return copyText(t.dataset.copy, t);
    if (t.dataset.confirm) { confirmOpen = t.dataset.confirm; render(); return; }
    if (t.hasAttribute("data-cancel")) { confirmOpen = null; render(); return; }
    if (t.hasAttribute("data-signin")) return signIn(t);
    const act = t.dataset.act;
    if (act) {
      if ((act === "new_key" || act === "sign_out" || act === "pause_streaming") && !t.dataset.confirmed) return;
      confirmOpen = null;
      t.disabled = true;
      try { await queue(act); } catch (e) { t.disabled = false; t.textContent = `failed: ${e.message}`; }
    }
  });

  async function saveSecret() {
    const input = $("#secret"), note = $("#secret-note");
    const v = input.value.trim();
    if (!v) { note.textContent = "Paste the client secret first."; note.className = "small err"; return; }
    $("#secret-save").disabled = true;
    try {
      await h.secretPut("client_secret", v);
      input.value = "";
      note.textContent = "Saved — sealed on this node.";
      note.className = "small okc";
      await queue("refresh");
    } catch (e) {
      note.textContent = `Couldn't save: ${e.message}`;
      note.className = "small err";
    } finally {
      $("#secret-save").disabled = false;
    }
  }
  $("#secret-save").addEventListener("click", () => { if (h) saveSecret(); });
  $("#secret").addEventListener("keydown", (ev) => { if (ev.key === "Enter" && h) { ev.preventDefault(); saveSecret(); } });

  window.hearth.then((client) => {
    h = client;
    poll();
    heartbeat();
    setInterval(() => { if (!document.hidden || Date.now() - lastStatusAt > 30e3) poll(); }, 3000);
    setInterval(heartbeat, 30e3);
    setInterval(render, 30e3); // keep "x min ago" honest
    document.addEventListener("visibilitychange", () => { if (!document.hidden) { poll(); heartbeat(); } });
  });
})();
