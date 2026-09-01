# prunto

Image host for pull-request screenshots. Drop a file, get a public URL, paste `![](url)` into
a PR body. Files delete themselves after 14 days.

GitHub has no API for attaching an image to a PR description, so an agent that just took a
screenshot has nowhere to put it. This is that somewhere.

A Go port of [priito](https://github.com/igorkasyanchuk/priito) (Rails 8). Same product, same
threat model, two dependencies and a 20 MB image. See [PORTING.md](PORTING.md) for what changed
and what got worse.

## Run it

```bash
docker run -p 3000:3000 -v prunto:/data ghcr.io/igorkasyanchuk/prunto
```

That is the whole thing. No Postgres, no Redis, no object store, no account anywhere: SQLite
lives in the volume, blobs sit beside it on disk and are served from `/blobs/:key`.

Those URLs point at your own host, which GitHub cannot reach, so this mode is for trying the
service and working on it. For real pull requests, point it at a bucket behind a CDN — see
[COOLIFY.md](COOLIFY.md) for a deploy that takes about twenty minutes.

Create a token before the first upload:

```bash
docker exec -it <container> /prunto token "my laptop"
```

## Stack

Go 1.24, SQLite, and an S3 client. That is the dependency list:

| | |
| --- | --- |
| HTTP, routing, templating | `net/http`, `html/template` — standard library |
| Database | `modernc.org/sqlite` — pure Go, no cgo |
| Object storage | `github.com/minio/minio-go/v7` |
| Background work | a `time.Ticker` goroutine |
| Rate limits and quotas | counter rows in SQLite |
| Image sanitising | byte-walking, standard library only |

No framework, no ORM, no job queue, no Redis, no libvips. `CGO_ENABLED=0` and a `FROM scratch`
image, so the container holds one static binary, a CA bundle and your data volume.

## Local development

```bash
go test ./...
go run . serve
go run . token "my laptop"
```

With no bucket configured, uploads go to `./data/blobs` and the whole flow works offline. The
test suite never touches the network.

## API

```bash
curl -H "Authorization: Bearer $PRUNTO_TOKEN" -F "file=@shot.png" \
  https://prunto.igorkasyanchuk.com/api/v1/uploads

curl -X DELETE -H "Authorization: Bearer $PRUNTO_TOKEN" \
  https://prunto.igorkasyanchuk.com/api/v1/uploads/DELETE_TOKEN
```

`POST /api/v1/uploads` → 201 with `url`, `markdown`, `content_type`, `delete_url`, `expires_at`
and `byte_size`. Errors return a JSON `error` string with 401, 403, 413, 422, 429 or 502.

Add `-F "expires_in=2h"` for a shorter life. `30m`, `6h`, `7d` or a plain number of seconds;
anything longer than the default is capped at it rather than refused. Longer retention is a
storage bill and a wider abuse window, and deciding who has earned either is what accounts are
for.

## Tokens

Every upload needs one. Anonymous upload was deliberately dropped: an anonymous public file
host is the highest-abuse-risk shape there is, and it buys nothing here, since a token is one
environment variable for a CLI.

```bash
prunto token "my laptop"   # prints the token once
```

Only the SHA-256 digest is stored, so a lost token cannot be recovered — create another. Revoke
from `/admin`. Every upload records the token that made it, so abuse always has an owner to
cut off.

## What the app enforces

Each line below is a test in [`sanitize_test.go`](sanitize_test.go) or
[`http_test.go`](http_test.go). If a rule cannot be expressed as a test, it is not a rule, it
is a hope.

- **Type comes from magic bytes**, never from the filename or the client's declared content
  type. Allowlist: PNG, JPEG, GIF, WebP, MP4, WebM. Nothing else.
  - **No SVG** — it carries JavaScript.
  - **No PDF** — a phishing and malware carrier with an entirely separate threat model.
  - **Two video containers, not three.** A `.mov` is refused; so is a Matroska file wearing
    WebM's EBML signature, which is checked by its DocType rather than its magic bytes alone.
- The stored extension comes from the sniffed type. The client's filename is kept for display
  only and never reaches the object key.
- **Images are stripped at the container level, not re-encoded.** Metadata chunks are dropped
  and everything past the format's end marker is truncated, so EXIF (and the GPS in it) and any
  appended payload never reach the bucket. Animated GIFs keep every frame and keep looping —
  that is the whole point of pasting a screen recording into a PR.
- A declared canvas over 50 megapixels is refused, read from the header.
- 10 MB per file, enforced by `http.MaxBytesReader` as the body is read rather than after it
  has been buffered. 20 uploads per hour and 200 MB per day, per token.
- A token in the query string is **refused**, not ignored: a query string leaks into access
  logs, browser history and `Referer`, so a caller sending it that way has already leaked it.
- Behind Cloudflare, `CF-Connecting-IP` is the only address used. `X-Forwarded-For` can be set
  by anyone, and trusting it would turn every limit here into decoration.
- `/admin` is HTTP basic auth with a constant-time compare, plus an `Origin` check on every
  POST — basic auth is replayed by the browser on cross-site requests, so without that check
  any page on the internet could drive the dashboard. With either credential unset it returns
  403: an unconfigured admin is closed, not open.
- Removed files can be blocked by hash so they cannot be re-uploaded.

### Why there is no re-encode

The Rails version decodes and re-encodes every image through libvips, so that only pixels
survive. This port does not, and the reasoning is worth stating plainly.

Re-encoding does not eliminate decoder risk, it **relocates** it. To run libvips you feed
attacker-controlled bytes to libpng, libjpeg-turbo, giflib and libwebp — the same libraries a
browser uses — in your own process, beside your database and bucket credentials, without the
browser's sandbox. Meanwhile the decompression bomb you then have to defend against exists
only because you decode.

What re-encoding genuinely buys is protection that survives a misconfigured deployment. That is
bought here instead by a **boot-time probe**: on startup the app writes a throwaway object,
fetches it back through the configured CDN, and refuses to start if `nosniff`, the sandbox CSP
or the content type are not what it asked for. A wrong header is a misconfiguration and stops
the boot; a failed request is probably the network and only warns.

What is knowingly accepted: a payload hidden inside the compressed pixel stream passes through
unchanged. The sniffed content type, `nosniff`, the sandbox CSP and the separate CDN origin are
what stop a browser treating it as anything but an image — the same bet the Rails version
already makes for every MP4 it stores untouched.

If that stops being enough, `Sanitize` in [`sanitize.go`](sanitize.go) is the only function
that would change.

## Environment

| Variable | Purpose |
| --- | --- |
| `PORT` | Listen port, default 3000 |
| `DATA_DIR` | SQLite file and, with no bucket, the blobs. Default `./data` |
| `PRUNTO_BASE_URL` | This instance's public origin. Required with a bucket configured |
| `B2_BUCKET` | Bucket name |
| `B2_KEY_ID` / `B2_APPLICATION_KEY` | Application key with read+write on that bucket |
| `B2_ENDPOINT` | e.g. `https://s3.us-west-004.backblazeb2.com` |
| `B2_REGION` | e.g. `us-west-004` |
| `CDN_BASE_URL` | CDN hostname in front of the bucket, e.g. `https://cdn-prunto.igorkasyanchuk.com` |
| `ADMIN_USER` / `ADMIN_PASSWORD` | Gate `/admin`. Either unset means closed |
| `TRUST_PROXY` | `cloudflare` when `CF-Connecting-IP` is authoritative, otherwise `none` |

The five bucket variables are all-or-nothing: set every one or none. A half-configured bucket
is the state where uploads succeed and the URLs point nowhere.

The bucket, its endpoint and region, and the application keys all come from the
[B2 buckets page](https://secure.backblaze.com/b2_buckets.htm).

## Deploying

[COOLIFY.md](COOLIFY.md) has the full walkthrough: bucket, CDN, transform rules, environment,
and the production checklist that has to be done before the instance is public.

## Claude Code skill

Served at `/prunto-screenshot/SKILL.md` and linked from the drop page, so anyone using an
instance can install it without cloning this repo:

```bash
mkdir -p ~/.claude/skills/prunto-screenshot
curl -sfo ~/.claude/skills/prunto-screenshot/SKILL.md https://prunto.igorkasyanchuk.com/prunto-screenshot/SKILL.md
export PRUNTO_API_TOKEN=your-token
```

It is rendered rather than static because every URL in it — the API endpoint, the CDN, the
install command — comes from the instance's own configuration, so a self-hosted deployment
hands out its own host and not someone else's.

## Not built yet

Accounts, per-user quotas, a dashboard of your own uploads. `RetentionFrom` and the
`api_token_id` column are the seams those hang off.

## Licence

MIT.
