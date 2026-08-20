# Deploying ThingsIndex

One-command installers, service templates, and HTTPS ingress examples.

## Installers

| Script | Runs on | Does |
| :--- | :--- | :--- |
| [`proxmox-install.sh`](proxmox-install.sh) | Proxmox VE host | Provisions an LXC container, builds the server, prints tokens |
| [`proxmox-update.sh`](proxmox-update.sh) | Proxmox VE host | Pulls, rebuilds, and restarts an installed server |
| [`mac-install.sh`](mac-install.sh) | a Mac with Things 3 | All-in-one local mode: attested binary + Claude Desktop config |
| [`mac-worker-install.sh`](mac-worker-install.sh) | a Mac with Things 3 | Worker mode: attested binary + setup wizard against a remote server |

Service templates for running the pieces by hand live in
[`systemd/`](systemd/) (Linux server) and [`launchd/`](launchd/) (Mac).

## HTTPS ingress: the contract

The server deliberately speaks plain HTTP on a loopback or private address
and leaves TLS to whatever fronts it. **Any reverse proxy or tunnel works** —
Caddy, nginx, HAProxy, Traefik, Cloudflare Tunnel, Tailscale Serve — as long
as it satisfies this contract:

- **Terminate TLS with a publicly trusted certificate.** The Mac worker
  validates against the system trust store, so self-signed certificates are
  rejected; for internal-only hostnames, Let's Encrypt via the DNS-01
  challenge issues real certificates without exposing anything.
- **Publish exactly `/mcp`** to wherever your MCP clients live. Keep
  `/worker/`, `/healthz`, and `/dashboard` off the public internet — they
  belong on private routing (LAN, VPN, or an authenticated tunnel policy).
- **No timeout tuning needed.** Every wait the server holds is 25 seconds or
  less (8s synchronous capture window, 25s worker long-poll), far below
  common proxy defaults.

Verify any setup the same way:

```sh
curl -s -o /dev/null -w '%{http_code}\n' https://<public-host>/mcp       # 401 - auth required
curl -s -o /dev/null -w '%{http_code}\n' https://<public-host>/healthz   # 404 - not published
```

## Worked examples

Two configurations this project's maintainer runs, kept current and
copy-paste ready — as examples, not endorsements:

- [`traefik/`](traefik/) — an inbound LAN reverse proxy: dynamic-config
  fragment with the `/mcp` route rate-limited and the worker/dashboard
  routes IP-restricted.
- [`cloudflare/`](cloudflare/) — an outbound Cloudflare Tunnel: publishes
  only `/mcp` to the internet with zero inbound ports on your network.

Bringing a different proxy? Point it at the server per the contract above
and run the two verification curls.

No domain? The worker's one exception to the contract is plain HTTP to a
literal loopback address:
[`launchd/com.nejmlabs.things-index-tunnel.plist.example`](launchd/com.nejmlabs.things-index-tunnel.plist.example)
keeps an SSH tunnel to the server pinned up across reboots for exactly that
— LAN-only, publishes nothing. Walkthrough in
[`docs/homelab.md`](../docs/homelab.md).
