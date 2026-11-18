# Production deployment

The relay at `https://diskspace.cc` is updated by a signed webhook that the
Release workflow (or a manual Deploy run) calls after an image is published.

## Host layout

```
/opt/diskspace/compose.yaml   # copy of deploy/compose.yaml
```

The relay runs as compose project `diskspace`, service `diskspace-relay`, image
`ghcr.io/zyno-io/diskspace-relay:latest`. It and the `cloudflared` connector
share the external Docker network `cloudflared-diskspace`.

## One-time sidecar install

The sidecar (`deploy/webhook/`) is a small container that verifies the request
and runs `docker compose up` for the single fixed service. Copy `deploy/webhook/`
to the host (for example `/opt/diskspace-deploy/`), then:

```sh
umask 077
sudo sh -c 'openssl rand -hex 32 > /opt/diskspace-deploy/secret'   # root-only, >= 32 chars
cd /opt/diskspace-deploy
sudo DISKSPACE_DEPLOY_SECRET_FILE=/opt/diskspace-deploy/secret \
  docker compose -f compose.yml up -d --build
```

Optional overrides: `DISKSPACE_DEPLOY_DIR` (default `/opt/diskspace`),
`DISKSPACE_DEPLOY_COMPOSE_FILE` (default `compose.yaml`),
`DISKSPACE_DEPLOY_NETWORK` (default `cloudflared-diskspace`).

The sidecar publishes no host ports. It joins the tunnel network with alias
`diskspace-deploy` and listens on port 9000.

## Cloudflare Tunnel route

In the Cloudflare dashboard, add a public hostname to the tunnel:

- Hostname `diskspace.cc`, path `^/hooks/diskspace-deploy$`
- Service `http://diskspace-deploy:9000`

Order it before the catch-all `diskspace.cc` route, or the relay will receive
the request instead.

## GitHub configuration

In the `release` environment of this repository:

- Secret `DEPLOY_WEBHOOK_SECRET`: the contents of the secret file above.
- Variable `DEPLOY_WEBHOOK_URL`: `https://diskspace.cc/hooks/diskspace-deploy`

## How a deploy works

`scripts/request-deploy.py` POSTs `{"version","timestamp"}` signed with
HMAC-SHA256 in `X-Diskspace-Signature`. The sidecar checks the signature, a
strict semver `version`, and a timestamp within -30s/+300s, takes a lock, then
runs `docker compose up --no-deps --no-build --pull always diskspace-relay` with
an override that changes only the image tag, and re-tags it `:latest`. Requests
cannot choose a command, repository or service. The workflow then polls
`https://diskspace.cc/version` and `/healthz`.

Full (non-prerelease) tags deploy automatically at the end of the Release
workflow.

## Rollback

Run the **Deploy published release** workflow manually with an older tag
(for example `v0.4.0`). Rollback is image-only; configuration lives in
`/opt/diskspace/compose.yaml`.
