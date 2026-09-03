# Distributing the skill

[docs/AGENTS.md](AGENTS.md) is for the person installing the skill. This one is for you, the
person running the instance, handing it to a team, a company or the internet.

The thing you distribute is a URL, not a file. Your instance renders
`/prunto-screenshot/SKILL.md` from its own configuration, so the copy someone downloads points
at your host, your blob host and your retention. Ship the URL and the copy is always current.
Ship a file and you have forked it.

Everything below assumes `https://prunto.example.com`. Substitute your own host.

## 1. Send the link

The lowest-effort distribution there is. The drop page already carries install snippets with
copy buttons and a token link, so a message with two URLs is a complete handover:

```text
Screenshots for PRs: https://prunto.example.com
Install snippet + token are on that page. Skill: https://prunto.example.com/prunto-screenshot/SKILL.md
```

Nothing to maintain, nothing to version. Use this unless you have a reason not to.

## 2. The one-liner

One command, no editing, works from any directory. This is the "one click" for anyone who
lives in a terminal:

```bash
mkdir -p ~/.claude/skills/prunto-screenshot && curl -sfo ~/.claude/skills/prunto-screenshot/SKILL.md https://prunto.example.com/prunto-screenshot/SKILL.md && echo installed
```

Repo-scoped, run in the repo root:

```bash
mkdir -p .claude/skills/prunto-screenshot && curl -sfo .claude/skills/prunto-screenshot/SKILL.md https://prunto.example.com/prunto-screenshot/SKILL.md && echo installed
```

Ready to paste into a README, with the token step included:

````markdown
### Screenshots in PRs

```bash
mkdir -p ~/.claude/skills/prunto-screenshot
curl -sfo ~/.claude/skills/prunto-screenshot/SKILL.md https://prunto.example.com/prunto-screenshot/SKILL.md
```

Then get a token from https://prunto.example.com/admin and put
`export PRUNTO_API_TOKEN=prunto_...` in your shell profile.
````

Do not distribute a `curl | sh` installer for this. There is nothing to install beyond one
file, and piping a remote script into a shell buys you nothing but a larger blast radius.

## 3. Let the agent install it

For people who would rather talk to their agent than open a terminal. The drop page has this
prompt behind a copy button; paste it into any assistant that can run shell commands:

```text
Please set up the "prunto-screenshot" instructions on this machine for me.

1. mkdir -p ~/.claude/skills/prunto-screenshot
   curl -sfo ~/.claude/skills/prunto-screenshot/SKILL.md https://prunto.example.com/prunto-screenshot/SKILL.md

2. Ask me for my prunto API token, then add export PRUNTO_API_TOKEN=<the token>
   to my shell profile and tell me to open a new terminal.

3. Read the downloaded file back to confirm it arrived, and summarise what it does.
```

It handles the shell profile edit, which is the step people get wrong.

## 4. Claude Code plugin, for actual one-click

A plugin marketplace is the only route where installing is a command rather than a shell
snippet: `/plugin install`, no `curl`, no `mkdir`, and updates arrive through
`/plugin marketplace update`. The cost is that the skill inside a plugin is a static file with
your host baked in, so this fits a company with one shared instance and is wrong for a project
whose users self-host.

Layout of the plugin repository:

```text
prunto-plugin/
  .claude-plugin/
    marketplace.json
    plugin.json
  skills/
    prunto-screenshot/
      SKILL.md
```

`.claude-plugin/marketplace.json`:

```json
{
  "name": "acme",
  "owner": { "name": "Acme Platform Team" },
  "plugins": [
    {
      "name": "prunto-screenshot",
      "source": "./",
      "description": "Upload a screenshot to prunto and put it in a GitHub PR body."
    }
  ]
}
```

`.claude-plugin/plugin.json`:

```json
{
  "name": "prunto-screenshot",
  "description": "Upload a screenshot to prunto and put it in a GitHub PR body.",
  "version": "1.0.0"
}
```

Fill `skills/` from your running instance rather than by hand, so the URLs stay correct:

```bash
mkdir -p skills/prunto-screenshot
curl -sfo skills/prunto-screenshot/SKILL.md https://prunto.example.com/prunto-screenshot/SKILL.md
git commit -am "Refresh the skill from prunto.example.com"
```

What you then tell people:

```text
/plugin marketplace add acme-corp/prunto-plugin
/plugin install prunto-screenshot@acme
```

Re-run the `curl` and commit whenever you upgrade prunto or change `PRUNTO_BASE_URL`; a stale
committed copy points agents at the wrong host and fails with a 401 or a DNS error.

## 5. Commit it into the team's repository

For a repo where everyone should get the skill by cloning:

```bash
mkdir -p .claude/skills/prunto-screenshot
curl -sfo .claude/skills/prunto-screenshot/SKILL.md https://prunto.example.com/prunto-screenshot/SKILL.md
git add .claude/skills/prunto-screenshot/SKILL.md
git commit -m "Add the prunto screenshot skill"
```

The file is public-safe: it contains URLs and instructions, never a token. Each person still
needs their own `PRUNTO_API_TOKEN`, which is the point - a revoked token takes one person's
access, not the team's.

## Tokens are handed out separately

Never put a token in a snippet you paste into Slack, a README or a plugin. One token per
person, created in `/admin` or from the host:

```bash
docker exec prunto /prunto token "alice laptop"
```

Name it after the human or the machine. That name is what you look for in `/admin` when you
revoke it.

## Keeping installed copies fresh

Re-running the install command overwrites the file, so "update" and "install" are the same
command. To see whether an installed copy has drifted from what your instance now serves:

```bash
diff <(curl -sf https://prunto.example.com/prunto-screenshot/SKILL.md) \
  ~/.claude/skills/prunto-screenshot/SKILL.md
```

A copy that drifts is only a problem when it drifts on hostnames or limits. If you changed
`PRUNTO_BASE_URL`, the blob host or retention, tell people to re-run the install command;
the served file changed underneath them and nothing pushes it out.
