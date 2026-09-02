# prunto

[![ci](https://github.com/igorkasyanchuk/prunto/actions/workflows/ci.yml/badge.svg)](https://github.com/igorkasyanchuk/prunto/actions/workflows/ci.yml)
[![licence](https://img.shields.io/badge/licence-MIT-green.svg)](LICENSE)
[![image](https://img.shields.io/badge/ghcr.io-igorkasyanchuk%2Fprunto-blue)](https://ghcr.io/igorkasyanchuk/prunto)

**Screenshots for pull requests, in one command.** Drop a file, get a public URL, paste it into
the PR. Gone in 14 days. Runs as one 12 MB binary with nothing else to operate.

GitHub has no API for attaching an image to a PR description. A human drags the file into the
text box. An AI coding agent that just took a screenshot has nowhere to put it. prunto is that
somewhere, and it ships the skill file that teaches the agent to use it.

![Drop a screenshot, copy the markdown, see it in admin](docs/demo.gif)

## Why you want it

- **Your agent opens PRs with screenshots in them.** Install the skill once. "Open a PR with a
  screenshot of the new page" then does exactly that.
- **Nothing to operate.** No Postgres, no Redis, no S3, no CDN account. One container, one
  volume, SQLite. Starts in tens of milliseconds, idles at a few MB.
- **Safe to expose.** Every upload needs a revocable token. Type is read from the bytes, never
  the filename. Metadata is stripped without a decoder in the process. Files are served as
  inert content and delete themselves.
- **Yours.** Self-hosted, MIT, `FROM scratch`, no outbound requests at all.

## Try it in two minutes

```bash
docker run -d -p 3000:3000 -v prunto:/data \
  -e PRUNTO_BASE_URL=http://localhost:3000 \
  -e ADMIN_USER=admin -e ADMIN_PASSWORD=change-me \
  --name prunto ghcr.io/igorkasyanchuk/prunto
docker exec prunto /prunto token "my laptop"
```

Open http://localhost:3000, paste the token, drop a screenshot. Or from a shell:

```bash
curl -H "Authorization: Bearer $PRUNTO_API_TOKEN" -F "file=@shot.png" \
  http://localhost:3000/api/v1/uploads
```

You get back `url`, ready-made `markdown`, a `delete_url` and `expires_at`.

## Give it to your agent

```bash
mkdir -p ~/.claude/skills/prunto-screenshot
curl -sfo ~/.claude/skills/prunto-screenshot/SKILL.md http://localhost:3000/prunto-screenshot/SKILL.md
export PRUNTO_API_TOKEN=prunto_...
```

The skill is served by your instance with your URLs in it. It tells the agent to look at the
image before uploading, to show you the PR body before posting, and that anything permanent
belongs in the repo, not here.

## What it looks like

![The drop page after an upload: preview, markdown, copy and delete buttons](docs/upload.png)

![The admin page: tokens, uploads, abuse reports, blocklist, audit trail](docs/admin.png)

## Read more

- [How it works](docs/HOW-IT-WORKS.md): the upload path, what is in the container, why
  uploads are never decoded, the numbers.
- [API and configuration](docs/API.md): endpoints, limits, errors, environment variables.
- [Using it from an agent](docs/AGENTS.md): install paths and what the skill enforces.
- [Deploying on Coolify](COOLIFY.md): a real deployment behind a hostname and Cloudflare, and
  the production checklist.
- [Security](SECURITY.md): the threat model, what it does not defend against, how to report.

Images are published as `:latest` from `master` and as `:1.2.3` / `:1.2` from every `v*` tag.

## Licence

MIT.
