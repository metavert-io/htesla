/* The Drives page: routes as SVG polylines (from drives/*.json), a
   drives-per-day calendar, and efficiency vs outside temperature. It
   reads only the ember's own KV and files — never Tesla, never tiles. */
(() => {
  "use strict";
  const $ = (s) => document.querySelector(s);
  const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const MAX_ROUTES = 60;
  const mi = (m) => m / 1609.344;
  // the drive's own local time (it carries its UTC offset)
  const local = (iso, off) => new Date(Date.parse(iso) + (off || 0) * 60e3);
  const dayKey = (d) => d.toISOString().slice(0, 10);
  const fmtWhen = (d) => d.toLocaleString([], { timeZone: "UTC", weekday: "short", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });

  let drives = [], routes = new Map(), selected = null;

  /* ———— routes ———— */
  function renderMap() {
    const box = $("#map");
    const list = drives.slice(-MAX_ROUTES).filter((d) => routes.get(d.key)?.length > 1);
    if (!list.length) {
      box.innerHTML = `<p class="empty">No routes yet — they appear after your car streams a drive.</p>`;
      return;
    }
    let minLa = 90, maxLa = -90, minLo = 180, maxLo = -180;
    for (const d of list) for (const [la, lo] of routes.get(d.key)) {
      minLa = Math.min(minLa, la); maxLa = Math.max(maxLa, la);
      minLo = Math.min(minLo, lo); maxLo = Math.max(maxLo, lo);
    }
    const kx = Math.cos(((minLa + maxLa) / 2) * Math.PI / 180);
    // draw at the box's own width so labels keep their size on a phone
    const W = Math.max(280, Math.min(1000, Math.round(box.clientWidth || 800))), pad = 24;
    const spanX = Math.max((maxLo - minLo) * kx, 1e-4), spanY = Math.max(maxLa - minLa, 1e-4);
    const H = Math.round(Math.min(520, Math.max(240, (W - 2 * pad) * spanY / spanX + 2 * pad)));
    const s = Math.min((W - 2 * pad) / spanX, (H - 2 * pad) / spanY);
    const ox = (W - spanX * s) / 2, oy = (H - spanY * s) / 2;
    const X = (lo) => ox + (lo - minLo) * kx * s, Y = (la) => H - (oy + (la - minLa) * s);
    let svg = `<svg viewBox="0 0 ${W} ${H}" style="max-width:${W}px" role="img" aria-label="Routes of your recent drives">`;
    const ends = new Map();
    for (const d of list) {
      const pts = routes.get(d.key);
      const p = pts.map(([la, lo]) => `${X(lo).toFixed(1)},${Y(la).toFixed(1)}`).join(" ");
      svg += `<polyline class="route${d.key === selected ? " sel" : ""}" data-key="${esc(d.key)}" points="${p}"><title>${esc(fmtWhen(local(d.start, d.tz_offset_minutes)))} · ${mi(d.distance_m).toFixed(1)} mi</title></polyline>`;
      for (const [pt, place] of [[pts[0], d.start_place], [pts[pts.length - 1], d.end_place]]) {
        const k = `${pt[0].toFixed(3)},${pt[1].toFixed(3)}`;
        if (!ends.has(k) || place) ends.set(k, { pt, place: place || ends.get(k)?.place });
      }
    }
    // draw the selected route on top
    if (selected && routes.get(selected)) {
      const pts = routes.get(selected);
      svg += `<polyline class="route sel" points="${pts.map(([la, lo]) => `${X(lo).toFixed(1)},${Y(la).toFixed(1)}`).join(" ")}"/>`;
    }
    for (const { pt, place } of ends.values()) {
      svg += `<circle class="end" cx="${X(pt[1]).toFixed(1)}" cy="${Y(pt[0]).toFixed(1)}" r="3.5"/>`;
      if (place) {
        const right = X(pt[1]) > W * 0.8; // near the right edge: label on the left
        svg += `<text class="lbl" x="${(X(pt[1]) + (right ? -7 : 7)).toFixed(1)}" y="${(Y(pt[0]) + 4).toFixed(1)}" text-anchor="${right ? "end" : "start"}">${esc(place)}</text>`;
      }
    }
    svg += `</svg>`;
    box.innerHTML = svg;
    const km = ((maxLo - minLo) * kx * 111.32).toFixed(1);
    $("#map-note").textContent = `${list.length} route${list.length === 1 ? "" : "s"} · about ${km} km across · north is up`;
    box.querySelectorAll("polyline[data-key]").forEach((el) => el.addEventListener("click", () => select(el.dataset.key)));
  }

  /* ———— drives per day ———— */
  function renderHeat() {
    const box = $("#heat");
    if (!drives.length) { box.innerHTML = `<p class="empty">Nothing yet.</p>`; return; }
    const counts = new Map();
    for (const d of drives) {
      const k = dayKey(local(d.start, d.tz_offset_minutes));
      counts.set(k, (counts.get(k) || 0) + 1);
    }
    const avail = box.clientWidth || 420;
    const cell = 13, gap = 3, left = 26, top = 16;
    const weeks = Math.max(8, Math.min(26, Math.floor((avail - left) / (cell + gap))));
    const today = new Date(); today.setUTCHours(0, 0, 0, 0);
    // columns are Sunday-started weeks; the last column is this week
    const start = new Date(today); start.setUTCDate(start.getUTCDate() - today.getUTCDay() - (weeks - 1) * 7);
    const max = Math.max(...counts.values(), 1);
    const W = left + weeks * (cell + gap), H = top + 7 * (cell + gap);
    let svg = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="Drives per day, last ${weeks} weeks">`;
    ["M", "W", "F"].forEach((l, i) => { svg += `<text class="tick" x="0" y="${top + (1 + 2 * i) * (cell + gap) + cell - 2}">${l}</text>`; });
    let lastMonth = -1, lastLabelW = -9;
    for (let w = 0; w < weeks; w++) {
      for (let dow = 0; dow < 7; dow++) {
        const d = new Date(start); d.setUTCDate(start.getUTCDate() + w * 7 + dow);
        if (d > today) continue;
        const n = counts.get(dayKey(d)) || 0;
        const x = left + w * (cell + gap), y = top + dow * (cell + gap);
        if (dow === 0 && d.getUTCMonth() !== lastMonth) {
          lastMonth = d.getUTCMonth();
          if (w - lastLabelW >= 3) { // keep month labels from colliding
            svg += `<text class="tick" x="${x}" y="10">${d.toLocaleString([], { month: "short", timeZone: "UTC" })}</text>`;
            lastLabelW = w;
          }
        }
        svg += n
          ? `<rect class="cell" x="${x}" y="${y}" width="${cell}" height="${cell}" rx="2" opacity="${(0.3 + 0.7 * n / max).toFixed(2)}"><title>${dayKey(d)}: ${n} drive${n > 1 ? "s" : ""}</title></rect>`
          : `<rect class="cell0" x="${x}" y="${y}" width="${cell}" height="${cell}" rx="2"><title>${dayKey(d)}: no drives</title></rect>`;
      }
    }
    box.innerHTML = svg + `</svg>`;
  }

  /* ———— efficiency vs temperature ———— */
  function niceTicks(lo, hi, n) {
    const span = Math.max(hi - lo, 1e-9);
    const step0 = span / n, mag = Math.pow(10, Math.floor(Math.log10(step0)));
    const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((m) => m >= step0) || step0;
    // from the step at or below lo to the step at or above hi
    const out = [];
    const first = Math.floor(lo / step), last = Math.ceil(hi / step);
    for (let i = first; i <= last; i++) out.push(Math.round(i * step * 1e6) / 1e6);
    return out;
  }
  function renderScatter() {
    const box = $("#scatter");
    const pts = drives.filter((d) => d.wh_per_km != null && d.outside_temp_c != null && d.distance_m >= 1000);
    if (!pts.length) { box.innerHTML = `<p class="empty">Needs drives over 1 km with energy and temperature.</p>`; return; }
    const W = Math.max(280, Math.min(560, Math.round(box.clientWidth || 420))), H = 260, m = { l: 44, r: 10, t: 10, b: 34 };
    const xs = pts.map((d) => d.outside_temp_c), ys = pts.map((d) => d.wh_per_km);
    const xt = niceTicks(Math.min(...xs, 0) - 2, Math.max(...xs, 0) + 2, 5);
    const yt = niceTicks(0, Math.max(...ys) * 1.1, 4);
    const x0 = xt[0], x1 = xt[xt.length - 1], y1 = yt[yt.length - 1];
    const X = (v) => m.l + (v - x0) / (x1 - x0) * (W - m.l - m.r);
    const Y = (v) => H - m.b - v / y1 * (H - m.t - m.b);
    let svg = `<svg viewBox="0 0 ${W} ${H}" role="img" aria-label="Energy per kilometre against outside temperature">`;
    for (const t of yt) svg += `<line class="grid-l" x1="${m.l}" x2="${W - m.r}" y1="${Y(t)}" y2="${Y(t)}"/><text class="tick" x="${m.l - 6}" y="${Y(t) + 3}" text-anchor="end">${t}</text>`;
    for (const t of xt) svg += `<text class="tick" x="${X(t)}" y="${H - m.b + 14}" text-anchor="middle">${t}°</text>`;
    if (x0 < 0 && x1 > 0) svg += `<line class="axis" x1="${X(0)}" x2="${X(0)}" y1="${m.t}" y2="${H - m.b}" stroke-dasharray="3 3"/>`;
    svg += `<line class="axis" x1="${m.l}" x2="${W - m.r}" y1="${H - m.b}" y2="${H - m.b}"/>`;
    svg += `<text class="tick" x="${(m.l + W - m.r) / 2}" y="${H - 4}" text-anchor="middle">outside temperature (°C)</text>`;
    svg += `<text class="tick" transform="rotate(-90)" x="${-(m.t + H - m.b) / 2}" y="11" text-anchor="middle">Wh/km</text>`;
    for (const d of pts) {
      svg += `<circle class="dot" cx="${X(d.outside_temp_c).toFixed(1)}" cy="${Y(d.wh_per_km).toFixed(1)}" r="${d.key === selected ? 6 : 4}"><title>${esc(fmtWhen(local(d.start, d.tz_offset_minutes)))}: ${d.wh_per_km} Wh/km at ${d.outside_temp_c}°C</title></circle>`;
    }
    box.innerHTML = svg + `</svg>`;
  }

  /* ———— list ———— */
  function renderList() {
    const box = $("#list");
    if (!drives.length) { box.innerHTML = `<p class="empty">No drives yet.</p>`; return; }
    const rows = drives.slice(-40).reverse().map((d) => {
      const when = local(d.start, d.tz_offset_minutes);
      const mins = Math.round((Date.parse(d.end) - Date.parse(d.start)) / 60e3);
      const from = d.start_place || "—", to = d.end_place || "—";
      return `<tr class="drive${d.key === selected ? " sel" : ""}" data-key="${esc(d.key)}">
        <td>${esc(fmtWhen(when))}${d.partial ? ` <span class="faint">(partial)</span>` : ""}</td>
        <td class="hide-s">${esc(from)} → ${esc(to)}</td>
        <td class="num">${mi(d.distance_m).toFixed(1)} mi</td>
        <td class="num hide-s">${mins} min</td>
        <td class="num">${d.energy_kwh != null ? d.energy_kwh.toFixed(1) + " kWh" : "—"}</td>
        <td class="num hide-s">${d.wh_per_km != null ? d.wh_per_km + " Wh/km" : "—"}</td></tr>`;
    }).join("");
    box.innerHTML = `<table><thead><tr><th>when</th><th class="hide-s">from → to</th><th style="text-align:right">distance</th>
      <th class="hide-s" style="text-align:right">time</th><th style="text-align:right">energy</th><th class="hide-s" style="text-align:right">efficiency</th></tr></thead><tbody>${rows}</tbody></table>`;
    box.querySelectorAll("tr.drive").forEach((tr) => tr.addEventListener("click", () => select(tr.dataset.key)));
  }

  function renderStats() {
    const n = drives.length;
    const m = drives.reduce((a, d) => a + d.distance_m, 0);
    const kwh = drives.reduce((a, d) => a + (d.energy_kwh || 0), 0);
    const eff = m > 0 && kwh > 0 ? Math.round(kwh * 1000 / (m / 1000)) : null;
    $("#stats").innerHTML = n ? `
      <div class="stat"><b>${n}</b><span>drives</span></div>
      <div class="stat"><b>${mi(m).toFixed(0)} mi</b><span>${(m / 1000).toFixed(0)} km</span></div>
      <div class="stat"><b>${kwh.toFixed(1)} kWh</b><span>used driving</span></div>
      ${eff ? `<div class="stat"><b>${eff} Wh/km</b><span>average</span></div>` : ""}` : "";
  }

  function select(key) {
    selected = selected === key ? null : key;
    renderMap(); renderScatter(); renderList();
  }

  window.hearth.then(async (h) => {
    try {
      const idx = await h.kv("drives");
      drives = (idx && Array.isArray(idx.drives) ? idx.drives : []).sort((a, b) => a.start.localeCompare(b.start));
    } catch (e) {
      $("#map").innerHTML = `<p class="empty">Couldn't read your drives: ${esc(e.message)}</p>`;
      return;
    }
    renderStats(); renderHeat(); renderScatter(); renderList();
    let resizeT = 0, lastW = innerWidth;
    addEventListener("resize", () => {
      if (innerWidth === lastW) return;
      lastW = innerWidth;
      clearTimeout(resizeT);
      resizeT = setTimeout(() => { renderMap(); renderHeat(); renderScatter(); }, 150);
    });
    const recent = drives.slice(-MAX_ROUTES);
    await Promise.all(recent.map(async (d) => {
      try {
        const doc = await h.file(d.route_file);
        if (doc && Array.isArray(doc.points)) routes.set(d.key, doc.points);
      } catch { /* a missing route just isn't drawn */ }
    }));
    renderMap();
  });
})();
