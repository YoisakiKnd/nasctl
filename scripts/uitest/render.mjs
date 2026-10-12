/*
 * Renders the console's page in a real DOM against a running server.
 *
 * This exists because static checks are not enough for a user interface. `node
 * --check` proves the script parses; it does not prove the page populates, that
 * every view resolves its panel, or that a control does what its label says. A
 * browser is not available in the build environment, so jsdom stands in for one:
 * it executes app.js for real, lets it call the API, and then inspects the
 * resulting DOM.
 *
 * It is NOT a substitute for looking at the page in a browser. Layout, CSS,
 * responsive breakpoints and anything visual are outside what this can see, and
 * the report says so.
 *
 * Usage:
 *   node scripts/uitest/render.mjs http://127.0.0.1:8090 [token]
 *
 * Exits non-zero when a view fails to populate or the page logs an error.
 */
/*
 * jsdom is resolved at run time rather than by a static import.
 *
 * The reason is practical: some build environments strip `node_modules` from the
 * checkout, so a plain `import ... from "jsdom"` fails even after a successful
 * `npm install`. Resolution therefore tries, in order:
 *
 *   1. a normal import (a local node_modules, the usual case);
 *   2. $JSDOM_PATH, which may name either the package directory or a directory
 *      containing node_modules.
 *
 * No machine-specific path is baked in: a hardcoded fallback would work on the
 * machine it was written on and fail everywhere else, which is worse than a clear
 * error message.
 *
 * This is a development-machine harness; none of it runs on the NAS.
 */
const loadJsdom = async () => {
  const candidates = ["jsdom"];
  if (process.env.JSDOM_PATH) {
    const p = process.env.JSDOM_PATH;
    candidates.push(p, p.replace(/\/+$/, "") + "/jsdom");
  }
  const tried = [];
  for (const c of candidates) {
    try {
      const spec = c.startsWith("/") ? "file://" + c + "/lib/api.js" : c;
      const mod = await import(spec);
      return mod.default && mod.default.JSDOM ? mod.default : mod;
    } catch (e) {
      tried.push(`${c} (${e.code || e.message})`);
    }
  }
  console.error("cannot load jsdom. Tried:");
  for (const t of tried) console.error("  " + t);
  console.error("\nInstall it on the development machine, then point the harness at it:");
  console.error("  npm install jsdom                       # in this directory, if possible");
  console.error("  JSDOM_PATH=/path/to/node_modules node render.mjs <url> [token]");
  process.exit(2);
};

const { JSDOM, VirtualConsole } = await loadJsdom();

const BASE = process.argv[2] || "http://127.0.0.1:8090";
const TOKEN = process.argv[3] || "";
const auth = TOKEN ? { Authorization: "Bearer " + TOKEN } : {};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const failures = [];
const check = (ok, what, detail) => {
  if (!ok) failures.push(detail ? `${what}: ${detail}` : what);
  console.log(`  ${ok ? "ok  " : "FAIL"} ${what}${detail && !ok ? " — " + detail : ""}`);
};

const get = async (path) => {
  const res = await fetch(BASE + path, { headers: auth });
  if (!res.ok) throw new Error(`GET ${path} -> ${res.status}`);
  return res.text();
};

