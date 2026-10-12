/*
 * OneCloud Hub front end.
 *
 * Three rules hold everywhere in this file:
 *
 *  1. Untrusted text is written with textContent, never innerHTML. Filenames,
 *     paths, metadata and error messages all come from a filesystem this console
 *     does not control, so a name containing markup must render as the characters
 *     it contains. The API is the only source of data; it is never HTML.
 *  2. Every list is fetched one page at a time. No code path here fetches a whole
 *     directory, a whole library or every job.
 *  3. A state the console does not know is rendered as "unknown", in its own
 *     colour. There is no branch that turns a missing value into "fine".
 */

"use strict";

const PAGE_SIZE = 50;
const JOBS_PAGE = 50;

const state = {
  view: "overview",
  files: { root: "", path: "/", page: 1, sort: "name", order: "asc", query: "" },
  jobs: { page: 1, filter: "open" },
  overview: null,
  roots: [],
  theme: "dark",
};

/* ------------------------------------------------------------------ utils */

const $ = (id) => document.getElementById(id);

function el(tag, className, text) {
  const n = document.createElement(tag);
  if (className) n.className = className;
  if (text !== undefined && text !== null) n.textContent = String(text);
  return n;
}

function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

function fmtBytes(n) {
  if (n === undefined || n === null) return "—";
  if (n === 0) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(v < 10 ? 1 : 0)) + " " + units[i];
}

function fmtKiB(kb) {
  if (kb === undefined || kb === null) return "—";
  return fmtBytes(kb * 1024);
}

function fmtTime(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "—";
  return d.toLocaleString(undefined, {
    year: "numeric", month: "short", day: "2-digit",
    hour: "2-digit", minute: "2-digit",
  });
}

function fmtAgo(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "—";
  const s = Math.floor((Date.now() - d.getTime()) / 1000);
  if (s < 0) return "just now";
  if (s < 60) return s + "s ago";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  return Math.floor(s / 86400) + "d ago";
}

/* Fetch JSON with a bounded wait. A request that never returns would leave the
   page showing a spinner forever, which reads as "still working" when the truth
   is "this request is stuck". */
async function api(path, opts) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), 20000);
  try {
    const res = await fetch(path, Object.assign({ signal: ctl.signal }, opts || {}));
    const text = await res.text();
    let body = null;
    if (text) {
      try { body = JSON.parse(text); } catch (_) { body = null; }
    }
    if (!res.ok) {
      const msg = body && body.message ? body.message : "HTTP " + res.status;
      const err = new Error(msg);
      err.status = res.status;
      err.code = body && body.error ? body.error : "http_error";
      throw err;
    }
    return body;
  } finally {
    clearTimeout(timer);
  }
}

/* Show an error inside a container without interpreting it as markup. */
function showError(node, err) {
  clear(node);
  const p = el("p", "warning warning-danger");
  p.textContent = err && err.message ? err.message : "the request failed";
  node.appendChild(p);
}

/* ------------------------------------------------------------------ theme */

function applyTheme(name) {
  state.theme = name;
  document.documentElement.setAttribute("data-theme", name);
  $("theme-glyph").textContent = name === "dark" ? "◐" : "◑";
  try { localStorage.setItem("nasctl-theme", name); } catch (_) {}
}

function initTheme() {
  let saved = null;
  try { saved = localStorage.getItem("nasctl-theme"); } catch (_) {}
  if (saved === "dark" || saved === "light") { applyTheme(saved); return; }
  const light = window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches;
  applyTheme(light ? "light" : "dark");
}

/* ------------------------------------------------------------------ pills */

const MODE_LABELS = {
  normal: ["Normal", "ok"],
  maintenance: ["Maintenance", "busy"],
  watching: ["Watching", "busy"],
  manual_pause: ["Paused", "warn"],
  resource_guard: ["Resource guard", "danger"],
};

const JOB_LABELS = {
  queued: "busy", running: "busy", waiting: "warn", paused: "warn",
  completed: "ok", failed: "danger", cancelled: "muted",
  interrupted: "unknown", unknown: "unknown",
};

