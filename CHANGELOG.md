# Changelog

## 0.9.0 - 2026-09-06

First public-candidate release.

### Added

- Self-hosted image host for pull request screenshots: PNG, JPEG, GIF, WebP, MP4 and WebM,
  typed from magic bytes, metadata stripped without decoding.
- Bearer-token uploads with per-token quotas (20 an hour, 200 MB a day, 10 MB a file).
- Uploads are kept until deleted. `RETENTION` gives them a lifetime; `expires_in` per upload
  can only shorten it.
- Admin dashboard: tokens, uploads, abuse reports, hash blocklist, audit trail, and storage
  totals overall and per token.
- Public abuse report form, rate-limited per address.
- Agent skill served from the instance at `/prunto-screenshot/SKILL.md`.
- `FROM scratch` image published to `ghcr.io/igorkasyanchuk/prunto` for amd64 and arm64.
- Video uploads come back as a markdown link, since GitHub strips a `<video>` tag that points
  outside its own upload host.
