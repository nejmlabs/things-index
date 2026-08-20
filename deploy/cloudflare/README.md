# Cloudflare Tunnel

An outbound Cloudflare Tunnel is the zero-inbound-ports way to give Pebble
Cloud (or any MCP client) HTTPS reachability to `/mcp`. cloudflared dials out
to Cloudflare; nothing on the LAN accepts inbound connections.

- **Locally-managed tunnel** (config file): see
  [`config.yml.example`](config.yml.example). Install cloudflared in the LXC
  or on the Docker host, `cloudflared tunnel create things-index`, fill in
  the UUID, route DNS with `cloudflared tunnel route dns`, and run it as a
  service.
- **Dashboard-managed tunnel** (token): see
  [`docker-compose.cloudflared.yml`](docker-compose.cloudflared.yml) for the
  Compose sidecar. Configure the public hostname and the `/mcp` path rule in
  Zero Trust > Networks > Tunnels.

Rules that keep the deployment safe:

- Publish only `^/mcp$`. The worker API, health endpoint, and dashboard stay
  private unless you deliberately opt the worker hostname in — and then put a
  Cloudflare Access service-token policy in front of it.
- All ThingsIndex responses are plain JSON and every wait (8s sync result,
  25s worker long poll) sits far below Cloudflare's proxied-request timeout,
  so no tunnel tuning is required.
- Cloudflare terminates TLS at its edge, so bearer tokens are visible to
  Cloudflare's infrastructure. That is the standard tunnel trade-off; use
  the Traefik profile instead if it is unacceptable.
