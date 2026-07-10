
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)


type Nexus struct {
	URL      string `json:"url"`
	Repo     string `json:"repo"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"` 
	GroupPath string `json:"groupPath"`
	Artifact  string `json:"artifact"`
}


type Cosign struct {
	PublicKeyPath string `json:"publicKeyPath"`
}

type Target struct {
	Host           string `json:"host"`
	Port           int    `json:"port"`
	User           string `json:"user"`
	SSHKeyPath     string `json:"sshKeyPath,omitempty"`    
	PasswordEnvVar string `json:"passwordEnvVar,omitempty"` 
	RemotePath     string `json:"remotePath"`
	BackupDir      string `json:"backupDir"`
	ServiceName    string `json:"serviceName"`
	BinaryName     string `json:"binaryName"`
	UseSudo        bool   `json:"useSudo"`
}


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


type SyncPolicy struct {
	Mode                string `json:"mode"`
	PollIntervalSeconds int    `json:"pollIntervalSeconds"`
	SelfHeal            bool   `json:"selfHeal"` 
}

func (s SyncPolicy) Interval() time.Duration {
	if s.PollIntervalSeconds <= 0 {
		return 60 * time.Second
	}
	return time.Duration(s.PollIntervalSeconds) * time.Second
}


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
