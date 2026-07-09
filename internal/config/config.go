// Package config defines the declarative, GitOps-style configuration for
// godeploy: the set of Applications it manages, where their artifacts come
// from, where they get deployed, and how health is verified.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Nexus describes where release artifacts are published to / read from.
type Nexus struct {
	URL      string `json:"url"`
	Repo     string `json:"repo"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"` // prefer env var below in production
	// PathTemplate is a printf-style template with a single %s placeholder
	// for the version, e.g. "myapp/%s/myapp-%s.war" style paths are built by
	// the caller — kept simple and explicit rather than magic.
	GroupPath string `json:"groupPath"`
	Artifact  string `json:"artifact"`
}

// Cosign holds the public key used to verify artifact signatures produced
// by `cosign sign-blob` in the build pipeline. Verification is skipped
// (with a loud warning) if PublicKeyPath is empty — never silently.
type Cosign struct {
	PublicKeyPath string `json:"publicKeyPath"`
}

// Target is the remote host an application is deployed onto.
type Target struct {
	Host           string `json:"host"`
	Port           int    `json:"port"`
	User           string `json:"user"`
	SSHKeyPath     string `json:"sshKeyPath,omitempty"`     // preferred
	PasswordEnvVar string `json:"passwordEnvVar,omitempty"` // fallback, e.g. "DMS_SSH_PASSWORD"
	RemotePath     string `json:"remotePath"`
	BackupDir      string `json:"backupDir"`
	ServiceName    string `json:"serviceName"`
	BinaryName     string `json:"binaryName"`
	UseSudo        bool   `json:"useSudo"`
}

// HealthCheck is polled after a restart to decide SUCCESS vs auto-rollback.
type HealthCheck struct {
	URL            string `json:"url"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
	Retries        int    `json:"retries"`
	IntervalSeconds int   `json:"intervalSeconds"`
}

func (h HealthCheck) Timeout() time.Duration {
	if h.TimeoutSeconds <= 0 {
		return 10 * time.Second
	}
	return time.Duration(h.TimeoutSeconds) * time.Second
}

func (h HealthCheck) Interval() time.Duration {
	if h.IntervalSeconds <= 0 {
		return 5 * time.Second
	}
	return time.Duration(h.IntervalSeconds) * time.Second
}

// SyncPolicy controls whether godeploy behaves purely on-demand ("manual",
// the default — a webhook or UI click triggers a deploy) or continuously
// reconciles against the latest published artifact ("auto", ArgoCD-style).
type SyncPolicy struct {
	Mode                string `json:"mode"` // "manual" | "auto"
	PollIntervalSeconds int    `json:"pollIntervalSeconds"`
	SelfHeal            bool   `json:"selfHeal"` // if true, auto re-deploy when drift is detected
}

func (s SyncPolicy) Interval() time.Duration {
	if s.PollIntervalSeconds <= 0 {
		return 60 * time.Second
	}
	return time.Duration(s.PollIntervalSeconds) * time.Second
}

// App is one deployable service under godeploy's control — conceptually
// equivalent to an ArgoCD Application, but the "cluster" is a systemd host
// and the "manifest" is a signed, versioned build artifact.
type App struct {
	Name                          string      `json:"name"`
	Nexus                         Nexus       `json:"nexus"`
	Cosign                        Cosign      `json:"cosign"`
	Target                        Target      `json:"target"`
	HealthCheck                   HealthCheck `json:"healthCheck"`
	SyncPolicy                    SyncPolicy  `json:"syncPolicy"`
	KeepBackups                   int         `json:"keepBackups"`
	AutoRollbackOnFailedHealth    bool        `json:"autoRollbackOnFailedHealthCheck"`
}

type BootstrapUser struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role,omitempty"`
}

type Auth struct {
	APIToken           string          `json:"apiToken"`
	APITokenEnvVar     string          `json:"apiTokenEnvVar,omitempty"`
	SessionCookieName  string          `json:"sessionCookieName,omitempty"`
	SessionTTLSeconds  int             `json:"sessionTTLSeconds,omitempty"`
	JenkinsWebhookToken string         `json:"jenkinsWebhookToken,omitempty"`
	BootstrapUsers     []BootstrapUser `json:"bootstrapUsers,omitempty"`
}

