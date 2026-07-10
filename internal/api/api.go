
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"godeploy/internal/auth"
	"godeploy/internal/config"
	"godeploy/internal/deploy"
	"godeploy/internal/registry"
	"godeploy/internal/store"
)

type API struct {
	cfg     *config.Config
	reg     *registry.Registry
	st      *store.Store
	engine  *deploy.Engine
	authMgr *auth.Manager
}

func New(cfg *config.Config, reg *registry.Registry, st *store.Store, engine *deploy.Engine, authMgr *auth.Manager) *API {
	return &API{cfg: cfg, reg: reg, st: st, engine: engine, authMgr: authMgr}
}

func (a *API) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", a.handleHealthz)
	mux.HandleFunc("/api/login", a.handleLogin)
	mux.HandleFunc("/api/logout", a.handleLogout)
	mux.HandleFunc("/api/me", a.withAuth(a.handleMe))
	mux.HandleFunc("/api/apps", a.withAuth(a.handleApps))
	mux.HandleFunc("/api/apps/", a.withAuth(a.handleAppSubroutes))
	mux.HandleFunc("/api/events", a.withAuth(a.handleListEvents))
	mux.HandleFunc("/api/events/stream", a.withAuth(a.handleEventsStream))
	mux.HandleFunc("/api/users", a.withAuth(a.handleUsers))
	mux.HandleFunc("/api/users/", a.withAuth(a.handleUserSubroutes))
	mux.HandleFunc("/api/webhook/jenkins", a.handleJenkinsWebhook)
	mux.HandleFunc("/api/webhook/jenkins/stages", a.handleJenkinsStagesWebhook)
	mux.HandleFunc("/api/jenkins/builds", a.withAuth(a.handleJenkinsBuilds))
	mux.HandleFunc("/api/metrics", a.withAuth(a.handleMetrics))
	mux.HandleFunc("/api/ws", a.withAuth(a.handleWS))
}

// --- auth ----------------------------------------------------------------

func (a *API) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if user, ok := a.authMgr.UserFromRequest(r); ok {
			next(w, a.authMgr.WithUserContext(r, user))
			return
		}

		if a.cfg.Auth.ResolvedToken() != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" {
				got = r.URL.Query().Get("token")
			}
			if got == a.cfg.Auth.ResolvedToken() {
				next(w, r)
				return
			}
		}
		writeErr(w, http.StatusUnauthorized, "authentication required")
	}
}

func (a *API) requireAdmin(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	user, ok := a.authMgr.UserFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusForbidden, "admin role required")
		return store.User{}, false
	}
	if !a.authMgr.RequireAdmin(user) {
		writeErr(w, http.StatusForbidden, "admin role required")
		return store.User{}, false
	}
	return user, true
}

// --- handlers --------------------------------------------------------------

func (a *API) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := a.authMgr.UserFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	token := ""
	if cookie, err := r.Cookie(a.authMgr.CookieName()); err == nil {
		token = cookie.Value
	} else if authHeader := r.Header.Get("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
		token = strings.TrimPrefix(authHeader, "Bearer ")
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"username": user.Username,
		"role":     user.Role,
		"token":    token,
	})
}

func (a *API) handleApps(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.handleListApps(w, r)
	case http.MethodPost:
		a.handleCreateApp(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Username == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, "username and password are required")
		return
	}
	token, err := a.authMgr.Authenticate(req.Username, req.Password)
	if err != nil {
		_ = a.st.PushEvent(store.Event{Level: "warn", Message: fmt.Sprintf("failed login attempt for user %q", req.Username)})
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	a.authMgr.SetSessionCookie(w, token)

	user, _ := a.st.GetUser(req.Username)
	_ = a.st.PushEvent(store.Event{Level: "info", Message: fmt.Sprintf("user %q logged in", req.Username)})
	writeJSON(w, http.StatusOK, loginResponse{
		Message:  "ok",
		Username: req.Username,
		Role:     user.Role,
		Token:    token,
	})
}

