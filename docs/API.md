# API and configuration

## Upload

```bash
curl -H "Authorization: Bearer $PRUNTO_API_TOKEN" -F "file=@shot.png" \
  https://your-host/api/v1/uploads
```

```json
{
  "url": "https://your-host/blobs/7sAwxTzz6KwSO0p5.png",
  "markdown": "![](https://your-host/blobs/7sAwxTzz6KwSO0p5.png)",
  "content_type": "image/png",
  "delete_url": "https://your-host/api/v1/uploads/9f3c...",
  "expires_at": null,
  "byte_size": 28311
}
```

`expires_at` is `null` on the default instance, where files are kept until deleted; with a
`RETENTION` or an `expires_in` it is an RFC 3339 timestamp such as `"2026-09-16T15:36:55Z"`.

Add `-F "expires_in=2h"` to have the file deleted on a schedule: `30m`, `6h`, `7d`, or a
number of seconds; `0` means no expiry of its own, the same as leaving it out. Without it the file lives as long as the instance's `RETENTION`, which is
forever unless set; anything longer than that retention is capped at it rather than refused.

For video the `markdown` field is a plain link, `[Watch the video](url)`. GitHub's sanitiser
removes a `<video>` tag whose source is not its own upload host (checked against the
`/markdown` API, which returns an empty paragraph for it), and `![](clip.mp4)` renders
nothing, so a link is the only markup that survives. For inline playback in a PR, record a
GIF. A `.mov` is refused; re-mux it first with `ffmpeg -i clip.mov -c copy clip.mp4`.

## Endpoints

| | |
| --- | --- |
| `POST /api/v1/uploads` | multipart `file`, optional `expires_in`. 201 with the JSON above. |
| `DELETE /api/v1/uploads/:delete_token` | 204. The token is in `delete_url`. Needs a bearer token too: any valid one, not only the uploader's, since the delete token is the secret. |
| `GET /blobs/:key` | the file, with Range support so `<video>` scrubs. 404 once expired. |
| `GET /prunto-screenshot/SKILL.md` | the agent skill, rendered with this instance's URLs. |
| `GET /abuse_reports/new` | public takedown form, no account needed. |
| `GET /admin` | dashboard, basic auth. |
| `GET /up` | health check, returns `ok`. |

Errors return a JSON `error` string:

| | |
| --- | --- |
| 401 | token missing, wrong or revoked. Also returned, with an explanation, when the token was sent in the query string |
| 403 | request did not arrive through the trusted proxy |
| 413 | file over 10 MB |
| 422 | type not allowed, file malformed, over 50 megapixels, or its hash is blocked |
| 429 | over 20 uploads this hour or 200 MB today for this token |
| 500 | the database refused a write; check the log |
| 502 | the data volume refused the write |

## Limits

| | |
| --- | --- |
| Types | PNG, JPEG, GIF, WebP, MP4, WebM. Decided by magic bytes, never by filename |
| Size | 10 MB per file, enforced as the body is read |
| Canvas | 50 megapixels, read from the header |
| Per token | 20 uploads per hour, 200 MB per day |
| Retention | Forever by default, `RETENTION` sets a limit, each upload can ask for less, never more |
| Abuse reports | 5 per hour per address |
| Admin logins | 10 wrong passwords per address within 15 minutes locks it for 15 minutes; a successful login clears the count |

## Configuration

| Variable | Purpose |
| --- | --- |
| `PORT` | Listen port, default 3000 |
| `DATA_DIR` | SQLite file and the blobs. Default `./data` |
| `PRUNTO_BASE_URL` | This instance's public origin. Every upload and delete URL is built on it. Validated at boot as a bare origin: a path, query, fragment or credentials in it are refused |
| `ADMIN_USER` / `ADMIN_PASSWORD` | Gate `/admin`. Either unset means closed |
| `RETENTION` | How long an upload lives: `never` (default), or `30d`, `12h`, `90m`, seconds. At least one minute. A per-upload `expires_in` can only shorten it |
| `ANALYTICS_HTML` | An HTML snippet rendered unescaped into the home page's `<head>`, meant for one tracker `<script>` tag such as Umami or Plausible. The https origin of every `src` in it is added to that page's `script-src` and `connect-src`; no other page renders it |
| `TRUST_PROXY` | `cloudflare` behind Cloudflare, `forwarded` behind a reverse proxy you control (Traefik, Caddy, nginx), `none` when exposed directly. Anything else is refused at boot |

`TRUST_PROXY` decides which address every rate limit is keyed on. `cloudflare` trusts only
`CF-Connecting-IP`; `forwarded` trusts only the last `X-Forwarded-For` entry, the one your
proxy wrote, across every line of that header. The entry must be an IP address; a port is
stripped, anything else is refused. In both modes a request without a usable address is
refused, which is also how you find out the origin is reachable directly. `forwarded` means
exactly one proxy: with Cloudflare in front of your own proxy, use `cloudflare`, or every
visitor through the same edge shares one address.

HSTS and the `Secure` flag on admin cookies follow the scheme of `PRUNTO_BASE_URL`, not
`TRUST_PROXY`.

## Commands

```bash
prunto serve          # the default
prunto token "label"  # print a new token once; only its digest is stored
prunto purge          # run the expiry sweep now
```

## Development

```bash
go test ./...
go run . serve
go run . token "my laptop"
```

Uploads go to `./data/blobs` and the whole flow works offline. The screenshots in the README
were taken against a local instance.