type Notifications struct {
	SlackWebhookURL      string `json:"slackWebhookURL"`
	GoogleChatWebhookURL string `json:"googleChatWebhookURL"`
}

type Server struct {
	ListenAddr string `json:"listenAddr"`
	DataDir    string `json:"dataDir"`
}

type Config struct {
	Server        Server        `json:"server"`
	Auth          Auth          `json:"auth"`
	Notifications Notifications `json:"notifications"`
	Apps          []App         `json:"apps"`
}

// ResolvedToken returns the effective API token, preferring the env var
// (so tokens don't have to live in a checked-in config file) over the
// inline value.
func (a Auth) ResolvedToken() string {
	if a.APITokenEnvVar != "" {
		if v := os.Getenv(a.APITokenEnvVar); v != "" {
			return v
		}
	}
	return a.APIToken
}

func (a Auth) SessionCookieNameOrDefault() string {
	if a.SessionCookieName != "" {
		return a.SessionCookieName
	}
	return "GODEPLOY_SESSION"
}

func (a Auth) SessionTTL() time.Duration {
	if a.SessionTTLSeconds <= 0 {
		return 24 * time.Hour
	}
	return time.Duration(a.SessionTTLSeconds) * time.Second
}

func (a Auth) HasBootstrapUsers() bool {
	return len(a.BootstrapUsers) > 0
}

func (a Auth) ResolvedWebhookToken() string {
	return a.JenkinsWebhookToken
}

func (c *Config) Validate() error {
	seen := map[string]struct{}{}
	for i := range c.Apps {
		if err := c.Apps[i].Validate(); err != nil {
			return fmt.Errorf("app %q: %w", c.Apps[i].Name, err)
		}
		if c.Apps[i].Name == "" {
			return fmt.Errorf("app at index %d has no name", i)
		}
		if _, ok := seen[c.Apps[i].Name]; ok {
			return fmt.Errorf("duplicate app name %q", c.Apps[i].Name)
		}
		seen[c.Apps[i].Name] = struct{}{}
	}
	return nil
}

func (a App) Validate() error {
	if a.Name == "" {
		return fmt.Errorf("name is required")
	}
	if a.Nexus.URL == "" || a.Nexus.Repo == "" || a.Nexus.GroupPath == "" || a.Nexus.Artifact == "" {
		return fmt.Errorf("nexus url/repo/groupPath/artifact are all required")
	}
	if a.Target.Host == "" || a.Target.User == "" || a.Target.RemotePath == "" || a.Target.BackupDir == "" || a.Target.ServiceName == "" || a.Target.BinaryName == "" {
		return fmt.Errorf("target host/user/remotePath/backupDir/serviceName/binaryName are required")
	}
	if a.Target.SSHKeyPath == "" && a.Target.PasswordEnvVar == "" {
		return fmt.Errorf("either target.sshKeyPath or target.passwordEnvVar is required")
	}
	if a.SyncPolicy.Mode == "" {
		a.SyncPolicy.Mode = "manual"
	}
	if a.KeepBackups <= 0 {
		a.KeepBackups = 5
	}
	return nil
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if c.Server.ListenAddr == "" {
		c.Server.ListenAddr = ":8090"
	}
	if c.Server.DataDir == "" {
		c.Server.DataDir = "./data"
	}
	for i := range c.Apps {
		if c.Apps[i].KeepBackups <= 0 {
			c.Apps[i].KeepBackups = 5
		}
		if c.Apps[i].SyncPolicy.Mode == "" {
			c.Apps[i].SyncPolicy.Mode = "manual"
		}
	}
	return &c, nil
}

func (c *Config) FindApp(name string) (*App, bool) {
	for i := range c.Apps {
		if c.Apps[i].Name == name {
			return &c.Apps[i], true
		}
	}
	return nil, false
}
