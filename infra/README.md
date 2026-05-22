# readperfect Deployment Scripts

Infrastructure scripts for deploying `readperfect` to your DigitalOcean droplet using the same pattern as `feedback-ok`.

## Quick Start

### First Time Setup

1. Create deployment config
   ```bash
   cp deploy.config.example deploy.config
   ```

2. Edit `deploy.config`
   ```bash
   nano deploy.config
   ```

3. Run setup scripts in order
   ```bash
   ./infra/remote-setup.sh
   ./infra/remote-setup-env.sh
   ./infra/remote-setup-service.sh
   ./infra/remote-setup-nginx.sh
   ```

4. Point DNS to your droplet
   - `readperfect.com` -> droplet IP
   - `www.readperfect.com` -> droplet IP

5. Once DNS has propagated, set up SSL
   ```bash
   ./infra/remote-setup-ssl.sh
   ```

## Regular Deployment

Deploy the current branch:

```bash
./infra/deploy.sh
```

## Available Scripts

| Script | Purpose |
| --- | --- |
| `remote-setup.sh` | Clone repo and build on the server |
| `remote-setup-env.sh` | Create or update `.env` on the server |
| `remote-setup-service.sh` | Install systemd service |
| `remote-setup-nginx.sh` | Install nginx site config |
| `remote-setup-ssl.sh` | Run certbot for the configured domain |
| `deploy.sh` | Pull latest code, build, and restart |
| `rollback.sh` | Restore a previous binary backup |
| `status.sh` | Check service, HTTP, disk, DB, last deploy |
| `logs.sh` | Tail service or nginx logs |
| `ssh.sh` | SSH into the droplet quickly |
| `backup-db.sh` | Download the production SQLite DB locally |

## Configuration

All scripts read from `deploy.config` in the repo root.

Example values:

```bash
SSH_HOST="203.0.113.10"
SSH_USER="kgthegreat"
SSH_PORT="22"
APP_HOME="/home/kgthegreat/readperfect"
APP_PORT="8085"
DOMAIN="readperfect.com"
GIT_REPO="git@github.com:yourusername/readperfect.git"
GO_PATH="/usr/local/go/bin/go"
DB_PATH="./readperfect.db"
SERVICE_NAME="readperfect"
BINARY_NAME="readperfect"
NGINX_SITE_NAME="readperfect"
```

## App Environment

`remote-setup-env.sh` writes a production `.env` on the server using the variables this app actually reads:

- `PORT`
- `DATABASE_PATH`
- `COOKIE_SECURE`
- `BOOTSTRAP_ADMIN_EMAIL`
- `GOOGLE_CLIENT_ID`
- `GOOGLE_CLIENT_SECRET`
- `GOOGLE_REDIRECT_URL`

## Notes

- `deploy.config` is local-only and should not be committed.
- The production database is SQLite, so regular backups matter.
- The service runs directly from `APP_HOME` and keeps binary backups during deploys.
