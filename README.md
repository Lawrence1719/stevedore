# Stevedore

A self-hosted mini-PaaS: `git push → build → deploy → rollback`, with live log streaming.  
Single operator, multiple Dockerfile-based apps, running on one VPS.

---

## Architecture

```
GitHub push → Nginx (TLS termination) → Stevedore agent (Go)
                                              │
                     ┌────────────────────────┤
                     │                        │
              Build Engine             Runtime Manager
           (Docker SDK build)      (Docker SDK run/stop/health)
                     │                        │
              Log Streamer ──────────── SQLite Version Store
                     │
              SSE → CLI / browser
```

See [system-plan-stevedore.md](./system-plan-stevedore.md) for full design rationale.

---

## Requirements

- Go 1.22+
- Docker (host daemon, socket at `/var/run/docker.sock`)
- Git (on the agent host, for `CloneOrPull`)
- Nginx + Certbot (existing setup, reused for TLS)

---

## Local Development & Testing (No VPS required)

You can run and test Stevedore fully on your local machine.

### 1. Set up directories
```bash
mkdir -p /tmp/stevedore/{logs,repos}
```

### 2. Start the Agent
Set the environment variables and run the agent binary:
```bash
export AGENT_API_TOKEN="localtest123"
export STEVEDORE_DB_PATH="/tmp/stevedore/data.db"
export STEVEDORE_LOG_DIR="/tmp/stevedore/logs"
export STEVEDORE_REPO_DIR="/tmp/stevedore/repos"
export STEVEDORE_ADDR=":8080"

go build -o agent ./cmd/agent
./agent
```

### 3. Configure the CLI
In a second terminal, build the CLI and configure it to talk to the local agent:
```bash
go build -o cli ./cmd/cli

mkdir -p ~/.stevedore
cat > ~/.stevedore/config <<EOF
AGENT_URL=http://localhost:8080
AGENT_TOKEN=localtest123
EOF
chmod 600 ~/.stevedore/config
```

### 4. Create a local Git repository for testing
To test without hitting Docker Hub limits or network blocks, create a local repository using a base image you already have cached locally (e.g., `ubuntu:24.04`):
```bash
mkdir -p /tmp/test-repo && cd /tmp/test-repo
git init
git config user.email "test@test.com"
git config user.name "Test"

cat > Dockerfile << 'EOF'
FROM ubuntu:24.04
CMD ["bash", "-c", "echo 'Stevedore deployed this!' && sleep infinity"]
EOF

git add . && git commit -m "Initial"
```

### 5. Register and Deploy the local app
```bash
# Register using file:// protocol
./cli apps register
# App name: local-test
# Git repo URL: file:///tmp/test-repo
# Branch: main (rename your local branch to main first if default is master: git branch -m main)
# Webhook / Env / Health check: press Enter to skip

# Deploy
./cli deploy local-test

# Check status and copy the generated deploy ID
./cli status local-test

# Stream logs
./cli logs local-test --deploy <deploy-id>
```

---

## Production VPS Setup (Requires a VPS)

> [!NOTE]
> The automated GitHub Actions workflow (`.github/workflows/build-and-deploy.yml`) is configured to deploy to a remote VPS. Until you provision a VPS and configure the repository secrets on GitHub, the deploy step in CI will fail. The rest of the setup below applies to your future VPS host.

### 1. Create the `stevedore` system user

```bash
sudo useradd --system --no-create-home --shell /bin/false stevedore
sudo usermod -aG docker stevedore   # needs Docker socket access
```

### 2. Create directories

```bash
sudo mkdir -p /opt/stevedore/{logs,repos,backups}
sudo chown -R stevedore:stevedore /opt/stevedore
```

### 3. Create env file

```bash
sudo tee /opt/stevedore/agent.env > /dev/null <<EOF
AGENT_API_TOKEN=$(openssl rand -hex 32)
STEVEDORE_DB_PATH=/opt/stevedore/data.db
STEVEDORE_LOG_DIR=/opt/stevedore/logs
STEVEDORE_REPO_DIR=/opt/stevedore/repos
STEVEDORE_ADDR=:8080
EOF
sudo chmod 600 /opt/stevedore/agent.env
sudo chown stevedore:stevedore /opt/stevedore/agent.env
```

### 4. Build and install binaries

```bash
CGO_ENABLED=0 go build -o agent ./cmd/agent
CGO_ENABLED=0 go build -o cli   ./cmd/cli
sudo cp agent /opt/stevedore/agent
sudo cp cli   /usr/local/bin/stevedore
```

### 5. Install systemd service

```bash
sudo cp stevedore.service /etc/systemd/system/stevedore.service
sudo systemctl daemon-reload
sudo systemctl enable --now stevedore
sudo systemctl status stevedore
```

### 6. Configure Nginx (reverse proxy)

```nginx
server {
    listen 443 ssl;
    server_name stevedore.yourdomain.com;

    # ... Certbot-managed SSL ...

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        # Required for SSE log streaming:
        proxy_set_header Connection '';
        proxy_buffering off;
        proxy_cache off;
        chunked_transfer_encoding on;
    }
}
```

### 7. Configure CLI

```bash
mkdir -p ~/.stevedore
cat > ~/.stevedore/config <<EOF
AGENT_URL=https://stevedore.yourdomain.com
AGENT_TOKEN=<token from agent.env>
EOF
chmod 600 ~/.stevedore/config
```

---

## Usage

### Register an app

```bash
stevedore apps register
# interactive: name, repo URL, branch, webhook secret, env file, health check URL
```

### Manual deploy

```bash
stevedore deploy myapp
stevedore deploy myapp --sha abc123def
```

### Rollback

```bash
stevedore rollback myapp
```

### Status

```bash
stevedore status myapp
```

### Live log streaming

```bash
stevedore logs myapp --deploy <deploy-id>
```

### GitHub webhook

Set the payload URL to:  
`https://stevedore.yourdomain.com/webhook/<app-name>`

Content type: `application/json`  
Secret: the `webhook_secret` you set when registering the app.

---

## Database backup

```bash
# Add to crontab (runs daily, keeps 7 days):
0 2 * * * sqlite3 /opt/stevedore/data.db ".backup /opt/stevedore/backups/data-$(date +\%F).db" && find /opt/stevedore/backups -mtime +7 -delete
```

---

## GitHub Actions secrets required

| Secret | Description |
|---|---|
| `VPS_HOST` | VPS IP or hostname |
| `VPS_USER` | SSH user with sudo access |
| `VPS_SSH_KEY` | Private SSH key (ed25519 recommended) |
| `AGENT_API_TOKEN` | Must match the token in `agent.env` |

---

## Project Structure

```
stevedore/
├── cmd/
│   ├── agent/main.go          # daemon entrypoint
│   └── cli/main.go            # CLI entrypoint
├── internal/
│   ├── api/                   # HTTP server, handlers, auth middleware
│   ├── build/                 # Docker image build + git clone/pull
│   ├── logstream/             # SSE fan-out + log file persistence
│   ├── orchestrator/          # per-app deploy pipeline coordination
│   ├── runtime/               # container lifecycle + health checks
│   ├── store/                 # SQLite store + models
│   └── webhook/               # GitHub HMAC webhook receiver
├── migrations/
│   └── 0001_init.sql
├── stevedore.service          # systemd unit
├── .github/workflows/
│   └── build-and-deploy.yml  # CI/CD pipeline
└── system-plan-stevedore.md  # full design rationale
```
