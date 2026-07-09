// Package store persists deployment history and current/desired state to a
// JSON file on disk (atomic write-rename), and provides an in-memory
// publish/subscribe bus so the UI can stream a deployment's logs live via
// Server-Sent Events instead of polling.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"godeploy/internal/config"
)

type Status string

const (
	StatusPending    Status = "PENDING"
	StatusDeploying  Status = "PROGRESSING"
	StatusSuccess    Status = "SUCCESS"
	StatusFailed     Status = "FAILED"
	StatusRolledBack Status = "ROLLED_BACK"
)

// SyncState mirrors ArgoCD's Synced / OutOfSync / Degraded vocabulary.
type SyncState string

const (
	SyncStateSynced      SyncState = "Synced"
	SyncStateOutOfSync   SyncState = "OutOfSync"
	SyncStateProgressing SyncState = "Progressing"
	SyncStateDegraded    SyncState = "Degraded"
	SyncStateUnknown     SyncState = "Unknown"
)

type Deployment struct {
	ID          string    `json:"id"`
	App         string    `json:"app"`
	Version     string    `json:"version"`
	ArtifactURL string    `json:"artifactUrl"`
	GitCommit   string    `json:"gitCommit,omitempty"`
	TriggeredBy string    `json:"triggeredBy"`
	Reason      string    `json:"reason,omitempty"`
	Status      Status    `json:"status"`
	BackupPath  string    `json:"backupPath,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt,omitempty"`
	Logs        []string  `json:"logs"`
}

type AppState struct {
	CurrentVersion string       `json:"currentVersion"`
	DesiredVersion string       `json:"desiredVersion"`
	SyncState      SyncState    `json:"syncState"`
	LastDeployment *Deployment  `json:"lastDeployment,omitempty"`
	History        []Deployment `json:"history"`
	LastError      string       `json:"lastError,omitempty"`
}

