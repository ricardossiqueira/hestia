// Package cydprovision implements the small first-boot ESP provisioning
// protocol. The browser never talks to a device endpoint: only the
// authenticated admin process does.
package cydprovision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	deviceStatus      = "unprovisioned"
	provisioningPort  = 8080
	maxResponseLength = 4096
)

var (
	ErrInvalidIPAddress = errors.New("device IP must be a private IPv4 address")
	ErrUnexpectedDevice = errors.New("address is not the expected unprovisioned device")
)

// DeviceInfo is the non-secret identity returned by GET /v1/device-info.
type DeviceInfo struct {
	Model           string `json:"model"`
	Status          string `json:"status"`
	ProtocolVersion uint32 `json:"protocol_version"`
	DeviceUID       string `json:"device_uid"`
	FirmwareVersion string `json:"firmware_version"`
	BootstrapID     string `json:"bootstrap_id"`
	MACAddress      string `json:"mac_address"`
}

// Settings is sent only from the gateway admin process to the device. Its
// password must never be logged, stored in SQLite, or sent back to a browser.
type Settings struct {
	DeviceID   string `json:"device_id"`
	BrokerHost string `json:"mqtt_host"`
	BrokerPort uint16 `json:"mqtt_port"`
	Username   string `json:"mqtt_username"`
	Password   string `json:"mqtt_password"`
}

// Client describes the operations admin needs, keeping its transaction logic
// unit-testable without a real ESP on the LAN.
type Client interface {
	Inspect(context.Context, string) (DeviceInfo, error)
	Provision(context.Context, string, Settings) error
}

type HTTPClient struct {
	client *http.Client
	model  string
}

// NewHTTPClientForModel returns an endpoint client that accepts exactly one
// unprovisioned device model. This prevents a LED request from accidentally
// sending MQTT credentials to a different ESP on the LAN.
func NewHTTPClientForModel(timeout time.Duration, model string) *HTTPClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &HTTPClient{client: &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}, model: model}
}

func (c *HTTPClient) Inspect(ctx context.Context, address string) (DeviceInfo, error) {
	endpoint, err := endpointFor(address)
	if err != nil {
		return DeviceInfo{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/device-info", nil)
	if err != nil {
		return DeviceInfo{}, err
	}
	response, err := c.client.Do(req)
	if err != nil {
		return DeviceInfo{}, fmt.Errorf("reach device at %s: %w", address, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return DeviceInfo{}, fmt.Errorf("inspect device at %s: HTTP %d", address, response.StatusCode)
	}
	var info DeviceInfo
	if err := decodeOneJSON(response.Body, &info); err != nil {
		return DeviceInfo{}, fmt.Errorf("decode device information at %s: %w", address, err)
	}
	if info.Model != c.model || info.Status != deviceStatus || info.ProtocolVersion == 0 ||
		strings.TrimSpace(info.DeviceUID) == "" || strings.TrimSpace(info.FirmwareVersion) == "" {
		return DeviceInfo{}, ErrUnexpectedDevice
	}
	return info, nil
}

func (c *HTTPClient) Provision(ctx context.Context, address string, settings Settings) error {
	endpoint, err := endpointFor(address)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode CYD provisioning request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/provision", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("send CYD provisioning to %s: %w", address, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("CYD provisioning at %s: HTTP %d", address, response.StatusCode)
	}
	var reply struct {
		Status   string `json:"status"`
		DeviceID string `json:"device_id"`
	}
	if err := decodeOneJSON(response.Body, &reply); err != nil {
		return fmt.Errorf("decode CYD provisioning response at %s: %w", address, err)
	}
	if reply.Status != "provisioned" || reply.DeviceID != settings.DeviceID {
		return fmt.Errorf("CYD provisioning at %s returned an unexpected acknowledgement", address)
	}
	return nil
}

func endpointFor(address string) (string, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(address))
	if err != nil || !ip.Is4() || !ip.IsPrivate() {
		return "", ErrInvalidIPAddress
	}
	return "http://" + ip.String() + ":" + strconv.Itoa(provisioningPort), nil
}

func decodeOneJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxResponseLength))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
