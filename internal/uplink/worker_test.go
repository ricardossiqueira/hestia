package uplink

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	uplinkv1 "github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/uplink/v1"
	"github.com/ricardossiqueira/iot-gateway/internal/outbox"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestWorkerDeliversLeasedMessagesAndAcknowledgesOnlyServerIDs(t *testing.T) {
	now := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	store := openStore(t, &now)
	for _, id := range []string{"first", "second"} {
		if _, err := store.Enqueue(context.Background(), outbox.Message{
			MessageID: id,
			DeviceID:  "sensor-1",
			Topic:     "devices/sensor-1/telemetry",
			Kind:      outbox.Telemetry,
			Payload:   []byte(`{"temperature_c":24.6}`),
		}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Nanosecond)
	}
	client := &fakePushClient{reply: func(request *uplinkv1.PushBatchRequest) (*uplinkv1.PushBatchResponse, error) {
		if request.GetGatewayId() != "orangepi-lab-01" || len(request.GetMessages()) != 2 {
			t.Fatalf("PushBatch request = %#v", request)
		}
		if request.GetMessages()[0].GetMessageId() != "first" || request.GetMessages()[1].GetMessageId() != "second" {
			t.Fatalf("message order = %#v", request.GetMessages())
		}
		if got := string(request.GetMessages()[0].GetPayloadJson()); got != `{"temperature_c":24.6}` {
			t.Fatalf("payload = %q", got)
		}
		return &uplinkv1.PushBatchResponse{
			BatchId:                request.GetBatchId(),
			AcknowledgedMessageIds: []string{"first", "second"},
		}, nil
	}}
	worker, err := NewWorker(store, client, WorkerOptions{
		GatewayID:   "orangepi-lab-01",
		MaxMessages: 10,
		MaxBytes:    1024,
		LeaseFor:    time.Minute,
		RetryDelay:  time.Second,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.DeliverOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Leased != 2 || result.Acknowledged != 2 || result.Released != 0 || client.request == nil {
		t.Fatalf("DeliverOnce() = %#v", result)
	}
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 0 {
		t.Fatalf("messages after acknowledgement = %d", stats.Messages)
	}
}

func TestWorkerReleasesLeaseAfterRetryableGRPCFailure(t *testing.T) {
	now := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	store := openStore(t, &now)
	if _, err := store.Enqueue(context.Background(), testMessage("message")); err != nil {
		t.Fatal(err)
	}
	worker, err := NewWorker(store, &fakePushClient{err: status.Error(codes.Unavailable, "offline")}, WorkerOptions{
		GatewayID:   "orangepi-lab-01",
		MaxMessages: 1,
		MaxBytes:    1024,
		LeaseFor:    time.Minute,
		RetryDelay:  time.Minute,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.DeliverOnce(context.Background())
	if status.Code(err) != codes.Unavailable || result.Leased != 1 || result.Released != 1 {
		t.Fatalf("DeliverOnce() = %#v, %v", result, err)
	}
	now = now.Add(30 * time.Second)
	beforeRetry, err := store.Lease(context.Background(), outbox.LeaseRequest{MaxMessages: 1, MaxBytes: 1024, Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeRetry.Messages) != 0 {
		t.Fatalf("lease before retry = %#v", beforeRetry.Messages)
	}
	now = now.Add(30 * time.Second)
	afterRetry, err := store.Lease(context.Background(), outbox.LeaseRequest{MaxMessages: 1, MaxBytes: 1024, Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRetry.Messages) != 1 || afterRetry.Messages[0].Attempts != 2 {
		t.Fatalf("lease after retry = %#v", afterRetry.Messages)
	}
}

func TestWorkerRejectsUnknownAcknowledgementWithoutDeletingMessages(t *testing.T) {
	now := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	store := openStore(t, &now)
	if _, err := store.Enqueue(context.Background(), testMessage("message")); err != nil {
		t.Fatal(err)
	}
	worker, err := NewWorker(store, &fakePushClient{reply: func(request *uplinkv1.PushBatchRequest) (*uplinkv1.PushBatchResponse, error) {
		return &uplinkv1.PushBatchResponse{BatchId: request.GetBatchId(), AcknowledgedMessageIds: []string{"unknown"}}, nil
	}}, WorkerOptions{
		GatewayID: "orangepi-lab-01", MaxMessages: 1, MaxBytes: 1024, LeaseFor: time.Minute, RetryDelay: time.Minute, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.DeliverOnce(context.Background())
	if !errors.Is(err, ErrInvalidAcknowledgement) || result.Acknowledged != 0 || result.Released != 1 {
		t.Fatalf("DeliverOnce() = %#v, %v", result, err)
	}
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Messages != 1 {
		t.Fatalf("messages after invalid acknowledgement = %d", stats.Messages)
	}
}

func TestWorkerUsesGeneratedGRPCClientAgainstInMemoryServer(t *testing.T) {
	now := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	store := openStore(t, &now)
	if _, err := store.Enqueue(context.Background(), testMessage("message")); err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	handler := &pushServer{}
	uplinkv1.RegisterUplinkServiceServer(server, handler)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	connection, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	worker, err := NewWorker(store, uplinkv1.NewUplinkServiceClient(connection), WorkerOptions{
		GatewayID: "orangepi-lab-01", MaxMessages: 1, MaxBytes: 1024, LeaseFor: time.Minute, RetryDelay: time.Second, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := worker.DeliverOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Acknowledged != 1 || handler.request == nil || handler.request.GetMessages()[0].GetMessageId() != "message" {
		t.Fatalf("DeliverOnce() = %#v, server request = %#v", result, handler.request)
	}
}

type fakePushClient struct {
	request *uplinkv1.PushBatchRequest
	reply   func(*uplinkv1.PushBatchRequest) (*uplinkv1.PushBatchResponse, error)
	err     error
}

func (c *fakePushClient) PushBatch(_ context.Context, request *uplinkv1.PushBatchRequest, _ ...grpc.CallOption) (*uplinkv1.PushBatchResponse, error) {
	c.request = request
	if c.reply != nil {
		return c.reply(request)
	}
	return nil, c.err
}

type pushServer struct {
	uplinkv1.UnimplementedUplinkServiceServer
	request *uplinkv1.PushBatchRequest
}

func (s *pushServer) PushBatch(_ context.Context, request *uplinkv1.PushBatchRequest) (*uplinkv1.PushBatchResponse, error) {
	s.request = request
	return &uplinkv1.PushBatchResponse{BatchId: request.GetBatchId(), AcknowledgedMessageIds: []string{request.GetMessages()[0].GetMessageId()}}, nil
}

func openStore(t *testing.T, now *time.Time) *outbox.Store {
	t.Helper()
	store, err := outbox.Open(context.Background(), filepath.Join(t.TempDir(), "outbox.db"), outbox.Options{
		MaxMessages: 100,
		MaxBytes:    1024 * 1024,
		MaxAge:      24 * time.Hour,
		Now:         func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testMessage(id string) outbox.Message {
	return outbox.Message{
		MessageID: id,
		DeviceID:  "sensor-1",
		Topic:     "devices/sensor-1/state",
		Kind:      outbox.State,
		Payload:   []byte(`{"on":true}`),
	}
}
