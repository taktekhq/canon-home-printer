"use strict";

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];

// Build DOM safely: h("li", {class: "x"}, "text", child)
function h(tag, attrs = {}, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const k of kids.flat()) if (k != null && k !== false) el.append(k instanceof Node ? k : String(k));
  return el;
}

class ApiError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

async function api(path, opts = {}) {
  let res;
  try {
    res = await fetch(path, { credentials: "same-origin", ...opts });
  } catch {
    throw new ApiError(0, "Can't reach the printer server. Check your internet connection.");
  }
  if (res.status === 401 || (res.redirected && !res.url.includes(location.host))) {
    throw new ApiError(401, "Your sign-in expired. Reload the page to sign in again.");
  }
  const type = res.headers.get("content-type") || "";
  const body = type.includes("json") ? await res.json() : await res.text();
  if (!res.ok) throw new ApiError(res.status, (body && body.detail) || `Something went wrong (${res.status}).`);
  return body;
}

function banner(text, kind = "error", ms = 0) {
  const b = $("#banner");
  if (!text) { b.hidden = true; return; }
  b.className = `banner ${kind}`;
  b.textContent = text;
  b.hidden = false;
  b.scrollIntoView({ block: "nearest", behavior: "smooth" });
  if (ms) setTimeout(() => { if (b.textContent === text) b.hidden = true; }, ms);
}

const ago = (iso) => {
  if (!iso) return "";
  const s = (Date.now() - new Date(iso).getTime()) / 1000;
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return new Date(iso).toLocaleDateString(undefined, { day: "numeric", month: "short" });
};
const clock = (iso) => iso ? new Date(iso).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" }) : "";
const size = (n) => n > 1e6 ? `${(n / 1e6).toFixed(1)} MB` : `${Math.max(1, Math.round(n / 1e3))} KB`;

/* ---------- tabs ---------- */
function showTab(name) {
  $$(".tabs button").forEach(b => b.setAttribute("aria-selected", b.dataset.tab === name));
  $$(".tab").forEach(t => t.hidden = t.id !== `tab-${name}`);
  try { localStorage.setItem("tab", name); } catch {}
  if (name === "status") refreshLog();
  if (name === "scan") refreshScans();
}
$$(".tabs button").forEach(b => b.addEventListener("click", () => showTab(b.dataset.tab)));
$("#health").addEventListener("click", () => showTab("status"));

/* ---------- me ---------- */
let ME = null;

/* ---------- jobs ---------- */
function issueList(issues) {
  if (!issues || !issues.length) return null;
  return h("ul", { class: "issues" }, issues.map(i =>
    h("li", { class: i.severity }, h("strong", {}, i.text), i.fix ? h("span", { class: "fix" }, i.fix) : null)));
}

function jobItem(j, showWho) {
  const actions = [];
  if (j.active && (j.mine || (ME && ME.can_maintain))) {
    actions.push(h("button", { type: "button", onclick: async (e) => {
      e.target.disabled = true;
      try { await api(`/api/jobs/${j.id}/cancel`, { method: "POST" }); banner("Print canceled.", "ok", 3000); refresh(); }
      catch (err) { banner(err.message); e.target.disabled = false; }
    } }, "Cancel"));
  }
  return h("li", {},
    h("div", { class: "job-top" },
      h("span", { class: "job-name" }, j.name || "Untitled"),
      h("span", { class: `state ${j.state}` }, j.state === "done" ? "Printed" : j.summary.split(":")[0])),
    h("div", { class: "job-meta" },
      [showWho && j.by ? j.by : null, j.printer === "maintenance" ? "maintenance" : null, ago(j.created),
       j.state === "printing" && j.message ? j.message : null].filter(Boolean).join(" · ")),
    issueList(j.issues),
    j.state !== "done"
      ? h("ul", { class: "timeline" }, j.timeline.map(t => h("li", {}, `${clock(t.at)} ${t.label}`)))
      : null,
    actions.length ? h("div", { class: "job-actions" }, actions) : null);
}

function renderJobs(el, jobs, showWho, emptyText) {
  el.replaceChildren(...(jobs.length ? jobs.map(j => jobItem(j, showWho)) : [h("li", { class: "empty" }, emptyText)]));
}

/* ---------- status ---------- */
let anyActive = false;

