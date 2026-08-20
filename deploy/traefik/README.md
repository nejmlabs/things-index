# Traefik reverse proxy

One of two worked examples of the HTTPS ingress contract in
[`../README.md`](../README.md) — any proxy satisfying that contract works.

[`things-index.yml`](things-index.yml) is a dynamic-configuration fragment for
an existing Traefik v2/v3 instance. It publishes:

- `things-index.example.com/mcp` — the public MCP endpoint (rate-limited);
- `things-index-worker.internal.example.com/worker/…` and `/healthz` — the Mac
  worker's private API (LAN and Tailscale ranges only);
- `things-index-dashboard.internal.example.com/dashboard` — the optional
  read-only dashboard (same restriction).

## Fill in

1. **Hostnames** — replace the `example.com` names, and create matching
   records in the internal DNS pointing at the Traefik host (see
   [`docs/homelab.md`](../../docs/homelab.md), "Internal DNS").
2. **Backend address** — point the `things-index` service URL at the server's
   address and port. Give that machine a static address or DHCP reservation:
   this file pins the address, and a reinstalled container that comes back on
   a new lease turns into silent 502s.
3. **Allow-list** — set `sourceRange` to the subnets the worker and admin
   machines actually connect from. A worker outside the range is rejected
   with 403 before authentication ever happens; if the worker cannot reach
   the server despite a valid token, check the Traefik access log for 403s
   and widen the range.
4. **Entry point and TLS** — `https` and the bare `tls: {}` assume Traefik
   already has an HTTPS entry point with a certificate resolver or default
   certificate. The Mac worker validates certificates against the system
   trust store, so use publicly trusted certificates; for internal-only
   hostnames, Let's Encrypt via the DNS-01 challenge issues them without
   exposing anything.

## Applying changes to a running Traefik

When the dynamic config is bind-mounted into the Traefik container **as a
single file**, most editors and `sed -i` replace the file's inode — the
container keeps reading the old contents and Traefik's file watcher never
fires. The symptom is an edit that visibly exists on the host while Traefik
keeps serving the previous routing (often as 502s). Restart the container
after every edit:

```sh
docker restart traefik
```

Mounting the containing directory instead of the single file avoids the
problem entirely. Either way, keep a backup copy before editing a live
config.

## Verify

```sh
curl -s -o /dev/null -w '%{http_code}\n' https://things-index.example.com/mcp        # 401 — auth required
curl -s -o /dev/null -w '%{http_code}\n' https://things-index-worker.internal.example.com/healthz  # 200
curl -s -o /dev/null -w '%{http_code}\n' https://things-index.example.com/healthz    # 404 — not published
```
