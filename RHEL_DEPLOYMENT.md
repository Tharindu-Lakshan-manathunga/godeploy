# godeploy — RHEL Deployment Guide

## Prerequisites

- RHEL 8 or 9 (Rocky, AlmaLinux also work)
- `go` 1.22+ installed (for cross-compile on your dev machine)
- SSH key access to the target RHEL box as a sudo user
- `systemd` (default on RHEL)
- `sshpass` installed on the RHEL host if you use password-based SSH to deploy targets

---

## 1. Build the Binary (on your dev machine)

```bash
# Cross-compile for Linux (from Windows PowerShell)
$env:GOOS="linux"; $env:GOARCH="amd64"
& "C:\Users\tharindu_m\Downloads\go1.26.4.windows-amd64\go\bin\go.exe" build -ldflags="-s -w" -o godeploy .
# Reset env
Remove-Item Env:\GOOS; Remove-Item Env:\GOARCH
```

This produces a single static `godeploy` binary (~12 MB). No runtime dependencies.

---

## 2. Prepare the RHEL Server

```bash
# Create a dedicated user (no login shell)
sudo useradd -r -s /sbin/nologin -d /opt/godeploy godeploy

# Create directories
sudo mkdir -p /opt/godeploy
sudo mkdir -p /etc/godeploy/keys
sudo mkdir -p /var/lib/godeploy

# Set ownership
sudo chown godeploy:godeploy /opt/godeploy /var/lib/godeploy
sudo chmod 750 /etc/godeploy /etc/godeploy/keys
```

---

## 3. Install the Binary

```bash
# SCP from your dev machine (run from Windows terminal):
scp godeploy rhel-user@RHEL_HOST:/tmp/godeploy

# On the RHEL host:
sudo mv /tmp/godeploy /usr/local/bin/godeploy
sudo chmod 755 /usr/local/bin/godeploy
sudo chown root:root /usr/local/bin/godeploy
```

---

## 4. Configure SSH Deploy Keys

For each target host that godeploy will SSH into to deploy services:

```bash
# Generate a deploy key pair (on the RHEL host running godeploy)
sudo -u godeploy ssh-keygen -t ed25519 -C "godeploy-deploy-key" \
  -f /etc/godeploy/keys/deploy-key -N ""

# Copy the public key to your deployment target host
ssh-copy-id -i /etc/godeploy/keys/deploy-key.pub deployuser@TARGET_HOST

# Fix permissions
sudo chmod 600 /etc/godeploy/keys/deploy-key
sudo chown godeploy:godeploy /etc/godeploy/keys/deploy-key
```

---

## 5. Create the Config File

```bash
sudo nano /etc/godeploy/config.json
```

Use the template below (also in `config.example.json`):

```json
{
  "server": {
    "listenAddr": ":8090",
    "dataDir": "/var/lib/godeploy"
  },
  "auth": {
    "sessionCookieName": "GODEPLOY_SESSION",
    "sessionTTLSeconds": 86400,
    "jenkinsWebhookToken": "REPLACE-WITH-LONG-RANDOM-SECRET",
    "bootstrapUsers": [
      {
        "username": "admin",
        "password": "REPLACE-WITH-STRONG-PASSWORD",
        "role": "admin"
      }
    ]
  },
  "notifications": {
    "slackWebhookURL": "",
    "googleChatWebhookURL": ""
  },
  "apps": [
    {
      "name": "my-service",
      "nexus": {
        "url": "http://YOUR-NEXUS:8081",
        "repo": "releases",
        "groupPath": "com/example/my-service",
        "artifact": "my-service.jar"
      },
      "cosign": { "publicKeyPath": "" },
      "target": {
        "host": "TARGET_HOST_IP",
        "port": 22,
        "user": "deployuser",
        "sshKeyPath": "/etc/godeploy/keys/deploy-key",
        "remotePath": "/opt/my-service",
        "backupDir": "/opt/my-service/backups",
        "serviceName": "my-service.service",
        "binaryName": "my-service.jar",
        "useSudo": true
      },
      "healthCheck": {
        "url": "http://TARGET_HOST_IP:8080/actuator/health",
        "timeoutSeconds": 10,
        "retries": 6,
        "intervalSeconds": 5
      },
      "syncPolicy": {
        "mode": "manual",
        "pollIntervalSeconds": 60,
        "selfHeal": false
      },
      "keepBackups": 5,
      "autoRollbackOnFailedHealthCheck": true
    }
  ]
}
```

