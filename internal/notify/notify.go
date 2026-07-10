package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"godeploy/internal/config"
)

type Notifier struct {
	cfg config.Notifications
}

func New(cfg config.Notifications) *Notifier {
	return &Notifier{cfg: cfg}
}

func (n *Notifier) DeploymentFinished(app, version, status, deploymentID string) {
	emoji := ""
	color := "#188038"
	switch status {
	case "FAILED":
		emoji, color = "", "#D93025"
	case "ROLLED_BACK":
		emoji, color = "", "#F9AB00"
	}
	text := fmt.Sprintf("%s *godeploy* — `%s` version `%s` → *%s*", emoji, app, version, status)

	if n.cfg.SlackWebhookURL != "" {
		payload := map[string]any{
			"attachments": []map[string]any{
				{
					"color": color,
					"blocks": []map[string]any{
						{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": text}},
						{"type": "context", "elements": []map[string]string{
							{"type": "mrkdwn", "text": fmt.Sprintf("deployment `%s` · %s", deploymentID, time.Now().Format(time.RFC1123))},
						}},
					},
				},
			},
		}
		n.post(n.cfg.SlackWebhookURL, payload)
	}

	if n.cfg.GoogleChatWebhookURL != "" {
		payload := map[string]any{
			"cardsV2": []map[string]any{
				{
					"cardId": "godeploy",
					"card": map[string]any{
						"header": map[string]string{"title": fmt.Sprintf("%s godeploy", emoji), "subtitle": fmt.Sprintf("%s → %s", app, status)},
						"sections": []map[string]any{
							{"widgets": []map[string]any{
								{"decoratedText": map[string]any{"topLabel": "Version", "text": version}},
								{"decoratedText": map[string]any{"topLabel": "Deployment", "text": deploymentID}},
							}},
						},
					},
				},
			},
		}
		n.post(n.cfg.GoogleChatWebhookURL, payload)
	}
}

func (n *Notifier) post(url string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
}
