# Cloudflare Tunnel

An outbound Cloudflare Tunnel is the zero-inbound-ports way to give Pebble
Cloud (or any MCP client) HTTPS reachability to `/mcp`. cloudflared dials out
to Cloudflare; nothing on the LAN accepts inbound connections.

## Locally-managed tunnel (config file)

Run on the machine serving ThingsIndex (the LXC or the Docker host), as root:

```sh
# 1. Install cloudflared
curl -fsSL -o /tmp/cloudflared.deb \
  "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-$(dpkg --print-architecture).deb"
dpkg -i /tmp/cloudflared.deb

# 2. Authenticate. This prints a https://dash.cloudflare.com/argotunnel?…
#    URL; open it in any browser (copy it to a desktop browser when the host
#    is headless) and select the zone. The certificate lands in
#    ~/.cloudflared/cert.pem.
cloudflared tunnel login

# 3. Create the tunnel and note the UUID it prints
cloudflared tunnel create things-index

# 4. Stage credentials and config (config.yml.example in this directory)
mkdir -p /etc/cloudflared
cp ~/.cloudflared/<UUID>.json /etc/cloudflared/
cp config.yml.example /etc/cloudflared/config.yml   # fill in the UUID and hostname

# 5. Validate the ingress rules before going live
cloudflared tunnel --config /etc/cloudflared/config.yml ingress validate

# 6. Create the public DNS record (a CNAME to <UUID>.cfargotunnel.com)
cloudflared tunnel route dns things-index things-index.example.com

# 7. Run as a service
cloudflared --config /etc/cloudflared/config.yml service install
systemctl is-active cloudflared && cloudflared tunnel info things-index
```

## Dashboard-managed tunnel (token)

See [`docker-compose.cloudflared.yml`](docker-compose.cloudflared.yml) for the
Compose sidecar. Configure the public hostname and the `/mcp` path rule in
Zero Trust > Networks > Tunnels.

## Verify from the edge

A LAN DNS record for the same hostname pointing at a local reverse proxy
(split-horizon) is a fine setup — LAN clients stay local, Pebble Cloud uses
the tunnel — but it means a plain `curl` from inside the LAN never touches
the tunnel. Pin the request to the Cloudflare edge to test the tunnel itself:

```sh
EDGE=$(dig +short things-index.example.com @1.1.1.1 | head -1)
curl -s -o /dev/null -w '%{http_code}\n' \
  --resolve "things-index.example.com:443:${EDGE}" \
  https://things-index.example.com/mcp       # 401 — reachable, auth enforced
curl -s -o /dev/null -w '%{http_code}\n' \
  --resolve "things-index.example.com:443:${EDGE}" \
  https://things-index.example.com/healthz   # 404 — correctly unpublished
```

## Rules that keep the deployment safe

- Publish only `^/mcp$`. The worker API, health endpoint, and dashboard stay
  private unless you deliberately opt the worker hostname in — and then put a
  Cloudflare Access service-token policy in front of it.
- All ThingsIndex responses are plain JSON and every wait (8s sync result,
  25s worker long poll) sits far below Cloudflare's proxied-request timeout,
  so no tunnel tuning is required.
- Cloudflare terminates TLS at its edge, so bearer tokens are visible to
  Cloudflare's infrastructure. That is the standard tunnel trade-off; if it
  is unacceptable, terminate TLS yourself at a proxy you control (contract
  in [`../README.md`](../README.md)).