function renderStatus(s) {
  const p = s.printer;
  const errors = s.issues.filter(i => i.severity === "error");
  const warns = s.issues.filter(i => i.severity === "warning");
  const pill = $("#health");
  if (errors.length) { pill.className = "pill err"; pill.textContent = "Needs attention"; }
  else if (warns.length) { pill.className = "pill warn"; pill.textContent = "Check status"; }
  else if (p.state === "printing" || s.active_jobs.length) { pill.className = "pill ok"; pill.textContent = "Printing"; }
  else { pill.className = "pill ok"; pill.textContent = "Ready"; }

  const stateWord = errors.length ? "Needs attention" : ({ idle: "Ready", printing: "Printing", stopped: "Stopped", missing: "Not set up" }[p.state] || p.state);
  $("#state-text").textContent = stateWord;
  $("#state-detail").textContent = [p.message, `checked ${clock(s.checked_at)}`].filter(Boolean).join(" · ");
  $("#issues").replaceChildren(...s.issues.map(i =>
    h("li", { class: i.severity }, h("strong", {}, i.text), i.fix ? h("span", { class: "fix" }, i.fix) : null)));
  $("#checks").replaceChildren(...s.checks.map(c =>
    h("li", {}, h("span", {}, c.name), h("span", { class: `v ${c.ok ? "ok" : "bad"}` }, c.text))));
  anyActive = s.active_jobs.length > 0;
}

async function refresh() {
  try {
    const [s, mine, all] = await Promise.all([api("/api/status"), api("/api/jobs?mine=true"), api("/api/jobs")]);
    renderStatus(s);
    renderJobs($("#my-jobs"), mine.slice(0, 10), false, "Nothing printed yet.");
    renderJobs($("#all-jobs"), all.slice(0, 20), true, "Nothing in the queue.");
  } catch (e) {
    $("#health").className = "pill err";
    $("#health").textContent = "Offline";
    if (e.status === 401) banner(e.message);
  }
}

async function refreshLog() {
  try {
    const log = await api("/api/log");
    $("#events").replaceChildren(...(log.events.length ? log.events.slice(0, 30).map(e =>
      h("li", {}, `${e.at.replace("T", " ")}  ${e.by || e.user}  ${e.action}` +
        (e.file ? `  "${e.file}"` : "") + (e.what ? `  ${e.what}` : "") + (e.job ? `  job ${e.job}` : ""))) : [h("li", {}, "No activity yet.")]));
    $("#cups-log").replaceChildren(...(log.cups.length ? log.cups.map(l =>
      h("li", { class: l.level }, `${(l.at || "").replace("T", " ").slice(0, 19)}  ${l.text}`)) : [h("li", {}, "No errors or warnings.")]));
  } catch (e) { $("#cups-log").replaceChildren(h("li", { class: "error" }, e.message)); }
}

$("#refresh").addEventListener("click", () => { refresh(); refreshLog(); });

/* ---------- print ---------- */
const fileInput = $("#file");
const drop = $("#drop");
function showFile() {
  const f = fileInput.files[0];
  drop.classList.toggle("has-file", !!f);
  $("#drop-text").replaceChildren(...(f
    ? [h("strong", {}, f.name), h("br"), h("small", {}, `${size(f.size)} · tap to choose another`)]
    : [h("strong", {}, "Choose a file"), h("br"), h("small", {}, ME && ME.office === false ? "PDF, photo (JPG/PNG) or text" : "PDF, Word, photo (JPG/PNG) or text")]));
}
fileInput.addEventListener("change", showFile);
["dragenter", "dragover"].forEach(ev => drop.addEventListener(ev, e => { e.preventDefault(); drop.classList.add("drag"); }));
["dragleave", "drop"].forEach(ev => drop.addEventListener(ev, e => { e.preventDefault(); drop.classList.remove("drag"); }));
drop.addEventListener("drop", e => { if (e.dataTransfer.files.length) { fileInput.files = e.dataTransfer.files; showFile(); } });

const pagesInput = $("#pages");
function showPages() {
  const some = $('input[name="which"]:checked').value === "some";
  pagesInput.hidden = !some;
  if (some) pagesInput.focus(); else pagesInput.value = "";
}
$$('input[name="which"]').forEach(r => r.addEventListener("change", showPages));

$("#print-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const f = fileInput.files[0];
  if (!f) { banner("Choose a file to print first."); return; }
  if (ME && f.size > ME.max_upload_mb * 1024 * 1024) { banner(`That file is bigger than ${ME.max_upload_mb} MB.`); return; }
  const form = e.target;
  const data = new FormData();
  data.append("file", f);
  data.append("copies", form.copies.value || "1");
  if (form.which.value === "some" && !form.pages.value.trim()) { banner("Type which pages to print, like 1-3, 5."); pagesInput.focus(); return; }
  data.append("color", form.color.value);
  data.append("pages", form.which.value === "some" ? form.pages.value.trim() : "");
  data.append("fit", form.fit.value);
  const btn = $("#print-btn");
  btn.disabled = true;
  btn.textContent = "Uploading…";
  // XHR so we can show upload progress on slow connections.
  const xhr = new XMLHttpRequest();
  xhr.open("POST", "/api/print");
  xhr.upload.onprogress = (ev) => {
    if (!ev.lengthComputable) return;
    const pct = Math.round(100 * ev.loaded / ev.total);
    btn.textContent = pct < 100 ? `Uploading… ${pct}%` : "Preparing…";
  };
  xhr.onload = () => {
    btn.disabled = false; btn.textContent = "Print";
    let body = {};
    try { body = JSON.parse(xhr.responseText); } catch {}
    if (xhr.status >= 200 && xhr.status < 300) {
      banner(`Sent to the printer. Watch its progress under “Your prints”.`, "ok", 6000);
      form.reset(); pagesInput.hidden = true; showFile(); refresh();
    } else if (xhr.status === 401) {
      banner("Your sign-in expired. Reload the page to sign in again.");
    } else {
      banner(body.detail || `Couldn't print (error ${xhr.status}).`);
    }
  };
  xhr.onerror = () => { btn.disabled = false; btn.textContent = "Print"; banner("Upload failed. Check your internet connection and try again."); };
  xhr.send(data);
});

