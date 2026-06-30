// Owner-only admin panel (Cloudflare Worker) for managing per-user access:
// create a user (mints a service token + Access policies + KV entry), list,
// edit servers, revoke. Must be fronted by a Cloudflare Access app (allow your
// email only); the ADMIN_EMAIL header check is a backstop, not the front door.
// Bindings/vars live in admin/wrangler.toml; CF_API_TOKEN is a wrangler secret.

const CF_API = "https://api.cloudflare.com/client/v4";

// Optional fallback if env.DISCORD_WEBHOOK is not set. Leave empty in the repo;
// set DISCORD_WEBHOOK as a Worker var if you want session logs.
const FALLBACK_WEBHOOK = "";

// The servers an admin can hand out. The Access Application ID is resolved
// automatically by hostname (see resolveAppId), so you only list the hostname
// here - just make sure a "Public DNS" Access app exists for each one.
const SERVERS = {
  chromacube: {
    label: "ChromaCube",
    hostname: "chromacube.deforce.site",
    protocol: "tcp",
    mcHost: "chromacube",
  },
  hbm: {
    label: "HBM",
    hostname: "hbm.deforce.site",
    protocol: "tcp",
    mcHost: "hbm",
  },
};

// Cache of hostname -> Access Application ID, resolved on demand from the API.
const appIdCache = {};

export default {
  async fetch(request, env) {
    // Defense in depth: Cloudflare Access must front this route and inject the
    // authenticated email. Reject anything that isn't the configured admin.
    const email = request.headers.get("Cf-Access-Authenticated-User-Email");
    if (!email || (env.ADMIN_EMAIL && email.toLowerCase() !== env.ADMIN_EMAIL.toLowerCase())) {
      return new Response("forbidden (admin only)", { status: 403 });
    }

    const url = new URL(request.url);
    const path = url.pathname.replace(/\/+$/, "");

    try {
      if (request.method === "GET" && (path === "" || path.endsWith("/admin"))) {
        return html(PAGE);
      }
      if (path.endsWith("/api/servers") && request.method === "GET") {
        return json({ servers: serverList() });
      }
      if (path.endsWith("/api/users") && request.method === "GET") {
        return json({ users: await listUsers(env) });
      }
      if (path.endsWith("/api/version") && request.method === "GET") {
        return json(await getVersionMeta(env));
      }
      if (path.endsWith("/api/version") && request.method === "POST") {
        return json(await setVersionMeta(env, await request.json()));
      }
      if (path.endsWith("/api/users") && request.method === "POST") {
        return json(await createUser(env, await request.json()));
      }
      if (path.endsWith("/api/users/servers") && request.method === "POST") {
        return json(await updateUserServers(env, await request.json()));
      }
      if (path.endsWith("/api/users/delete") && request.method === "POST") {
        return json(await deleteUser(env, await request.json()));
      }
      return new Response("not found", { status: 404 });
    } catch (e) {
      return json({ error: String(e && e.message ? e.message : e) }, 500);
    }
  },
};

// ----- Cloudflare API helper -------------------------------------------------

