// ChromaCube Launcher frontend. Backend methods are at window.go.main.App.*,
// events via window.runtime.EventsOn. No build step or framework.

const targets = new Map(); // id -> last known TargetState
const pings = new Map(); // id -> latency ms (or -1)
const meta = new Map(); // id -> { motd, favicon, playersOnline, playersMax, version }
const pendingMapOpen = new Set(); // web target ids waiting to auto-open once connected
const removedIds = new Set(); // ids revoked live; ignore any late status for them
let binaryReady = false;
let whatsNewShown = false; // the post-update popup is shown once per launch

// Characters used to animate obfuscated (§k) MOTD text, à la Minecraft.
const OBF_CHARS = "abcdefghijklmnopqrstuvwxyz0123456789!?#@%&*+=";

// Splash-text lines shown under the title on the code-entry screen, à la the
// Minecraft main menu. One is picked at random per launch (module load), not
// re-rolled on every re-render, so it stays put for the whole session.
const SPLASH_TEXTS = [
  "Time to play!",
  "Also try connecting!",
  "100% cube-approved!",
  "Now with more chroma!",
  "Diamonds not included!",
  "Tunnels, not portals!",
  "No lag, just love!",
  "Connect and conquer!",
  "Cloudflare-powered!",
  "Fresh from the nether!",
  "Server's up, jump in!",
  "Loaded with chunks of fun!",
  "Fewer bugs than a farm!",
  "Made of pure chroma!",
  "Now rendering reality!",
  "Boots on, ready to go!",
  "Tick tock, next tick!",
  "Whitelisted and ready!",
  "Cubed for your pleasure!",
  "Connecting the blocks!",
  "Better than bedrock!",
  "Straight outta spawn!",
  "One click, zero grief!",
  "Chroma-tastic!",
  "Now on Bedrock too!",
];
const splashText = SPLASH_TEXTS[Math.floor(Math.random() * SPLASH_TEXTS.length)];

function whenReady(fn) {
  if (window.runtime && window.go && window.go.main && window.go.main.App) {
    fn();
  } else {
    setTimeout(() => whenReady(fn), 50);
  }
}

whenReady(init);

function init() {
  const App = window.go.main.App;

  window.runtime.EventsOn("status", (s) => {
    // A tunnel revoked live may still emit a late "stopped": never resurrect it.
    if (removedIds.has(s.id)) return;
    targets.set(s.id, s);
    // If the user asked to open a web map before it was up, open it now that it
    // has connected. Clear the pending flag first so the card re-renders with the
    // normal "Open Map" label rather than "Opening...".
    const doOpen = s.web && s.status === "connected" && pendingMapOpen.has(s.id);
    if (doOpen) pendingMapOpen.delete(s.id);
    renderCard(s);
    refreshConnectButton();
    if (doOpen) window.go.main.App.OpenWeb(s.id).catch((e) => console.error(e));
  });

  window.runtime.EventsOn("binary", (b) => {
    binaryReady = b.state === "ready";
    updateBinaryPill(b);
    refreshConnectButton();
  });

  window.runtime.EventsOn("ping", (p) => {
    pings.set(p.id, p.ms);
    // A reachable ping carries the server's live MOTD/icon/players; cache it and
    // paint it into the card. Failed pings (ms < 0) keep the last known values.
    if (p.ms >= 0) {
      meta.set(p.id, {
        motd: p.motd || [],
        favicon: p.favicon || "",
        playersOnline: p.playersOnline || 0,
        playersMax: p.playersMax || 0,
        version: p.version || "",
      });
      updateMotd(p.id);
    }
    updatePing(p.id);
  });

  window.runtime.EventsOn("update", (d) => applyUpdate(d));

  window.runtime.EventsOn("updateProgress", (p) => {
    const el = document.getElementById("update-status");
    if (!p || !p.message) {
      el.classList.add("hidden");
      return;
    }
    el.textContent = p.message;
    el.classList.remove("hidden");
  });

  window.runtime.EventsOn("updated", (d) => showWhatsNew(d));

  window.runtime.EventsOn("auth", (a) => showAuthBanner(a.url));

  window.runtime.EventsOn("hostsError", (h) => {
    document.getElementById("hosts-message").textContent = h.message;
    document.getElementById("hosts-banner").classList.remove("hidden");
  });

  // A server was removed from this user's access live: drop its card + state.
  window.runtime.EventsOn("removed", (d) => removeCard(d && d.id));

  // Access was revoked entirely: show a notice on the code screen. The backend
  // emits "reloaded" right after (needsCode = true) which swaps to that screen.
  window.runtime.EventsOn("revoked", (d) => showRevokedNotice(d && d.message));

  // Backend re-renders after a code is submitted or cleared.
  window.runtime.EventsOn("reloaded", (state) => renderFromState(state));

  // Single network button toggles all tunnels.
  document.getElementById("connect-btn").onclick = () => {
    const state = overallState();
    if (state === "connected" || state === "connecting") {
      hideAuthBanner();
      App.DisconnectAll();
    } else {
      App.ConnectAll();
    }
  };

  // Access-code entry.
  document.getElementById("code-submit").onclick = submitCode;
  document.getElementById("code-input").addEventListener("keydown", (e) => {
    if (e.key === "Enter") submitCode();
  });
  document.getElementById("change-code").onclick = (e) => {
    e.preventDefault();
    App.ClearAccessCode();
  };

  // Settings tab.
  document.getElementById("settings-btn").onclick = openSettings;
  document.getElementById("settings-back").onclick = () => App.GetState().then(renderFromState);
  document.getElementById("autostart-toggle").onchange = (e) => {
    App.SetAutostart(e.target.checked).catch((err) => console.error(err));
  };

  // Forced-update screen: download + swap + relaunch, all in-app.
  document.getElementById("update-now").onclick = () => {
    const btn = document.getElementById("update-now");
    const status = document.getElementById("update-status");
    btn.disabled = true;
    btn.textContent = "Updating...";
    status.classList.remove("hidden");
    status.textContent = "Starting update...";
    // On success the app relaunches, so there is no success branch to handle.
    App.SelfUpdate().catch((e) => {
      status.textContent = String(e);
      btn.disabled = false;
      btn.textContent = "Try again";
    });
  };

  document.getElementById("whatsnew-close").onclick = () => {
    document.getElementById("whatsnew-screen").classList.add("hidden");
  };

  // Animate any obfuscated (§k) MOTD glyphs like Minecraft does.
  setInterval(scrambleObfuscated, 90);

  App.GetState().then(renderFromState);
}

