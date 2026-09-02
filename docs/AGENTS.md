# Using prunto from an AI coding agent

Every instance serves a rendered skill at `/prunto-screenshot/SKILL.md`. Every URL in it comes
from that instance's configuration, so a self-hosted deployment hands out its own host and
never someone else's. Anyone with a token can install it without cloning this repo.

## Install for every project

```bash
mkdir -p ~/.claude/skills/prunto-screenshot
curl -sfo ~/.claude/skills/prunto-screenshot/SKILL.md https://your-host/prunto-screenshot/SKILL.md
export PRUNTO_API_TOKEN=prunto_...
```

Put the `export` in your shell profile so it survives a new terminal.

## Install for one repository

Same two commands with `.claude/skills/prunto-screenshot/` in the repo root instead of
`~/.claude/skills/`. Commit the file if the whole team should have it; the token stays in each
person's environment.

## Or let the agent install itself

The drop page carries a setup prompt with a copy button. Paste it into any assistant that can
run shell commands and it downloads the skill, asks you for a token, and confirms what it
installed.

## What the skill tells the agent

- Look at the image before uploading it. Nothing showing keys, tokens, `.env` contents or
  customer data goes to a public URL.
- The file is world-readable at an unguessable URL until it expires, and the image in the PR
  breaks when that happens. Anything that must outlive the review belongs in the repo.
- Upload with one `curl`, take `.markdown` for the PR body, keep `.delete_url` in the reply so
  the user can remove the file early.
- Show the user the PR body before running `gh pr create` or `gh pr edit`, and append to an
  existing body, never overwrite it.
- A `.mov` is refused; re-mux to MP4 first. GIF is the better choice for anything short.

The skill is compatible with Claude Code and anything that loads reusable instructions from
`~/.claude/skills`. For another agent, put the file wherever it reads instructions from; the
content is plain Markdown.
