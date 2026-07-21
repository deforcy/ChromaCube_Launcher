// Per-user launcher config endpoint (Cloudflare Worker).
//   GET /?code=<code>   -> that user's config from the LAUNCHER KV namespace
//   GET /version        -> version manifest (for the in-app updater)
//   GET /download       -> redirect to the current build
// Users are managed by the admin panel (admin/); KV is the source of truth.

// The web map follows the ChromaCube grant. Rather than writing the map target
// into every user's stored config (a KV write per user, and easy to miss for
// users created before the map existed), we INJECT it at read time for anyone
// whose config already unlocks ChromaCube. The map's Access policy is still
// attached to the token by the admin panel; this only adds the launcher-visible
// target so the Open Map button appears. Keep these fields in sync with
// admin-worker.js `mapTarget()`.
const MAP_COUPLED_HOST = "chromacube.deforce.site";
const MAP_TARGET = {
  label: "ChromaCube Map",
  hostname: "map.deforce.site",
  protocol: "tcp",
  mcHost: "map.chromacube.localhost",
  web: true,
  webPort: 80,
  coupledTo: MAP_COUPLED_HOST,
};

// How stale the recorded access has to be before we write a fresh _access entry.
// The launcher re-fetches config every 30s; without this throttle each active
// launcher would burn ~2,880 KV writes/day (free tier is 1,000/day) and lock out
// all writes, including admin edits.
const ACCESS_WRITE_THROTTLE_MS = 15 * 60 * 1000;

export default {
  async fetch(request, env, ctx) {
    if (request.method !== "GET") {
      return json({ error: "method not allowed" }, 405);
    }

    const url = new URL(request.url);

    if (!env.LAUNCHER) {
      return json({ error: "server not configured (no KV binding)" }, 500);
    }

    const cleanPath = url.pathname.replace(/\/+$/, "");

    // Version + download are both backed by the same admin-managed manifest in
    // KV (_meta:version = { latest, url, notes }). `url` is wherever the current
    // build is actually hosted; everything points at the stable /download link.
    if (cleanPath.endsWith("/version") || cleanPath.endsWith("/download")) {
      let meta = {};
      try {
        const raw = await env.LAUNCHER.get("_meta:version");
        if (raw) meta = JSON.parse(raw);
      } catch (_) {}

      // Permanent download link: always 302 to the current hosted build.
      if (cleanPath.endsWith("/download")) {
        if (!meta.url) return json({ error: "no download configured yet" }, 404);
        return Response.redirect(meta.url, 302);
      }

      // Version manifest the app polls on launch. We hand back the STABLE
      // /download link (not the raw host) so the app always uses the permanent
      // URL, and the gate only turns on once a download actually exists.
      const downloadLink = meta.url ? `${url.protocol}//${url.host}/download` : "";
      return json({ latest: meta.latest || "", url: downloadLink, notes: meta.notes || "", sha256: meta.sha256 || "" }, 200);
    }

    const code = (url.searchParams.get("code") || "").trim();
    if (!code) {
      return json({ error: "missing code" }, 400);
    }

    const raw = await env.LAUNCHER.get(code);
    if (!raw) {
      return json({ error: "unknown code" }, 404);
    }

    let cfg;
    try {
      cfg = JSON.parse(raw);
    } catch (_) {
      return json({ error: "stored config is corrupt" }, 500);
    }

    // Record when/where this code was last used so the admin panel can show
    // last-access + IP. This goes to a SEPARATE key ("_access:<code>"), never the
    // config value itself: rewriting the config here would race the admin panel
    // under KV's eventual consistency and could clobber a just-saved server change.
    if (ctx && ctx.waitUntil) {
      const loc = request.cf || {};
      const ip = request.headers.get("CF-Connecting-IP") || "";
      ctx.waitUntil(
        (async () => {
          let acc = {};
          try {
            const r = await env.LAUNCHER.get("_access:" + code);
            if (r) acc = JSON.parse(r);
          } catch (_) {}
          // Throttle: only write if it's been a while OR the IP changed. The
          // launcher polls every 30s, so writing every time would exhaust the
          // daily KV write quota and lock out admin edits.
          const last = Date.parse(acc.lastAccess || "") || 0;
          const stale = Date.now() - last >= ACCESS_WRITE_THROTTLE_MS;
          const ipChanged = ip && ip !== acc.lastIp;
          if (!stale && !ipChanged) return;
          acc.lastAccess = new Date().toISOString();
          acc.lastIp = ip;
          acc.lastLocation = [loc.city, loc.region, loc.country].filter(Boolean).join(", ");
          acc.accessCount = (acc.accessCount || 0) + 1;
          try {
            await env.LAUNCHER.put("_access:" + code, JSON.stringify(acc));
          } catch (_) {
            // Out of daily write budget (or KV hiccup): drop it. Access logging
            // is best-effort and must never break the config response.
          }
        })()
      );
    }

    // Strip admin-only bookkeeping (token id / policy ids / access log) before
    // handing the config to the launcher; it only needs displayName/webhook/
    // token/targets.
    const out = { ...cfg };
    delete out._admin;

    // Auto-attach the web map for anyone who unlocks ChromaCube, without needing
    // a per-user KV write. If chromacube is present and the map target isn't
    // already there, add it so the launcher shows the Open Map button.
    const targets = Array.isArray(out.targets) ? out.targets.slice() : [];
    const hasChromacube = targets.some((t) => t && t.hostname === MAP_COUPLED_HOST);
    const hasMap = targets.some((t) => t && (t.web === true || t.hostname === MAP_TARGET.hostname));
    if (hasChromacube && !hasMap) {
      targets.push({ ...MAP_TARGET });
      out.targets = targets;
    }
    return json(out, 200);
  },
};

function json(obj, status) {
  return new Response(JSON.stringify(obj), {
    status,
    headers: {
      "content-type": "application/json; charset=utf-8",
      "cache-control": "no-store",
    },
  });
}