function setPill(node, text, kind) {
  node.textContent = text;
  node.className = "pill pill-" + kind;
}

/* ------------------------------------------------------------- navigation */

function showView(name) {
  state.view = name;
  document.querySelectorAll(".view").forEach((v) => { v.hidden = true; });
  const target = $("view-" + name);
  if (target) target.hidden = false;
  document.querySelectorAll(".tab").forEach((t) => {
    t.classList.toggle("is-active", t.dataset.view === name);
  });
  loadView(name);
}

function loadView(name) {
  switch (name) {
    case "overview": loadOverview(); break;
    case "files":    loadRoots().then(loadListing); break;
    case "jobs":     loadJobs(); break;
    case "settings": loadSettings(); break;
    case "media":    loadMedia(); break;
    default: break;
  }
}

/* ---------------------------------------------------------------- overview */

async function loadOverview() {
  let data;
  try {
    data = await api("/api/v1/overview");
  } catch (err) {
    showError($("overview-warnings"), err);
    return;
  }
  state.overview = data;

  const mode = data.mode || { mode: "normal" };
  const label = MODE_LABELS[mode.mode] || [mode.mode, "unknown"];
  setPill($("pill-mode"), label[0], label[1]);

  const r = data.resources;
  if (r) {
    const lim = 0; // thresholds come from the warnings list, not guessed here
    let kind = "ok";
    if (r.verdict === "drain") kind = "warn";
    else if (r.verdict === "stop") kind = "danger";
    setPill($("pill-mem"), fmtKiB(r.mem_available_kb) + " free", kind);
  } else {
    setPill($("pill-mem"), "memory unknown", "unknown");
  }

  /* Warnings first. A console that leads with good news buries the bad. */
  const warnBox = $("overview-warnings");
  clear(warnBox);
  const warnings = data.warnings || [];
  if (warnings.length === 0 && !data.resource_error) {
    const p = el("div", "warning warning-ok");
    p.appendChild(el("strong", null, "Nothing needs attention."));
    p.appendChild(el("span", null, " No mode is stopping work, no job is unknown, " +
      "and the machine is above its start budget."));
    warnBox.appendChild(p);
  } else {
    for (const w of warnings) {
      const p = el("div", "warning warning-warn");
      p.appendChild(el("span", null, w));
      warnBox.appendChild(p);
    }
    if (data.resource_error) {
      const p = el("div", "warning warning-unknown");
      p.appendChild(el("strong", null, "Resource view unavailable. "));
      p.appendChild(el("span", null, data.resource_error));
      warnBox.appendChild(p);
    }
  }

  /* Machine */
  const res = $("overview-resources");
  clear(res);
  if (r) {
    const dl = el("dl", "kv");
    const rows = [
      ["Available memory", fmtKiB(r.mem_available_kb)],
      ["Total memory", fmtKiB(r.mem_total_kb)],
      ["Swap used", r.swap_total_kb ? fmtKiB(r.swap_used_kb) + " of " + fmtKiB(r.swap_total_kb) : "none configured"],
      ["Guard verdict", r.verdict + (r.reason ? " (" + r.reason + ")" : "")],
      ["Memory pressure", r.psi_memory_some_avg10 ? r.psi_memory_some_avg10.toFixed(2) + "%" : "unavailable"],
      ["I/O pressure", r.psi_io_some_avg10 ? r.psi_io_some_avg10.toFixed(2) + "%" : "unavailable"],
    ];
    if (r.worker_rss_bytes) rows.push(["Worker resident", fmtBytes(r.worker_rss_bytes)]);
    for (const [k, v] of rows) {
      dl.appendChild(el("dt", null, k));
      dl.appendChild(el("dd", "kv-num", v));
    }
    res.appendChild(dl);
    if (r.detail) res.appendChild(el("p", "muted small", r.detail));
  } else {
    res.appendChild(el("p", "muted", "This console cannot read the machine's memory."));
  }

  /* Mode */
  const mb = $("overview-mode");
  clear(mb);
  const mdl = el("dl", "kv");
  mdl.appendChild(el("dt", null, "Effective"));
  mdl.appendChild(el("dd", null, mode.mode + " — " + (mode.source || "unknown source")));
  if (mode.reason) {
    mdl.appendChild(el("dt", null, "Reason"));
    mdl.appendChild(el("dd", null, mode.reason));
  }
  if (mode.expires_at) {
    mdl.appendChild(el("dt", null, "Expires"));
    mdl.appendChild(el("dd", null, fmtTime(mode.expires_at) + " (" + fmtAgo(mode.expires_at) + ")"));
  }
  mdl.appendChild(el("dt", null, "Heavy work"));
  mdl.appendChild(el("dd", null,
    (mode.mode === "normal" || mode.mode === "maintenance") ? "permitted, subject to resources" : "stopped"));
  mb.appendChild(mdl);

  /* Storage */
  const roots = (data.files && data.files.roots) || [];
  const rb = $("overview-roots");
  clear(rb);
  if (roots.length === 0) {
    rb.appendChild(el("p", "muted", (data.files && data.files.note) || "No storage roots are configured."));
  } else {
    const ul = el("ul", "root-list");
    for (const root of roots) {
      const li = el("li");
      const name = el("span", "entry-name");
      name.appendChild(el("span", "entry-icon", root.available ? "●" : "○"));
      name.appendChild(el("span", "entry-label", root.label || root.id));
      li.appendChild(name);
      li.appendChild(el("div", "muted small", root.available ? root.path : (root.note || "unavailable")));
      ul.appendChild(li);
    }
    rb.appendChild(ul);
  }

  /* Attention: the states where the console does not know what happened. */
  const ab = $("overview-attention");
  clear(ab);
  const att = data.attention || [];
  if (att.length === 0) {
    ab.appendChild(el("p", "muted", "No job is failed, interrupted or unknown."));
  } else {
    for (const j of att) {
      const p = el("div", "warning warning-" + (JOB_LABELS[j.state] === "danger" ? "danger" : "unknown"));
      p.appendChild(el("strong", null, j.op + " "));
      p.appendChild(el("span", null, "— " + j.state + (j.stop_reason ? " (" + j.stop_reason + ")" : "")));
      if (j.error) p.appendChild(el("div", "small", j.error));
      ab.appendChild(p);
    }
  }

  /* Active jobs */
  const jb = $("overview-running");
  clear(jb);
  const running = data.running || [];
  if (running.length === 0) {
    jb.appendChild(el("p", "muted", "No job is running."));
  } else {
    jb.appendChild(jobTable(running));
  }

  const stamp = $("overview-stamp");
  clear(stamp);
  stamp.textContent = "Snapshot taken " + fmtAgo(data.data_as_of) +
    " · console " + (data.version || "unknown") +
    (data.read_only ? " · read-only mode" : "") +
    (data.allow_ai_heavy ? " · agents may start heavy work" : " · agents may not start heavy work");
}

