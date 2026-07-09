# godeploy

A small, self-contained **GitOps-style continuous deployment controller**
for services that live outside Kubernetes — the "ArgoCD pattern" (declared
desired state, continuous reconciliation, Synced/OutOfSync/Degraded status,
one-click rollback) applied to a signed build artifact and a `systemd` unit
on a plain host, instead of a manifest and a cluster.

It replaces this block at the end of a CI pipeline:

```groovy
sshpass -p $SSH_PASS scp ... target/app.war $SSH_USER@$HOST:$REMOTE_PATH/
sshpass -p $SSH_PASS ssh ... "sudo systemctl restart myapp.service"
```

with a real control loop that:

- downloads the artifact itself (from Nexus) instead of trusting whatever
  the CI runner happens to have on disk
- **verifies the cosign signature** before it will touch a production host
- takes a timestamped backup before overwriting anything, and prunes old
  backups automatically
- restarts the service and **gates success on a real health check**, with
  configurable retries
- **automatically rolls back** to the previous artifact if the health check
  fails, instead of leaving a broken service running
- keeps a persistent, queryable **deployment history** per app (who, when,
  which version, why, full log)
- can run in **manual** mode (deploy on request/webhook) or **auto** mode,
  continuously polling for the latest published version and self-healing
  drift — the actual ArgoCD reconciliation loop, just pointed at Nexus
  instead of a git repo of manifests
- serializes concurrent deploys of the same app so two triggers can't race
  each other onto the same host
- ships a single dark, information-dense dashboard: live SSE log streaming
  per deployment, a literal pipeline-stage rail (fetch → verify → backup →
  ship → restart → health → synced) so you can see *where* a rollout is
  stuck, sync/rollback buttons, and full history — no separate log
  aggregator needed for this purpose

It is one Go binary (with the whole UI embedded via `go:embed`) plus one
JSON config file. No database, no message broker, no k8s.

## Architecture

```
internal/config   declarative app/target/notification config (JSON)
internal/store    JSON-file persisted state + history + SSE pub/sub
internal/deploy    the actual reconcile step: fetch → verify → backup →
                   ship → restart → health-check → (rollback | success)
internal/notify    Slack / Google Chat webhook notifications
internal/api       REST + SSE HTTP surface, Jenkins webhook receiver
web/               embedded dashboard (vanilla HTML/CSS/JS, no build step)
```

## Running it

```bash
go build -o godeploy .
GODEPLOY_API_TOKEN=$(openssl rand -hex 24) ./godeploy -config config.json
```

Open `http://<host>:8090`, paste the token into the top bar, and you'll see
every configured app as a card with its current sync state.

## Configuring an app

See `config.example.json` for a complete example modeled directly on the
`dms-integration-service` deploy target in the existing Jenkinsfile. Key
fields:

| Field | Purpose |
|---|---|
| `nexus.url` / `repo` / `groupPath` / `artifact` | Where to download the versioned artifact from |
| `cosign.publicKeyPath` | If set, the artifact's `.sig`/`.pem` are fetched and verified with `cosign verify-blob` before deploy proceeds |
| `target.sshKeyPath` **or** `target.passwordEnvVar` | SSH auth — a key is strongly preferred over `sshpass`; the password path exists for parity with the current pipeline and reads the secret from an env var, never the config file |
| `healthCheck.url` | Polled after restart; `autoRollbackOnFailedHealthCheck` controls whether a failed check triggers automatic rollback |
| `syncPolicy.mode` | `"manual"` (default) — only deploys when told to. `"auto"` — polls `<nexus>/<repo>/<groupPath>/latest.txt` on an interval and, if `selfHeal: true`, redeploys automatically on drift |
| `keepBackups` | How many backup artifacts to retain on the target host |

## Wiring it into the existing Jenkinsfile

Delete the `Deploy to Remote Server` stage's `sshpass scp/ssh` block and
replace it with a single webhook call after publishing to Nexus:

```groovy
stage('Trigger Deployment') {
    when { expression { return env.DEPLOY_AUTHORIZED == 'true' } }
    steps {
        withCredentials([string(credentialsId: 'godeploy-webhook-token', variable: 'GODEPLOY_TOKEN')]) {
            sh """
                curl -sf -X POST http://192.168.1.117:8090/api/webhook/jenkins \\
                  -H "Authorization: Bearer \$GODEPLOY_TOKEN" \\
                  -H "Content-Type: application/json" \\
                  -d '{
                        "app": "dms-integration-service",
                        "version": "${BUILD_NUMBER}",
                        "gitCommit": "${GIT_SHORT_HASH}",
                        "triggeredBy": "jenkins:${env.BUILD_USER ?: 'automated'}"
                      }'
            """
        }
    }
}
```

CI's job becomes: build, scan, sign, publish to Nexus, then *tell* godeploy
a new version exists. godeploy owns verification, rollout, health-gating
and rollback — which also means a bad deploy can be rolled back from the
dashboard without touching Jenkins at all, and the pipeline no longer needs
raw SSH/`sshpass` credentials scoped to production hosts.

## API summary

| Endpoint | Purpose |
|---|---|
| `GET /api/apps` | List all apps with current sync/version state |
| `GET /api/apps/{name}` | Full state for one app |
| `GET /api/apps/{name}/deployments` | Deployment history |
| `POST /api/apps/{name}/sync {version}` | Deploy a specific version now |
| `POST /api/apps/{name}/rollback {toDeploymentId}` | Redeploy a prior, already-verified version |
| `GET /api/apps/{name}/stream/{deploymentId}` | Live log stream (SSE) |
| `POST /api/webhook/jenkins` | CI hands off a freshly published version |

All except the webhook use the `auth.apiToken`/`apiTokenEnvVar` bearer
token; the webhook uses its own `auth.jenkinsWebhookToken` so CI doesn't
need the operator's interactive credential.

## Notes on security posture

- Prefer `target.sshKeyPath` (a dedicated, narrowly-scoped deploy key) over
  password auth in every environment that matters.
- `auth.apiTokenEnvVar` / passwords are read from environment variables at
  startup, not committed to the JSON config, so the config file itself is
  safe to keep in version control.
- A deploy is refused outright if a `cosign.publicKeyPath` is configured
  but the signature can't be fetched or doesn't verify — there's no
  "continue anyway" path for that check, unlike the pipeline's original
  findings-tolerant policy for other scanners.
