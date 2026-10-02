package apigateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// InternalTelemetryReader implements V2TelemetryReader by forwarding to
// internal/api's loopback-only telemetry endpoint - the only process holding
// the live MQTT connection (ADR-009). It mirrors InternalCommandPublisher
// for the read direction - see that type's doc comment.
type InternalTelemetryReader struct {
	// BaseURL is the same internal API URL this package's reverse proxy and
	// InternalCommandPublisher already target, e.g. "http://127.0.0.1:8083".
	BaseURL string
	Client  *http.Client
}

// LastTelemetry returns ok=false (with no error) when the device has not
// published telemetry since the sandboxed process started - that is a
// normal, expected state, not a failure to report to the caller.
func (r InternalTelemetryReader) LastTelemetry(ctx context.Context, deviceID string) (payload json.RawMessage, timestamp, messageID string, ok bool, err error) {
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	target := strings.TrimRight(r.BaseURL, "/") + "/internal/v2/device-telemetry?device_id=" + url.QueryEscape(deviceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("build internal telemetry request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("internal API unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, "", "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, "", "", false, fmt.Errorf("get device telemetry: internal API returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	var body struct {
		MessageID string          `json:"messageId"`
		Timestamp string          `json:"timestamp"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return nil, "", "", false, fmt.Errorf("decode device telemetry response: %w", err)
	}
	return body.Payload, body.Timestamp, body.MessageID, true, nil
}