// scrambleObfuscated replaces the glyphs of every obfuscated MOTD span with fresh
// random characters (keeping spaces and length), giving the flickering look
// Minecraft uses for §k text.
function scrambleObfuscated() {
  document.querySelectorAll(".motd-obf").forEach((el) => {
    const orig = el.dataset.text || "";
    let out = "";
    for (const ch of orig) {
      out += ch === " " ? " " : OBF_CHARS[(Math.random() * OBF_CHARS.length) | 0];
    }
    el.textContent = out;
  });
}

// Show the post-update popup with the release notes.
function showWhatsNew(d) {
  // Delivered both by the "updated" event and by GetState (whatsNew) so a slow or
  // early subscription can never miss it; show it only once per launch.
  if (!d || whatsNewShown) return;
  whatsNewShown = true;
  const title = document.getElementById("whatsnew-title");
  const notes = document.getElementById("whatsnew-notes");
  // Prefer this build's baked version (authoritative) over any GitHub tag.
  const ver = d.version ? "v" + d.version : (d.tag || "");
  title.textContent = ver ? "Updated to " + ver : "Updated";
  const body = (d.notes || "").trim();
  notes.textContent = body || "The launcher was updated to the latest version.";
  document.getElementById("whatsnew-screen").classList.remove("hidden");
}

function openSettings() {
  const App = window.go.main.App;
  document.getElementById("code-screen").classList.add("hidden");
  document.getElementById("main-ui").classList.add("hidden");
  document.getElementById("settings-screen").classList.remove("hidden");
  App.GetSettings().then((s) => {
    document.getElementById("autostart-toggle").checked = !!s.autostart;
  });
}

function submitCode() {
  const App = window.go.main.App;
  const input = document.getElementById("code-input");
  const btn = document.getElementById("code-submit");
  const err = document.getElementById("code-error");
  const code = input.value.trim();
  if (!code) return;

  err.classList.add("hidden");
  btn.disabled = true;
  btn.textContent = "Checking...";

  App.SubmitAccessCode(code)
    .then(() => {
      // Success: backend emits "reloaded"; GetState also refreshes here.
      App.GetState().then(renderFromState);
    })
    .catch((e) => {
      err.textContent = String(e);
      err.classList.remove("hidden");
    })
    .finally(() => {
      btn.disabled = false;
      btn.textContent = "Continue";
    });
}