```bash
# Lock down the config (contains passwords)
sudo chmod 640 /etc/godeploy/config.json
sudo chown root:godeploy /etc/godeploy/config.json
```

---

## 6. (Optional) Session Key Environment Variable

For a stable session key across restarts (otherwise a new key is generated each restart and all sessions are invalidated):

```bash
sudo nano /etc/godeploy/secrets.env
```

```
GODEPLOY_SESSION_KEY=generate-at-least-32-random-bytes-here
GODEPLOY_SESSION_SECURE=1
```

```bash
sudo chmod 640 /etc/godeploy/secrets.env
sudo chown root:godeploy /etc/godeploy/secrets.env
```

Then uncomment `EnvironmentFile` in the systemd unit (see step 7).

---

## 7. Install the systemd Service

```bash
# Copy the unit file
sudo cp godeploy.service /etc/systemd/system/godeploy.service

# Reload, enable, start
sudo systemctl daemon-reload
sudo systemctl enable godeploy
sudo systemctl start godeploy

# Check status
sudo systemctl status godeploy
```

---

## 8. Configure Firewall

```bash
# Open the godeploy dashboard port (8090)
sudo firewall-cmd --permanent --add-port=8090/tcp
sudo firewall-cmd --reload

# Verify
sudo firewall-cmd --list-ports
```

---

## 9. Reverse Proxy with Nginx (Recommended for Production)

```bash
sudo dnf install -y nginx
sudo nano /etc/nginx/conf.d/godeploy.conf
```

```nginx
server {
    listen 80;
    server_name godeploy.yourdomain.com;

    # Redirect to HTTPS
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    server_name godeploy.yourdomain.com;

    ssl_certificate     /etc/pki/tls/certs/godeploy.crt;
    ssl_certificate_key /etc/pki/tls/private/godeploy.key;

    # Required for SSE (disable buffering)
    proxy_buffering off;
    proxy_cache off;

    location / {
        proxy_pass http://127.0.0.1:8090;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # SSE / streaming: disable response buffering
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        chunked_transfer_encoding on;
    }
}
```

```bash
sudo systemctl enable --now nginx
# When using HTTPS, set GODEPLOY_SESSION_SECURE=1 in secrets.env
```

---

## 10. Jenkins CI Integration

In your Jenkinsfile, after publishing to Nexus:

```groovy
sh """
  curl -s -X POST https://godeploy.yourdomain.com/api/webhook/jenkins \\
    -H 'Authorization: Bearer ${GODEPLOY_WEBHOOK_TOKEN}' \\
    -H 'Content-Type: application/json' \\
    -d '{"app":"my-service","version":"${BUILD_VERSION}","triggeredBy":"jenkins"}'
"""
```

Store `GODEPLOY_WEBHOOK_TOKEN` in Jenkins credentials matching the `jenkinsWebhookToken` in your config.

---

## 11. Log Monitoring

```bash
# Follow live logs
sudo journalctl -u godeploy -f

# Last 100 lines
sudo journalctl -u godeploy -n 100

# Logs since yesterday
sudo journalctl -u godeploy --since yesterday
```

---

## 12. Adding Users at Runtime

After logging in to the dashboard as `admin`:
1. Click your username in the top bar
2. Use the **User Management** panel to create new users
3. Set role to `admin` (full access) or `viewer` (read-only, no sync/rollback/service management)

---

## 13. SELinux Considerations (RHEL)

If SELinux is enforcing, allow godeploy to bind the port and make network connections:

```bash
# Allow binding to port 8090
sudo semanage port -a -t http_port_t -p tcp 8090

# Allow network connections to Nexus / target SSH hosts
sudo setsebool -P httpd_can_network_connect 1
```

Or run `audit2allow` on `/var/log/audit/audit.log` AVC denials if you see permission issues.

---

## 14. Update the Binary

```bash
# Build new binary on dev machine (same cross-compile command as step 1)
# Then on RHEL:
sudo systemctl stop godeploy
sudo cp /tmp/godeploy /usr/local/bin/godeploy
sudo systemctl start godeploy
sudo systemctl status godeploy
```

State (deployment history, dynamic apps, users) is preserved in `/var/lib/godeploy/state.json`.
