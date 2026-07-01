// ChromaCube Launcher frontend. Backend methods are at window.go.main.App.*,
// events via window.runtime.EventsOn. No build step or framework.

const targets = new Map(); // id -> last known TargetState
const pings = new Map(); // id -> latency ms (or -1)
let binaryReady = false;

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
    targets.set(s.id, s);
    renderCard(s);
    refreshConnectButton();
  });

  window.runtime.EventsOn("binary", (b) => {
    binaryReady = b.state === "ready";
    updateBinaryPill(b);
    refreshConnectButton();
  });

  window.runtime.EventsOn("ping", (p) => {
    pings.set(p.id, p.ms);
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

  App.GetState().then(renderFromState);
}

// Show the post-update popup with the release notes.
function showWhatsNew(d) {
  if (!d) return;
  const title = document.getElementById("whatsnew-title");
  const notes = document.getElementById("whatsnew-notes");
  const ver = d.tag || (d.version ? "v" + d.version : "");
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
    document.getElementById("subtitle").textContent = "Private Minecraft network access";
    return;
  }
  codeScreen.classList.add("hidden");
  mainUI.classList.remove("hidden");

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
  document.getElementById("server-list").innerHTML = "";
  (state.targets || []).forEach((t) => {
    targets.set(t.id, t);
    renderCard(t);
  });
  refreshConnectButton();
}

// ---------- Overall network state ----------

function overallState() {
  const list = [...targets.values()].filter((t) => t.id !== "config-error");
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

  card.innerHTML = `
    <div class="card-info">
      <h3>${escapeHtml(t.label)}</h3>
      ${!isError ? `
        <div class="card-addr">
          <span>Address:</span>
          <code class="addr-code">${escapeHtml(t.localAddr)}</code>
          <button class="copy-btn" data-addr="${escapeHtml(t.localAddr)}">copy</button>
        </div>` : ""}
    </div>
    <div class="status">
      <span class="ping"></span>
      <span class="dot ${t.status}"></span>
      <span class="status-text ${t.status}" title="${escapeHtml(statusText)}">${escapeHtml(truncate(statusText, 38))}</span>
    </div>
  `;

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
