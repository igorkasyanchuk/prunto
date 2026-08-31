# Porting priito from Rails to Go

The Rails version is at [igorkasyanchuk/priito](https://github.com/igorkasyanchuk/priito) and
still runs. This is what changed, why, and what got worse.

## What stayed identical

The threat model, and every rule that came out of it: magic-byte sniffing, the six-format
allowlist, no SVG, no PDF, `.mov` refused, extension from the bytes, token in the header only,
per-token quotas, hash blocklist, the append-only audit trail that outlives the file it
describes, per-row expiry, the public takedown form with no account behind it.

Those were right in Ruby and they are right in Go. Porting them was translation, and the tests
in `sanitize_test.go` and `http_test.go` are the same corpus in a different language.

## What changed

**The 10 MB cap is enforced as the body is read.** The Rails version documents its own gap:
the check runs after Rails has buffered the whole request, so it stops the record being created
but not the bandwidth being spent. `http.MaxBytesReader` closes that. This was the reason to
port at all — without it the exercise would have been a translation.

**No libvips, and no re-encode.** The Rails version decodes and re-encodes through libvips so
that only pixels survive. This one strips at the container level: metadata chunks dropped,
everything past the end marker truncated. The full argument is in the README; the short version
is that re-encoding relocates decoder risk from a sandboxed browser to an unsandboxed server
process holding credentials, and creates the decompression bomb it then has to defend against.

That single decision is what makes the rest possible: `CGO_ENABLED=0`, a `FROM scratch` image,
and a 20 MB image instead of roughly 400.

**A boot-time CDN probe replaces what re-encoding was buying.** Re-encoding is protection that
survives a misconfigured deployment. So is refusing to start when the CDN is not serving
`nosniff` — and it costs twenty lines instead of a C dependency.

**Postgres, Redis, Sidekiq and sidekiq-cron are gone.** SQLite in a volume, counter rows for
rate limits, a `time.Ticker` goroutine for expiry. One container, `docker run`, no services.

**Two dependencies** instead of roughly eighty gems: a pure-Go SQLite driver and an S3 client.
`net/http`, `html/template` and `database/sql` cover everything else.

**A token in the query string is refused rather than ignored.** Same reasoning, stricter
behaviour: a caller who sent it that way has already leaked it and needs to be told.

**`CF-Connecting-IP` is handled explicitly.** Rails gave `request.remote_ip` with a
trusted-proxy list; Go gives a header string and no opinion. Getting this wrong would have
silently turned every rate limit into decoration, which makes it the sharpest thing the
framework was doing for free.

## What got worse

**Every security control is now hand-written.** Rails supplied `force_ssl`, CSP configuration,
escaped ERB, parameterised queries and strong parameters. Here they are lines of code that can
be forgotten, and nothing complains when one is. The test corpus exists because of this.

**No migrations.** The schema is an idempotent statement list run at boot. That works at this
size and will not work at three times it.

**Admin is hand-rolled HTML.** No form helpers, no partial rendering, no `link_to`. Fine for
five tables, tedious past that.

**More code.** Roughly 1,800 lines of Go against roughly 700 of Ruby for the same behaviour.

**One process, one machine.** SQLite and in-file counters mean a second replica would be wrong
in both directions. Rails could have scaled horizontally today.

**A payload inside the compressed pixel stream now survives.** The Rails version's re-encode
would have destroyed it. This is the one control genuinely given up, and it is bounded by the
same headers and separate origin that both versions already rely on for video.

## What it does not prove

That Go is better than Rails for this. It is a smaller, cheaper, more self-contained
deployment, and it is more work to change. If the roadmap in the README — accounts, per-user
quotas, a dashboard — actually happens, Rails does that in a weekend and this does not.
