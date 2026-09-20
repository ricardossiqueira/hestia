// Package uplink connects the durable outbox to the versioned gRPC contract.
// It intentionally has no WireGuard or TLS setup: those are transport
// concerns supplied by the caller when it creates the generated gRPC client.
package uplink

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	uplinkv1 "github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/uplink/v1"
	"github.com/ricardossiqueira/iot-gateway/internal/outbox"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	// ErrInvalidAcknowledgement means the VPS response cannot safely remove
	// any outbox message from this lease.
	ErrInvalidAcknowledgement = errors.New("invalid uplink acknowledgement")
)

type pushClient interface {
	PushBatch(context.Context, *uplinkv1.PushBatchRequest, ...grpc.CallOption) (*uplinkv1.PushBatchResponse, error)
}

// WorkerOptions defines the bounded work performed by one delivery attempt.
type WorkerOptions struct {
	GatewayID   string
	MaxMessages int
	MaxBytes    int64
	LeaseFor    time.Duration
	RetryDelay  time.Duration
	Now         func() time.Time
}

// Worker moves leased outbox records to UplinkService.PushBatch.
type Worker struct {
	store   *outbox.Store
	client  pushClient
	options WorkerOptions
}

// DeliveryResult describes one call to DeliverOnce without exposing payloads.
type DeliveryResult struct {
	BatchID      string
	Leased       int
	Acknowledged int
	Released     int
}

