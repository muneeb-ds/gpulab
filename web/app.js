"use strict";
const $ = (s, el = document) => el.querySelector(s);
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const gb = (mb) => (mb / 1024).toFixed(mb >= 10240 ? 0 : 1) + " GB";
let state = null;
let tab = "workers";
let token = "";
try { token = localStorage.getItem("gpulab-token") || ""; } catch {}

async function api(method, path, body) {
  const res = await fetch(path, {
    method, headers: { Authorization: "Bearer " + token, "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 401) { showLogin(); throw new Error("Not signed in"); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

function toast(msg) {
  const t = $("#toast"); t.textContent = msg; t.hidden = false;
  clearTimeout(toast.t); toast.t = setTimeout(() => (t.hidden = true), 3500);
}
async function act(fn, ok) {
  try { await fn(); if (ok) toast(ok); await refresh(); } catch (e) { toast(e.message); }
}

// ---------- auth & tabs ----------
function showLogin() {
  document.querySelectorAll(".tab").forEach((s) => (s.hidden = true));
  $("#login").hidden = false; $("#tabs").hidden = true; $("#logout").hidden = true;
}
$("#login-form").onsubmit = async (e) => {
  e.preventDefault(); token = $("#token").value.trim();
  try { localStorage.setItem("gpulab-token", token); } catch {}
  $("#login").hidden = true; $("#tabs").hidden = false; $("#logout").hidden = false;
  showTab(tab); refresh();
};
$("#logout").onclick = () => { token = ""; try { localStorage.removeItem("gpulab-token"); } catch {} showLogin(); };
document.querySelectorAll("#tabs button").forEach((b) => (b.onclick = () => showTab(b.dataset.tab)));
function showTab(t) {
  tab = t;
  document.querySelectorAll("#tabs button").forEach((b) => b.classList.toggle("active", b.dataset.tab === t));
  document.querySelectorAll(".tab").forEach((s) => (s.hidden = s.id !== t));
  render();
}

// ---------- data ----------
async function refresh() {
  if (!token) return showLogin();
  try {
    state = await api("GET", "/api/overview");
    if (tab === "activity") state.ops = await api("GET", "/api/operations");
    render();
  } catch (e) { /* shown via login or next refresh */ }
}
setInterval(() => { if (!$("#dlg").open) refresh(); }, 3000);

const nodeById = (id) => state.nodes.find((n) => n.id === id);
const gpuById = (id) => state.gpus.find((g) => g.id === id);
const userById = (id) => state.users.find((u) => u.id === id);
const ctById = (id) => state.containers.find((c) => c.id === id);

function sliceLabel(s) {
  const g = gpuById(s.gpu_id);
  return s.kind === "full" ? `Full ${g ? g.model.replace("NVIDIA ", "") : "GPU"}` : `MIG ${s.profile}`;
}

function render() {
  if (!state) return;
  if (tab === "workers") renderWorkers();
  if (tab === "containers") renderContainers();
  if (tab === "users") renderUsers();
  if (tab === "activity") renderActivity();
}

// ---------- workers ----------
$("#add-worker").onclick = () => act(async () => {
  const j = await api("POST", "/api/join-tokens");
  const el = $("#join"); el.hidden = false;
  el.innerHTML = `<h3>Add a GPU worker</h3>
    <p class="muted small">Prerequisites on the worker: NVIDIA driver, <code>nvidia-container-toolkit</code>, Incus initialised.
    Run this as root (token is single-use, valid ${esc(j.expires_in)}):</p>
    <div class="cmd"><code>${esc(j.install_command)}</code><button type="button" id="copy-cmd">Copy</button></div>
    <p class="muted small">The worker then shows up below as <b>pending</b>; approve it to start using its GPUs.</p>`;
  $("#copy-cmd").onclick = () => navigator.clipboard.writeText(j.install_command).then(() => toast("Copied"));
});

function statusPill(s) {
  const cls = { ready: "ok", running: "ok", done: "ok", pending: "warn", applying: "warn", creating: "warn", queued: "warn", stopped: "", error: "bad", failed: "bad", missing: "bad", disabled: "" }[s] ?? "";
  return `<span class="pill ${cls}">${esc(s)}</span>`;
}

function slotCount(g) {
  let n = 0;
  for (const p of g.profiles || []) for (const pl of p.placements || []) n = Math.max(n, pl.start + pl.size);
  return n || 8;
}
function placementSize(g, profile, start) {
  const p = (g.profiles || []).find((p) => p.name === profile);
  const pl = p && p.placements.find((x) => x.start === start);
  return pl ? pl.size : 1;
}

// Draws a GPU's memory slots with its slices placed on them.
function sliceBar(g, entries) {
  if (!entries.length && g.mode !== "mig") return `<div class="bar"><div class="cell empty muted">no slices</div></div>`;
  if (entries.length === 1 && entries[0].kind === "full") {
    const s = entries[0];
    return `<div class="bar" style="grid-template-columns:1fr">${cell(s, "1 / -1")}</div>`;
  }
  const n = slotCount(g);
  const taken = new Array(n).fill(false);
  let html = "";
  for (const s of entries) {
    const size = placementSize(g, s.profile, s.placement_start);
    for (let i = s.placement_start; i < s.placement_start + size && i < n; i++) taken[i] = true;
    html += cell(s, `${s.placement_start + 1} / span ${size}`);
  }
  taken.forEach((t, i) => { if (!t) html += `<div class="cell empty" style="grid-column:${i + 1}/span 1"></div>`; });
  return `<div class="bar" style="grid-template-columns:repeat(${n},1fr)">${html}</div>`;
}
function cell(s, col) {
  const ct = s.container_id ? ctById(s.container_id) : null;
  const cls = s.state === "missing" ? "missing" : s.container_id ? "used" : "";
  const title = s.kind === "full" ? "Full GPU" : s.profile;
  const sub = s.state === "missing" ? "missing" : ct ? esc(ct.name) : s.planned ? "planned" : "free";
  return `<div class="cell ${cls}" style="grid-column:${col}" title="${esc(s.mig_uuid || "")}"><span>${esc(title)}</span><span class="sub">${sub}</span></div>`;
}

function renderWorkers() {
  const list = $("#node-list");
  if (!state.nodes.length) {
    list.innerHTML = `<div class="card empty-state">No workers yet. Click <b>Add worker</b> to get an install command.</div>`;
    return;
  }
  list.innerHTML = state.nodes.map((n) => {
    const gpus = state.gpus.filter((g) => g.node_id === n.id);
    return `<div class="card">
      <div class="node-head">
        <div>
          <h3><span class="dot ${n.online ? "on" : ""}"></span>${esc(n.name)} ${statusPill(n.status)}
            ${n.status === "ready" && !n.trusted ? '<span class="pill warn">setting up trust…</span>' : ""}</h3>
          <div class="facts muted small">
            <span>${n.cpu_count} CPU · ${gb(n.memory_mb)} RAM</span>
            <span>Driver ${esc(n.driver_version || "?")} · CUDA ${esc(n.cuda_version || "?")}</span>
            <span class="mono">${esc(n.incus_url)}</span>
            <span>SSH host: <span class="mono">${esc(n.ssh_host || "—")}</span></span>
          </div>
          ${n.inventory_error ? `<div class="err">${esc(n.inventory_error)}</div>` : ""}
        </div>
        <div class="actions">
          ${n.status === "pending" ? `<button class="primary" data-approve="${n.id}">Approve</button>` : ""}
          <button data-sshhost="${n.id}">Set SSH host</button>
          <button class="danger" data-delnode="${n.id}">Remove</button>
        </div>
      </div>
      ${gpus.map((g) => gpuBlock(g, n)).join("") || '<p class="muted">No GPUs reported.</p>'}
    </div>`;
  }).join("");

  list.querySelectorAll("[data-approve]").forEach((b) => (b.onclick = () => act(() => api("POST", `/api/nodes/${b.dataset.approve}/approve`), "Approving…")));
  list.querySelectorAll("[data-sshhost]").forEach((b) => (b.onclick = () => {
    const n = nodeById(+b.dataset.sshhost);
    const v = prompt("Hostname or IP users should SSH to for containers on this worker:", n.ssh_host);
    if (v !== null) act(() => api("PATCH", `/api/nodes/${n.id}`, { ssh_host: v }), "Saved");
  }));
  list.querySelectorAll("[data-delnode]").forEach((b) => (b.onclick = () => {
    if (confirm("Remove this worker from gpulab? (Its containers must be deleted first.)")) act(() => api("DELETE", `/api/nodes/${b.dataset.delnode}`), "Removed");
  }));
  list.querySelectorAll("[data-layout]").forEach((b) => (b.onclick = async () => { const id = +b.dataset.layout; await refresh(); layoutDialog(gpuById(id)); }));
}

function gpuBlock(g, n) {
  const slices = state.slices.filter((s) => s.gpu_id === g.id);
  const canEdit = g.mig_capable && n.status === "ready" && n.trusted && g.status !== "applying";
  return `<div class="gpu">
    <div class="gpu-head">
      <div><b>GPU ${g.index}</b> · ${esc(g.model)} · ${gb(g.memory_mb)} <span class="muted mono small">${esc(g.pci)}</span>
        ${statusPill(g.status)} <span class="pill">${g.mode === "mig" ? "MIG" : "Full GPU"}</span>
        ${g.mig_pending ? '<span class="pill warn">MIG change pending reset</span>' : ""}</div>
      ${g.mig_capable ? `<button data-layout="${g.id}" ${canEdit ? "" : "disabled"}>Configure slices</button>` : '<span class="muted small">No MIG support: assigned whole</span>'}
    </div>
    ${sliceBar(g, slices)}
    ${g.error ? `<div class="err">${esc(g.error)}</div>` : ""}
  </div>`;
}

function layoutDialog(g) {
  const counts = {};
  for (const e of g.layout || []) counts[e.profile] = (counts[e.profile] || 0) + 1;
  let mode = g.mode;
  const profiles = (g.profiles || []).slice().sort((a, b) => b.compute_slices - a.compute_slices || b.memory_mb - a.memory_mb);
  const body = $("#dlg-body");
  const draw = () => {
    body.innerHTML = `<h3>Slice GPU ${g.index} · ${esc(g.model)}</h3>
      <label>Mode<select id="lmode"><option value="exclusive">Full GPU (one container gets the whole card)</option>
        <option value="mig">MIG slices (hardware-isolated partitions)</option></select></label>
      <div id="lprof" ${mode === "mig" ? "" : "hidden"}>
        <div class="profiles">${profiles.map((p) => `<div class="prof"><span><b>${esc(p.name)}</b>
          <span class="muted small">${p.compute_slices}/7 compute</span></span>
          <button type="button" data-dec="${esc(p.name)}">−</button><b>${counts[p.name] || 0}</b><button type="button" data-inc="${esc(p.name)}">+</button></div>`).join("")}</div>
        <div id="lpreview"></div>
      </div>
      <p class="muted small">Slices in use by containers stay where they are. Unused slices may be recreated, which gives them new MIG UUIDs.</p>
      <div class="dlg-actions"><button value="cancel">Cancel</button><button type="button" class="primary" id="lapply">Apply</button></div>`;
    $("#lmode").value = mode;
    $("#lmode").onchange = (e) => { mode = e.target.value; draw(); };
    body.querySelectorAll("[data-inc]").forEach((b) => (b.onclick = () => { counts[b.dataset.inc] = (counts[b.dataset.inc] || 0) + 1; draw(); }));
    body.querySelectorAll("[data-dec]").forEach((b) => (b.onclick = () => { counts[b.dataset.dec] = Math.max(0, (counts[b.dataset.dec] || 0) - 1); draw(); }));
    $("#lapply").onclick = () => act(async () => {
      await api("POST", `/api/gpus/${g.id}/layout`, req());
      $("#dlg").close();
    }, "Applying layout…");
    validate();
  };
  const req = () => ({ mode, profiles: Object.entries(counts).flatMap(([p, n]) => Array(n).fill(p)) });
  const validate = async () => {
    const r = req();
    if (mode !== "mig") { $("#lapply").disabled = false; return; }
    const prev = $("#lpreview");
    if (!r.profiles.length) { prev.innerHTML = '<p class="muted small">Add at least one profile.</p>'; $("#lapply").disabled = true; return; }
    try {
      const v = await api("POST", `/api/gpus/${g.id}/validate`, r);
      $("#lapply").disabled = !v.ok;
      prev.innerHTML = v.ok ? sliceBar(g, v.layout.map((e) => ({ kind: "mig", profile: e.profile, placement_start: e.start, state: "ready", planned: true, container_id: (state.slices.find((s) => s.gpu_id === g.id && s.profile === e.profile && s.placement_start === e.start) || {}).container_id }))) : `<div class="err">${esc(v.error)}</div>`;
    } catch (e) { prev.innerHTML = `<div class="err">${esc(e.message)}</div>`; }
  };
  draw();
  $("#dlg").showModal();
}

// ---------- containers ----------
function freeSlicesPicker(nodeFilter, selfId, checked) {
  const groups = {};
  for (const s of state.slices) {
    const g = gpuById(s.gpu_id); const n = g && nodeById(g.node_id);
    if (!g || !n || n.status !== "ready" || !n.trusted || s.state !== "ready" || g.status === "applying") continue;
    if (s.container_id && s.container_id !== selfId) continue;
    if (nodeFilter && n.id !== nodeFilter) continue;
    const key = `${n.name} · GPU ${g.index} ${g.model.replace("NVIDIA ", "")}`;
    (groups[key] ||= []).push(s);
  }
  const keys = Object.keys(groups);
  if (!keys.length) return '<p class="muted small">No free slices. Configure GPUs on the Workers tab.</p>';
  return keys.map((k) => `<div class="grp">${esc(k)}</div>` + groups[k].map((s) =>
    `<label><input type="checkbox" name="slice" value="${s.id}" ${checked.includes(s.id) ? "checked" : ""}> ${esc(sliceLabel(s))}
     <span class="muted small">${gb(s.memory_mb)}</span></label>`).join("")).join("");
}

$("#new-ct").onclick = async () => {
  await refresh();
  if (!state.users.length) return toast("Add a user first (Users tab).");
  const readyNodes = state.nodes.filter((n) => n.status === "ready" && n.trusted);
  $("#dlg-body").innerHTML = `<h3>New container</h3>
    <div class="grid2">
      <label>Name<input id="cname" placeholder="alice-box1" required></label>
      <label>Owner<select id="cuser">${state.users.map((u) => `<option value="${u.id}">${esc(u.name)}</option>`).join("")}</select></label>
      <label>CPU cores<input id="ccpu" type="number" min="1" value="4"></label>
      <label>Memory (GB)<input id="cmem" type="number" min="1" value="16"></label>
      <label class="span2">Image<input id="cimg" value="${esc(state.default_image)}"></label>
    </div>
    <div><label>GPU slices (all must be on one worker)</label><div class="slice-pick">${freeSlicesPicker(0, 0, [])}</div></div>
    <label>Worker (only used when no slice is selected)<select id="cnode">${readyNodes.map((n) => `<option value="${n.id}">${esc(n.name)}</option>`).join("")}</select></label>
    <p class="muted small">One CUDA process uses one MIG slice. Several slices let several processes run side by side; for a bigger job pick a bigger profile.</p>
    <div class="dlg-actions"><button value="cancel">Cancel</button><button type="button" class="primary" id="ccreate">Create</button></div>`;
  $("#ccreate").onclick = () => act(async () => {
    const ids = [...document.querySelectorAll('#dlg input[name="slice"]:checked')].map((i) => +i.value);
    await api("POST", "/api/containers", {
      name: $("#cname").value.trim(), user_id: +$("#cuser").value, cpu: +$("#ccpu").value, memory_mb: Math.round(+$("#cmem").value * 1024),
      image: $("#cimg").value.trim(), slice_ids: ids, node_id: +$("#cnode").value || 0,
    });
    $("#dlg").close();
  }, "Creating container…");
  $("#dlg").showModal();
};

function renderContainers() {
  const el = $("#ct-list");
  if (!state.containers.length) { el.innerHTML = `<div class="card empty-state">No containers yet.</div>`; return; }
  el.innerHTML = `<div class="table-wrap"><table><thead><tr><th>Name</th><th>Owner</th><th>Worker</th><th>GPU slices</th><th>State</th><th>SSH</th><th></th></tr></thead><tbody>
    ${state.containers.map((c) => {
      const n = nodeById(c.node_id); const u = userById(c.user_id);
      const sl = state.slices.filter((s) => s.container_id === c.id);
      const ssh = `ssh -p ${c.ssh_port} ${u ? u.name : "user"}@${n && n.ssh_host ? n.ssh_host : "worker"}`;
      return `<tr>
        <td><b>${esc(c.name)}</b><div class="muted small">${c.cpu_limit} CPU · ${gb(c.memory_mb)}</div></td>
        <td>${esc(u ? u.name : "?")}</td><td>${esc(n ? n.name : "?")}</td>
        <td>${sl.map((s) => `<div>${esc(sliceLabel(s))}${s.state !== "ready" ? ' <span class="pill bad">missing</span>' : ""}</div>`).join("") || '<span class="muted">none</span>'}</td>
        <td>${statusPill(c.state)}${c.desired_state !== (c.state === "running" ? "running" : c.state) && c.desired_state !== "running" ? ` <span class="muted small">→ ${esc(c.desired_state)}</span>` : ""}
          ${c.error ? `<div class="err">${esc(c.error)}</div>` : ""}</td>
        <td><code>${esc(ssh)}</code></td>
        <td><div class="actions">
          ${c.desired_state === "running" ? `<button data-stop="${c.id}">Stop</button>` : `<button data-start="${c.id}">Start</button>`}
          <button data-slices="${c.id}">Slices</button>
          <button class="danger" data-del="${c.id}">Delete</button></div></td></tr>`;
    }).join("")}</tbody></table></div>`;
  el.querySelectorAll("[data-start]").forEach((b) => (b.onclick = () => act(() => api("POST", `/api/containers/${b.dataset.start}/start`), "Starting…")));
  el.querySelectorAll("[data-stop]").forEach((b) => (b.onclick = () => act(() => api("POST", `/api/containers/${b.dataset.stop}/stop`), "Stopping…")));
  el.querySelectorAll("[data-del]").forEach((b) => (b.onclick = () => {
    const c = ctById(+b.dataset.del);
    if (confirm(`Delete ${c.name}? Its filesystem is destroyed.`)) act(() => api("DELETE", `/api/containers/${c.id}`), "Deleting…");
  }));
  el.querySelectorAll("[data-slices]").forEach((b) => (b.onclick = async () => {
    await refresh();
    const c = ctById(+b.dataset.slices);
    const mine = state.slices.filter((s) => s.container_id === c.id).map((s) => s.id);
    $("#dlg-body").innerHTML = `<h3>GPU slices for ${esc(c.name)}</h3>
      <div class="slice-pick">${freeSlicesPicker(c.node_id, c.id, mine)}</div>
      <p class="muted small">MIG devices can't be hot-plugged: the container restarts if it is running.</p>
      <div class="dlg-actions"><button value="cancel">Cancel</button><button type="button" class="primary" id="ssave">Save</button></div>`;
    $("#ssave").onclick = () => act(async () => {
      const ids = [...document.querySelectorAll('#dlg input[name="slice"]:checked')].map((i) => +i.value);
      await api("PUT", `/api/containers/${c.id}/slices`, { slice_ids: ids });
      $("#dlg").close();
    }, "Updating slices…");
    $("#dlg").showModal();
  }));
}

// ---------- users ----------
$("#user-form").onsubmit = (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  act(async () => { await api("POST", "/api/users", { name: f.get("name"), ssh_keys: f.get("ssh_keys") }); e.target.reset(); }, "User added");
};
function renderUsers() {
  const el = $("#user-list");
  if (!state.users.length) { el.innerHTML = ""; return; }
  el.innerHTML = `<div class="table-wrap"><table><thead><tr><th>User</th><th>SSH keys</th><th>Containers</th><th></th></tr></thead><tbody>
    ${state.users.map((u) => `<tr><td><b>${esc(u.name)}</b></td>
      <td class="mono small">${esc(u.ssh_keys).split("\n").map((k) => esc(k.slice(0, 40) + (k.length > 40 ? "…" : ""))).join("<br>") || '<span class="muted">none</span>'}</td>
      <td>${state.containers.filter((c) => c.user_id === u.id).length}</td>
      <td><button class="danger" data-deluser="${u.id}">Delete</button></td></tr>`).join("")}</tbody></table></div>`;
  el.querySelectorAll("[data-deluser]").forEach((b) => (b.onclick = () => {
    if (confirm("Delete this user?")) act(() => api("DELETE", `/api/users/${b.dataset.deluser}`), "Deleted");
  }));
}

// ---------- activity ----------
function renderActivity() {
  const ops = state.ops || [];
  $("#op-list").innerHTML = ops.length ? `<div class="table-wrap"><table><thead><tr><th>#</th><th>Operation</th><th>Target</th><th>Status</th><th>When</th></tr></thead><tbody>
    ${ops.map((o) => `<tr><td>${o.id}</td><td>${esc(o.type.replace(/_/g, " "))}</td><td>${o.target_id}</td>
      <td>${statusPill(o.status)}${o.error ? `<div class="err">${esc(o.error)}</div>` : ""}</td>
      <td class="muted small">${new Date(o.updated_at * 1000).toLocaleString()}</td></tr>`).join("")}</tbody></table></div>`
    : `<div class="card empty-state">Nothing has happened yet.</div>`;
}

if (token) { $("#login").hidden = true; showTab("workers"); refresh(); } else showLogin();
