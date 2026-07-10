package deploy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"godeploy/internal/config"
	"godeploy/internal/notify"
	"godeploy/internal/registry"
	"godeploy/internal/store"
)

type Engine struct {
	reg      *registry.Registry
	st       *store.Store
	notifier *notify.Notifier

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex 
}

func New(reg *registry.Registry, st *store.Store, n *notify.Notifier) *Engine {
	return &Engine{reg: reg, st: st, notifier: n, locks: map[string]*sync.Mutex{}}
}

func (e *Engine) lockFor(app string) *sync.Mutex {
	e.locksMu.Lock()
	defer e.locksMu.Unlock()
	l, ok := e.locks[app]
	if !ok {
		l = &sync.Mutex{}
		e.locks[app] = l
	}
	return l
}

type Request struct {
	AppName     string
	Version     string 
	ArtifactURL string
	GitCommit   string
	TriggeredBy string
	Reason      string
}

func (e *Engine) Trigger(req Request) (string, error) {
	app, ok := e.reg.FindApp(req.AppName)
	if !ok {
		return "", fmt.Errorf("unknown app %q", req.AppName)
	}
	artifactURL := req.ArtifactURL
	if artifactURL == "" {
		artifactURL = fmt.Sprintf("%s/repository/%s/%s/%s/%s",
			app.Nexus.URL, app.Nexus.Repo, app.Nexus.GroupPath, req.Version, app.Nexus.Artifact)
	}

	dep := e.st.StartDeployment(app.Name, req.Version, artifactURL, req.GitCommit, req.TriggeredBy, req.Reason)

	go e.run(app, dep.ID, req.Version, artifactURL, req.GitCommit, req.TriggeredBy)

	return dep.ID, nil
}

func (e *Engine) log(depID, format string, a ...any) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
	e.st.AppendLog(depID, line)
}