type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"passwordHash"`
	Salt         string `json:"salt"`
	Role         string `json:"role"`
}

type Event struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Timestamp    time.Time `json:"timestamp"`
	Level        string    `json:"level"` // info | warn | error
	App          string    `json:"app,omitempty"`
	DeploymentID string    `json:"deploymentId,omitempty"`
	Message      string    `json:"message"`
}

type JenkinsStage struct {
	Name     string `json:"name"`
	Status   string `json:"status"`   // SUCCESS | FAILED | RUNNING | PENDING
	Duration int    `json:"duration"` // seconds
}

type JenkinsBuild struct {
	BuildNumber string         `json:"buildNumber"`
	App         string         `json:"app"`
	GitCommit   string         `json:"gitCommit"`
	TriggeredBy string         `json:"triggeredBy"`
	Status      string         `json:"status"` // SUCCESS | FAILED | RUNNING
	StartedAt   time.Time      `json:"startedAt"`
	Stages      []JenkinsStage `json:"stages"`
}

type data struct {
	Apps          map[string]*AppState  `json:"apps"`
	DynamicApps   map[string]config.App `json:"dynamicApps,omitempty"`
	Users         map[string]User       `json:"users,omitempty"`
	Events        []Event               `json:"events,omitempty"`
	JenkinsBuilds []JenkinsBuild        `json:"jenkinsBuilds,omitempty"`
}

type Store struct {
	mu      sync.RWMutex
	path    string
	d       data
	subs    map[string][]chan string // deploymentID -> subscriber channels for live logs
	subsMu  sync.Mutex
	maxHist int

	// global event subscribers (for /api/events/stream SSE)
	evSubs   map[chan Event]struct{}
	evSubsMu sync.Mutex
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{
		path:    filepath.Join(dataDir, "state.json"),
		d:       data{Apps: map[string]*AppState{}, DynamicApps: map[string]config.App{}, Users: map[string]User{}, Events: []Event{}, JenkinsBuilds: []JenkinsBuild{}},
		subs:    map[string][]chan string{},
		maxHist: 50,
		evSubs:  map[chan Event]struct{}{},
	}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.d)
	}
	if s.d.Apps == nil {
		s.d.Apps = map[string]*AppState{}
	}
	if s.d.DynamicApps == nil {
		s.d.DynamicApps = map[string]config.App{}
	}
	if s.d.Users == nil {
		s.d.Users = map[string]User{}
	}
	if s.d.Events == nil {
		s.d.Events = []Event{}
	}
	if s.d.JenkinsBuilds == nil {
		s.d.JenkinsBuilds = []JenkinsBuild{}
	}
	return s, nil
}

func (s *Store) persistLocked() error {
	b, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) EnsureApp(name string) *AppState {
	s.mu.Lock()
	defer s.mu.Unlock()
	as, ok := s.d.Apps[name]
	if !ok {
		as = &AppState{SyncState: SyncStateUnknown}
		s.d.Apps[name] = as
	}
	return as
}

func (s *Store) GetAppState(name string) (AppState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	as, ok := s.d.Apps[name]
	if !ok {
		return AppState{}, false
	}
	return *as, true
}

func (s *Store) AllAppStates() map[string]AppState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]AppState{}
	for k, v := range s.d.Apps {
		out[k] = *v
	}
	return out
}

// StartDeployment records a new PENDING deployment and returns it.
func (s *Store) StartDeployment(app, version, artifactURL, commit, triggeredBy, reason string) Deployment {
	s.mu.Lock()
	defer s.mu.Unlock()
	as := s.d.Apps[app]
	if as == nil {
		as = &AppState{}
		s.d.Apps[app] = as
	}
	dep := Deployment{
		ID:          fmt.Sprintf("%s-%d", app, time.Now().UnixNano()),
		App:         app,
		Version:     version,
		ArtifactURL: artifactURL,
		GitCommit:   commit,
		TriggeredBy: triggeredBy,
		Reason:      reason,
		Status:      StatusPending,
		StartedAt:   time.Now(),
	}
	as.SyncState = SyncStateProgressing
	as.DesiredVersion = version
	as.LastDeployment = &dep
	_ = s.persistLocked()
	return dep
}

// AppendLog adds a line to a deployment's log and fans it out to SSE subscribers.
func (s *Store) AppendLog(depID, line string) {
	s.mu.Lock()
	for _, as := range s.d.Apps {
		if as.LastDeployment != nil && as.LastDeployment.ID == depID {
			as.LastDeployment.Logs = append(as.LastDeployment.Logs, line)
		}
	}
	_ = s.persistLocked()
	s.mu.Unlock()

	s.subsMu.Lock()
	for _, ch := range s.subs[depID] {
		select {
		case ch <- line:
		default:
		}
	}
	s.subsMu.Unlock()
}

// FinishDeployment marks the deployment terminal, updates sync state, and closes subscribers.
func (s *Store) FinishDeployment(depID, app string, status Status, backupPath string) {
	s.mu.Lock()
	as := s.d.Apps[app]
	if as != nil && as.LastDeployment != nil && as.LastDeployment.ID == depID {
		as.LastDeployment.Status = status
		as.LastDeployment.FinishedAt = time.Now()
		as.LastDeployment.BackupPath = backupPath

		switch status {
		case StatusSuccess:
			as.CurrentVersion = as.LastDeployment.Version
			as.SyncState = SyncStateSynced
			as.LastError = ""
		case StatusRolledBack:
			as.SyncState = SyncStateDegraded
			as.LastError = "deployment rolled back to previous version"
		case StatusFailed:
			as.SyncState = SyncStateDegraded
			as.LastError = fmt.Sprintf("deployment %s failed at %s", depID, time.Now().Format(time.RFC3339))
		}

		as.History = append(as.History, *as.LastDeployment)
		if len(as.History) > s.maxHist {
			as.History = as.History[len(as.History)-s.maxHist:]
		}
	}
	_ = s.persistLocked()
	s.mu.Unlock()

	s.subsMu.Lock()
	for _, ch := range s.subs[depID] {
		close(ch)
	}
	delete(s.subs, depID)
	s.subsMu.Unlock()
}

func (s *Store) SetSyncState(app string, st SyncState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	as := s.d.Apps[app]
	if as == nil {
		as = &AppState{}
		s.d.Apps[app] = as
	}
	as.SyncState = st
	_ = s.persistLocked()
}

func (s *Store) SetLastError(app, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	as := s.d.Apps[app]
	if as == nil {
		as = &AppState{}
		s.d.Apps[app] = as
	}
	as.LastError = errMsg
	_ = s.persistLocked()
}

// --- Dynamic app CRUD ---

func (s *Store) ListDynamicApps() []config.App {
	s.mu.RLock()
	defer s.mu.RUnlock()
	apps := make([]config.App, 0, len(s.d.DynamicApps))
	for _, app := range s.d.DynamicApps {
		apps = append(apps, app)
	}
	return apps
}

func (s *Store) GetDynamicApp(name string) (config.App, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	app, ok := s.d.DynamicApps[name]
	return app, ok
}

func (s *Store) SaveDynamicApp(app config.App) error {
	if app.Name == "" {
		return fmt.Errorf("name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.DynamicApps[app.Name] = app
	return s.persistLocked()
}

func (s *Store) DeleteDynamicApp(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.d.DynamicApps, name)
	return s.persistLocked()
}

// --- User CRUD ---

func (s *Store) ListUsers() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	users := make([]User, 0, len(s.d.Users))
	for _, user := range s.d.Users {
		users = append(users, user)
	}
	return users
}

func (s *Store) GetUser(username string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	user, ok := s.d.Users[username]
	return user, ok
}

func (s *Store) SaveUser(user User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Users[user.Username] = user
	return s.persistLocked()
}

func (s *Store) DeleteUser(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.d.Users[username]; !ok {
		return fmt.Errorf("user %q not found", username)
	}
	delete(s.d.Users, username)
	return s.persistLocked()
}

// --- Global event (notification) feed ---

// PushEvent persists an event and fans it out to all SSE event subscribers.
func (s *Store) PushEvent(event Event) error {
	s.mu.Lock()
	event.ID = fmt.Sprintf("evt-%d", time.Now().UnixNano())
	event.Timestamp = time.Now()
	s.d.Events = append(s.d.Events, event)
	if len(s.d.Events) > 200 {
		s.d.Events = s.d.Events[len(s.d.Events)-200:]
	}
	_ = s.persistLocked()
	s.mu.Unlock()

	// fan out to SSE subscribers
	s.evSubsMu.Lock()
	for ch := range s.evSubs {
		select {
		case ch <- event:
		default:
		}
	}
	s.evSubsMu.Unlock()
	return nil
}

func (s *Store) ListEvents() []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Event(nil), s.d.Events...)
}

// SubscribeEvents returns a channel that receives live events. Call UnsubscribeEvents to clean up.
func (s *Store) SubscribeEvents() chan Event {
	ch := make(chan Event, 64)
	s.evSubsMu.Lock()
	s.evSubs[ch] = struct{}{}
	s.evSubsMu.Unlock()
	return ch
}

// UnsubscribeEvents removes a channel from the event fan-out.
func (s *Store) UnsubscribeEvents(ch chan Event) {
	s.evSubsMu.Lock()
	delete(s.evSubs, ch)
	s.evSubsMu.Unlock()
}

// --- Deployment log streaming ---

// Subscribe returns a channel of log lines for a (possibly still running)
// deployment. Replays what's already been logged, then streams new lines
// until the deployment finishes or the caller stops reading.
func (s *Store) Subscribe(depID string) (<-chan string, []string) {
	s.mu.RLock()
	var existing []string
	for _, as := range s.d.Apps {
		if as.LastDeployment != nil && as.LastDeployment.ID == depID {
			existing = append(existing, as.LastDeployment.Logs...)
		}
		// Also check history in case the deployment already finished
		for _, dep := range as.History {
			if dep.ID == depID {
				if len(existing) == 0 {
					existing = append([]string(nil), dep.Logs...)
				}
			}
		}
	}
	s.mu.RUnlock()

	ch := make(chan string, 256)
	s.subsMu.Lock()
	s.subs[depID] = append(s.subs[depID], ch)
	s.subsMu.Unlock()
	return ch, existing
}

// --- Jenkins Builds ---

func (s *Store) SaveJenkinsBuild(b JenkinsBuild) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Find and update if exists, otherwise append
	found := false
	for i, jb := range s.d.JenkinsBuilds {
		if jb.BuildNumber == b.BuildNumber && jb.App == b.App {
			s.d.JenkinsBuilds[i] = b
			found = true
			break
		}
	}
	if !found {
		s.d.JenkinsBuilds = append(s.d.JenkinsBuilds, b)
	}
	if len(s.d.JenkinsBuilds) > 100 {
		s.d.JenkinsBuilds = s.d.JenkinsBuilds[len(s.d.JenkinsBuilds)-100:]
	}
	_ = s.persistLocked()
}

func (s *Store) ListJenkinsBuilds() []JenkinsBuild {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]JenkinsBuild(nil), s.d.JenkinsBuilds...)
}
