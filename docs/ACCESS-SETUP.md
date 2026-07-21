# Multi-user access setup

This guide covers the per-user system: each friend-group gets its own `.exe` with
a code baked in, which fetches its server list (and a Cloudflare Access service
token) from a Worker on `deforce.site`. Real isolation is enforced by Cloudflare
Access policies, not by the app's UI.

```
 friend-group .exe (code: sv-7h3k)
        |
        |  GET deforce.site/api/launcher?code=sv-7h3k
        v
   Cloudflare Worker  --->  { displayName, discordWebhook, serviceToken, targets[] }
        |
        v
 cloudflared access tcp ... (TUNNEL_SERVICE_TOKEN_ID/SECRET)
        |
        v
 Cloudflare Access policy on chromacube.deforce.site
   "Service Auth: allow token = Survival" --> only that group gets in
```

There are three pieces to set up: **service tokens + policies** (the real gate),
the **Worker** (what each code is told), and the **per-group builds**.

---

## 1. Cloudflare Access: service tokens (the real security)

Hiding servers in the app is not security. This step is what actually stops one
group reaching another's server.

1. Cloudflare dashboard -> **Zero Trust** -> **Access** -> **Service Auth** ->
   **Service Tokens** -> **Create Service Token**.
   - Name it per group, e.g. `survival`. Save the **Client ID** (ends in
     `.access`) and **Client Secret** (shown once).
   - Repeat for each group (e.g. `hbm`).
2. For **each Minecraft hostname** (`chromacube.deforce.site`,
   `hbm.deforce.site`), make sure there is a self-hosted **Access application**
   covering it (Zero Trust -> Access -> Applications -> Add an application ->
   Self-hosted -> set the application domain to the hostname).
3. In that application's **Policies**, add a policy:
   - **Action: Service Auth** (this is the non-interactive action for tokens).
   - **Include -> Service Token ->** the token for the group that should reach
     this server (e.g. `survival` on `chromacube.deforce.site`).
   - Do **not** add another group's token here. That exclusivity is the isolation.
   - Keep any existing "Allow" policy with your own email so you can still sign in
     interactively as the admin.

Result: `chromacube.deforce.site` only accepts the `survival` token (+ your admin
email); `hbm.deforce.site` only accepts `hbm`. A launcher with the wrong token is
rejected at Cloudflare, regardless of what the app shows.

---

## 2. Deploy the Worker

The Worker maps each access code to a config. Source is in `worker/`.

```
cd worker
npm i -g wrangler            # if you don't have it
wrangler login
wrangler deploy              # publishes and binds the deforce.site/api/launcher route
```

Put each group's config (display name, webhook, service token, servers) into the
Worker. Two options:

- **Quick start (inline):** edit the `CONFIGS` map in `launcher-worker.js`,
  replacing the token placeholders with the real Client ID / Secret from step 1.
- **Recommended (KV, keeps secrets out of code):**
  ```
  wrangler kv namespace create LAUNCHER       # paste the id into wrangler.toml, uncomment the binding
  wrangler kv key put --binding LAUNCHER sv-7h3k '{"displayName":"Survival Crew","discordWebhook":"https://discord.com/api/webhooks/...","serviceTokenId":"xxx.access","serviceTokenSecret":"yyy","targets":[{"label":"ChromaCube","hostname":"chromacube.deforce.site","protocol":"tcp","mcHost":"chromacube"}]}'
  wrangler deploy
  ```

Test it:
```
curl "https://deforce.site/api/launcher?code=sv-7h3k"
```
You should get that group's JSON. An unknown code returns HTTP 404.

The JSON shape is exactly the launcher's config (see the main README's config
table) plus a top-level `displayName`, `serviceTokenId`, and `serviceTokenSecret`
that apply to every target in the response.

---

## 3. Build a per-group .exe

```
.\build-group.ps1 -Code sv-7h3k -Name chromacube-survival
.\build-group.ps1 -Code hbm-9x2 -Name chromacube-hbm -Version 1.2.0
```

This injects the code into the binary (`-ldflags -X main.buildCode=...`). The
resulting `build\bin\<Name>.exe` fetches that code's config on launch with no user
input. Hand each group only their own exe.

A plain `wails build` (no code) stays a **developer build** that uses the local
`config.json` and does no remote fetch, so you can keep testing locally.

---

## How it behaves at runtime

- On launch the exe calls the Worker with its code, caches the response to
  `%AppData%\ChromaCubeLauncher\remote-config.json` (mode 0600), and shows only
  that group's servers.
- If the Worker is unreachable, it uses the last cached config (offline grace). If
  there is no cache, it shows a clear error instead of the wrong servers.
- The service token is passed to cloudflared via `TUNNEL_SERVICE_TOKEN_ID/SECRET`
  environment variables (never on the command line), so it does not appear in the
  process list.
- Every session logs a rich Discord embed (user, OS, version, IP/location, server,
  status) to the webhook in that group's config.

### Rotating / revoking access
- Remove a code from the Worker (or KV) -> that exe can no longer fetch a config.
- Delete/rotate a service token in Cloudflare -> that group is cut off at the
  network layer immediately, even with a cached config.
- **Live effect (v1.4.0+):** a *running* launcher re-fetches its config every 30 s,
  so a server you revoke in the admin panel is force-disconnected in the app, and a
  fully revoked code drops the user back to the code screen — no restart needed.
  See `docs/MAP-AND-LIVE-ACCESS.md`.
