# prunto

**Image host for pull-request screenshots.** Drop a file, get a public URL, paste `![](url)`
into a PR body. Files delete themselves after 14 days.

GitHub has no API for attaching an image to a PR description, so an agent that just took a
screenshot has nowhere to put it. This is that somewhere.

```bash
docker run -p 3000:3000 -v prunto:/data ghcr.io/igorkasyanchuk/prunto
docker exec -it <container> /prunto token "my laptop"
```

That is the entire deployment. Open the page, paste the token, drop a screenshot.

## By the numbers

| | |
| --- | --- |
| Services required | none — no Postgres, no Redis, no object store, no account anywhere |
| Direct dependencies | **1** (`modernc.org/sqlite`, pure Go, no cgo) |
| Module graph | 30 modules |
| Go source | 2,229 lines, plus 1,298 lines of tests |
| Tests | 38, **71%** statement coverage, none touch the network |
| Binary | **11.9 MB**, static, `CGO_ENABLED=0` |
| Image | `FROM scratch` — the binary and your data volume, no shell, no CA bundle |
| Cold start | ~45 ms from exec to first served request (local run) |
| Memory | 3–5 MB RSS in the production container |

Binary measured with the Dockerfile's own flags (`-trimpath -ldflags="-s -w"`); coverage from
`go test -cover ./...`.

## Why it looks like this

**One process, one file, one volume.** SQLite holds the metadata, the blobs sit beside it on
disk, and the same process streams them back from `/blobs/:key`. Rate limits and quotas are
counter rows; the expiry sweep is a `time.Ticker` goroutine. There is no queue to run, no
cache to warm, and nothing to restore in the right order. The cost is that it scales to
exactly one container — two replicas would each get their own database and their own idea of
your rate limits.

**Nothing dials out.** With no bucket to talk to, the process makes no outbound request at
all, which is why the image carries no CA bundle. The whole attack surface is the port it
listens on.

**Uploads are never decoded.** The obvious way to neutralise a hostile image is to decode and
re-encode it so only pixels survive. That does not remove decoder risk, it *relocates* it:
running libvips means feeding attacker-controlled bytes to libpng, libjpeg-turbo, giflib and
libwebp — the same libraries a browser uses — inside your own process, next to your database,
without the browser's sandbox. The decompression bomb you then have to defend against exists
only because you decode.

So `Sanitize` walks the container instead. Metadata chunks are dropped and everything past the
format's end marker is truncated, so EXIF (and the GPS in it) and any appended payload never
reach the volume, while an animated GIF keeps every frame. What that knowingly accepts is a
payload hidden inside the compressed pixel stream: the sniffed content type, `nosniff`,
`Content-Disposition: inline` and a `sandbox` CSP — all set by the same handler that opens the
file — are what stop a browser treating one as active content. If that stops being enough,
`Sanitize` in [`sanitize.go`](sanitize.go) is the only function that changes.

**Blobs share the app's origin.** There is no second hostname, so those headers are also the
only thing between a stored file and the admin session. Pointing a second hostname at this same
container and serving `/blobs` only from it buys the separation back without adding a bucket.

## What it enforces

Each line is a test in [`sanitize_test.go`](sanitize_test.go) or [`http_test.go`](http_test.go).
A rule that cannot be expressed as a test is not a rule, it is a hope.

- **Type comes from magic bytes**, never the filename or the declared content type. PNG, JPEG,
  GIF, WebP, MP4, WebM — nothing else. No SVG (it carries JavaScript), no PDF (a separate
  threat model entirely), and a `.mov` is refused, as is Matroska wearing WebM's EBML signature.
- The stored extension comes from the sniffed type; the client's filename is display-only and
  never reaches the object key.
- A declared canvas over 50 megapixels is refused, read from the header.
- 10 MB per file, enforced by `http.MaxBytesReader` as the body is read rather than after it is
  buffered. 20 uploads per hour and 200 MB per day, per token.
- Expiry is enforced on the read path, not just by the sweep — an upload stops being served the
  moment its row says it expired.
- A token in the query string is **refused**, not ignored: query strings leak into access logs,
  browser history and `Referer`, so a caller sending it that way has already leaked it.
- Behind Cloudflare, `CF-Connecting-IP` is the only address used. `X-Forwarded-For` can be set
  by anyone, and trusting it would make every limit here decoration.
- `/admin` is basic auth with a constant-time compare plus a per-form CSRF token. Headers alone
  could not do it: WebKit omits `Origin` on same-origin form posts, and this app's own
  `Referrer-Policy: no-referrer` opaques it to `null`. Either credential unset returns 403 — an
  unconfigured admin is closed, not open.
- Removed files can be blocked by hash so they cannot be re-uploaded.

## API

```bash
curl -H "Authorization: Bearer $PRUNTO_API_TOKEN" -F "file=@shot.png" \
  https://prunto.igorkasyanchuk.com/api/v1/uploads

curl -X DELETE -H "Authorization: Bearer $PRUNTO_API_TOKEN" \
  https://prunto.igorkasyanchuk.com/api/v1/uploads/DELETE_TOKEN
```

`POST /api/v1/uploads` → 201 with `url`, `markdown`, `content_type`, `delete_url`, `expires_at`
and `byte_size`. Errors return a JSON `error` string with 401, 403, 413, 422, 429 or 502.

Add `-F "expires_in=2h"` for a shorter life — `30m`, `6h`, `7d`, or seconds. Anything longer
than the default is capped at it rather than refused.

## Tokens

Every upload needs one; anonymous upload was deliberately dropped, because an anonymous public
file host is the highest-abuse-risk shape there is and a token costs one environment variable.
Only the SHA-256 digest is stored, so a lost token cannot be recovered — create another from
`/admin`. Every upload records the token that made it, so abuse always has an owner to cut off.

## Configuration

| Variable | Purpose |
| --- | --- |
| `PORT` | Listen port, default 3000 |
| `DATA_DIR` | SQLite file and the blobs. Default `./data` |
| `PRUNTO_BASE_URL` | This instance's public origin. Every upload and delete URL is built on it |
| `ADMIN_USER` / `ADMIN_PASSWORD` | Gate `/admin`. Either unset means closed |
| `TRUST_PROXY` | `cloudflare` when `CF-Connecting-IP` is authoritative, otherwise `none` |

`PRUNTO_BASE_URL` is validated at boot as a bare origin — a path, query, fragment or
credentials in it are refused by name.

## Agent skill

Served at `/prunto-screenshot/SKILL.md` and linked from the drop page, so anyone using an
instance can install it without cloning this repo. It is rendered rather than static: every URL
in it comes from that instance's own configuration, so a self-hosted deployment hands out its
own host and not someone else's. The drop page also carries a setup prompt you can paste into
any AI assistant.

## Development

```bash
go test ./...
go run . serve
go run . token "my laptop"
```

Uploads go to `./data/blobs` and the whole flow works offline.

[COOLIFY.md](COOLIFY.md) is the deployment walkthrough and the production checklist to do
before an instance is public.

## Not built yet

Accounts, per-user quotas, a dashboard of your own uploads. `RetentionFrom` and the
`api_token_id` column are the seams those hang off.

## Licence

MIT.