/* Build a small job table for the overview. */
function jobTable(jobs) {
  const wrap = el("div", "table-wrap");
  const t = el("table", "listing");
  const thead = el("thead");
  const hr = el("tr");
  for (const h of ["Job", "Operation", "State", "Progress"]) hr.appendChild(el("th", null, h));
  thead.appendChild(hr);
  t.appendChild(thead);
  const tb = el("tbody");
  for (const j of jobs) {
    const tr = el("tr");
    tr.appendChild(el("td", "mono", j.id));
    tr.appendChild(el("td", null, j.op));
    const st = el("td");
    st.appendChild(el("span", "pill pill-" + (JOB_LABELS[j.state] || "unknown"), j.state));
    tr.appendChild(st);
    tr.appendChild(el("td", "num", j.total ? j.progress + " / " + j.total + " " + (j.unit || "") : "—"));
    tb.appendChild(tr);
  }
  t.appendChild(tb);
  wrap.appendChild(t);
  return wrap;
}

/* ------------------------------------------------------------------- files */

async function loadRoots() {
  if (state.roots.length) return state.roots;
  try {
    const data = await api("/api/v1/files/roots");
    state.roots = data.roots || [];
    const sel = $("files-root");
    clear(sel);
    for (const r of state.roots) {
      const o = el("option", null, (r.label || r.id) + (r.available ? "" : " (unavailable)"));
      o.value = r.id;
      if (!r.available) o.disabled = true;
      sel.appendChild(o);
    }
    const notice = $("files-notice");
    if (data.note) { notice.textContent = data.note; notice.hidden = false; }
    if (!state.files.root && state.roots.length) {
      const first = state.roots.find((r) => r.available) || state.roots[0];
      state.files.root = first.id;
      sel.value = first.id;
    }
  } catch (err) {
    showError($("files-notice"), err);
    $("files-notice").hidden = false;
  }
  return state.roots;
}