// Apply a full AppState snapshot: choose the code screen vs the main UI, then
// render everything.
function renderFromState(state) {
  binaryReady = state.binaryReady;
  // Post-update notes ride along on the state snapshot too (not just the event),
  // so they show even if the "updated" event fired before we subscribed.
  if (state.whatsNew) showWhatsNew(state.whatsNew);

  applyUpdate({
    required: state.updateRequired,
    url: state.updateURL,
    latest: state.latestVersion,
    current: state.appVersion,
  });

  // Show the running version (footer + settings).
  const verText = state.appVersion ? "v" + state.appVersion : "";
  document.getElementById("app-version").textContent = verText;
  document.getElementById("settings-version").textContent = verText
    ? "ChromaCube Launcher " + verText
    : "";

  document.getElementById("settings-screen").classList.add("hidden");
  const codeScreen = document.getElementById("code-screen");
  const mainUI = document.getElementById("main-ui");
  if (state.needsCode) {
    codeScreen.classList.remove("hidden");
    mainUI.classList.add("hidden");
    document.getElementById("subtitle").textContent = splashText;
    return;
  }
  codeScreen.classList.add("hidden");
  mainUI.classList.remove("hidden");
  // We're online: clear any lingering revocation notice.
  const notice = document.getElementById("code-notice");
  if (notice) notice.classList.add("hidden");

  // Header subtitle shows who this build is for.
  if (state.displayName) {
    document.getElementById("subtitle").textContent = state.displayName;
  }
  // "Change code" only makes sense for code-mode builds.
  document.getElementById("change-code").classList.toggle("hidden", !state.codeMode);

  updateBinaryPill({
    state: state.binaryReady ? "ready" : "downloading",
    message: state.binaryStatus,
    progress: 0,
  });

  // Rebuild the card list from scratch (servers may have changed after a reload).
  targets.clear();
  meta.clear();
  removedIds.clear(); // fresh target set: previously-removed ids no longer apply
  document.getElementById("server-list").innerHTML = "";
  (state.targets || []).forEach((t) => {
    targets.set(t.id, t);
    renderCard(t);
  });
  refreshConnectButton();
}

// Remove a single server card and its cached state (used when access to that
// server is revoked live).
function removeCard(id) {
  if (!id) return;
  removedIds.add(id);
  targets.delete(id);
  pings.delete(id);
  meta.delete(id);
  pendingMapOpen.delete(id);
  const card = document.getElementById("card-" + id);
  if (card) card.remove();
  refreshConnectButton();
}

// Open a live web map. If its tunnel is already connected we open it right away;
// otherwise we connect it and let the "status" handler open it automatically the
// moment it comes up (the button shows "Opening..." until then).
function openMap(id) {
  const App = window.go.main.App;
  const t = targets.get(id);
  if (t && t.status === "connected") {
    App.OpenWeb(id).catch((e) => console.error(e));
    return;
  }
  pendingMapOpen.add(id);
  if (t) renderCard(t);
  App.Connect(id).catch((e) => {
    pendingMapOpen.delete(id);
    if (t) renderCard(t);
    console.error(e);
  });
}

// Show the "access revoked" notice on the code-entry screen.
function showRevokedNotice(message) {
  const notice = document.getElementById("code-notice");
  if (!notice) return;
  notice.textContent = message || "Your access was revoked. Enter a new code to continue.";
  notice.classList.remove("hidden");
}

// ---------- Overall network state ----------

function overallState() {
  // The map (a web target) is an independent convenience, not part of the game
  // network, so it never drives the Connect/Disconnect button.
  const list = [...targets.values()].filter((t) => t.id !== "config-error" && !t.web);
  if (list.length === 0) return "idle";
  const statuses = list.map((t) => t.status);
  if (statuses.some((s) => s === "starting" || s === "waiting_auth")) return "connecting";
  if (statuses.some((s) => s === "connected")) return "connected";
  if (statuses.some((s) => s === "error")) return "error";
  return "idle";
}

function refreshConnectButton() {
  const btn = document.getElementById("connect-btn");
  const state = overallState();

  btn.classList.remove("btn-primary", "btn-danger", "btn-busy");
  switch (state) {
    case "connected":
      btn.textContent = "Disconnect from network";
      btn.classList.add("btn-danger");
      btn.disabled = false;
      break;
    case "connecting":
      btn.textContent = "Connecting...";
      btn.classList.add("btn-busy");
      btn.disabled = false;
      break;
    default:
      btn.textContent = "Connect to network";
      btn.classList.add("btn-primary");
      btn.disabled = !binaryReady;
  }
}

