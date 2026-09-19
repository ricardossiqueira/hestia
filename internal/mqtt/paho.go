package mqtt

import (
	"context"
	"fmt"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
)

// NewPahoClient builds the production MQTT transport. The credentials are
// supplied separately so they are never read from or stored in YAML.
func NewPahoClient(cfg config.MQTT, credentials Credentials) (Client, error) {
	if cfg.URL == "" || cfg.ClientID == "" {
		return nil, fmt.Errorf("MQTT URL and client ID are required")
	}
	options := paho.NewClientOptions().
		AddBroker(cfg.URL).
		SetClientID(cfg.ClientID).
		SetCleanSession(false).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetOrderMatters(false)
	if credentials.Username != "" || credentials.Password != "" {
		options.SetUsername(credentials.Username)
		options.SetPassword(credentials.Password)
	}
	return &pahoClient{client: paho.NewClient(options)}, nil
}

type pahoClient struct {
	client paho.Client
}

func (c *pahoClient) Connect(ctx context.Context) error {
	if err := waitToken(ctx, c.client.Connect()); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	return nil
}

func (c *pahoClient) Subscribe(ctx context.Context, topic string, handler MessageHandler) error {
	token := c.client.Subscribe(topic, qosAtLeastOnce, func(_ paho.Client, message paho.Message) {
		handler(context.Background(), message.Topic(), message.Payload())
	})
	if err := waitToken(ctx, token); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	return nil
}

func (c *pahoClient) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	if err := waitToken(ctx, c.client.Publish(topic, qos, retain, payload)); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	return nil
}

func (c *pahoClient) Close() { c.client.Disconnect(250) }

func waitToken(ctx context.Context, token paho.Token) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-token.Done():
		return token.Error()
	}
}
