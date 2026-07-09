package api

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // embedded app runs on same host/port, but allow dev environments
	},
}

type wsMessage struct {
	Type         string          `json:"type"`                   // subscribe_logs, unsubscribe_logs, subscribe_metrics, unsubscribe_metrics
	AppName      string          `json:"appName,omitempty"`      // for logs
	DeploymentID string          `json:"deploymentId,omitempty"` // for logs
	Payload      json.RawMessage `json:"payload,omitempty"`
}

type wsResponse struct {
	Type         string `json:"type"`                   // log_line, log_done, system_metrics, service_metrics, event, jenkins_update
	AppName      string `json:"appName,omitempty"`      // for logs
	DeploymentID string `json:"deploymentId,omitempty"` // for logs
	Data         any    `json:"data,omitempty"`
}

func (a *API) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws: upgrade error: %v", err)
		return
	}
	defer conn.Close()

	// Client tracking state
	var (
		mu              sync.Mutex
		subbedLogs      bool
		logApp          string
		logDepID        string
		logCancel       chan struct{}
		subbedMetrics   bool
		metricsCancel   chan struct{}
	)

	logCancel = make(chan struct{})
	metricsCancel = make(chan struct{})

	// helper to send messages safely across goroutines
	send := func(msg wsResponse) error {
		mu.Lock()
		defer mu.Unlock()
		return conn.WriteJSON(msg)
	}

	// 1. Subscribe to global system events to fan them out to the client in real-time
	evCh := a.st.SubscribeEvents()
	defer a.st.UnsubscribeEvents(evCh)

	// Spin up goroutine to read client commands
	go func() {
		for {
			_, msgBytes, err := conn.ReadMessage()
			if err != nil {
				// Client disconnected
				mu.Lock()
				close(logCancel)
				close(metricsCancel)
				mu.Unlock()
				return
			}

			var req wsMessage
			if err := json.Unmarshal(msgBytes, &req); err != nil {
				continue
			}

			switch req.Type {
			case "subscribe_logs":
				mu.Lock()
				// Cancel previous log subscription if any
				close(logCancel)
				logCancel = make(chan struct{})
				subbedLogs = true
				logApp = req.AppName
				logDepID = req.DeploymentID
				localCancel := logCancel
				mu.Unlock()

				// Start streaming logs in a new goroutine
				go func(appName, depID string, cancel chan struct{}) {
					ch, existing := a.st.Subscribe(depID)
					// First replay existing logs
					for _, line := range existing {
						select {
						case <-cancel:
							return
						default:
							_ = send(wsResponse{
								Type:         "log_line",
								AppName:      appName,
								DeploymentID: depID,
								Data:         line,
							})
						}
					}

					// Then stream incoming lines
					for {
						select {
						case <-cancel:
							return
						case line, ok := <-ch:
							if !ok {
								_ = send(wsResponse{
									Type:         "log_done",
									AppName:      appName,
									DeploymentID: depID,
								})
								return
							}
							_ = send(wsResponse{
								Type:         "log_line",
								AppName:      appName,
								DeploymentID: depID,
								Data:         line,
							})
						}
					}
				}(logApp, logDepID, localCancel)

			case "unsubscribe_logs":
				mu.Lock()
				if subbedLogs {
					close(logCancel)
					logCancel = make(chan struct{})
					subbedLogs = false
				}
				mu.Unlock()

			case "subscribe_metrics":
				mu.Lock()
				if !subbedMetrics {
					close(metricsCancel)
					metricsCancel = make(chan struct{})
					subbedMetrics = true
					localCancel := metricsCancel
					mu.Unlock()

					// Start telemetry streams
					go func(cancel chan struct{}) {
						ticker := time.NewTicker(2 * time.Second)
						defer ticker.Stop()

						// Immediate push
						pushMetrics(a, send)

						for {
							select {
							case <-cancel:
								return
							case <-ticker.C:
								pushMetrics(a, send)
							}
						}
					}(localCancel)
				} else {
					mu.Unlock()
				}

			case "unsubscribe_metrics":
				mu.Lock()
				if subbedMetrics {
					close(metricsCancel)
					metricsCancel = make(chan struct{})
					subbedMetrics = false
				}
				mu.Unlock()
			}
		}
	}()

	// Stream global events (notifications) to the socket
	for {
		select {
		case ev, ok := <-evCh:
			if !ok {
				return
			}
			if err := send(wsResponse{Type: "event", Data: ev}); err != nil {
				return
			}
		case <-logCancel:
			// check loop to prevent blocking if event loop is idle
		}
	}
}

func pushMetrics(a *API, send func(wsResponse) error) {
	// Gather system metrics
	sys := GetSystemMetrics()
	_ = send(wsResponse{
		Type: "system_metrics",
		Data: sys,
	})

	// Gather service metrics for each configured app
	apps, _ := a.reg.AllApps()
	srvMap := make(map[string]ServiceMetrics)
	for _, app := range apps {
		srvMap[app.Name] = GetServiceMetrics(app.Name)
	}
	_ = send(wsResponse{
		Type: "service_metrics",
		Data: srvMap,
	})
}