// ---------- Per-server status cards ----------

const STATUS_LABEL = {
  idle: "Not connected",
  starting: "Connecting...",
  waiting_auth: "Waiting for sign-in...",
  connected: "Connected",
  error: "Error",
  stopped: "Not connected",
};

function renderCard(t) {
  // A web target (the live map) is not a card of its own: it surfaces as an
  // "Open Map" button embedded in its coupled server's card. Re-render that parent
  // so the button reflects the map's latest state, and clear any stray map card.
  if (t.web) {
    const stray = document.getElementById("card-" + t.id);
    if (stray) stray.remove();
    const parent = parentOfWeb(t);
    if (parent) {
      renderCard(parent);
    } else {
      renderWebCard(t); // no coupled server found: fall back to a standalone card
    }
    return;
  }

  const list = document.getElementById("server-list");
  let card = document.getElementById("card-" + t.id);
  if (!card) {
    card = document.createElement("div");
    card.className = "card";
    card.id = "card-" + t.id;
    list.appendChild(card);
  }

  const active = t.status === "connected" || t.status === "starting" || t.status === "waiting_auth";
  const label = STATUS_LABEL[t.status] || t.status;
  const statusText = active && t.message ? t.message : label;
  const isError = t.id === "config-error";

  // A coupled web target (the live map) becomes an "Open Map" button placed just
  // left of the status block, which stays pinned to the card's right edge.
  const web = webChildOf(t);

  card.innerHTML = `
    <img class="server-icon hidden" alt="" />
    <div class="card-info">
      <div class="card-name">
        <h3>${escapeHtml(t.label)}</h3>
        <span class="players"></span>
      </div>
      ${!isError ? `
        <div class="card-addr">
          <span>Address:</span>
          <code class="addr-code">${escapeHtml(t.localAddr)}</code>
          <button class="copy-btn" data-addr="${escapeHtml(t.localAddr)}">copy</button>
        </div>` : ""}
    </div>
    <div class="motd"></div>
    ${web ? mapButtonHtml(web) : ""}
    <div class="status">
      <span class="ping"></span>
      <span class="dot ${t.status}"></span>
      <span class="status-text ${t.status}" title="${escapeHtml(statusText)}">${escapeHtml(truncate(statusText, 38))}</span>
    </div>
  `;

  updateMotd(t.id);
  updatePing(t.id);

  const copy = card.querySelector(".copy-btn");
  if (copy) {
    copy.onclick = () => {
      navigator.clipboard.writeText(copy.dataset.addr).then(() => {
        copy.textContent = "copied!";
        setTimeout(() => (copy.textContent = "copy"), 1200);
      });
    };
  }

  const open = card.querySelector(".map-open");
  if (open && web) open.onclick = () => openMap(web.id);
}

// The coupled Minecraft server for a web target (matched by hostname), or
// undefined if that server isn't in the current access set.
function parentOfWeb(web) {
  return [...targets.values()].find((x) => !x.web && x.hostname === web.coupledTo);
}

// The web target (if any) coupled to this Minecraft server.
function webChildOf(t) {
  return [...targets.values()].find((x) => x.web && x.coupledTo === t.hostname);
}

// The "Open Map" button HTML. It shows "Opening..." while a connect-then-open is
// in flight (see openMap).
function mapButtonHtml(web) {
  const opening = pendingMapOpen.has(web.id);
  const lbl = opening ? "Opening..." : "Open Map";
  return `<button class="btn btn-primary map-open"${opening ? " disabled" : ""}>${lbl}</button>`;
}

// Fallback for a web target with no coupled server in the access set: render it as
// its own standalone card so the map is still reachable.
function renderWebCard(t) {
  const list = document.getElementById("server-list");
  let card = document.getElementById("card-" + t.id);
  if (!card) {
    card = document.createElement("div");
    card.className = "card";
    card.id = "card-" + t.id;
    list.appendChild(card);
  }
  const active = t.status === "connected" || t.status === "starting" || t.status === "waiting_auth";
  const label = STATUS_LABEL[t.status] || t.status;
  const statusText = active && t.message ? t.message : label;
  card.innerHTML = `
    <img class="server-icon hidden" alt="" />
    <div class="card-info">
      <div class="card-name">
        <h3>${escapeHtml(t.label)}</h3>
        <span class="players">Live map</span>
      </div>
      <div class="card-addr">
        <span>Opens in your browser at</span>
        <code class="addr-code">${escapeHtml(t.webURL || t.localAddr)}</code>
      </div>
    </div>
    <div class="motd"></div>
    ${mapButtonHtml(t)}
    <div class="status">
      <span class="ping"></span>
      <span class="dot ${t.status}"></span>
      <span class="status-text ${t.status}" title="${escapeHtml(statusText)}">${escapeHtml(truncate(statusText, 24))}</span>
    </div>
  `;
  const open = card.querySelector(".map-open");
  if (open) open.onclick = () => openMap(t.id);
}

