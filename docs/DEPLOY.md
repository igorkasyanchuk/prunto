# Deploying it

prunto is one container that keeps everything - the SQLite database, the audit trail and the
uploaded files - in `/data`. Two facts follow, and they decide every platform choice below.

**A platform without a persistent disk cannot run prunto.** There is no bucket to fall back
on. Every deploy, restart and scale event on a disk-less platform silently discards the
database and every uploaded file, so pull requests across your organisation fill with broken
images. This rules out DigitalOcean App Platform, Heroku, Vercel, Netlify, Cloudflare Workers,
Google Cloud Run and AWS App Runner. Not "works with a caveat" - do not deploy it there.

**One instance, never two.** Two containers mean two SQLite files and two halves of your
uploads. Leave autoscaling off and pin the count to 1. For the same reason, keep the volume on
real block storage: SQLite on a network share (NFS, SMB, Azure Files, gcsfuse) corrupts under
locking that those filesystems do not implement properly.

Everything else is a detail. Any platform that runs a Docker image with a volume at `/data`
works, and the four environment variables never change:

```bash
PRUNTO_BASE_URL=https://prunto.example.com   # exact public origin, no trailing slash
ADMIN_USER=admin
ADMIN_PASSWORD=something-you-did-not-reuse
TRUST_PROXY=forwarded                        # cloudflare behind Cloudflare, none if exposed directly
```

## What is in this repository

| File | Used by | State |
| --- | --- | --- |
| `docker-compose.yml` | Docker, Coolify, Dokploy, Portainer, Easypanel, any VPS, most NAS boxes | Boot-tested |
| `render.yaml` | Render blueprint, behind the deploy button | Untested by us |
| `captain-definition` | CapRover (`caprover deploy`) | Untested by us |
| [COOLIFY.md](../COOLIFY.md) | Coolify, step by step with the production checklist | Written from a live deploy |

## Compose, which covers most of the list

Coolify, Dokploy, Easypanel, Portainer and a bare VPS all take the compose file straight from
the repository. So does Synology's Container Manager and any other NAS that speaks compose.

```bash
ADMIN_PASSWORD='something-you-did-not-reuse' \
PRUNTO_BASE_URL=https://prunto.example.com \
TRUST_PROXY=forwarded \
docker compose up -d
```

`ADMIN_PASSWORD` is required and the file refuses to start without it: with no admin there is
no way to mint a token in a browser.

## Render

`render.yaml` is a blueprint, so this is a genuine one-click: point Render at the repository
and it builds the Dockerfile, attaches a 5 GB disk at `/data` and generates the admin password.

```markdown
[![Deploy to Render](https://render.com/images/deploy-to-render-button.svg)](https://render.com/deploy?repo=https://github.com/igorkasyanchuk/prunto)
```

Two things it cannot do for you. `PRUNTO_BASE_URL` is marked `sync: false`, so Render asks
during setup - you do not know the hostname until the service exists, so enter the
`onrender.com` name it offers and correct it afterwards if you move to a custom domain. And
disks need a paid instance type; a free service keeps no files at all.

## Railway

Railway buttons come from a template you create in the dashboard, not from a file in the
repository, so nobody can hand you one from here. Build the template once against this repo
with a volume mounted at `/data` and the four variables above, and Railway hands you a
`railway.com/deploy?template=...` URL to paste anywhere. Every deploy after that is one click.

## Fly.io

Volumes are native. `fly launch` is interactive and writes its own config, so there is nothing
useful to commit:

```bash
fly launch --image ghcr.io/igorkasyanchuk/prunto:latest --no-deploy
fly volumes create prunto_data --size 5
fly secrets set ADMIN_USER=admin ADMIN_PASSWORD='...' \
  PRUNTO_BASE_URL=https://your-app.fly.dev TRUST_PROXY=forwarded
fly deploy
```

Mount the volume at `/data` in `fly.toml` and keep `min_machines_running = 1` with autoscaling
off - a second machine gets a second, empty volume.

## CapRover

`captain-definition` points at the Dockerfile, so a CapRover box deploys the repo directly:

```bash
caprover deploy
```

Then add a persistent directory mapped to `/data` in the app's **App Configs**, set the four
variables, and enable HTTPS. CapRover restarts the container on every config change, which is
the fastest way to confirm the volume is real: your token should survive it.

## Koyeb, Northflank, Zeabur, Sliplane, Elestio

Same shape on all five, none of which needs this repository at all - point them at
`ghcr.io/igorkasyanchuk/prunto:latest`, mount a volume at `/data`, set the four variables,
expose port 3000, and fix the instance count at 1. Koyeb and Northflank both have deploy-button
URLs generated from their dashboards; Zeabur and Elestio use templates. Check volume support in
your region before you start - on several of these it is newer than the platform and not
offered everywhere.

## Kubernetes, and anything built on it

A `StatefulSet` with one replica and a `volumeClaimTemplate`, which is the whole design:

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: prunto
spec:
  serviceName: prunto
  replicas: 1 # never more: one SQLite file, one volume
  selector:
    matchLabels: { app: prunto }
  template:
    metadata:
      labels: { app: prunto }
    spec:
      containers:
        - name: prunto
          image: ghcr.io/igorkasyanchuk/prunto:latest
          ports: [{ containerPort: 3000 }]
          envFrom: [{ secretRef: { name: prunto } }]
          volumeMounts: [{ name: data, mountPath: /data }]
          readinessProbe:
            httpGet: { path: /up, port: 3000 }
  volumeClaimTemplates:
    - metadata: { name: data }
      spec:
        accessModes: [ReadWriteOnce]
        resources: { requests: { storage: 5Gi } }
```

`ReadWriteOnce` on purpose. The four variables go in a `Secret` named `prunto`, and the
rollout strategy has to be `Recreate`-shaped - a rolling update wants two pods holding one
volume, and that is the one way to corrupt the database.

## Any VPS - the shortest path that exists

```bash
docker run -d --name prunto -p 3000:3000 -v prunto:/data \
  -e PRUNTO_BASE_URL=https://prunto.example.com \
  -e ADMIN_USER=admin -e ADMIN_PASSWORD='...' \
  -e TRUST_PROXY=forwarded \
  --restart unless-stopped ghcr.io/igorkasyanchuk/prunto
```

Hetzner, DigitalOcean Droplets, Lightsail instances, EC2, a Raspberry Pi, an old laptop. Put a
reverse proxy in front for TLS and cap the request body there too - bytes refused at the proxy
are bytes prunto never pays for.

## Before you call any of these done

- `GET /up` returns 200.
- `/admin` asks for the password you set, and minting a token works.
- Upload a file and open the returned URL in a private window - that proves `PRUNTO_BASE_URL`
  and the blob path agree.
- Restart the container and check the token and the file are still there. This is the step
  that catches a missing volume, and the only one that still matters a week later.
- Back the volume up. It holds the database and every upload, and there is no second copy.

If the first boot log says permission denied under `/data`, the platform attached the volume
as root while the image runs as uid 65532. That is a platform-level fix - some let you set the
mount's owner, others need an init container to `chown` it once.