// NewWorker validates dependencies without opening a network connection.
func NewWorker(store *outbox.Store, client pushClient, options WorkerOptions) (*Worker, error) {
	if store == nil {
		return nil, errors.New("uplink outbox store is required")
	}
	if client == nil {
		return nil, errors.New("uplink gRPC client is required")
	}
	if strings.TrimSpace(options.GatewayID) == "" {
		return nil, errors.New("uplink gateway ID is required")
	}
	if options.MaxMessages <= 0 {
		return nil, errors.New("uplink max messages must be greater than zero")
	}
	if options.MaxBytes <= 0 {
		return nil, errors.New("uplink max bytes must be greater than zero")
	}
	if options.LeaseFor <= 0 {
		return nil, errors.New("uplink lease duration must be greater than zero")
	}
	if options.RetryDelay <= 0 {
		return nil, errors.New("uplink retry delay must be greater than zero")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Worker{store: store, client: client, options: options}, nil
}

// DeliverOnce sends at most one leased batch. A transport or protocol failure
// releases the lease for a later attempt; it never deletes unacknowledged data.
func (w *Worker) DeliverOnce(ctx context.Context) (DeliveryResult, error) {
	if w == nil {
		return DeliveryResult{}, errors.New("uplink worker is nil")
	}
	lease, err := w.store.Lease(ctx, outbox.LeaseRequest{
		MaxMessages: w.options.MaxMessages,
		MaxBytes:    w.options.MaxBytes,
		Duration:    w.options.LeaseFor,
	})
	if err != nil {
		return DeliveryResult{}, err
	}
	result := DeliveryResult{Leased: len(lease.Messages)}
	if len(lease.Messages) == 0 {
		return result, nil
	}

	request, err := w.pushBatchRequest(lease)
	if err != nil {
		return w.releaseAfterFailure(ctx, lease.Token, result, err)
	}
	result.BatchID = request.GetBatchId()
	response, err := w.client.PushBatch(ctx, request)
	if err != nil {
		return w.releaseAfterFailure(ctx, lease.Token, result, err)
	}
	acknowledged, err := validAcknowledgement(request, response)
	if err != nil {
		return w.releaseAfterFailure(ctx, lease.Token, result, err)
	}
	if len(acknowledged) > 0 {
		ackResult, err := w.store.Acknowledge(ctx, lease.Token, acknowledged)
		if err != nil {
			return w.releaseAfterFailure(ctx, lease.Token, result, err)
		}
		result.Acknowledged = ackResult.Deleted
	}
	if result.Acknowledged == result.Leased {
		return result, nil
	}
	released, err := w.store.Release(ctx, lease.Token, w.now().Add(w.options.RetryDelay), "partial_acknowledgement")
	if err != nil {
		return result, fmt.Errorf("release partially acknowledged uplink lease: %w", err)
	}
	result.Released = released
	return result, nil
}

func (w *Worker) pushBatchRequest(lease outbox.Lease) (*uplinkv1.PushBatchRequest, error) {
	request := &uplinkv1.PushBatchRequest{
		GatewayId: w.options.GatewayID,
		BatchId:   uuid.NewString(),
		SentAt:    timestamppb.New(w.now()),
		Messages:  make([]*uplinkv1.OutboundMessage, 0, len(lease.Messages)),
	}
	for _, message := range lease.Messages {
		kind, err := protobufKind(message.Kind)
		if err != nil {
			return nil, err
		}
		request.Messages = append(request.Messages, &uplinkv1.OutboundMessage{
			MessageId:   message.MessageID,
			Sequence:    uint64(message.Sequence),
			DeviceId:    message.DeviceID,
			Kind:        kind,
			Topic:       message.Topic,
			EnqueuedAt:  timestamppb.New(message.EnqueuedAt),
			PayloadJson: append([]byte(nil), message.Payload...),
		})
	}
	return request, nil
}

func (w *Worker) releaseAfterFailure(ctx context.Context, token string, result DeliveryResult, deliveryErr error) (DeliveryResult, error) {
	released, releaseErr := w.store.Release(ctx, token, w.now().Add(w.options.RetryDelay), deliveryErrorCode(deliveryErr))
	result.Released = released
	if releaseErr != nil {
		return result, fmt.Errorf("%w; release uplink lease: %v", deliveryErr, releaseErr)
	}
	return result, deliveryErr
}

func (w *Worker) now() time.Time { return w.options.Now().UTC() }

func protobufKind(kind outbox.Kind) (uplinkv1.MessageKind, error) {
	switch kind {
	case outbox.Telemetry:
		return uplinkv1.MessageKind_MESSAGE_KIND_TELEMETRY, nil
	case outbox.State:
		return uplinkv1.MessageKind_MESSAGE_KIND_STATE, nil
	case outbox.Event:
		return uplinkv1.MessageKind_MESSAGE_KIND_EVENT, nil
	default:
		return uplinkv1.MessageKind_MESSAGE_KIND_UNSPECIFIED, fmt.Errorf("unsupported uplink message kind %q", kind)
	}
}

func validAcknowledgement(request *uplinkv1.PushBatchRequest, response *uplinkv1.PushBatchResponse) ([]string, error) {
	if response == nil || response.GetBatchId() != request.GetBatchId() {
		return nil, fmt.Errorf("%w: batch ID does not match", ErrInvalidAcknowledgement)
	}
	leased := make(map[string]struct{}, len(request.GetMessages()))
	for _, message := range request.GetMessages() {
		leased[message.GetMessageId()] = struct{}{}
	}
	acknowledged := response.GetAcknowledgedMessageIds()
	seen := make(map[string]struct{}, len(acknowledged))
	for _, messageID := range acknowledged {
		if _, found := leased[messageID]; !found {
			return nil, fmt.Errorf("%w: message ID %q was not leased", ErrInvalidAcknowledgement, messageID)
		}
		if _, duplicate := seen[messageID]; duplicate {
			return nil, fmt.Errorf("%w: message ID %q is duplicated", ErrInvalidAcknowledgement, messageID)
		}
		seen[messageID] = struct{}{}
	}
	return acknowledged, nil
}

func deliveryErrorCode(err error) string {
	if errors.Is(err, ErrInvalidAcknowledgement) {
		return "invalid_acknowledgement"
	}
	return "grpc_delivery_failed"
}
