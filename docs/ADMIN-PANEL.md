# Admin control panel

A small owner-only web panel (`admin/`) to manage per-user access without the CLI.
Create a user and it auto-generates a code, creates a Cloudflare Access service
token, attaches it to the chosen servers, and writes the KV entry the launcher
reads. Revoke deletes all of that. No per-user builds - everyone uses the one
universal exe and types their code.

## Prerequisites (one time)

### 1. Create a "Public DNS" Access app per server
Zero Trust -> Access -> Applications -> Add an application -> **Public DNS**.
- One for `chromacube.deforce.site`, one for `hbm.deforce.site`.
- Leave the **path blank** (the policy should cover the whole hostname).
- Add an **Allow** policy with *your* email (`you@example.com`) so you can sign
  in interactively to test. The panel adds the per-user service-token policies
  automatically.

The panel looks up each app's Application ID by hostname, so there is nothing to
copy - just make sure the apps exist.

### 2. Create a shared KV namespace (read by the launcher, written by the admin)
```
cd admin
npx wrangler kv namespace create LAUNCHER
```
Copy the printed `id` into BOTH:
- `admin/wrangler.toml`  -> uncomment the `[[kv_namespaces]]` block, paste the id
- `worker/wrangler.toml` -> uncomment the `[[kv_namespaces]]` block, paste the SAME id

### 3. Create the API token and store it as a secret
Create a Cloudflare API token (My Profile -> API Tokens -> Create Token -> Custom)
with: **Access: Service Tokens -> Edit** and **Access: Apps and Policies -> Edit**,
Account Resources = your account. Then, from `admin/`:
```
npx wrangler secret put CF_API_TOKEN
```
Paste the token at the prompt (never in a file or chat).

### 4. Protect admin.deforce.site with Cloudflare Access
The admin panel runs on its own subdomain (`admin.deforce.site`, created
automatically when you deploy). Protect it: Zero Trust -> Access -> Applications
-> Add an application -> **Public DNS** -> application domain `admin.deforce.site`
(no path). Policy: **Allow**, Include -> Emails -> `you@example.com`. This is
the real lock on the panel; the Worker also checks the email header as a backstop.

## Deploy

```
cd admin
npx wrangler deploy
```
This creates the `admin.deforce.site` custom domain automatically. Then open
`https://admin.deforce.site`, sign in with your email.

(The launcher endpoint lives on its own subdomain too: `https://api.deforce.site/`.
Both are Worker Custom Domains, so Cloudflare creates their proxied DNS records for
you - no apex-path routes, which the main site shadows.)

## Daily use

1. Open `deforce.site/admin`.
2. Type a display name, tick the servers that user may reach, click **Create user**.
3. Copy the generated **code** and send it to that person.
4. They run the universal `ChromaCube-Launcher.exe`, paste the code once, and connect.
5. To cut someone off: click **revoke** - their token, policies and KV entry are deleted.

## How it stays isolated
Each user gets their own service token, and each token is only added to the Access
policies of the servers you ticked. A user physically cannot reach a server their
token was never attached to - the launcher UI and Cloudflare Access agree.