func (e *Engine) run(app config.App, depID, version, artifactURL, commit, triggeredBy string) {
	lock := e.lockFor(app.Name)
	lock.Lock()
	defer lock.Unlock()

	finalStatus := store.StatusFailed
	backupPath := ""

	_ = e.st.PushEvent(store.Event{Level: "info", App: app.Name, DeploymentID: depID,
		Message: fmt.Sprintf("deployment started for %q version %s by %s", app.Name, version, triggeredBy)})

	defer func() {
		if r := recover(); r != nil {
			e.log(depID, "🛑 panic during deploy: %v", r)
			finalStatus = store.StatusFailed
		}
		e.st.FinishDeployment(depID, app.Name, finalStatus, backupPath)
		e.notifier.DeploymentFinished(app.Name, version, string(finalStatus), depID)

		// Push a global notification for the outcome
		level := "info"
		msg := fmt.Sprintf(" deployment of %q version %s succeeded", app.Name, version)
		switch finalStatus {
		case store.StatusFailed:
			level = "error"
			msg = fmt.Sprintf(" deployment of %q version %s FAILED", app.Name, version)
		case store.StatusRolledBack:
			level = "warn"
			msg = fmt.Sprintf(" deployment of %q rolled back to previous version", app.Name)
		}
		_ = e.st.PushEvent(store.Event{Level: level, App: app.Name, DeploymentID: depID, Message: msg})
	}()


	e.log(depID, "▶ starting deploy of %s version %s", app.Name, version)

	tmpDir, err := os.MkdirTemp("", "godeploy-"+app.Name+"-")
	if err != nil {
		e.log(depID, " could not create temp dir: %v", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	localArtifact := filepath.Join(tmpDir, app.Target.BinaryName)
	e.log(depID, "⬇ downloading artifact from %s", artifactURL)
	if err := downloadFile(artifactURL, localArtifact, app.Nexus.Username, app.Nexus.Password); err != nil {
		e.log(depID, " artifact download failed: %v", err)
		return
	}
	e.log(depID, " artifact downloaded (%s)", humanSize(localArtifact))

	if app.Cosign.PublicKeyPath != "" {
		sigURL := artifactURL + ".sig"
		certURL := artifactURL + ".pem"
		sigPath := localArtifact + ".sig"
		certPath := localArtifact + ".pem"
		e.log(depID, " fetching signature for verification")
		sigErr := downloadFile(sigURL, sigPath, app.Nexus.Username, app.Nexus.Password)
		certErr := downloadFile(certURL, certPath, app.Nexus.Username, app.Nexus.Password)
		if sigErr != nil || certErr != nil {
			e.log(depID, " could not fetch signature/certificate — refusing to deploy an unverifiable artifact")
			return
		}
		out, err := runCmd(30*time.Second, "cosign", "verify-blob",
			"--key", app.Cosign.PublicKeyPath,
			"--signature", sigPath,
			localArtifact,
		)
		if err != nil {
			e.log(depID, " cosign verification FAILED: %v\n%s", err, out)
			return
		}
		e.log(depID, "signature verified against %s", app.Cosign.PublicKeyPath)
	} else {
		e.log(depID, "no cosign public key configured for %s — skipping signature verification", app.Name)
	}

	sshTarget := fmt.Sprintf("%s@%s", app.Target.User, app.Target.Host)
	sshBase, scpBase, cleanup, err := sshArgs(app.Target)
	if err != nil {
		e.log(depID, "ssh auth setup failed: %v", err)
		return
	}
	defer cleanup()

	ts := time.Now().Format("02-01-2006-150405")
	backupPath = fmt.Sprintf("%s/%s-%s%s", app.Target.BackupDir, app.Target.BinaryName, ts, "")
	remoteBin := filepath.Join(app.Target.RemotePath, app.Target.BinaryName)
	backupScript := fmt.Sprintf(`set -e
mkdir -p %q
if [ -f %q ]; then
  %s mv %q %q
  echo "backup created: %s"
else
  echo "no existing artifact to back up"
fi`, app.Target.BackupDir, remoteBin, sudoPrefix(app.Target), remoteBin, backupPath, backupPath)

	e.log(depID, " backing up current artifact on %s", app.Target.Host)
	out, err := sshRun(sshBase, sshTarget, backupScript)
	if err != nil {
		e.log(depID, " backup step failed: %v\n%s", err, out)
		return
	}
	e.log(depID, "%s", trimmed(out))

	e.log(depID, " uploading new artifact to %s:%s", app.Target.Host, app.Target.RemotePath)
	if err := scpRun(scpBase, localArtifact, sshTarget, app.Target.RemotePath+"/"); err != nil {
		e.log(depID, " upload failed: %v", err)
		e.attemptRestore(depID, app, sshBase, backupPath, remoteBin)
		return
	}
	e.log(depID, " upload complete")

	restartScript := fmt.Sprintf("set -e\n%s systemctl restart %s\nsleep 3\n%s systemctl is-active %s",
		sudoPrefix(app.Target), app.Target.ServiceName, sudoPrefix(app.Target), app.Target.ServiceName)
	e.log(depID, " restarting %s", app.Target.ServiceName)
	out, err = sshRun(sshBase, sshTarget, restartScript)
	if err != nil {
		e.log(depID, " service restart failed: %v\n%s", err, out)
		e.attemptRestore(depID, app, sshBase, backupPath, remoteBin)
		return
	}
	e.log(depID, "%s", trimmed(out))

	if app.HealthCheck.URL != "" {
		e.log(depID, "🩺 running health check against %s", app.HealthCheck.URL)
		ok := waitHealthy(app.HealthCheck, func(msg string) { e.log(depID, "%s", msg) })
		if !ok {
			e.log(depID, " health check did not pass after %d attempt(s)", maxi(app.HealthCheck.Retries, 1))
			if app.AutoRollbackOnFailedHealth && backupPath != "" {
				e.log(depID, " auto-rollback enabled — restoring previous artifact")
				if e.restore(sshBase, scpBase, backupPath, remoteBin, app) == nil {
					_, _ = sshRun(sshBase, sshTarget, fmt.Sprintf("%s systemctl restart %s", sudoPrefix(app.Target), app.Target.ServiceName))
					e.log(depID, " rolled back to previous artifact and restarted service")
					finalStatus = store.StatusRolledBack
					return
				}
				e.log(depID, " rollback restore also failed — manual intervention required")
			}
			finalStatus = store.StatusFailed
			return
		}
		e.log(depID, " health check passed")
	} else {
		e.log(depID, " no health check configured — trusting systemctl is-active result only")
	}

	e.log(depID, " deployment SUCCESS — %s now running version %s", app.Name, version)
	finalStatus = store.StatusSuccess

	e.pruneBackups(sshBase, sshTarget, app)
}

func (e *Engine) attemptRestore(depID string, app config.App, sshBase []string, backupPath, remoteBin string) {
	if backupPath == "" {
		return
	}
	e.log(depID, "attempting to restore previous artifact after failure")
	script := fmt.Sprintf("set -e\nif [ -f %q ]; then %s mv %q %q; echo restored; else echo 'no backup to restore'; fi",
		backupPath, sudoPrefix(app.Target), backupPath, remoteBin)
	sshTarget := fmt.Sprintf("%s@%s", app.Target.User, app.Target.Host)
	out, err := sshRun(sshBase, sshTarget, script)
	if err != nil {
		e.log(depID, " restore failed too: %v\n%s", err, out)
		return
	}
	e.log(depID, "%s", trimmed(out))
}

func (e *Engine) restore(sshBase, _ []string, backupPath, remoteBin string, app config.App) error {
	sshTarget := fmt.Sprintf("%s@%s", app.Target.User, app.Target.Host)
	script := fmt.Sprintf("set -e\n%s mv %q %q", sudoPrefix(app.Target), backupPath, remoteBin)
	_, err := sshRun(sshBase, sshTarget, script)
	return err
}

func (e *Engine) pruneBackups(sshBase []string, sshTarget string, app config.App) {
	keep := app.KeepBackups
	if keep <= 0 {
		keep = 5
	}
	script := fmt.Sprintf(
		`cd %q 2>/dev/null && ls -1t %s-* 2>/dev/null | tail -n +%d | xargs -r rm -f --`,
		app.Target.BackupDir, app.Target.BinaryName, keep+1,
	)
	_, _ = sshRun(sshBase, sshTarget, script)
}


func sudoPrefix(t config.Target) string {
	if t.UseSudo {
		return "sudo"
	}
	return ""
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func trimmed(s string) string {
	const max = 4000
	if len(s) > max {
		return s[:max] + "... (truncated)"
	}
	return s
}

func humanSize(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return "unknown size"
	}
	sz := float64(fi.Size())
	units := []string{"B", "KB", "MB", "GB"}
	i := 0
	for sz >= 1024 && i < len(units)-1 {
		sz /= 1024
		i++
	}
	return fmt.Sprintf("%.1f%s", sz, units[i])
}

func downloadFile(url, dest, user, pass string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d fetching %s", resp.StatusCode, url)
	}
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func runCmd(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func sshArgs(t config.Target) (sshBase, scpBase []string, cleanup func(), err error) {
	cleanup = func() {}
	port := t.Port
	if port == 0 {
		port = 22
	}

	if t.SSHKeyPath != "" {
		sshBase = []string{"ssh", "-i", t.SSHKeyPath, "-p", fmt.Sprintf("%d", port), "-o", "StrictHostKeyChecking=no", "-o", "BatchMode=yes"}
		scpBase = []string{"scp", "-i", t.SSHKeyPath, "-P", fmt.Sprintf("%d", port), "-o", "StrictHostKeyChecking=no", "-o", "BatchMode=yes"}
		return sshBase, scpBase, cleanup, nil
	}

	if t.PasswordEnvVar == "" {
		return nil, nil, cleanup, fmt.Errorf("target %s has neither sshKeyPath nor passwordEnvVar configured", t.Host)
	}
	pass := os.Getenv(t.PasswordEnvVar)
	if pass == "" {
		return nil, nil, cleanup, fmt.Errorf("env var %s is empty — cannot authenticate to %s", t.PasswordEnvVar, t.Host)
	}
	sshBase = []string{"sshpass", "-e", "ssh", "-p", fmt.Sprintf("%d", port), "-o", "StrictHostKeyChecking=no"}
	scpBase = []string{"sshpass", "-e", "scp", "-P", fmt.Sprintf("%d", port), "-o", "StrictHostKeyChecking=no"}

	if err := os.Setenv("SSHPASS", pass); err != nil {
		return nil, nil, cleanup, err
	}
	cleanup = func() { os.Unsetenv("SSHPASS") }
	return sshBase, scpBase, cleanup, nil
}

func sshRun(sshBase []string, target, script string) (string, error) {
	args := append(append([]string{}, sshBase[1:]...), target, script)
	return runCmd(2*time.Minute, sshBase[0], args...)
}

func scpRun(scpBase []string, localPath, target, remoteDir string) error {
	args := append(append([]string{}, scpBase[1:]...), localPath, target+":"+remoteDir)
	out, err := runCmd(5*time.Minute, scpBase[0], args...)
	if err != nil {
		return fmt.Errorf("%w: %s", err, trimmed(out))
	}
	return nil
}

func waitHealthy(hc config.HealthCheck, logf func(string)) bool {
	retries := hc.Retries
	if retries <= 0 {
		retries = 5
	}
	client := &http.Client{
		Timeout: hc.Timeout(),
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, 
	}
	for i := 1; i <= retries; i++ {
		resp, err := client.Get(hc.URL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				logf(fmt.Sprintf("  attempt %d/%d: HTTP %d — healthy", i, retries, resp.StatusCode))
				return true
			}
			logf(fmt.Sprintf("  attempt %d/%d: HTTP %d", i, retries, resp.StatusCode))
		} else {
			logf(fmt.Sprintf("  attempt %d/%d: %v", i, retries, err))
		}
		if i < retries {
			time.Sleep(hc.Interval())
		}
	}
	return false
}