/* ---------- scan ---------- */
async function refreshScans() {
  try {
    const files = await api("/api/scans");
    $("#my-scans").replaceChildren(...(files.length ? files.map(f => h("li", {},
      h("div", { class: "job-top" }, h("span", { class: "job-name" }, f.name), h("span", { class: "job-meta" }, size(f.size))),
      h("div", { class: "job-meta" }, ago(f.created)),
      h("div", { class: "job-actions" },
        h("a", { href: f.url, target: "_blank", rel: "noopener" }, "Open"),
        h("a", { href: `${f.url}?download=true` }, "Download"),
        h("button", { type: "button", onclick: async () => {
          if (!(await confirmDialog(`Delete ${f.name}?`))) return;
          try { await api(f.url, { method: "DELETE" }); refreshScans(); } catch (e) { banner(e.message); }
        } }, "Delete")))) : [h("li", { class: "empty" }, "No scans yet.")]));
  } catch (e) { if (e.status !== 401) banner(e.message); }
}

$("#scan-btn").addEventListener("click", async () => {
  const btn = $("#scan-btn");
  const val = (n) => $(`input[name="${n}"]:checked`).value;
  btn.disabled = true;
  $("#scan-progress").hidden = false;
  $("#scan-progress-text").textContent = "Starting the scanner…";
  banner("");
  try {
    let job = await api("/api/scan", { method: "POST", headers: { "content-type": "application/json" },
      body: JSON.stringify({ resolution: Number(val("sres")), mode: val("smode"), format: val("sfmt") }) });
    const t0 = Date.now();
    while (job.state === "queued" || job.state === "scanning") {
      $("#scan-progress-text").textContent = `Scanning… ${Math.round((Date.now() - t0) / 1000)} s (normal takes about 30 s)`;
      await new Promise(r => setTimeout(r, 1500));
      job = await api(`/api/scan/${job.id}`);
    }
    if (job.state === "done") {
      banner(`Scan ready (${size(job.size)}, ${job.seconds} s).`, "ok", 6000);
      window.open(job.url, "_blank", "noopener");
    } else {
      banner(`${job.error} ${job.fix}`);
    }
  } catch (e) {
    banner(e.message);
  } finally {
    btn.disabled = false;
    $("#scan-progress").hidden = true;
    refreshScans(); refresh();
  }
});

/* ---------- maintenance ---------- */
function confirmDialog(text) {
  const d = $("#confirm");
  $("#confirm-text").textContent = text;
  d.showModal();
  return new Promise(res => d.addEventListener("close", () => res(d.returnValue === "yes"), { once: true }));
}

const MAINT_TEXT = {
  nozzle_check: "Print a nozzle check pattern? It uses one sheet of paper. Look for gaps in each colour.",
  clean: "Run a print head cleaning? The printer whirs for about a minute and uses a little ink.",
  deep_clean: "Run a deep cleaning? It uses a lot of ink. Only do this if two normal cleanings didn't help. Afterwards, leave the printer alone for 10 minutes.",
};
$$("[data-maint]").forEach(b => b.addEventListener("click", async () => {
  const action = b.dataset.maint;
  if (!(await confirmDialog(MAINT_TEXT[action]))) return;
  b.disabled = true;
  try {
    const r = await api("/api/maintenance", { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ action }) });
    banner(`${r.title} sent. The printer should start within a few seconds. If it stays silent, it ignored the command.`, "ok", 9000);
    refresh(); refreshLog();
  } catch (e) { banner(e.message); }
  finally { b.disabled = false; }
}));

/* ---------- start ---------- */
(async function init() {
  try {
    ME = await api("/api/me");
    $("#printer-name").textContent = ME.printer;
    document.title = ME.printer;
    $("#who").textContent = `Signed in as ${ME.name} (${ME.email})`;
    $("#maint").hidden = !ME.can_maintain;
  } catch (e) {
    banner(e.message);
  }
  let tab = "print";
  try { tab = localStorage.getItem("tab") || "print"; } catch {}
  showTab(["print", "scan", "status"].includes(tab) ? tab : "print");
  await refresh();
  // Poll faster while something is printing.
  (function loop() {
    setTimeout(async () => { if (!document.hidden) await refresh(); loop(); }, anyActive ? 2500 : 10000);
  })();
  document.addEventListener("visibilitychange", () => { if (!document.hidden) refresh(); });
})();