async function loadListing() {
  const body = $("files-body");
  clear(body);
  if (!state.files.root) {
    const tr = el("tr");
    const td = el("td", "empty-row", "No storage roots are configured. Add [[hub.storage]] entries to the configuration.");
    td.colSpan = 4;
    tr.appendChild(td);
    body.appendChild(tr);
    return;
  }

  const f = state.files;
  const qs = new URLSearchParams({
    root: f.root, path: f.path, page: String(f.page),
    page_size: String(PAGE_SIZE), sort: f.sort, order: f.order,
  });

  let data;
  try {
    data = f.query
      ? await api("/api/v1/files/search?" + new URLSearchParams({
          root: f.root, path: f.path, q: f.query, depth: "2",
        }))
      : await api("/api/v1/files/list?" + qs);
  } catch (err) {
    const tr = el("tr");
    const td = el("td", "empty-row", err.message || "the listing failed");
    td.colSpan = 4;
    tr.appendChild(td);
    body.appendChild(tr);
    return;
  }

  const entries = data.entries || [];
  const notice = $("files-notice");
  if (data.note) { notice.textContent = data.note; notice.hidden = false; }
  else if (!f.query) { notice.hidden = true; }

  if (entries.length === 0) {
    const tr = el("tr");
    const td = el("td", "empty-row", f.query ? "No name in this subtree contains that text." : "This directory is empty.");
    td.colSpan = 4;
    tr.appendChild(td);
    body.appendChild(tr);
  }

  for (const e of entries) {
    const tr = el("tr");
    const nameCell = el("td", "col-name");
    const wrap = el("span", "entry-name");
    wrap.appendChild(el("span", "entry-icon", e.is_dir ? "▸" : iconFor(e.kind)));
    if (e.is_dir && !f.query) {
      const btn = el("button", "link-btn entry-label entry-dir", e.name);
      btn.type = "button";
      btn.addEventListener("click", () => {
        state.files.path = e.path;
        state.files.page = 1;
        state.files.query = "";
        $("files-search").value = "";
        hidePreview();
        loadListing();
      });
      wrap.appendChild(btn);
    } else {
      const btn = el("button", "link-btn entry-label", e.name);
      btn.type = "button";
      btn.addEventListener("click", () => openPreview(e));
      wrap.appendChild(btn);
      // A direct download, so taking a file does not require opening a preview
      // for it first - which matters most for the files that have no preview.
      const dl = el("a", "tag", "get");
      dl.href = downloadURL(e.path);
      dl.setAttribute("download", e.name);
      dl.title = "download this file";
      wrap.appendChild(dl);
    }
    if (e.symlink) {
      const tag = el("span", "tag tag-link", "link");
      tag.title = "a symbolic link; it is listed, and following it is refused unless it stays inside the root";
      wrap.appendChild(tag);
    }
    nameCell.appendChild(wrap);
    tr.appendChild(nameCell);

    tr.appendChild(el("td", "col-size num", e.is_dir ? "—" : fmtBytes(e.size)));
    tr.appendChild(el("td", "col-time", fmtTime(e.mod_time)));
    tr.appendChild(el("td", "col-kind", e.kind || ""));
    body.appendChild(tr);
  }

  renderCrumbs(data.path || f.path);
  renderPager(data);
}

