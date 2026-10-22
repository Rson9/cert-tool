# cert-tool

> One command to issue HTTPS certificates and generate production-ready Nginx reverse proxies — with automatic renewal, wildcard support and declarative management.

[![CI](https://github.com/Rson9/cert-tool/actions/workflows/ci.yml/badge.svg)](https://github.com/Rson9/cert-tool/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Rson9/cert-tool)](https://github.com/Rson9/cert-tool/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**English** | [简体中文](README.zh_CN.md)

<p align="center"><img src="docs/demo.gif" alt="Demo: install → add site → verify → wildcard & declarative apply → clean uninstall" width="800"></p>
<p align="center"><sub>install · init · one-command HTTPS · wildcard + declarative apply · clean uninstall (~1 min, <a href="docs/demo.mp4">MP4</a>)</sub></p>

`cert` is a single-binary CLI for Linux servers (VPS, home labs, self-hosted services). It automates the whole HTTPS workflow with a **built-in ACME client**: certificate issuance via [Let's Encrypt](https://letsencrypt.org) (HTTP-01 and DNS-01), Nginx reverse proxy generation, WebSocket support, unattended renewal and declarative, idempotent site management.

---

## ✨ Features

- **Built-in ACME client** — no external dependencies to install. Powered by [lego](https://github.com/go-acme/lego); the ACME account key and certificates are managed on disk, everything else is automatic.
- **HTTP-01 and DNS-01 challenges** — HTTP-01 through a shared webroot that never disturbs running sites; DNS-01 for **wildcard certificates** (`*.example.com`) with first-class support for Cloudflare, AliDNS, DNSPod, Tencent Cloud, DuckDNS, Route 53 — and a guided `manual` mode that works with any DNS host.
- **Declarative management** — describe all sites in `cert-tool.yml` and run `cert apply`: concurrent provisioning, idempotent re-runs, `--dry-run` plans and safe `--prune` cleanup of managed sites only.
- **Real auto-renewal** — a daily `cert renew` job (installed as `/etc/cron.d/cert-tool`) renews certificates 30 days before expiry and reloads Nginx. Idempotent and safe to run at any time.
- **Production-grade Nginx config** — HTTP → HTTPS redirect, TLS 1.2/1.3, modern cipher suite, WebSocket upgrade support, sane timeouts, automatic adaptation to the installed Nginx version.
- **Distro-agnostic** — detects `apt` / `dnf` / `yum` / `apk` / `pacman` and works with `systemd` or `OpenRC`.
- **Safe by design** — strict validation of every input before it reaches the filesystem, the Nginx config or a shell; only files marked as managed are ever modified; private keys stored with `600`.
- **Diagnostics** — `cert list` shows upstreams and certificate expiry countdowns; `cert test` validates Nginx, public HTTPS and the backend service in one go.

## 📋 Requirements

- A Linux server with a public IP (macOS binaries are provided for development/testing).
- For HTTP-01: a domain whose DNS **A/AAAA record points to the server**, with ports **80 and 443** reachable from the internet.
- For DNS-01 / wildcards: API credentials for a supported DNS provider (or an interactive terminal for `manual` mode).
- Root or `sudo` (commands that manage the system re-run themselves through `sudo`, one prompt per run).
- Installed automatically by `cert init`: `nginx`, `curl`, `openssl`, `cron`.

## 🚀 Installation

### One-line install (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/Rson9/cert-tool/main/install.sh | sudo bash
```

Install a specific version or to a custom directory:

```bash
curl -fsSL https://raw.githubusercontent.com/Rson9/cert-tool/main/install.sh | sudo bash -s -- --version v1.0.0
curl -fsSL https://raw.githubusercontent.com/Rson9/cert-tool/main/install.sh | sudo bash -s -- --install-dir /opt/bin
```

The installer detects your OS/architecture, downloads the matching release binary, verifies its SHA-256 checksum and installs it to `/usr/local/bin/cert`.

> Using a mirror or self-hosted releases? Set `CERT_TOOL_BASE_URL=https://your.mirror/releases` (see [install.sh](install.sh) for all options).

### Manual install

Download the binary for your platform from the [releases page](https://github.com/Rson9/cert-tool/releases):

```bash
sudo install -m 755 cert-linux-amd64 /usr/local/bin/cert
```

### Build from source

```bash
git clone https://github.com/Rson9/cert-tool.git
cd cert-tool
make build          # or: make release  (all platforms, into dist/)
sudo make install
```

## 🏁 Quick start

Two commands:

```bash
# 1. Initialize the server (installs nginx, creates the shared ACME endpoint and renewal cron job)
sudo cert init

# 2. Add a site: api.example.com -> http://127.0.0.1:8080
sudo cert add api.example.com -p 8080
```

Verify with `curl -I https://api.example.com` — done. Certificates renew automatically.

Optionally set defaults so you can use short names and receive expiry reminders:

```bash
sudo cert init -d example.com -e admin@example.com   # then: cert add api -p 8080
```

Everything below (wildcards, DNS-01, declarative management) is **optional** — the two commands above are all most users need.

Managing many sites? You can also describe everything declaratively:

```bash
cat > cert-tool.yml <<'EOF'
email: admin@example.com
base_domain: example.com
default_port: 8080
sites:
  - domain: api
  - domain: "*.example.com"
    dns_provider: cloudflare
EOF

sudo cert apply -f cert-tool.yml
```

## 📖 Usage

### Imperative: add and remove sites

```bash
cert add api                       # api.example.com -> http://127.0.0.1:8080 (HTTP-01)
cert add web -p 3000               # web.example.com -> http://127.0.0.1:3000
cert add foo.bar.com -p 5000       # standalone domain  -> http://127.0.0.1:5000
cert add node -u http://10.0.0.2   # proxy to another machine on the LAN
cert add api -d other.com          # api.other.com, overriding the base domain
cert add api --san www.example.com # one certificate covering both names
cert add '*.example.com' --dns cloudflare   # wildcard certificate (DNS-01)
cert add api --staging             # issue against the Let's Encrypt staging environment
cert remove api                    # remove a site again
```

| Flag | Description |
| --- | --- |
| `-p, --port <port>` | Local upstream port (default: the configured `default_port`) |
| `-u, --upstream <url>` | Full upstream: `host[:port]`, `http(s)://host[:port][/path]` or `unix:/path.sock` |
| `-d, --domain <domain>` | Base domain for short subdomain names |
| `--dns <provider>` | Use DNS-01 with the given provider (required for wildcards) |
| `--san <domain>` | Add a Subject Alternative Name to the certificate |
| `--staging` | Use the Let's Encrypt staging environment (for testing) |

### Declarative: `cert apply` (optional — for many sites at once)

`cert-tool.yml` (YAML or JSON) describes the desired state of the server:

```yaml
email: admin@example.com      # ACME account e-mail
base_domain: example.com      # short names expand against this
default_port: 8080            # default upstream port
dns_provider: cloudflare      # optional default for all sites
staging: false                # optional, use LE staging
sites:
  - domain: api               # -> api.example.com
  - domain: foo.bar.com
    upstream: http://10.0.0.2:5000
  - domain: "*.example.com"
    dns_provider: cloudflare
  - domain: app.example.com
    upstream: unix:/run/app.sock
    sans: [www.example.com]
```

```bash
cert apply --dry-run                 # show the plan without changing anything
cert apply                           # create/update sites (3 workers in parallel)
cert apply --prune                   # also delete managed sites missing from the manifest
cert apply --workers 5 -f other.yml  # custom parallelism and manifest path
```

Safety guarantees:

- **Idempotent** — re-running `apply` with an unchanged manifest does nothing.
- **Non-destructive** — only Nginx configs marked `# Managed by cert-tool` are ever modified or deleted; your own configs are untouchable.
- **Concurrent** — certificate issuance for multiple sites runs in parallel (bounded worker pool).

### Other commands

| Command | Description |
| --- | --- |
| `cert init [flags]` | Initialize the server: dependencies, shared ACME endpoint, renewal cron job |
| `cert renew [--days N] [--force]` | Renew certificates expiring within N days (default 30) |
| `cert list` | List sites with upstreams and certificate expiry countdowns |
| `cert test <domain>` | Diagnose Nginx syntax, public HTTPS and the backend service |
| `cert remove <domain>` | Remove a site's Nginx config, certificate and metadata |
| `cert check` | Check dependencies, privileges and current settings |
| `cert dns` | List supported DNS-01 providers and their credentials |
| `cert config show\|path\|set` | Inspect or change global settings |
| `cert install` | Install the current binary to `/usr/local/bin/cert` |
| `cert update [--check]` | Self-update the cert binary to the latest release |
| `cert version` | Print the version |

### Global configuration

```bash
cert config set domain example.com     # base domain for short subdomains
cert config set port 8080              # default upstream port
cert config set email admin@example.com
cert config show                       # print the effective configuration
cert config path                       # print the config file location
```

Configuration lives in `/etc/cert-tool/config.json` (or `~/.config/cert-tool/config.json` for non-root usage; override with `CERT_CONFIG`).

## 🌐 Wildcard certificates & DNS-01 (optional)

Wildcard certificates can only be issued via DNS-01. Pick a provider (`cert dns` for the full list) and export its credentials as environment variables:

```bash
export CLOUDFLARE_DNS_API_TOKEN=...
sudo --preserve-env=CLOUDFLARE_DNS_API_TOKEN cert add '*.example.com' --dns cloudflare
```

| Provider | Credentials (environment variables) |
| --- | --- |
| `cloudflare` | `CLOUDFLARE_DNS_API_TOKEN` |
| `alidns` (Alibaba Cloud) | `ALICLOUD_ACCESS_KEY`, `ALICLOUD_SECRET_KEY` |
| `dnspod` (DNSPod) | `DNSPOD_API_KEY` |
| `tencentcloud` (Tencent Cloud) | `TENCENTCLOUD_SECRET_ID`, `TENCENTCLOUD_SECRET_KEY` |
| `duckdns` | `DUCKDNS_TOKEN` |
| `route53` (AWS) | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` |
| `manual` | none — prints the TXT record and waits for you (interactive) |

> The `manual` provider works with *any* DNS host but is interactive, so it can only be used for initial issuance — pair it with an API-backed provider for unattended renewal, or renew manually with `cert renew --force`.

## ⚙️ How it works (for the curious)

1. **ACME account** — on first issuance an ECDSA account key is generated at `/etc/cert-tool/account/account.key` and registered with Let's Encrypt. This key *is* your account — back it up.
2. **Challenge** — HTTP-01 serves `/.well-known/acme-challenge/` from `/var/www/cert-challenge` via a shared Nginx endpoint (`00-acme-challenge.conf`), so issuance never needs a free port and existing sites are undisturbed. DNS-01 creates TXT records through the provider API (required for wildcards).
3. **Issuance** — the built-in ACME client (lego) runs the order and returns the certificate (ECC P-256 by default, bundled chain).
4. **Installation** — certificate and key land in `/etc/nginx/ssl/` (key mode `600`), with a small `.meta.json` describing how the certificate was obtained (challenge type, provider, SANs) so renewal can repeat it.
5. **Reverse proxy** — a site config is generated in `/etc/nginx/conf.d/`, validated with `nginx -t` and reloaded. The template adapts to your Nginx version (e.g. `http2 on;` for ≥ 1.25.1).
6. **Renewal** — `/etc/cron.d/cert-tool` runs `cert renew --quiet` daily. Certificates within 30 days of expiry are re-issued (concurrently) and Nginx is reloaded once. Everything else is skipped, so the job is safe to run at any time.

Files written by the tool:

| Path | Purpose |
| --- | --- |
| `/etc/nginx/conf.d/<domain>.conf` | Per-site Nginx config (marked `# Managed by cert-tool`) |
| `/etc/nginx/conf.d/00-acme-challenge.conf` | Shared ACME HTTP-01 endpoint |
| `/etc/nginx/conf.d/00-cert-tool-map.conf` | Shared WebSocket `Connection` header map |
| `/etc/nginx/ssl/<domain>.pem` / `.key` | Certificate / private key (mode `600`) |
| `/etc/nginx/ssl/<domain>.meta.json` | Issuance metadata used by renewal |
| `/etc/cert-tool/account/account.key` | ACME account key (**back this up**) |
| `/etc/cert-tool/config.json` | Tool configuration |
| `/etc/cron.d/cert-tool` | Daily renewal job |

> Conflicting default sites on port 80 are **moved aside, never deleted** (`default.conf` → `default.conf.bak`, `sites-enabled/default` → `sites-enabled/default.disabled-by-cert-tool`).

## 🤝 Coexisting with an existing Nginx

The tool **coexists with your Nginx — it never takes it over**. Your server blocks, modules and settings are never rewritten; the tool only adds its own files (marked `# Managed by cert-tool`) and removes exactly those on uninstall.

Before provisioning, it inspects your full configuration (`nginx -T`) and adapts to it:

| Situation | Behaviour |
| --- | --- |
| Nginx already installed with your own sites | Never reinstalled or removed; only tool-owned files are added |
| You have your own `default_server` on port 80 | The shared ACME endpoint **backs off** and skips `default_server` — a duplicate would prevent Nginx from starting |
| Your `nginx.conf` does not include `conf.d/*.conf` | Clear error explaining what to add, instead of silently doing nothing |
| A foreign server block already serves the domain | Refused **before** issuing (HTTP-01 would fail there): disable that block or use DNS-01 |
| Distro default welcome page on port 80 | Moved aside (backed up) and **restored on uninstall** |

One thing to know: certificates for a domain whose traffic already hits a foreign server block need DNS-01 (`--dns <provider>`), because the HTTP-01 challenge is answered by that block — this is a property of HTTP-01 itself, not of this tool.

## 🧹 Uninstall — restore the pre-installation state

```bash
# one-line uninstall (downloads a matching binary if needed)
curl -fsSL https://raw.githubusercontent.com/Rson9/cert-tool/main/uninstall.sh | sudo bash

# or, with the binary still installed
sudo cert uninstall --dry-run    # preview the plan first
sudo cert uninstall              # then run it
```

Uninstall is **non-invasive by design**. The tool snapshots what installation changed (`/etc/cert-tool/state.json`) and reverses exactly that — nothing more, nothing less:

| Action | Details |
| --- | --- |
| Restore | default site files moved aside at install time are moved back |
| Remove | managed site configs, certificates, shared ACME endpoint and WebSocket map |
| Remove | renewal cron job, renewal log, tool configuration and ACME account |
| Restore | nginx is stopped / disabled again if installation started / enabled it |
| Remove | packages that `cert init` installed **and that were absent before** |
| Remove | the binary itself |

Flags:

- `--keep-sites` — keep Nginx configs and certificates, only remove the tool (renewal stops: renew manually or keep the cron job!)
- `--keep-packages` — keep packages installed by `cert init`
- `--dry-run` — show the plan without changing anything

Anything not listed in the plan is never touched: your own Nginx configs, files and packages stay exactly as they are.

## ❓ FAQ & troubleshooting

**`cert add` fails with "certificate issuance failed"**
- HTTP-01: the DNS A record must resolve to this server (`dig +short api.example.com`), port 80 must be reachable, and CDN proxies (e.g. Cloudflare orange cloud) must be disabled for the domain.
- DNS-01: check the provider credentials (`cert dns` lists what each provider needs) and watch for TXT propagation delays.
- Rate limits: Let's Encrypt allows ~50 certificates per registered domain per week. Use `--staging` while testing. See [rate limits](https://letsencrypt.org/docs/rate-limits/).

**How do I test without burning rate limits?** `cert add api --staging` issues an untrusted staging certificate. Everything works the same; re-issue without `--staging` when you're ready.

**How do I upgrade the tool itself?** `sudo cert update` (or `cert update --check` to only look). It downloads the matching release binary, verifies its SHA-256 checksum, smoke-tests it and atomically replaces the running one. Certificates and renewal keep working unchanged.

**How do I check that renewal works?**
```bash
sudo cert renew --force      # force a renewal run right now
cat /etc/cron.d/cert-tool    # the daily job
tail /var/log/cert-tool-renew.log
```

**Where is my certificate?** `/etc/nginx/ssl/<domain>.pem` (full chain) and `.key` (private key). Wildcards use `_wildcard` as the file name prefix.

**Can I use a different CA?** The engine speaks standard ACME. Set a custom directory URL in the code (`caProduction`) or open an issue if you need a configurable CA.

**Nginx warns about `listen ... http2`.** The tool detects your Nginx version and emits `http2 on;` for Nginx ≥ 1.25.1, legacy syntax otherwise.

**How do I remove the tool completely?** See [Uninstall](#-uninstall--restore-the-pre-installation-state): `sudo cert uninstall` restores the server to its pre-installation state.

## 🔐 Security notes

- Commands that manage the system re-execute themselves through `sudo`, so privilege handling is centralized and auditable.
- Domain and upstream inputs are strictly validated before touching the filesystem, Nginx config or any shell command.
- Generated files: Nginx configs `644`, private keys `600`, account key `600`.
- `apply` never touches Nginx configs that lack the `# Managed by cert-tool` marker.
- `uninstall` only ever reverses what installation changed (recorded in an install snapshot) and shows a full plan before doing anything.
- `install.sh` verifies SHA-256 checksums of downloaded binaries when published.

## 🤝 Contributing

Issues and pull requests are welcome!

```bash
git clone https://github.com/Rson9/cert-tool.git
cd cert-tool
make vet && make test
make release   # verify all release binaries build
```

Please keep the CLI output and both READMEs in sync when changing behavior.

## 📄 License

[MIT](LICENSE)

## 🙏 Acknowledgements

- [lego](https://github.com/go-acme/lego) — the ACME client library doing the heavy lifting.
- [Let's Encrypt](https://letsencrypt.org) — free certificates for everyone.
- [Nginx](https://nginx.org/) — the web server this tool configures.
