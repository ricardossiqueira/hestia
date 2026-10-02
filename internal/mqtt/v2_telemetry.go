package mqtt

// telemetryMu guards lastTelemetry, separate from both configMu (control-plane
// policy) and eventsMu (the activity ring buffer) - same reasoning as that
// separation in gateway.go: a data-plane cache with its own narrow lock.
//
// lastTelemetry holds only the most recent "telemetry" message per device,
// keyed by device ID. No history, no persistence - a restart forgets it,
// same tradeoff already made for recentEvents.

// recordTelemetry stores message as the latest "telemetry" snapshot for its
// device. Call sites must only pass messages whose Kind is "telemetry".
func (g *Gateway) recordTelemetry(message Message) {
	g.telemetryMu.Lock()
	defer g.telemetryMu.Unlock()
	if g.lastTelemetry == nil {
		g.lastTelemetry = map[string]Message{}
	}
	g.lastTelemetry[message.DeviceID] = message
}

// LastTelemetry returns the most recent accepted "telemetry" message for
// deviceID, if any has been received since this process started.
func (g *Gateway) LastTelemetry(deviceID string) (Message, bool) {
	g.telemetryMu.RLock()
	defer g.telemetryMu.RUnlock()
	message, ok := g.lastTelemetry[deviceID]
	return message, ok
}