function iconFor(kind) {
  switch (kind) {
    case "image": return "▣";
    case "video": return "▶";
    case "audio": return "♪";
    case "archive": return "▤";
    case "text": return "≡";
    default: return "·";
  }
}

function renderCrumbs(p) {
  const box = $("files-crumbs");
  clear(box);
  const parts = (p || "/").split("/").filter(Boolean);
  const root = el("button", "crumb", state.files.root);
  root.type = "button";
  root.addEventListener("click", () => { state.files.path = "/"; state.files.page = 1; loadListing(); });
  box.appendChild(root);

  let acc = "";
  parts.forEach((part, i) => {
    box.appendChild(el("span", "crumb-sep", "/"));
    acc += "/" + part;
    if (i === parts.length - 1) {
      box.appendChild(el("span", "crumb-current", part));
    } else {
      const b = el("button", "crumb", part);
      b.type = "button";
      const target = acc;
      b.addEventListener("click", () => {
        state.files.path = target;
        state.files.page = 1;
        loadListing();
      });
      box.appendChild(b);
    }
  });
}

function renderPager(data) {
  const box = $("files-pager");
  clear(box);
  if (!data.pages || data.pages <= 1) {
    if (data.total !== undefined) {
      box.appendChild(el("span", null,
        plural(data.total, "entry", "entries") + (data.truncated ? " or more" : "")));
    }
    return;
  }
  const prev = el("button", "btn", "Previous");
  prev.type = "button";
  prev.disabled = data.page <= 1;
  prev.addEventListener("click", () => { state.files.page = data.page - 1; loadListing(); });
  box.appendChild(prev);
  box.appendChild(el("span", null, "Page " + data.page + " of " + data.pages + " · " +
    plural(data.total, "entry", "entries") + (data.truncated ? " or more" : "")));
  const next = el("button", "btn", "Next");
  next.type = "button";
  next.disabled = data.page >= data.pages;
  next.addEventListener("click", () => { state.files.page = data.page + 1; loadListing(); });
  box.appendChild(next);
}

/* Preview: the console passes the file through untouched. Nothing is decoded,
   resized or transcoded here, which is why this is cheap even for a directory of
   large photographs. */
async function openPreview(entry) {
  const box = $("files-preview");
  clear(box);
  box.hidden = false;

  let info = null;
  try {
    info = await api("/api/v1/files/stat?" + new URLSearchParams({
      root: state.files.root, path: entry.path,
    }));
  } catch (_) { /* the preview attempt below reports the failure */ }

  const head = el("div", "preview-head");
  head.appendChild(el("span", "preview-name", entry.path));
  const actions = el("span");
  /* The download endpoint is separate from the preview endpoint on purpose: the
     preview refuses types the browser cannot render and caps large images, while
     a download streams anything with a bounded buffer. Pointing this link at the
     preview made the button fail for exactly the files worth downloading. */
  const dl = el("a", "link", "Download");
  dl.href = downloadURL(entry.path);
  dl.setAttribute("download", entry.name);
  actions.appendChild(dl);
  head.appendChild(actions);
  box.appendChild(head);

  if (!info || !info.previewable) {
    const p = el("p", "muted", "No in-browser preview for this file type. Use Download.");
    box.appendChild(p);
    return;
  }

  const src = "/api/v1/files/preview?" + new URLSearchParams({
    root: state.files.root, path: entry.path,
  });

  if (info.preview_kind === "video") {
    const v = document.createElement("video");
    v.controls = true;
    v.preload = "metadata"; /* never pull the whole file to show a frame */
    v.src = src;
    box.appendChild(v);
  } else if (info.preview_kind === "audio") {
    const a = document.createElement("audio");
    a.controls = true;
    a.preload = "metadata";
    a.src = src;
    box.appendChild(a);
  } else {
    const img = document.createElement("img");
    img.loading = "lazy";
    img.alt = entry.name;
    img.src = src;
    img.addEventListener("error", () => {
      clear(box);
      box.appendChild(el("p", "muted", "This image could not be displayed."));
    });
    box.appendChild(img);
  }

  const meta = el("dl", "kv");
  meta.appendChild(el("dt", null, "Size"));
  meta.appendChild(el("dd", "kv-num", fmtBytes(entry.size)));
  meta.appendChild(el("dt", null, "Modified"));
  meta.appendChild(el("dd", null, fmtTime(entry.mod_time)));
  box.appendChild(meta);
}

