package dynsec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

const (
	requestTopic  = "$CONTROL/dynamic-security/v1"
	responseTopic = "$CONTROL/dynamic-security/v1/response"
)

// PahoController is deliberately short-lived per request. Its mutex prevents
// two callers from confusing the uncorrelated response topic in Mosquitto 2.0.
type PahoController struct {
	brokerURL string
	username  string
	password  string
	clientID  string
	mu        sync.Mutex
}

func NewPahoController(brokerURL, username, password string) (*PahoController, error) {
	if strings.TrimSpace(brokerURL) == "" || strings.TrimSpace(username) == "" || password == "" {
		return nil, errors.New("dynsec broker URL, username, and password are required")
	}
	return &PahoController{brokerURL: brokerURL, username: username, password: password, clientID: "iot-gateway-dynsec-control"}, nil
}

func (c *PahoController) Execute(ctx context.Context, commands []Command) error {
	if len(commands) == 0 {
		return errors.New("at least one dynsec command is required")
	}
	payload, err := json.Marshal(map[string]any{"commands": commands})
	if err != nil {
		return fmt.Errorf("encode dynsec command: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	responses := make(chan []byte, 1)
	opts := mqtt.NewClientOptions().AddBroker(c.brokerURL).SetClientID(c.clientID).
		SetUsername(c.username).SetPassword(c.password).SetCleanSession(true).
		SetConnectTimeout(10 * time.Second).SetOrderMatters(false)
	client := mqtt.NewClient(opts)
	defer client.Disconnect(250)
	if err := waitToken(ctx, client.Connect()); err != nil {
		return fmt.Errorf("connect to dynsec broker: %w", err)
	}
	if err := waitToken(ctx, client.Subscribe(responseTopic, 1, func(_ mqtt.Client, message mqtt.Message) {
		copyPayload := append([]byte(nil), message.Payload()...)
		select {
		case responses <- copyPayload:
		default:
		}
	})); err != nil {
		return fmt.Errorf("subscribe to dynsec response: %w", err)
	}
	if err := waitToken(ctx, client.Publish(requestTopic, 1, false, payload)); err != nil {
		return fmt.Errorf("publish dynsec command: %w", err)
	}
	select {
	case response := <-responses:
		return checkResponse(response, commands)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitToken(ctx context.Context, token mqtt.Token) error {
	select {
	case <-token.Done():
		return token.Error()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func checkResponse(payload []byte, commands []Command) error {
	var response struct {
		Responses []struct {
			Command string `json:"command"`
			Error   string `json:"error"`
		} `json:"responses"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return fmt.Errorf("decode dynsec response: %w", err)
	}
	if len(response.Responses) != len(commands) {
		return fmt.Errorf("dynsec response count %d, want %d", len(response.Responses), len(commands))
	}
	for index, item := range response.Responses {
		want, _ := commands[index]["command"].(string)
		if item.Command != want {
			return fmt.Errorf("dynsec response command %q, want %q", item.Command, want)
		}
		if item.Error != "" {
			return errors.New(item.Error)
		}
	}
	return nil
}