const main = async () => {
  const vc = new VirtualConsole();
  const pageErrors = [];
  vc.on("jsdomError", (e) => pageErrors.push(e.message));

  const html = await get("/");
  const dom = new JSDOM(html, {
    url: BASE + "/",
    runScripts: "outside-only",
    virtualConsole: vc,
    pretendToBeVisual: true,
  });
  const { window } = dom;
  // A fetch that carries the token, so the page's own calls are authorised.
  window.fetch = (input, init) => {
    const opts = Object.assign({}, init);
    opts.headers = Object.assign({}, (init && init.headers) || {});
    if (TOKEN) opts.headers.Authorization = "Bearer " + TOKEN;
    return fetch(typeof input === "string" ? new URL(input, BASE).href : input, opts);
  };
  window.AbortController = AbortController;
  window.matchMedia = () => ({ matches: false, addEventListener() {}, removeEventListener() {} });

  window.eval(await get("/app.js"));
  await sleep(2500);

  const d = window.document;
  const text = (sel) => (d.querySelector(sel) || {}).textContent || "";
  const clickTab = async (view) => {
    d.querySelector(`.tab[data-view="${view}"]`).dispatchEvent(new window.Event("click", { bubbles: true }));
    await sleep(1300);
  };

  console.log("shell");
  check(pageErrors.length === 0, "the page logged no error", pageErrors.join("; "));
  check(d.querySelectorAll(".tab").length === 7, "seven navigation tabs");
  check(d.querySelectorAll(".view").length === 7, "seven views exist");

  console.log("overview");
  check(!text("#pill-mode").includes("unknown"), "the mode pill resolved", text("#pill-mode"));
  check(d.querySelectorAll("#overview-warnings .warning").length > 0, "the warnings block is populated");
  check(!text("#overview-resources").includes("Loading"), "the machine panel resolved");
  check(text("#overview-stamp").length > 0, "the freshness stamp is set");
  check(d.querySelectorAll("#overview-roots li").length > 0, "the storage panel lists a root");

  console.log("files");
  await clickTab("files");
  check(!text("#files-body").includes("the listing failed"), "the listing loaded");
  const rows = [...d.querySelectorAll("#files-body .entry-label")].map((n) => n.textContent);
  check(rows.length > 0, "the listing has entries");
  check(/entry|entries/.test(text("#files-pager")), "the pager reports a count", text("#files-pager").trim());
  check(!/\b1 entries\b/.test(text("#files-pager")), "the count is grammatical for one", text("#files-pager").trim());

  // Breadcrumbs: enter a directory and come back. The current directory renders
  // as .crumb-current, so a segment count must include both classes: at the root
  // there is one .crumb, and inside a directory there is one .crumb plus one
  // .crumb-current.
  const segs = () => d.querySelectorAll("#files-crumbs .crumb, #files-crumbs .crumb-current").length;
  const dirBtn = d.querySelector("#files-body button.entry-dir");
  if (dirBtn) {
    dirBtn.dispatchEvent(new window.Event("click", { bubbles: true }));
    await sleep(1000);
    check(segs() >= 2, "the breadcrumb grew on descent", text("#files-crumbs"));
    const back = d.querySelector("#files-crumbs .crumb");
    back.dispatchEvent(new window.Event("click", { bubbles: true }));
    await sleep(1000);
    check(segs() === 1, "the breadcrumb returned to the root", text("#files-crumbs"));
  } else {
    check(false, "a subdirectory was available to enter");
  }

  // Every file row must offer a download, because a file with no preview has no
  // other way to be taken.
  //
  // The check descends first: a storage root commonly contains only directories,
  // so asserting this on the root page would fail against a perfectly good
  // configuration - which is exactly what happened the first time this ran
  // against the synthetic library.
  let sawFile = false;
  for (let hop = 0; hop < 3 && !sawFile; hop++) {
    const rows = [...d.querySelectorAll("#files-body tr")];
    if (rows.some((r) => r.querySelector('a[href*="files/download"]'))) {
      sawFile = true;
      break;
    }
    const nextDir = d.querySelector("#files-body button.entry-dir");
    if (!nextDir) break;
    nextDir.dispatchEvent(new window.Event("click", { bubbles: true }));
    await sleep(900);
  }
  const dl = d.querySelectorAll('#files-body a[href*="files/download"]');
  check(sawFile && dl.length > 0, "file rows offer a download link",
    sawFile ? "" : "no file row found within three directory levels");
  check([...dl].every((a) => a.getAttribute("download")), "download links carry a filename");

  // Return to the root so the search below starts from a known place.
  const rootCrumb = d.querySelector("#files-crumbs .crumb");
  if (rootCrumb) {
    rootCrumb.dispatchEvent(new window.Event("click", { bubbles: true }));
    await sleep(900);
  }

  // Search.
  const q = d.querySelector("#files-search");
  q.value = "e";
  d.querySelector("#files-search-form").dispatchEvent(new window.Event("submit", { bubbles: true, cancelable: true }));
  await sleep(1000);
  check(!text("#files-body").includes("the listing failed"), "the search returned");
  q.value = "";
  q.dispatchEvent(new window.Event("input", { bubbles: true }));
  await sleep(900);
  check(d.querySelectorAll("#files-body .entry-label").length > 0, "clearing the search restored the listing");

  console.log("jobs");
  await clickTab("jobs");
  check(!text("#jobs-body").includes("the job list failed"), "the job list loaded");
  check(d.querySelectorAll("#jobs-body tr").length > 0, "the job table has a row (empty or otherwise)");

  console.log("settings");
  await clickTab("settings");
  check(!text("#settings-modes").includes("Loading"), "the mode panel resolved", text("#settings-modes").trim());
  check(!text("#settings-approvals").includes("Loading"), "the approvals panel resolved");
  check(d.querySelectorAll("#settings-ops tr").length > 10, "the operation table is populated",
    `${d.querySelectorAll("#settings-ops tr").length} rows`);
  check(!text("#settings-audit").includes("Loading"), "the audit panel resolved");

  console.log("watch");
  await clickTab("media");
  check(text("#media-state").length > 0, "the watch panel says something", text("#media-state").trim());

  console.log("album and backup");
  await clickTab("album");
  check(text("#view-album").length > 0, "the album panel renders");
  await clickTab("backup");
  check(text("#view-backup").length > 0, "the backup panel renders");

  console.log("theme");
  const before = d.documentElement.getAttribute("data-theme");
  d.querySelector("#theme-toggle").dispatchEvent(new window.Event("click", { bubbles: true }));
  await sleep(200);
  check(d.documentElement.getAttribute("data-theme") !== before, "the theme toggle switches theme");

  console.log("");
  console.log("NOT verified by this harness: layout, CSS, responsive breakpoints, and");
  console.log("anything visual. Those need a real browser.");
  if (failures.length) {
    console.log(`\n${failures.length} check(s) failed:`);
    for (const f of failures) console.log("  - " + f);
    // The page runs a refresh timer, which keeps Node's event loop alive. The
    // window has to be torn down or the process hangs after reporting - a harness
    // that never exits is worse than one that fails, because a caller waits.
    window.close();
    process.exit(1);
  }
  console.log("\nall DOM checks passed");
  window.close();
  process.exit(0);
};

main().catch((e) => {
  console.error("harness failed:", e.stack || e.message);
  process.exit(2);
});
