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
		return true 
	},
}

type wsMessage struct {
	Type         string          `json:"type"`                   
	AppName      string          `json:"appName,omitempty"`      // for logs
	DeploymentID string          `json:"deploymentId,omitempty"` // for logs
	Payload      json.RawMessage `json:"payload,omitempty"`
}

type wsResponse struct {
	Type         string `json:"type"`                 
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

	
	send := func(msg wsResponse) error {
		mu.Lock()
		defer mu.Unlock()
		return conn.WriteJSON(msg)
	}


	evCh := a.st.SubscribeEvents()
	defer a.st.UnsubscribeEvents(evCh)


	go func() {
		for {
			_, msgBytes, err := conn.ReadMessage()
			if err != nil {
			
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
				close(logCancel)
				logCancel = make(chan struct{})
				subbedLogs = true
				logApp = req.AppName
				logDepID = req.DeploymentID
				localCancel := logCancel
				mu.Unlock()

				go func(appName, depID string, cancel chan struct{}) {
					ch, existing := a.st.Subscribe(depID)
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

					go func(cancel chan struct{}) {
						ticker := time.NewTicker(2 * time.Second)
						defer ticker.Stop()

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

		}
	}
}

func pushMetrics(a *API, send func(wsResponse) error) {

	sys := GetSystemMetrics()
	_ = send(wsResponse{
		Type: "system_metrics",
		Data: sys,
	})

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
