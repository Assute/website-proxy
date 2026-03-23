# Alpine Deployment

The `website-proxy` binary in this repository is built for Alpine Linux.

This release uses:

- binary: `website-proxy`
- OpenRC script: `website-proxy.initd`
- config file: `config.json`
- default port: `16800`

Upload these files to the server first:

- `/opt/website-proxy/website-proxy`
- `/opt/website-proxy/website-proxy.initd`
- `/opt/website-proxy/config.json`

## First Install

Run:

```sh
mkdir -p /opt/website-proxy
chmod +x /opt/website-proxy/website-proxy
cp /opt/website-proxy/website-proxy.initd /etc/init.d/website-proxy
chmod +x /etc/init.d/website-proxy
pkill -f /opt/website-proxy/website-proxy || true
rm -f /run/website-proxy.pid
rc-update add website-proxy default
rc-service website-proxy start
rc-service website-proxy status
tail -n 50 /var/log/website-proxy.log
netstat -lntp 2>/dev/null | grep 16800
```

## Update Existing Version

If you upload a new binary or a new init script later, run:

```sh
rc-service website-proxy stop || true
pkill -f /opt/website-proxy/website-proxy || true
rm -f /run/website-proxy.pid
cp /opt/website-proxy/website-proxy.initd /etc/init.d/website-proxy
chmod +x /opt/website-proxy/website-proxy
chmod +x /etc/init.d/website-proxy
rc-service website-proxy start
rc-service website-proxy status
tail -n 50 /var/log/website-proxy.log
```

## Edit Website Without Rebuilding

The program now reads `/opt/website-proxy/config.json` automatically.

Common fields:

- `target_url`: main website opened by `/go`
- `allowed_host_suffixes`: allowed root domains and all their subdomains
- `port`: listen port
- `upstream_proxy_on`: whether to enable upstream SOCKS5
- `upstream_proxy_url`: SOCKS5 URL
- `access_log`: whether to print per-request logs

Example:

```json
{
  "target_url": "https://example.com",
  "allowed_host_suffixes": [
    "example.com"
  ],
  "port": 16800,
  "upstream_proxy_on": false,
  "upstream_proxy_url": "",
  "access_log": false
}
```

After editing `config.json`, just restart:

```sh
rc-service website-proxy stop || true
pkill -f /opt/website-proxy/website-proxy || true
rm -f /run/website-proxy.pid
rc-service website-proxy start
tail -n 50 /var/log/website-proxy.log
```

## Quick Upgrade

If you only replaced `/opt/website-proxy/website-proxy`, run:

```sh
rc-service website-proxy stop || true
pkill -f /opt/website-proxy/website-proxy || true
rm -f /run/website-proxy.pid
chmod +x /opt/website-proxy/website-proxy
rc-service website-proxy start
rc-service website-proxy status
tail -n 50 /var/log/website-proxy.log
```

## Common Commands

```sh
rc-service website-proxy start
rc-service website-proxy stop
rc-service website-proxy restart
rc-service website-proxy status
tail -f /var/log/website-proxy.log
ps | grep website-proxy
```

## If Port 16800 Is Already In Use

Run:

```sh
rc-service website-proxy stop || true
pkill -f /opt/website-proxy/website-proxy || true
rm -f /run/website-proxy.pid
: > /var/log/website-proxy.log
rc-service website-proxy start
rc-service website-proxy status
tail -n 50 /var/log/website-proxy.log
```

重构
```sh
rc-service website-proxy stop || true
pkill -f /opt/website-proxy/website-proxy || true
rm -f /run/website-proxy.pid
chmod +x /opt/website-proxy/website-proxy
rc-service website-proxy start
tail -n 50 /var/log/website-proxy.log

```

## Notes

- Use `rc-service` to manage the service. Do not also run `./website-proxy` manually in another terminal.
- If you see `bind: address already in use`, an old process is still alive. Stop it with `pkill -f /opt/website-proxy/website-proxy || true`, remove `/run/website-proxy.pid`, and start again.
- If `netstat` is missing on Alpine, install it with `apk add net-tools`.
- Main proxy entry example: `http://your-server:16800/go`
- Direct proxy entry example: `http://your-server:16800/go/https/app.example.com/#/login`
- Root path `/` now returns `404` and no longer exposes the default site content.
- To reduce CPU, request access logging is now disabled by default. If you really need it, start the service with `ACCESS_LOG=1`.
- If `config.json` exists in `/opt/website-proxy`, it will be loaded automatically. Environment variables still override the config file.