// Paint the cached MOTD, server icon and player count onto a card. The cached
// values persist across a disconnect (we keep showing the last MOTD until the
// next successful ping refreshes it), so this renders whenever we have data.
function updateMotd(id) {
  const card = document.getElementById("card-" + id);
  if (!card) return;
  const m = meta.get(id);
  const icon = card.querySelector(".server-icon");
  const motdEl = card.querySelector(".motd");
  const playersEl = card.querySelector(".players");

  if (!m) {
    if (icon) { icon.classList.add("hidden"); icon.removeAttribute("src"); }
    if (motdEl) motdEl.innerHTML = "";
    if (playersEl) playersEl.textContent = "";
    return;
  }
  if (icon) {
    if (m.favicon && m.favicon.startsWith("data:image/")) {
      icon.src = m.favicon;
      icon.classList.remove("hidden");
    } else {
      icon.classList.add("hidden");
      icon.removeAttribute("src");
    }
  }
  if (motdEl) motdEl.innerHTML = renderMotdHtml(m.motd);
  if (playersEl) {
    playersEl.textContent = m.playersMax > 0 ? `${m.playersOnline}/${m.playersMax} online` : "";
  }
}

// Unicode small-capital letters (ᴄ ʜ ʀ …) that decorative MOTDs use. The bundled
// Minecraft font has no glyphs for these code points, so we map them back to real
// capitals and render them as small-caps in the Minecraft font (see .sc in CSS).
const SMALL_CAPS = {
  "ᴀ": "A", "ʙ": "B", "ᴄ": "C", "ᴅ": "D", "ᴇ": "E",
  "ꜰ": "F", "ɢ": "G", "ʜ": "H", "ɪ": "I", "ᴊ": "J",
  "ᴋ": "K", "ʟ": "L", "ᴍ": "M", "ɴ": "N", "ᴏ": "O",
  "ᴘ": "P", "ꞯ": "Q", "ʀ": "R", "ꜱ": "S", "ᴛ": "T",
  "ᴜ": "U", "ᴠ": "V", "ᴡ": "W", "ʏ": "Y", "ᴢ": "Z",
};

function isSmallCap(ch) {
  return Object.prototype.hasOwnProperty.call(SMALL_CAPS, ch);
}

// A run of this many spaces (or more) is treated as an intended line break:
// servers pad the MOTD with spaces so it wraps into two centred lines in-game.
const MOTD_GAP = 5;

// Build safe HTML for a MOTD from the backend's styled spans, mimicking how
// Minecraft draws it in the server list: up to two centred lines. There is
// usually no newline - the "second line" comes from a long run of padding spaces
// wrapping at the fixed MOTD width - so we split on newlines OR long space runs,
// trim each line's edge spaces, and centre them. Text is escaped and colours are
// whitelisted to valid hex so a hostile MOTD stays inert.
function renderMotdHtml(spans) {
  if (!Array.isArray(spans) || spans.length === 0) return "";
  // Flatten to styled characters.
  const chars = [];
  for (const s of spans) {
    for (const ch of String(s.text == null ? "" : s.text)) chars.push({ ch, s });
  }
  // Break into lines at newlines or long space runs.
  const lines = [];
  let line = [];
  let run = 0;
  for (const c of chars) {
    if (c.ch === "\n") { lines.push(line); line = []; run = 0; continue; }
    if (c.ch === " ") { run++; line.push(c); continue; }
    if (run >= MOTD_GAP) {
      while (line.length && line[line.length - 1].ch === " ") line.pop();
      lines.push(line);
      line = [];
    }
    run = 0;
    line.push(c);
  }
  lines.push(line);
  // Trim edge spaces, drop empty lines, keep the first two (like Minecraft).
  const cleaned = lines.map(trimSpaces).filter((l) => l.length > 0).slice(0, 2);
  return cleaned.map((l) => `<div class="motd-line">${charsToHtml(l)}</div>`).join("");
}

function trimSpaces(line) {
  let a = 0;
  let b = line.length;
  while (a < b && line[a].ch === " ") a++;
  while (b > a && line[b - 1].ch === " ") b--;
  return line.slice(a, b);
}

