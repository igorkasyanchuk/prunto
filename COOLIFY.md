# Deploying prunto on Coolify

One container, one volume, one hostname. There is no bucket, no CDN account and no object
store to configure - the app serves uploads off its own data volume. Do the production
checklist at the bottom before anyone else can reach it.

---

## Deploy it

**Create the resource.** Coolify -> your project -> **+ New** -> **Public Repository** (or the
GitHub App source, which also gives you a deploy on every push), paste this repo's URL. Build
Pack: **Dockerfile**. Coolify finds the `Dockerfile` at the root on its own.

**Set the port.** Ports Exposes: `3000`.

**Add a persistent volume.** Storages -> **+ Add** -> Volume Mount:

| | |
| --- | --- |
| Name | `prunto-data` |
| Destination Path | `/data` |

Without this, every redeploy wipes the database *and every uploaded file with it*. There is no
bucket holding a second copy - this volume is the only copy.

**Point a hostname at it** and let Coolify issue the certificate. Proxying it through
Cloudflare is worthwhile: it caches the blobs, absorbs traffic, and gives you the CSAM scan
below.

**Set the environment.**

```
PRUNTO_BASE_URL=https://<the domain>
ADMIN_USER=admin
ADMIN_PASSWORD=<something long>
TRUST_PROXY=cloudflare
```

`PRUNTO_BASE_URL` has to match the domain exactly, scheme included and no trailing slash. It is
what the app hands out in blob URLs, delete URLs and the skill, and it is what the admin
`Origin` check compares against - get it wrong and every admin action returns 403. It is
validated at boot as a bare origin: a path, a query, a fragment or credentials in it are all
refused by name.

`TRUST_PROXY=cloudflare` makes `CF-Connecting-IP` the only address the app will use. Every rate
limit is keyed on it, and a request arriving without that header is refused - which is also how
you find out the origin is reachable directly. Set it **last**, once the hostname actually
resolves through Cloudflare; flip it while the record is still grey-clouded and every request is
refused, including your own.

Not using Cloudflare? Coolify's Traefik is still a proxy, and with `TRUST_PROXY=none` every
request would carry Traefik's address: one shared rate-limit bucket for the whole world, and an
audit trail full of the same IP. Set `TRUST_PROXY=forwarded` instead. Only the last
`X-Forwarded-For` entry, the one Traefik itself appends, is trusted; a request without the
header is refused. Leave `TRUST_PROXY=none` only when the process is exposed directly.

**Turn the health check off.** Coolify's health check runs `curl` or `wget` *inside* the
container. The image is `scratch`: it has neither, and no shell to run them from, so an enabled
check marks the container unhealthy forever. Traefik still routes to it while the check is off.

**Deploy.** Then create a token - over SSH, not Coolify's web terminal, which opens a shell the
image does not have. `docker exec` on the binary directly works, because it is static:

```bash
ssh root@your-server 'docker exec $(docker ps -qf name=prunto | head -1) /prunto token "my laptop"'
```

Hand the token to your agent, or upload from a shell with `curl`. Done.

---

## Production checklist

In rough order of consequence.

**Know that a deleted file lives on in the CDN cache.** Blobs are served
`max-age=31536000, immutable`, so a delete at the origin — the expiry sweep, a delete URL, or a
takedown from `/admin` — does not un-publish it. The origin is genuinely clean (a request with a
cache-busting query returns 404); the public URL is not. For a takedown, purge that URL from
Cloudflare too, or the file is still up.

**Enable Cloudflare's CSAM Scanning Tool** if the instance is proxied through Cloudflare. Free
on all plans and it no longer requires NCMEC credentials. Dashboard → the zone → Caching →
Configuration → CSAM Scanning Tool → Configure, then give it an email address. It fuzzy-hashes
images against known-CSAM lists, blocks matches and tells you. It only sees images that pass
through the cache, so it needs the blob URLs to be cached.

**Consider a second hostname for `/blobs`.** Uploads are attacker-controlled bytes served from
the same origin as `/admin`. The app sets `nosniff`, a `sandbox` CSP and the recorded content
type on every blob response, which is what stops a browser treating one as active content — but
a separate hostname pointed at this same container would keep a slip from reaching the admin
session's origin.

**Set a body-size limit at the edge.** 12 MB or so. The app's own 10 MB cap is enforced as the
body is read, which stops the allocation but not all of the bandwidth.

**Have a takedown path.** `/abuse_reports/new` is public and needs no account — someone who
found the content in a pull request has no token and no reason to get one. Reports land in
`/admin`, which can remove a file, remove-and-block it, revoke a token or mark a report handled.
Publish an `abuse@` address too: takedown pipelines expect one, and its absence reads as bad
faith.

**Put Cloudflare Access in front of `/admin`** if you want more than HTTP basic auth.

**Back up the volume.** `/data` holds everything: the database (tokens, the audit trail, the
hash blocklist, every open report) *and* every uploaded file. Nothing here has a second copy
anywhere, which is the price of dropping the bucket. Coolify can back the volume up on a
schedule. Unless `RETENTION` is set the blobs never expire, and the database cannot be rebuilt
either way. The
database is SQLite in WAL mode, which changes how to copy it: see the backup note in
[DEPLOY.md](docs/DEPLOY.md#before-you-call-any-of-these-done).

---

## Notes

**One container is the whole deployment.** No worker, no Redis, no Postgres. The expiry sweep
runs on a ticker inside the process, so `WEB_CONCURRENCY`-style scaling does not apply — there
is one process and it does everything.

**Running more than one replica** would give each its own SQLite file and its own rate-limit
counters, which is wrong in both directions. If this ever needs to scale past one container,
the counters and the database move to Postgres first.

**Upgrades** are a redeploy. The schema applies itself at boot and is written to be idempotent,
so there is no migration step.