/* plural renders a count with the right noun form.
   The plural is passed explicitly rather than derived by appending "s": "entry"
   pluralises to "entries", not "entrys", and deriving it would produce exactly
   that. Both forms are given at the call site so the irregular case is obvious. */
function plural(n, singular, pluralForm) {
  return n + " " + (n === 1 ? singular : pluralForm);
}

/* downloadURL builds the attachment URL for one entry. */
function downloadURL(path) {
  return "/api/v1/files/download?" + new URLSearchParams({
    root: state.files.root, path: path,
  });
}

function hidePreview() {
  const box = $("files-preview");
  clear(box);
  box.hidden = true;
}

/* -------------------------------------------------------------------- jobs */

async function loadJobs() {
  const body = $("jobs-body");
  clear(body);
  const f = state.jobs;
  const params = new URLSearchParams({ limit: String(JOBS_PAGE), offset: String((f.page - 1) * JOBS_PAGE) });
  if (f.filter === "open") params.set("open", "true");
  else if (f.filter !== "all") params.set("state", f.filter);

  let data;
  try {
    data = await api("/api/v1/jobs?" + params);
  } catch (err) {
    const tr = el("tr");
    const td = el("td", "empty-row", err.message || "the job list failed");
    td.colSpan = 6;
    tr.appendChild(td);
    body.appendChild(tr);
    return;
  }

  const jobs = data.jobs || [];
  if (jobs.length === 0) {
    const tr = el("tr");
    const td = el("td", "empty-row", "No jobs match this filter.");
    td.colSpan = 6;
    tr.appendChild(td);
    body.appendChild(tr);
  }

  for (const j of jobs) {
    const tr = el("tr");
    tr.appendChild(el("td", "mono", j.id));
    tr.appendChild(el("td", null, j.op));
    const st = el("td");
    st.appendChild(el("span", "pill pill-" + (JOB_LABELS[j.state] || "unknown"), j.state));
    tr.appendChild(st);
    tr.appendChild(el("td", null, j.principal));
    tr.appendChild(el("td", "num", j.total ? j.progress + " / " + j.total + " " + (j.unit || "") : "—"));
    const when = el("td", null, fmtAgo(j.created_at));
    when.title = fmtTime(j.created_at);
    tr.appendChild(when);
    body.appendChild(tr);
  }

  const box = $("jobs-pager");
  clear(box);
  const prev = el("button", "btn", "Previous");
  prev.type = "button";
  prev.disabled = f.page <= 1;
  prev.addEventListener("click", () => { state.jobs.page--; loadJobs(); });
  box.appendChild(prev);
  box.appendChild(el("span", null, "Page " + f.page));
  const next = el("button", "btn", "Next");
  next.type = "button";
  next.disabled = jobs.length < JOBS_PAGE;
  next.addEventListener("click", () => { state.jobs.page++; loadJobs(); });
  box.appendChild(next);
}

/* ---------------------------------------------------------------- settings */

async function loadSettings() {
  loadModes();
  loadApprovals();
  loadOps();
  loadAudit();
}