func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if user, ok := a.authMgr.UserFromRequest(r); ok {
		_ = a.st.PushEvent(store.Event{Level: "info", Message: fmt.Sprintf("user %q logged out", user.Username)})
	}
	a.authMgr.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"message": "logged out"})
}

func (a *API) handleListEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	writeJSON(w, http.StatusOK, a.st.ListEvents())
}

func (a *API) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	recent := a.st.ListEvents()
	for _, ev := range recent {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	flusher.Flush()

	ch := a.st.SubscribeEvents()
	defer a.st.UnsubscribeEvents(ch)

	ctx := r.Context()
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (a *API) handleListApps(w http.ResponseWriter, r *http.Request) {
	states := a.st.AllAppStates()
	apps, _ := a.reg.AllApps()
	dynSet := map[string]bool{}
	for _, da := range a.st.ListDynamicApps() {
		dynSet[da.Name] = true
	}
	out := make([]appSummaryFull, 0, len(apps))
	for _, app := range apps {
		st := states[app.Name]
		if st.SyncState == "" {
			st.SyncState = store.SyncStateUnknown
		}
		out = append(out, appSummaryFull{
			Name:       app.Name,
			SyncPolicy: app.SyncPolicy.Mode,
			Target:     fmt.Sprintf("%s:%d", app.Target.Host, app.Target.Port),
			Service:    app.Target.ServiceName,
			State:      st,
			IsDynamic:  dynSet[app.Name],
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	var app config.App
	if err := json.NewDecoder(r.Body).Decode(&app); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := a.reg.ValidateApp(app); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.st.SaveDynamicApp(app); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = a.st.PushEvent(store.Event{Level: "info", App: app.Name, Message: fmt.Sprintf("app %q created by %s", app.Name, user.Username)})
	writeJSON(w, http.StatusCreated, app)
}

func (a *API) handleUpdateApp(w http.ResponseWriter, r *http.Request, name string) {
	user, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	var app config.App
	if err := json.NewDecoder(r.Body).Decode(&app); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if app.Name != name {
		writeErr(w, http.StatusBadRequest, "app name in path and body must match")
		return
	}
	if _, ok := a.reg.FindApp(name); !ok {
		writeErr(w, http.StatusNotFound, "unknown app")
		return
	}
	if err := app.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.st.SaveDynamicApp(app); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = a.st.PushEvent(store.Event{Level: "info", App: app.Name, Message: fmt.Sprintf("app %q updated by %s", app.Name, user.Username)})
	writeJSON(w, http.StatusOK, app)
}

func (a *API) handleDeleteApp(w http.ResponseWriter, r *http.Request, name string) {
	user, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}

	if _, ok := a.st.GetDynamicApp(name); !ok {
		writeErr(w, http.StatusForbidden, "only dynamically-added apps can be deleted; edit the config file to remove static apps")
		return
	}
	if err := a.st.DeleteDynamicApp(name); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = a.st.PushEvent(store.Event{Level: "warn", App: name, Message: fmt.Sprintf("app %q deleted by %s", name, user.Username)})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) handleAppSubroutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/apps/")
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch {
	case action == "" && r.Method == http.MethodPut:
		a.handleUpdateApp(w, r, name)
		return
	case action == "" && r.Method == http.MethodDelete:
		a.handleDeleteApp(w, r, name)
		return
	}

	app, ok := a.reg.FindApp(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown app")
		return
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		st, _ := a.st.GetAppState(name)
		writeJSON(w, http.StatusOK, st)

	case action == "config" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, app)

	case action == "deployments" && r.Method == http.MethodGet:
		st, _ := a.st.GetAppState(name)
		writeJSON(w, http.StatusOK, st.History)

	case action == "sync" && r.Method == http.MethodPost:
		a.handleSync(w, r, app)

	case action == "rollback" && r.Method == http.MethodPost:
		a.handleRollback(w, r, app)

	case strings.HasPrefix(action, "stream/") && r.Method == http.MethodGet:
		depID := strings.TrimPrefix(action, "stream/")
		a.handleStream(w, r, depID)

	default:
		writeErr(w, http.StatusNotFound, "no such route")
	}
}

// --- User management -------------------------------------------------------

func (a *API) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := a.requireAdmin(w, r); !ok {
			return
		}
		users := a.st.ListUsers()
	
		type safeUser struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		}
		safe := make([]safeUser, 0, len(users))
		for _, u := range users {
			safe = append(safe, safeUser{Username: u.Username, Role: u.Role})
		}
		writeJSON(w, http.StatusOK, safe)
	case http.MethodPost:
		admin, ok := a.requireAdmin(w, r)
		if !ok {
			return
		}
		var req createUserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if err := a.authMgr.CreateUser(req.Username, req.Password, req.Role); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = a.st.PushEvent(store.Event{Level: "info", Message: fmt.Sprintf("user %q created by %s", req.Username, admin.Username)})
		writeJSON(w, http.StatusCreated, map[string]string{"username": req.Username, "role": req.Role})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *API) handleUserSubroutes(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimPrefix(r.URL.Path, "/api/users/")
	if username == "" {
		writeErr(w, http.StatusBadRequest, "username required")
		return
	}
	admin, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if username == admin.Username {
			writeErr(w, http.StatusBadRequest, "cannot delete your own account")
			return
		}
		if err := a.st.DeleteUser(username); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		_ = a.st.PushEvent(store.Event{Level: "warn", Message: fmt.Sprintf("user %q deleted by %s", username, admin.Username)})
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// --- Sync / Rollback -------------------------------------------------------

type syncRequest struct {
	Version     string `json:"version"`
	TriggeredBy string `json:"triggeredBy"`
}

func (a *API) handleSync(w http.ResponseWriter, r *http.Request, app config.App) {
	var req syncRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Version == "" {
		writeErr(w, http.StatusBadRequest, "version is required")
		return
	}
	triggeredBy := req.TriggeredBy
	if triggeredBy == "" {
		if user, ok := a.authMgr.UserFromContext(r.Context()); ok {
			triggeredBy = user.Username
		} else {
			triggeredBy = "ui"
		}
	}
	depID, err := a.engine.Trigger(deploy.Request{
		AppName:     app.Name,
		Version:     req.Version,
		TriggeredBy: triggeredBy,
		Reason:      "manual sync",
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = a.st.PushEvent(store.Event{Level: "info", App: app.Name, DeploymentID: depID,
		Message: fmt.Sprintf("sync triggered for %q version %s by %s", app.Name, req.Version, triggeredBy)})
	writeJSON(w, http.StatusAccepted, map[string]string{"deploymentId": depID})
}

type rollbackRequest struct {
	ToDeploymentID string `json:"toDeploymentId"`
	TriggeredBy    string `json:"triggeredBy"`
}

func (a *API) handleRollback(w http.ResponseWriter, r *http.Request, app config.App) {
	var req rollbackRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	st, ok := a.st.GetAppState(app.Name)
	if !ok {
		writeErr(w, http.StatusNotFound, "no history for app")
		return
	}
	var target *store.Deployment
	for i := range st.History {
		if st.History[i].ID == req.ToDeploymentID {
			target = &st.History[i]
			break
		}
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, "deployment id not found in history")
		return
	}
	triggeredBy := req.TriggeredBy
	if triggeredBy == "" {
		if user, ok := a.authMgr.UserFromContext(r.Context()); ok {
			triggeredBy = user.Username
		} else {
			triggeredBy = "ui"
		}
	}
	depID, err := a.engine.Trigger(deploy.Request{
		AppName:     app.Name,
		Version:     target.Version,
		ArtifactURL: target.ArtifactURL,
		TriggeredBy: triggeredBy,
		Reason:      fmt.Sprintf("rollback to %s (deployment %s)", target.Version, target.ID),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = a.st.PushEvent(store.Event{Level: "warn", App: app.Name, DeploymentID: depID,
		Message: fmt.Sprintf("rollback triggered for %q to version %s by %s", app.Name, target.Version, triggeredBy)})
	writeJSON(w, http.StatusAccepted, map[string]string{"deploymentId": depID})
}

// --- Log streaming ---------------------------------------------------------

func (a *API) handleStream(w http.ResponseWriter, r *http.Request, depID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, existing := a.st.Subscribe(depID)
	for _, line := range existing {
		fmt.Fprintf(w, "data: %s\n\n", jsonEscapeSSE(line))
	}
	flusher.Flush()

	ctx := r.Context()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-ch:
			if !ok {
				fmt.Fprintf(w, "event: done\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", jsonEscapeSSE(line))
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// --- Jenkins webhook -------------------------------------------------------

type jenkinsWebhookPayload struct {
	App         string `json:"app"`
	Version     string `json:"version"`
	ArtifactURL string `json:"artifactUrl,omitempty"`
	GitCommit   string `json:"gitCommit,omitempty"`
	TriggeredBy string `json:"triggeredBy,omitempty"`
}

func (a *API) handleJenkinsWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if a.cfg.Auth.JenkinsWebhookToken != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != a.cfg.Auth.JenkinsWebhookToken {
			writeErr(w, http.StatusUnauthorized, "invalid webhook token")
			return
		}
	}
	var p jenkinsWebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if p.App == "" || p.Version == "" {
		writeErr(w, http.StatusBadRequest, "app and version are required")
		return
	}
	if _, ok := a.reg.FindApp(p.App); !ok {
		writeErr(w, http.StatusNotFound, "unknown app: "+p.App)
		return
	}
	if p.TriggeredBy == "" {
		p.TriggeredBy = "jenkins"
	}
	depID, err := a.engine.Trigger(deploy.Request{
		AppName:     p.App,
		Version:     p.Version,
		ArtifactURL: p.ArtifactURL,
		GitCommit:   p.GitCommit,
		TriggeredBy: p.TriggeredBy,
		Reason:      "CI publish webhook",
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = a.st.PushEvent(store.Event{Level: "info", App: p.App, DeploymentID: depID,
		Message: fmt.Sprintf("Jenkins webhook triggered deploy of %q version %s", p.App, p.Version)})
	writeJSON(w, http.StatusAccepted, map[string]string{"deploymentId": depID})
}

// --- small helpers ---------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func jsonEscapeSSE(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}



func (a *API) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	sys := GetSystemMetrics()
	apps, _ := a.reg.AllApps()
	srvMap := make(map[string]ServiceMetrics)
	for _, app := range apps {
		srvMap[app.Name] = GetServiceMetrics(app.Name)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"system":   sys,
		"services": srvMap,
	})
}

type jenkinsStagesPayload struct {
	BuildNumber string               `json:"buildNumber"`
	App         string               `json:"app"`
	GitCommit   string               `json:"gitCommit"`
	TriggeredBy string               `json:"triggeredBy"`
	Status      string               `json:"status"`
	Stages      []store.JenkinsStage `json:"stages"`
}

func (a *API) handleJenkinsStagesWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if a.cfg.Auth.JenkinsWebhookToken != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != a.cfg.Auth.JenkinsWebhookToken {
			writeErr(w, http.StatusUnauthorized, "invalid webhook token")
			return
		}
	}
	var p jenkinsStagesPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if p.App == "" || p.BuildNumber == "" {
		writeErr(w, http.StatusBadRequest, "app and buildNumber are required")
		return
	}

	build := store.JenkinsBuild{
		BuildNumber: p.BuildNumber,
		App:         p.App,
		GitCommit:   p.GitCommit,
		TriggeredBy: p.TriggeredBy,
		Status:      p.Status,
		StartedAt:   time.Now(),
		Stages:      p.Stages,
	}
	a.st.SaveJenkinsBuild(build)


	_ = a.st.PushEvent(store.Event{
		Level:   "info",
		App:     p.App,
		Type:    "jenkins_update",
		Message: fmt.Sprintf("Jenkins Build #%s status update: %s", p.BuildNumber, p.Status),
	})

	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

func (a *API) handleJenkinsBuilds(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	writeJSON(w, http.StatusOK, a.st.ListJenkinsBuilds())
}
