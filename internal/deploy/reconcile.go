package deploy

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"godeploy/internal/config"
	"godeploy/internal/registry"
	"godeploy/internal/store"
)


type Reconciler struct {
	reg    *registry.Registry
	engine *Engine
	st     *store.Store
	stopCh chan struct{}
}

func NewReconciler(reg *registry.Registry, engine *Engine, st *store.Store) *Reconciler {
	return &Reconciler{reg: reg, engine: engine, st: st, stopCh: make(chan struct{})}
}

func (r *Reconciler) Start() {
	apps, _ := r.reg.AllApps()
	for _, app := range apps {
		if app.SyncPolicy.Mode != "auto" {
			continue
		}
		go r.loop(app)
	}
}

func (r *Reconciler) Stop() { close(r.stopCh) }

func (r *Reconciler) loop(app config.App) {
	ticker := time.NewTicker(app.SyncPolicy.Interval())
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.reconcileOnce(app)
		}
	}
}

func (r *Reconciler) reconcileOnce(app config.App) {
	latest, err := fetchLatestVersion(app)
	if err != nil {
		return
	}
	as, _ := r.st.GetAppState(app.Name)
	if as.CurrentVersion == latest {
		r.st.SetSyncState(app.Name, store.SyncStateSynced)
		return
	}
	r.st.SetSyncState(app.Name, store.SyncStateOutOfSync)
	if !app.SyncPolicy.SelfHeal {
		return 
	}
	_, _ = r.engine.Trigger(Request{
		AppName:     app.Name,
		Version:     latest,
		TriggeredBy: "auto-sync",
		Reason:      fmt.Sprintf("self-heal: drift detected (running %s, latest is %s)", as.CurrentVersion, latest),
	})
}

func fetchLatestVersion(app config.App) (string, error) {
	url := fmt.Sprintf("%s/repository/%s/%s/latest.txt", app.Nexus.URL, app.Nexus.Repo, app.Nexus.GroupPath)
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if app.Nexus.Username != "" {
		req.SetBasicAuth(app.Nexus.Username, app.Nexus.Password)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