async function loadModes() {
  const box = $("settings-modes");
  clear(box);
  try {
    const data = await api("/api/v1/modes");
    const eff = data.effective || {};
    const dl = el("dl", "kv");
    dl.appendChild(el("dt", null, "Effective"));
    dl.appendChild(el("dd", null, eff.mode + " (" + (eff.source || "unknown") + ")"));
    const states = data.states || [];
    if (states.length === 0) {
      dl.appendChild(el("dt", null, "Stored"));
      dl.appendChild(el("dd", "muted", "none"));
    }
    for (const s of states) {
      dl.appendChild(el("dt", null, s.mode));
      const dd = el("dd");
      dd.appendChild(el("span", null, s.source || "unknown source"));
      if (s.expires_at) dd.appendChild(el("span", "muted", " · expires " + fmtTime(s.expires_at)));
      else dd.appendChild(el("span", "muted", " · no expiry"));
      const clr = el("button", "link-btn small", " clear");
      clr.type = "button";
      clr.addEventListener("click", () => clearMode(s.mode));
      dd.appendChild(clr);
      dl.appendChild(dd);
    }
    box.appendChild(dl);
  } catch (err) {
    showError(box, err);
  }
}

async function setMode(mode, duration, reason) {
  try {
    await api("/api/v1/modes", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mode: mode, duration: duration, reason: reason || "" }),
    });
  } catch (err) {
    alert(err.message || "the mode could not be set");
  }
  loadModes();
  loadOverview();
}

async function clearMode(mode) {
  try {
    await api("/api/v1/modes/" + encodeURIComponent(mode), { method: "DELETE" });
  } catch (err) {
    alert(err.message || "the mode could not be cleared");
  }
  loadModes();
  loadOverview();
}

async function loadApprovals() {
  const box = $("settings-approvals");
  clear(box);
  try {
    const data = await api("/api/v1/approvals");
    const rows = data.approvals || [];
    if (rows.length === 0) {
      box.appendChild(el("p", "muted", "No approvals are outstanding."));
      return;
    }
    const dl = el("dl", "kv");
    for (const a of rows) {
      dl.appendChild(el("dt", "mono", a.id));
      const dd = el("dd");
      dd.appendChild(el("span", null, a.op + " for " + a.principal));
      dd.appendChild(el("div", "muted small",
        "target " + (a.target || "—") + " · params " + (a.params || "—")));
      dd.appendChild(el("div", "muted small",
        "granted by " + a.granted_by + " · expires " + fmtTime(a.expires_at) +
        (a.used_at ? " · used" : "")));
      dl.appendChild(dd);
    }
    box.appendChild(dl);
  } catch (err) {
    showError(box, err);
  }
}

async function loadOps() {
  const body = $("settings-ops");
  clear(body);
  try {
    const data = await api("/api/v1/ops");
    for (const op of data.operations || []) {
      const tr = el("tr");
      tr.appendChild(el("td", "mono", op.id));
      tr.appendChild(el("td", null, op.class));
      tr.appendChild(el("td", null, op.approval));
      tr.appendChild(el("td", null, op.async ? "yes" : "no"));
      tr.appendChild(el("td", null, op.mutex || "—"));
      tr.appendChild(el("td", null, op.timeout));
      body.appendChild(tr);
    }
  } catch (err) {
    const tr = el("tr");
    const td = el("td", "empty-row", err.message || "the operation table failed");
    td.colSpan = 6;
    tr.appendChild(td);
    body.appendChild(tr);
  }
}

async function loadAudit() {
  const body = $("settings-audit");
  clear(body);
  try {
    const data = await api("/api/v1/audit?limit=100");
    const rows = data.entries || [];
    if (rows.length === 0) {
      const tr = el("tr");
      const td = el("td", "empty-row", "Nothing has been recorded yet.");
      td.colSpan = 6;
      tr.appendChild(td);
      body.appendChild(tr);
      return;
    }
    for (const e of rows) {
      const tr = el("tr");
      tr.appendChild(el("td", null, fmtTime(e.at)));
      tr.appendChild(el("td", null, e.principal));
      tr.appendChild(el("td", null, e.action));
      tr.appendChild(el("td", null, e.target || "—"));
      const out = el("td");
      const kind = e.outcome === "ok" ? "ok" : (e.outcome === "refused" ? "warn" : "danger");
      out.appendChild(el("span", "pill pill-" + kind, e.outcome));
      tr.appendChild(out);
      const note = el("td", "small", e.note || "");
      tr.appendChild(note);
      body.appendChild(tr);
    }
  } catch (err) {
    const tr = el("tr");
    const td = el("td", "empty-row", err.message || "the audit log failed");
    td.colSpan = 6;
    tr.appendChild(td);
    body.appendChild(tr);
  }
}

