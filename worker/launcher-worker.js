// Per-user launcher config endpoint (Cloudflare Worker).
//   GET /?code=<code>   -> that user's config from the LAUNCHER KV namespace
//   GET /version        -> version manifest (for the in-app updater)
//   GET /download       -> redirect to the current build
// Users are managed by the admin panel (admin/); KV is the source of truth.

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
    // last-access + IP. We write the FULL value (with _admin) back to KV in the
    // background via waitUntil so it never delays the launcher's response.
    const admin = cfg._admin || (cfg._admin = {});
    const loc = request.cf || {};
    admin.lastAccess = new Date().toISOString();
    admin.lastIp = request.headers.get("CF-Connecting-IP") || "";
    admin.lastLocation = [loc.city, loc.region, loc.country].filter(Boolean).join(", ");
    admin.accessCount = (admin.accessCount || 0) + 1;
    if (ctx && ctx.waitUntil) {
      ctx.waitUntil(env.LAUNCHER.put(code, JSON.stringify(cfg)));
    }

    // Strip admin-only bookkeeping (token id / policy ids / access log) before
    // handing the config to the launcher; it only needs displayName/webhook/
    // token/targets.
    const out = { ...cfg };
    delete out._admin;
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
