package mqtt

import (
	"context"
	"log/slog"

	"github.com/ricardossiqueira/iot-gateway/internal/outbox"
)

// SlogLogger writes accepted and rejected message metadata as structured logs.
// Payloads and credentials are intentionally excluded.
type SlogLogger struct {
	logger *slog.Logger
}

func NewSlogLogger(logger *slog.Logger) *SlogLogger {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlogLogger{logger: logger}
}

func (l *SlogLogger) Accepted(ctx context.Context, message Message) {
	l.logger.InfoContext(ctx, "MQTT message accepted",
		"device_id", message.DeviceID,
		"kind", message.Kind,
		"topic", message.Topic,
		"message_id", message.MessageID,
		"timestamp", message.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
	)
}

func (l *SlogLogger) Rejected(ctx context.Context, message RejectedMessage) {
	l.logger.WarnContext(ctx, "MQTT message rejected",
		"device_id", message.DeviceID,
		"kind", message.Kind,
		"topic", message.Topic,
		"reason", message.Reason,
	)
}

func (l *SlogLogger) OutboxStored(ctx context.Context, message Message, result outbox.EnqueueResult) {
	l.logger.InfoContext(ctx, "outbox message stored",
		"device_id", message.DeviceID,
		"kind", message.Kind,
		"topic", message.Topic,
		"message_id", message.MessageID,
		"duplicate", result.Duplicate,
		"evicted", result.Evicted,
		"expired", result.Expired,
	)
}

func (l *SlogLogger) OutboxDiscarded(ctx context.Context, message Message, result outbox.EnqueueResult) {
	l.logger.WarnContext(ctx, "outbox message discarded",
		"device_id", message.DeviceID,
		"kind", message.Kind,
		"topic", message.Topic,
		"message_id", message.MessageID,
		"reason", result.DiscardReason,
		"expired", result.Expired,
	)
}

func (l *SlogLogger) OutboxFailed(ctx context.Context, message Message, err error) {
	l.logger.ErrorContext(ctx, "outbox enqueue failed",
		"device_id", message.DeviceID,
		"kind", message.Kind,
		"topic", message.Topic,
		"message_id", message.MessageID,
		"error", err,
	)
}