// Group consecutive characters that share a style (and small-caps flag) into one
// span each, applying the small-caps mapping where needed.
function charsToHtml(line) {
  let html = "";
  let i = 0;
  while (i < line.length) {
    const s = line[i].s;
    const sc = isSmallCap(line[i].ch);
    let text = "";
    while (i < line.length && line[i].s === s && isSmallCap(line[i].ch) === sc) {
      text += sc ? SMALL_CAPS[line[i].ch] : line[i].ch;
      i++;
    }
    html += spanHtml(s, text, sc);
  }
  return html;
}

// spanHtml renders one run of text with its colour/formatting. smallCaps wraps it
// so the mapped capitals show as Minecraft-style small caps.
function spanHtml(s, text, smallCaps) {
  if (text === "") return "";
  const safe = escapeHtml(text);
  let style = "";
  const color = cssColor(s.color);
  if (color) style += `color:${color};`;
  if (s.bold) style += "font-weight:700;";
  if (s.italic) style += "font-style:italic;";
  const deco = [];
  if (s.underline) deco.push("underline");
  if (s.strike) deco.push("line-through");
  if (deco.length) style += `text-decoration:${deco.join(" ")};`;
  const cls = [];
  if (smallCaps) cls.push("sc");
  if (s.obf) cls.push("motd-obf");
  const classAttr = cls.length ? ` class="${cls.join(" ")}"` : "";
  // Store the real text for the obfuscated scramble animation.
  const dataAttr = s.obf ? ` data-text="${escapeHtml(text)}"` : "";
  return `<span${classAttr}${dataAttr} style="${style}">${safe}</span>`;
}

// cssColor returns c only if it is a valid #hex colour, else "" (drops anything
// that could be used to inject extra CSS/attributes).
function cssColor(c) {
  return typeof c === "string" && /^#[0-9a-fA-F]{3,8}$/.test(c) ? c : "";
}

// Show the latest ping on a card (only meaningful while connected).
function updatePing(id) {
  const card = document.getElementById("card-" + id);
  if (!card) return;
  const el = card.querySelector(".ping");
  if (!el) return;
  const t = targets.get(id);
  const ms = pings.get(id);
  if (!t || t.status !== "connected" || ms == null || ms < 0) {
    el.textContent = "";
    el.className = "ping";
    return;
  }
  el.textContent = ms + " ms";
  el.className = "ping " + (ms < 80 ? "good" : ms < 160 ? "ok" : "bad");
}

function updateBinaryPill(b) {
  const pill = document.getElementById("binary-pill");
  pill.className = "pill";
  if (b.state === "ready") {
    pill.classList.add("pill-green");
    pill.textContent = "Ready";
  } else if (b.state === "error") {
    pill.classList.add("pill-red");
    pill.textContent = b.message || "cloudflared error";
  } else {
    pill.classList.add("pill-amber");
    const pct = b.progress ? ` (${b.progress}%)` : "";
    pill.textContent = (b.message || "Downloading cloudflared...") + pct;
  }
}

// ---------- Forced-update gate ----------

// Show or hide the full-screen "update required" overlay. While shown it covers
// the entire UI so the app cannot be used until the user installs the new build.
function applyUpdate(d) {
  const screen = document.getElementById("update-screen");
  if (!d || !d.required) {
    screen.classList.add("hidden");
    return;
  }
  document.getElementById("update-current").textContent = "v" + (d.current || "?");
  document.getElementById("update-latest").textContent = "v" + (d.latest || "?");
  const dl = document.getElementById("update-download");
  dl.classList.toggle("hidden", !d.url);
  dl.onclick = (e) => {
    e.preventDefault();
    if (d.url) window.runtime.BrowserOpenURL(d.url);
  };
  screen.classList.remove("hidden");
}

// ---------- Auth banner ----------

function showAuthBanner(url) {
  const banner = document.getElementById("auth-banner");
  const link = document.getElementById("auth-link");
  link.href = url;
  link.onclick = (e) => {
    e.preventDefault();
    window.runtime.BrowserOpenURL(url);
  };
  banner.classList.remove("hidden");
}

function hideAuthBanner() {
  document.getElementById("auth-banner").classList.add("hidden");
}

// ---------- Utils ----------

function escapeHtml(s) {
  return String(s == null ? "" : s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function truncate(s, n) {
  s = String(s || "");
  return s.length > n ? s.slice(0, n - 1) + "..." : s;
}
