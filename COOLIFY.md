# Deploying prunto on Coolify

Two paths. Start with the first to see it running in five minutes, then do the second before
anyone else can reach it.

---

## 1. Just run it (five minutes, no accounts)

This gets a working instance with blobs on a disk volume. URLs point at your own host, which
GitHub cannot reach, so it is for trying the thing — not for real pull requests.

**Create the resource.** Coolify → your project → **+ New** → **Public Repository**, paste this
repo's URL. Build Pack: **Dockerfile**. Coolify finds the `Dockerfile` at the root on its own.

**Set the port.** Ports Exposes: `3000`.

**Add a persistent volume.** Storages → **+ Add** → Volume Mount:

| | |
| --- | --- |
| Name | `prunto-data` |
| Destination Path | `/data` |

Without this, every redeploy wipes the database and every uploaded file with it.

**Set the environment.**

```
PRUNTO_BASE_URL=https://<the domain Coolify gave you>
ADMIN_USER=admin
ADMIN_PASSWORD=<something long>
TRUST_PROXY=none
```

`PRUNTO_BASE_URL` has to match the domain exactly, scheme included and no trailing slash. It is
what the app hands out in delete URLs and in the skill, and it is what the admin `Origin` check
compares against — get it wrong and every admin action returns 403. It is validated at boot as a
bare origin: a path, a query, a fragment or credentials in it are all refused by name.

**Turn the health check off.** Coolify's health check runs `curl` or `wget` *inside* the
container. The image is `scratch`: it has neither, and no shell to run them from, so an enabled
check marks the container unhealthy forever. Traefik still routes to it while the check is off.

**Deploy.** Then mint a token — over SSH, not Coolify's web terminal, which opens a shell the
image does not have. `docker exec` on the binary directly works, because it is static:

```bash
ssh root@your-server 'docker exec $(docker ps -qf name=prunto | head -1) /prunto token "my laptop"'
```

Open the domain, paste the token into the drop page, drop a screenshot. Done.

---

## 2. Make it real (bucket, CDN, and the checklist)

Uploads have to be served from somewhere GitHub can fetch and from a hostname that is not the
app's. This part is not optional if the instance is public.

Prerequisite: `igorkasyanchuk.com` has to be on Cloudflare as a full zone — Transform Rules,
Cache Rules and the CSAM scan below are all zone features, and the free plan carries all three.
Both hostnames stay one level deep, which is all Universal SSL covers on a full setup; a
`cdn.prunto.igorkasyanchuk.com` would need Total TLS or Advanced Certificate Manager instead.

### Backblaze B2

