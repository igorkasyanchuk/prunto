# Security

## Reporting a vulnerability

Email **igorkasyanchuk@gmail.com** with the details. Do not open a public issue for anything
that could be exploited before it is fixed. You will get an acknowledgement within a few days
and a fix or a plan before anything is disclosed.

## What prunto defends against

Every upload is an attacker-controlled file served from a public URL, so the design starts
from that.

- **Type from magic bytes.** PNG, JPEG, GIF, WebP, MP4, WebM and nothing else. The filename and
  declared content type are ignored. No SVG, no PDF.
- **No decoder in the process.** Images are rebuilt at the container level: metadata chunks
  (EXIF, XMP, ICC, comments) are dropped and anything after the end marker is truncated. Bytes
  are never decoded, so there is no libpng/libjpeg/libwebp attack surface and no
  decompression bomb.
- **Blobs are served as inert content.** `X-Content-Type-Options: nosniff`, the recorded
  content type, `Content-Disposition: inline` and `Content-Security-Policy: sandbox` on every
  blob response.
- **Every upload has an owner.** No anonymous uploads. Tokens are 24 bytes of `crypto/rand`,
  stored only as a SHA-256 digest, and refused if sent in a query string.
- **Quotas per token:** 10 MB per file, 20 uploads per hour, 200 MB per day, 50 megapixels per
  image. The body limit is enforced as the body is read.
- **Admin is closed by default.** `/admin` returns 403 until both `ADMIN_USER` and
  `ADMIN_PASSWORD` are set. Constant-time credential compare, per-form CSRF token, failed
  logins rate-limited per address, `Cache-Control: no-store`.
- **Nothing dials out.** The process makes no outbound request. The image is `FROM scratch`,
  runs as a non-root user, and carries no shell, no CA bundle and no libc.
- **Expiry is enforced on read**, not just by the hourly sweep.
- **Abuse path.** Public report form, hash blocklist so removed content cannot be re-uploaded,
  90-day append-only audit trail.

## What it does not defend against

- **A payload hidden inside a valid pixel stream.** Sanitising without decoding cannot see
  inside compressed image data. The response headers above are what keep a browser from
  treating such a file as active content. Re-encode uploads yourself if that is not enough.
- **CDN caches.** Blobs are served `immutable` for a year. Deleting at the origin does not
  purge a CDN in front of it; purge that URL too.
- **Direct access to the origin when `TRUST_PROXY` is set.** `cloudflare` trusts
  `CF-Connecting-IP` because Cloudflare overwrites it; `forwarded` trusts the last
  `X-Forwarded-For` entry because your proxy appends it. Both assume the origin is reachable
  only through that proxy. If it is not, anyone can set the header. Firewall the origin.
- **A shared-origin admin.** Blobs and `/admin` live on one origin. If you want hard
  separation, point a second hostname at the same container and serve `/blobs` only from it.

## Supported versions

The latest image on `ghcr.io/igorkasyanchuk/prunto` and the `master` branch. CI runs
`go vet`, the test suite with the race detector, and `govulncheck` on every push.