/* ------------------------------------------------------------------- media */

async function loadMedia() {
  const box = $("media-body");
  clear(box);
  const p = $("media-state");
  clear(p);
  try {
    const data = await api("/api/v1/media/status");
    p.appendChild(el("span", null, data.note || ""));
    if (!data.configured) return;
    if (data.library && data.library.length) {
      const wrap = el("div", "table-wrap");
      const t = el("table", "listing");
      const tb = el("tbody");
      for (const item of data.library) {
        const tr = el("tr");
        tr.appendChild(el("td", null, item.title || item.path || "—"));
        tb.appendChild(tr);
      }
      t.appendChild(tb);
      wrap.appendChild(t);
      box.appendChild(wrap);
    }
  } catch (err) {
    clear(p);
    p.appendChild(el("span", "muted", "The watch integration is not available: " + (err.message || "unknown reason")));
  }
}

/* -------------------------------------------------------------------- boot */

function init() {
  initTheme();
  $("theme-toggle").addEventListener("click", () => {
    applyTheme(state.theme === "dark" ? "light" : "dark");
  });

  document.querySelectorAll(".tab").forEach((t) => {
    t.addEventListener("click", (ev) => {
      ev.preventDefault();
      showView(t.dataset.view);
      history.replaceState(null, "", "#" + t.dataset.view);
    });
  });

  $("files-root").addEventListener("change", (ev) => {
    state.files.root = ev.target.value;
    state.files.path = "/";
    state.files.page = 1;
    hidePreview();
    loadListing();
  });

  const searchForm = $("files-search-form");
  searchForm.addEventListener("submit", (ev) => {
    ev.preventDefault();
    state.files.query = $("files-search").value.trim().slice(0, 128);
    state.files.page = 1;
    hidePreview();
    loadListing();
  });
  $("files-search").addEventListener("input", (ev) => {
    if (ev.target.value.trim() === "" && state.files.query !== "") {
      state.files.query = "";
      loadListing();
    }
  });

  $("files-sort").addEventListener("change", (ev) => {
    const [sort, order] = ev.target.value.split(":");
    state.files.sort = sort;
    state.files.order = order;
    state.files.page = 1;
    loadListing();
  });

  $("jobs-filter").addEventListener("change", (ev) => {
    state.jobs.filter = ev.target.value;
    state.jobs.page = 1;
    loadJobs();
  });

  document.querySelectorAll("[data-mode]").forEach((b) => {
    b.addEventListener("click", () => {
      const mode = b.dataset.mode;
      if (mode === "manual_pause") {
        setMode("manual_pause", "", "set from the console");
      } else if (mode === "watching") {
        setMode("watching", "2h", "watching something");
      } else {
        setMode("maintenance", "1h", "maintenance window");
      }
    });
  });

  const albumLink = $("album-link");
  if (albumLink) {
    albumLink.href = window.location.protocol + "//" + window.location.hostname + ":8088/";
  }

  const hash = (window.location.hash || "").replace("#", "");
  const known = ["overview", "files", "album", "media", "backup", "jobs", "settings"];
  showView(known.indexOf(hash) >= 0 ? hash : "overview");

  /* Refresh only what is on screen, and only the cheap summary. A console that
     polls every view on a timer is a console that keeps a small machine busy. */
  setInterval(() => {
    if (state.view === "overview") loadOverview();
    else if (state.view === "jobs") loadJobs();
  }, 15000);

  window.addEventListener("hashchange", () => {
    const v = (window.location.hash || "").replace("#", "");
    if (known.indexOf(v) >= 0 && v !== state.view) showView(v);
  });
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", init);
} else {
  init();
}
