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
  "expires_at": "2026-09-16T15:36:55Z",
  "byte_size": 28311
}
```

Add `-F "expires_in=2h"` for a shorter life: `30m`, `6h`, `7d`, or a number of seconds.
Anything longer than the default 14 days is capped at it rather than refused.

For video the `markdown` field is a `<video>` tag, because GitHub renders that and shows
nothing for `![](clip.mp4)`. A `.mov` is refused; re-mux it first with
`ffmpeg -i clip.mov -c copy clip.mp4`.

## Endpoints

| | |
| --- | --- |
| `POST /api/v1/uploads` | multipart `file`, optional `expires_in`. 201 with the JSON above. |
| `DELETE /api/v1/uploads/:delete_token` | 204. The token is in `delete_url`. Needs the bearer token too. |
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
| 502 | the data volume refused the write |

## Limits

| | |
| --- | --- |
| Types | PNG, JPEG, GIF, WebP, MP4, WebM. Decided by magic bytes, never by filename |
| Size | 10 MB per file, enforced as the body is read |
| Canvas | 50 megapixels, read from the header |
| Per token | 20 uploads per hour, 200 MB per day |
| Retention | 14 days by default, shorter on request, never longer |
| Abuse reports | 5 per hour per address |
| Admin logins | 10 wrong passwords per address, then locked for 15 minutes |

## Configuration

| Variable | Purpose |
| --- | --- |
| `PORT` | Listen port, default 3000 |
| `DATA_DIR` | SQLite file and the blobs. Default `./data` |
| `PRUNTO_BASE_URL` | This instance's public origin. Every upload and delete URL is built on it. Validated at boot as a bare origin: a path, query, fragment or credentials in it are refused |
| `ADMIN_USER` / `ADMIN_PASSWORD` | Gate `/admin`. Either unset means closed |
| `TRUST_PROXY` | `cloudflare` behind Cloudflare, `forwarded` behind a reverse proxy you control (Traefik, Caddy, nginx), `none` when exposed directly. Anything else is refused at boot |

`TRUST_PROXY` decides which address every rate limit is keyed on. `cloudflare` trusts only
`CF-Connecting-IP`; `forwarded` trusts only the last `X-Forwarded-For` entry, the one your
proxy wrote. In both modes a request without the header is refused, which is also how you find
out the origin is reachable directly.

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

Uploads go to `./data/blobs` and the whole flow works offline. The screenshots and the GIF in
the README were recorded against a local instance with Playwright; every step in the recording
is asserted, so the demo cannot show a flow that does not work.