async function cf(env, method, apiPath, body) {
  const headers = { Authorization: "Bearer " + env.CF_API_TOKEN };
  if (body) headers["content-type"] = "application/json";
  const res = await fetch(CF_API + apiPath, {
    method,
    headers,
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let data = {};
  try { data = JSON.parse(text); } catch (_) {}
  if (!res.ok || data.success === false) {
    let detail = "";
    if (data.errors && data.errors.length) {
      detail = data.errors
        .map((e) => {
          let m = (e.code ? e.code + " " : "") + (e.message || "");
          if (e.error_chain && e.error_chain.length) {
            m += " [" + e.error_chain.map((c) => c.message).join("; ") + "]";
          }
          return m;
        })
        .join("; ");
    }
    if (!detail) detail = text.slice(0, 300);
    throw new Error("Cloudflare API " + res.status + " on " + method + " " + apiPath + " - " + detail);
  }
  return data.result;
}

// resolveAppId finds the Access Application ID for a hostname by listing the
// account's apps and matching the domain (so the admin never pastes an App ID).
async function resolveAppId(env, hostname) {
  const want = hostname.toLowerCase();
  if (appIdCache[want]) return appIdCache[want];

  const apps = await cf(env, "GET", `/accounts/${env.CF_ACCOUNT_ID}/access/apps`);
  const norm = (d) => String(d || "").replace(/^https?:\/\//, "").replace(/\/.*$/, "").toLowerCase();
  const match = (apps || []).find((a) => {
    if (norm(a.domain) === want) return true;
    if (Array.isArray(a.self_hosted_domains) && a.self_hosted_domains.some((d) => norm(d) === want)) return true;
    if (Array.isArray(a.destinations) && a.destinations.some((d) => norm(d.uri || d.hostname) === want)) return true;
    return false;
  });
  if (!match) {
    throw new Error("No Access application found for " + hostname + " - create a Public DNS app for it first.");
  }
  appIdCache[want] = match.id;
  return match.id;
}

// ----- User operations -------------------------------------------------------

function serverList() {
  return Object.keys(SERVERS).map((k) => ({ key: k, label: SERVERS[k].label, hostname: SERVERS[k].hostname }));
}

async function listUsers(env) {
  const out = [];
  // KV list() returns at most 1000 keys per page; follow the cursor so every
  // user shows up, not just the first page.
  let cursor;
  do {
    const list = await env.LAUNCHER.list(cursor ? { cursor } : undefined);
    for (const k of list.keys) {
      if (k.name.startsWith("_meta")) continue; // reserved (e.g. version manifest)
      const raw = await env.LAUNCHER.get(k.name);
      if (!raw) continue;
      let cfg = {};
      try { cfg = JSON.parse(raw); } catch (_) { continue; }
      const a = cfg._admin || {};
      out.push({
        code: k.name,
        displayName: cfg.displayName || "",
        servers: a.servers || (cfg.targets || []).map((t) => t.label),
        createdAt: a.createdAt || "",
        lastAccess: a.lastAccess || "",
        lastIp: a.lastIp || "",
        lastLocation: a.lastLocation || "",
        accessCount: a.accessCount || 0,
      });
    }
    cursor = list.list_complete ? undefined : list.cursor;
  } while (cursor);

  // Newest first.
  out.sort((x, y) => (y.createdAt || "").localeCompare(x.createdAt || ""));
  return out;
}

async function createUser(env, body) {
  const displayName = (body.displayName || "").trim();
  const serverKeys = Array.isArray(body.servers) ? body.servers : [];
  if (!displayName) throw new Error("display name is required");
  if (serverKeys.length === 0) throw new Error("pick at least one server");
  for (const key of serverKeys) {
    if (!SERVERS[key]) throw new Error("unknown server: " + key);
  }

  const accountId = env.CF_ACCOUNT_ID;
  const code = slug(displayName) + "-" + rand(4);

  // Resolve each server's Access app up front so we fail before creating a token
  // if an app is missing.
  const appIds = {};
  for (const key of serverKeys) {
    appIds[key] = await resolveAppId(env, SERVERS[key].hostname);
  }

  // 1. Create a service token for this user.
  const token = await cf(env, "POST", `/accounts/${accountId}/access/service_tokens`, {
    name: "launcher-" + code,
  });

  // 2. Attach the token to each chosen server's Access app via a Service Auth
  //    (non_identity) policy.
  const policies = [];
  const targets = [];
  for (const key of serverKeys) {
    const s = SERVERS[key];
    const appId = appIds[key];
    const policy = await cf(env, "POST", `/accounts/${accountId}/access/apps/${appId}/policies`, {
      name: "launcher " + code,
      decision: "non_identity",
      include: [{ service_token: { token_id: token.id } }],
    });
    policies.push({ appId, policyId: policy.id });
    targets.push({ label: s.label, hostname: s.hostname, protocol: s.protocol, mcHost: s.mcHost });
  }

  // 3. Write the KV entry the launcher reads (plus _admin metadata for revoke).
  const value = {
    displayName,
    discordWebhook: env.DISCORD_WEBHOOK || FALLBACK_WEBHOOK,
    serviceTokenId: token.client_id,
    serviceTokenSecret: token.client_secret,
    targets,
    _admin: {
      tokenId: token.id,
      policies,
      servers: serverKeys,
      createdAt: new Date().toISOString(),
    },
  };
  await env.LAUNCHER.put(code, JSON.stringify(value));

  return { code, displayName, servers: serverKeys };
}

// updateUserServers changes which servers an existing code unlocks, reusing the
// user's existing service token: it removes Access policies for servers being
// dropped, adds policies for servers being granted, and rewrites the KV targets.
async function updateUserServers(env, body) {
  const code = (body.code || "").trim();
  const serverKeys = Array.isArray(body.servers) ? body.servers : [];
  if (!code) throw new Error("code is required");
  if (serverKeys.length === 0) throw new Error("pick at least one server");
  for (const key of serverKeys) {
    if (!SERVERS[key]) throw new Error("unknown server: " + key);
  }

  const raw = await env.LAUNCHER.get(code);
  if (!raw) throw new Error("unknown code");
  const value = JSON.parse(raw);
  const admin = value._admin || {};
  const tokenId = admin.tokenId;
  if (!tokenId) throw new Error("this user has no service token on record - revoke and recreate them");

  const accountId = env.CF_ACCOUNT_ID;
  const current = new Set(admin.servers || []);
  const want = new Set(serverKeys);
  let policies = (admin.policies || []).slice();

  // Drop policies for servers no longer granted.
  for (const key of current) {
    if (want.has(key)) continue;
    const appId = await resolveAppId(env, SERVERS[key].hostname);
    const keep = [];
    for (const p of policies) {
      if (p.appId === appId) {
        try {
          await cf(env, "DELETE", `/accounts/${accountId}/access/apps/${appId}/policies/${p.policyId}`);
        } catch (_) {}
      } else {
        keep.push(p);
      }
    }
    policies = keep;
  }

  // Add policies for newly granted servers.
  for (const key of want) {
    if (current.has(key)) continue;
    const appId = await resolveAppId(env, SERVERS[key].hostname);
    const policy = await cf(env, "POST", `/accounts/${accountId}/access/apps/${appId}/policies`, {
      name: "launcher " + code,
      decision: "non_identity",
      include: [{ service_token: { token_id: tokenId } }],
    });
    policies.push({ appId, policyId: policy.id });
  }

  // Rebuild the launcher-visible targets from the final server set.
  value.targets = serverKeys.map((key) => {
    const s = SERVERS[key];
    return { label: s.label, hostname: s.hostname, protocol: s.protocol, mcHost: s.mcHost };
  });
  value._admin = { ...admin, policies, servers: serverKeys };
  await env.LAUNCHER.put(code, JSON.stringify(value));

  return { ok: true, code, servers: serverKeys };
}

async function deleteUser(env, body) {
  const code = (body.code || "").trim();
  if (!code) throw new Error("code is required");
  const raw = await env.LAUNCHER.get(code);
  const meta = raw ? (JSON.parse(raw)._admin || {}) : {};
  const accountId = env.CF_ACCOUNT_ID;

  // Best-effort teardown: remove policies, then the token, then the KV entry.
  for (const p of meta.policies || []) {
    try { await cf(env, "DELETE", `/accounts/${accountId}/access/apps/${p.appId}/policies/${p.policyId}`); } catch (_) {}
  }
  if (meta.tokenId) {
    try { await cf(env, "DELETE", `/accounts/${accountId}/access/service_tokens/${meta.tokenId}`); } catch (_) {}
  }
  await env.LAUNCHER.delete(code);
  return { ok: true, code };
}

// ----- app version (forced update) -------------------------------------------

const VERSION_KEY = "_meta:version";

async function getVersionMeta(env) {
  let m = {};
  try {
    const raw = await env.LAUNCHER.get(VERSION_KEY);
    if (raw) m = JSON.parse(raw);
  } catch (_) {}
  return { latest: m.latest || "", url: m.url || "", notes: m.notes || "", sha256: m.sha256 || "" };
}

async function setVersionMeta(env, body) {
  const latest = (body.latest || "").trim();
  const url = (body.url || "").trim();
  const notes = (body.notes || "").trim();
  const sha256 = (body.sha256 || "").trim().toLowerCase();
  if (latest && !/^\d+(\.\d+)*$/.test(latest)) throw new Error("version should look like 1.2.0");
  if (url && !/^https:\/\//i.test(url)) throw new Error("download URL must start with https://");
  if (sha256 && !/^[a-f0-9]{64}$/.test(sha256)) throw new Error("sha256 must be 64 hex characters");
  await env.LAUNCHER.put(VERSION_KEY, JSON.stringify({ latest, url, notes, sha256 }));
  return { ok: true, latest, url, notes, sha256 };
}

// ----- helpers ---------------------------------------------------------------

function slug(s) {
  return s.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 20) || "user";
}
function rand(n) {
  const a = "abcdefghijklmnopqrstuvwxyz0123456789";
  let out = "";
  const buf = new Uint8Array(n);
  crypto.getRandomValues(buf);
  for (let i = 0; i < n; i++) out += a[buf[i] % a.length];
  return out;
}
function json(obj, status = 200) {
  return new Response(JSON.stringify(obj), {
    status,
    headers: { "content-type": "application/json; charset=utf-8", "cache-control": "no-store" },
  });
}
function html(body) {
  return new Response(body, { headers: { "content-type": "text/html; charset=utf-8" } });
}

// ----- Admin UI --------------------------------------------------------------

const PAGE = `<!DOCTYPE html><html><head><meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>ChromaCube Admin</title>
<style>
  body{font-family:system-ui,Segoe UI,sans-serif;background:#111217;color:#e7e9f0;margin:0;padding:24px;}
  h1{font-size:18px;} h2{font-size:14px;color:#9aa0b2;margin-top:28px;}
  .card{background:#1a1c25;border:1px solid #2c2f3c;border-radius:12px;padding:16px;max-width:720px;margin-bottom:16px;}
  label{display:block;font-size:12px;color:#9aa0b2;margin:8px 0 4px;}
  input[type=text]{width:100%;padding:9px 11px;border-radius:8px;border:1px solid #2c2f3c;background:#21242f;color:#e7e9f0;box-sizing:border-box;}
  .servers label{display:inline-flex;align-items:center;gap:6px;color:#e7e9f0;margin-right:16px;font-size:14px;}
  button{cursor:pointer;border:1px solid #6c8cff;background:#6c8cff;color:#0c1020;padding:9px 14px;border-radius:8px;font-weight:600;margin-top:12px;}
  button.danger{background:transparent;border-color:#ff6b6b;color:#ff6b6b;padding:4px 10px;margin:0;font-weight:600;}
  button.mini{padding:4px 10px;margin:0;font-size:12px;}
  button.ghost{background:transparent;border-color:#2c2f3c;color:#9aa0b2;}
  table{width:100%;border-collapse:collapse;max-width:1040px;}
  th,td{text-align:left;padding:8px;border-bottom:1px solid #2c2f3c;font-size:13px;vertical-align:top;}
  td.muted,.muted{color:#9aa0b2;}
  code{background:#21242f;padding:2px 6px;border-radius:5px;}
  .msg{margin-top:12px;font-size:13px;}
  .ok{color:#38d39f;} .err{color:#ff6b6b;}
</style></head><body>
<h1>ChromaCube - User Access</h1>

<div class="card">
  <h2 style="margin-top:0">Create a user</h2>
  <label>Display name</label>
  <input id="name" type="text" placeholder="Alice"/>
  <label>Servers</label>
  <div class="servers" id="servers"></div>
  <button id="create">Create user</button>
  <div id="msg" class="msg"></div>
</div>

<div class="card">
  <h2 style="margin-top:0">App version (forced update)</h2>
  <p class="muted" style="margin:0 0 6px">Set the latest version and where the new build is hosted. Launchers older than this are locked and cannot connect until the user installs it. Leave version blank to disable the gate.</p>
  <label>Latest version</label>
  <input id="ver" type="text" placeholder="1.2.0"/>
  <label>Hosted .exe URL (https) - where you actually uploaded the build</label>
  <input id="verurl" type="text" placeholder="https://.../ChromaCube.exe"/>
  <label>SHA-256 (optional) - run: certutil -hashfile ChromaCube.exe SHA256</label>
  <input id="versha" type="text" placeholder="64 hex characters, or leave blank"/>
  <button id="saveVer">Save version</button>
  <div id="vermsg" class="msg"></div>
  <p class="muted" style="margin:10px 0 0">Permanent link to share / inside the app (always points to the current build):<br/>
    <code id="permalink">https://api.deforce.site/download</code></p>
</div>

<h2>Users</h2>
<table><thead><tr>
  <th>Code</th><th>Name</th><th>Servers</th><th>Issued</th><th>Last seen</th><th>IP / location</th><th></th>
</tr></thead>
<tbody id="rows"></tbody></table>

<script>
async function api(path, method, body){
  const r = await fetch(path, {method: method||'GET', headers:{'content-type':'application/json'}, body: body?JSON.stringify(body):undefined});
  const d = await r.json().catch(()=>({}));
  if(!r.ok) throw new Error(d.error||('HTTP '+r.status));
  return d;
}
let SERVERS_CACHE = [];
const USERS = {};
function srvLabel(key){ const s = SERVERS_CACHE.find(x=>x.key===key); return s?s.label:key; }
async function loadServers(){
  const {servers} = await api('api/servers');
  SERVERS_CACHE = servers;
  document.getElementById('servers').innerHTML = servers.map(s=>
    '<label><input type="checkbox" value="'+esc(s.key)+'"> '+esc(s.label)+'</label>').join('');
}
async function loadUsers(){
  const {users} = await api('api/users');
  for (const k in USERS) delete USERS[k];
  users.forEach(u=>{ USERS[u.code]=u; });
  document.getElementById('rows').innerHTML = users.map(row).join('') ||
    '<tr><td colspan="7" class="muted">No users yet.</td></tr>';
}
function row(u){
  const ip = u.lastIp
    ? esc(u.lastIp) + (u.lastLocation ? ' <span class="muted">('+esc(u.lastLocation)+')</span>' : '')
    : '<span class="muted">-</span>';
  const seen = u.lastAccess
    ? fmt(u.lastAccess) + (u.accessCount ? ' <span class="muted">('+u.accessCount+'x)</span>' : '')
    : '<span class="muted">never</span>';
  return '<tr id="r-'+esc(u.code)+'">'+
    '<td><code>'+esc(u.code)+'</code></td>'+
    '<td>'+esc(u.displayName)+'</td>'+
    '<td class="srv">'+(u.servers||[]).map(k=>esc(srvLabel(k))).join(', ')+'</td>'+
    '<td>'+(u.createdAt?fmt(u.createdAt):'<span class="muted">-</span>')+'</td>'+
    '<td>'+seen+'</td>'+
    '<td>'+ip+'</td>'+
    '<td style="white-space:nowrap">'+
      '<button class="mini ghost" onclick="edit(\\''+esc(u.code)+'\\')">edit</button> '+
      '<button class="danger" onclick="del(\\''+esc(u.code)+'\\')">revoke</button>'+
    '</td></tr>';
}
window.edit = function(code){
  const u = USERS[code]; if(!u) return;
  const have = new Set(u.servers||[]);
  const cell = document.querySelector('#r-'+code+' .srv');
  if(!cell) return;
  cell.innerHTML =
    SERVERS_CACHE.map(s=>'<label style="display:inline-flex;align-items:center;gap:5px;margin-right:12px">'+
      '<input type="checkbox" value="'+esc(s.key)+'"'+(have.has(s.key)?' checked':'')+'> '+esc(s.label)+'</label>').join('')+
    '<div style="margin-top:8px">'+
      '<button class="mini" onclick="saveServers(\\''+code+'\\')">save</button> '+
      '<button class="mini ghost" onclick="loadUsers()">cancel</button></div>';
};
window.saveServers = async function(code){
  const cell = document.querySelector('#r-'+code+' .srv');
  if(!cell) return;
  const servers = [...cell.querySelectorAll('input:checked')].map(c=>c.value);
  if(servers.length===0){ alert('Pick at least one server.'); return; }
  try{ await api('api/users/servers','POST',{code,servers}); await loadUsers(); }
  catch(e){ alert(e.message); }
};
function fmt(iso){ try{ return new Date(iso).toLocaleString(); }catch(_){ return esc(iso); } }
function esc(s){return String(s||'').replace(/[&<>]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]));}
document.getElementById('create').onclick = async ()=>{
  const name = document.getElementById('name').value.trim();
  const servers = [...document.querySelectorAll('#servers input:checked')].map(c=>c.value);
  const msg = document.getElementById('msg');
  msg.textContent=''; msg.className='msg';
  try{
    const r = await api('api/users','POST',{displayName:name, servers});
    msg.innerHTML = 'Created. Give them this code: <code>'+r.code+'</code>';
    msg.classList.add('ok');
    document.getElementById('name').value='';
    document.querySelectorAll('#servers input:checked').forEach(c=>c.checked=false);
    loadUsers();
  }catch(e){ msg.textContent = e.message; msg.classList.add('err'); }
};
async function del(code){
  if(!confirm('Revoke '+code+'? This deletes their token and access.')) return;
  try{ await api('api/users/delete','POST',{code}); loadUsers(); }
  catch(e){ alert(e.message); }
}
async function loadVersion(){
  const m = await api('api/version');
  document.getElementById('ver').value = m.latest||'';
  document.getElementById('verurl').value = m.url||'';
  document.getElementById('versha').value = m.sha256||'';
}
document.getElementById('saveVer').onclick = async ()=>{
  const latest = document.getElementById('ver').value.trim();
  const url = document.getElementById('verurl').value.trim();
  const sha256 = document.getElementById('versha').value.trim();
  const msg = document.getElementById('vermsg'); msg.textContent=''; msg.className='msg';
  try{ await api('api/version','POST',{latest,url,sha256}); msg.textContent='Saved. Older launchers will now be prompted to update.'; msg.classList.add('ok'); }
  catch(e){ msg.textContent=e.message; msg.classList.add('err'); }
};
loadServers(); loadUsers(); loadVersion();
</script>
</body></html>`;
