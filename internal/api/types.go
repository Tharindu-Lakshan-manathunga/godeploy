package api

import (
	"time"

	"godeploy/internal/config"
	"godeploy/internal/store"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Message  string `json:"message"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Token    string `json:"token,omitempty"`
}

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

type createAppRequest config.App

type eventResponse struct {
	ID           string    `json:"id"`
	Timestamp    time.Time `json:"timestamp"`
	Level        string    `json:"level"`
	App          string    `json:"app,omitempty"`
	DeploymentID string    `json:"deploymentId,omitempty"`
	Message      string    `json:"message"`
}

type appSummaryFull struct {
	Name       string         `json:"name"`
	SyncPolicy string         `json:"syncPolicy"`
	Target     string         `json:"target"`
	Service    string         `json:"service"`
	State      store.AppState `json:"state"`
	IsDynamic  bool           `json:"isDynamic"`
}