1. [B2 buckets page](https://secure.backblaze.com/b2_buckets.htm) → **Create a Bucket**.
   Public. Note the **Endpoint** and the region inside it (`s3.us-west-004.backblazeb2.com`
   means the region is `us-west-004`).
2. **App Keys** → **Add a New Application Key**, scoped to that bucket, read and write. The
   key is shown once.

### Cloudflare in front of it

1. Add a CNAME for `cdn-prunto.igorkasyanchuk.com` pointing at the B2 endpoint host, proxied
   (orange cloud). B2 is in Cloudflare's Bandwidth Alliance, so egress through this path is
   free.
2. **Serve the CDN from a different hostname than the app.** `cdn-prunto.igorkasyanchuk.com`,
   never `prunto.igorkasyanchuk.com/files/...`. Uploads are attacker-controlled bytes, and a
   file on the app's own origin that a browser decides to treat as HTML is stored XSS against
   the app's own credentials.
3. Point `prunto.igorkasyanchuk.com` at the Coolify server too, also proxied. `TRUST_PROXY`
   below refuses any request that arrives without `CF-Connecting-IP`, so the app hostname has
   to be behind Cloudflare as well — not just the CDN.

### Response headers (Transform Rule)

B2 sets none of these, and this app is never in the serving path — the object key *is* the
public URL. Since this port does not re-encode images, **these headers are the control**.

Cloudflare → Rules → **Transform Rules** → **Modify Response Header**, matching
`http.host eq "cdn-prunto.igorkasyanchuk.com"`, three static headers:

| Header | Value | Why |
| --- | --- | --- |
| `X-Content-Type-Options` | `nosniff` | Stops a browser second-guessing the content type |
| `Content-Security-Policy` | `sandbox` | Neuters anything that slips through as active content |
| `X-Robots-Tag` | `noindex, nofollow, noarchive` | Keeps uploads out of search |

The app checks the first two at boot and **refuses to start** if they are missing. That is
deliberate: a bucket fronted by a CDN with no `nosniff` is a silent hole, since uploads keep
working and nothing looks wrong.

### Cache rule

Add a Cache Rule that actually caches `cdn-prunto.igorkasyanchuk.com`. Two things depend on
it: the free egress, and the CSAM scanning below, which only sees images that pass through the
Cloudflare cache.

### Environment

```
PRUNTO_BASE_URL=https://prunto.igorkasyanchuk.com
B2_BUCKET=your-bucket
B2_KEY_ID=...
B2_APPLICATION_KEY=...
B2_ENDPOINT=https://s3.us-west-004.backblazeb2.com
B2_REGION=us-west-004
CDN_BASE_URL=https://cdn-prunto.igorkasyanchuk.com
ADMIN_USER=admin
ADMIN_PASSWORD=<something long>
TRUST_PROXY=cloudflare
```

`TRUST_PROXY=cloudflare` makes `CF-Connecting-IP` the only address the app will use. Every rate
limit is keyed on it, and a request arriving without that header is refused — which is also how
you find out the origin is reachable directly.

Redeploy. Watch the logs for `CDN headers verified`. If the boot fails instead, the message
names the header that is missing. Note the asymmetry: a probe that comes back with the wrong
headers is fatal, but a probe that cannot be reached at all only warns — so a CDN hostname that
is failing TLS will not stop the deploy.

Set `TRUST_PROXY=cloudflare` **last**, once the app hostname actually resolves through
Cloudflare. Flip it while the record is still grey-clouded and every request is refused,
including your own. If Coolify's Let's Encrypt issuance fails behind the orange cloud, set
Cloudflare SSL/TLS to **Full (strict)**, or grey-cloud the record until the certificate issues
and proxy it again afterwards. Never leave it on **Flexible**; that is a redirect loop.

---

## Production checklist

In rough order of consequence.

**Lock the bucket to Cloudflare.** If the B2 bucket is reachable directly, its raw URL bypasses
Cloudflare completely — and with it the WAF, the rate limits, the response headers and the CSAM
scanning below. Every control here is worthless while the origin is open.

**Enable Cloudflare's CSAM Scanning Tool.** Free on all plans and it no longer requires NCMEC
credentials. Dashboard → the CDN zone → Caching → Configuration → CSAM Scanning Tool →
Configure, then give it an email address. It fuzzy-hashes images against known-CSAM lists,
blocks matches and tells you. It only sees images that pass through the cache, so the Cache
Rule above is a dependency, and it is worthless if the origin is reachable directly.

**Set a body-size limit at the edge.** 12 MB or so. The app's own 10 MB cap is enforced as the
body is read, which stops the allocation but not all of the bandwidth.

**Have a takedown path.** `/abuse_reports/new` is public and needs no account — someone who
found the content in a pull request has no token and no reason to get one. Reports land in
`/admin`, which can remove a file, remove-and-block it, revoke a token or mark a report handled.
Publish an `abuse@` address too: takedown pipelines expect one, and its absence reads as bad
faith.

**Put Cloudflare Access in front of `/admin`** if you want more than HTTP basic auth.

**Back up the volume.** `/data` holds the database: tokens, the audit trail, the hash blocklist
and every open report. The blobs are in B2 and expire anyway; the database is the part that
cannot be rebuilt.

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
