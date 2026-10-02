package apigateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// InternalCommandPublisher implements V2CommandPublisher by forwarding an
// already-built, already-validated command envelope to internal/api's
// loopback-only publish endpoint - the only process holding the live MQTT
// connection (ADR-009). It mirrors, for v2, why DeviceService.PublishCommand
// is reverse-proxied there instead of answered in this (root) process
// directly - see this package's doc comment.
type InternalCommandPublisher struct {
	// BaseURL is the same internal API URL this package's reverse proxy
	// already targets, e.g. "http://127.0.0.1:8083".
	BaseURL string
	Client  *http.Client
}

func (p InternalCommandPublisher) Publish(ctx context.Context, topic string, payload []byte) error {
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	body, err := json.Marshal(struct {
		Topic   string          `json:"topic"`
		Payload json.RawMessage `json:"payload"`
	}{Topic: topic, Payload: payload})
	if err != nil {
		return fmt.Errorf("encode internal publish request: %w", err)
	}
	url := strings.TrimRight(p.BaseURL, "/") + "/internal/v2/publish-command"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build internal publish request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("internal API unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("publish command: internal API returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	return nil
}
