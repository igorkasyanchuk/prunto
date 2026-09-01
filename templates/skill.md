---
name: prunto-screenshot
description: Upload a local image, GIF or short video to prunto and embed the returned public URL in a GitHub pull request body. Use when asked to attach a screenshot, recording, or diagram to a PR, to open a PR with a screenshot in it, or to get a shareable public link for a local image file. Does not capture the screenshot itself - it takes a file path you already have.
---

# prunto-screenshot

GitHub has no API for attaching an image to a PR description. This skill closes that gap:
upload the file to {{.BaseURL}}, get back a public URL, paste the markdown into the PR body.

## Before you upload

**Anything uploaded is world-readable at an unguessable URL until it expires**
({{.Retention}}). Look at the image first. Do not upload a screenshot showing API keys,
tokens, passwords, `.env` contents, customer data, or an internal system the user has not said
is safe to share. If unsure, ask.

Uploads are deleted after {{.Retention}}, and the image in the PR breaks when that happens. For
anything that has to outlive the review, tell the user so they can commit the file to the repo
instead.

## Getting the file

This skill does not capture anything. Use whatever is already available:

- browser MCP `computer` screenshot for a web page, or its GIF recorder for a flow
- iOS Simulator `control` screenshot for an app
- `screencapture -x out.png` (macOS) for the desktop
- a path the user gave you

## Uploading

Needs `PRUNTO_API_TOKEN` in the environment, holding a token for {{.BaseURL}}. If it is not
set, ask the user for one rather than guessing - they create it in that instance's `/admin`.

```bash
curl -sf -H "Authorization: Bearer $PRUNTO_API_TOKEN" -F "file=@PATH" \
  "{{.BaseURL}}/api/v1/uploads"
```

Add `-F "expires_in=2h"` to have it deleted sooner than the default. `30m`, `6h` and `7d` all
work; anything longer than the default is capped at the default rather than refused.

Returns:

```json
{
  "url": "{{.BlobBaseURL}}/AbC123xyz789.png",
  "markdown": "![]({{.BlobBaseURL}}/AbC123xyz789.png)",
  "content_type": "image/png",
  "delete_url": "{{.BaseURL}}/api/v1/uploads/9f3c...",
  "expires_at": "2026-09-14T10:00:00Z",
  "byte_size": 184320
}
```

Take `.markdown` for a PR body. Keep `.delete_url` in your reply so the user can remove the
file early:

```bash
curl -X DELETE -H "Authorization: Bearer $PRUNTO_API_TOKEN" "DELETE_URL"
```

Limits: {{.MaxMB}} MB per file; **PNG, JPEG, GIF, WebP, MP4 and WebM only** - no PDF, no SVG,
no other video container; 20 uploads per hour per token. On 4xx the body has an `error` field -
show it to the user rather than retrying. A 401 means the token is missing, wrong, or revoked.

The token goes in the `Authorization` header and nowhere else. A request carrying it in the
query string is refused outright, because a query string leaks into access logs, browser
history and Referer headers.

## Putting it in the PR

New PR:

```bash
gh pr create --title "TITLE" --body "BODY

## Screenshot

MARKDOWN"
```

Existing PR - append, never overwrite:

```bash
gh pr edit NUMBER --body "$(gh pr view NUMBER --json body -q .body)

## Screenshot

MARKDOWN"
```

Creating or editing a PR is outward-facing. Show the user the URL and the exact body you are
about to post, and wait for a yes before running `gh`.

A screen recording can go up as MP4 or WebM; `.mov` is refused, so re-mux it first:

```bash
ffmpeg -i clip.mov -c copy clip.mp4
```

For video the `markdown` field is a `<video>` tag rather than `![]()`, because that is what
GitHub renders. Paste it into the PR body exactly as returned. A GIF still works and still
autoplays, which is the better choice for anything short.

## Installing

```bash
mkdir -p ~/.claude/skills/prunto-screenshot
curl -sfo ~/.claude/skills/prunto-screenshot/SKILL.md {{.SkillURL}}
export PRUNTO_API_TOKEN=your-token
```

That is every project. For one repo, save it under `.claude/skills/prunto-screenshot/SKILL.md`
instead. Put the `export` in your shell profile so it survives a new terminal.
