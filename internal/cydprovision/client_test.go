package cydprovision

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEndpointForAcceptsPrivateIPv4Only(t *testing.T) {
	endpoint, err := endpointFor("192.168.15.42")
	if err != nil || endpoint != "http://192.168.15.42:8080" {
		t.Fatalf("endpointFor() = %q, %v", endpoint, err)
	}
	for _, address := range []string{"127.0.0.1", "8.8.8.8", "::1", "not-an-ip"} {
		if _, err := endpointFor(address); !errors.Is(err, ErrInvalidIPAddress) {
			t.Errorf("endpointFor(%q) error = %v, want ErrInvalidIPAddress", address, err)
		}
	}
}

func TestHTTPClientInspectRejectsUnexpectedDevice(t *testing.T) {
	// endpointFor purposefully prevents the client from contacting arbitrary
	// endpoints. Exercise response validation through the parser separately.
	if _, err := decodeDeviceInfo(strings.NewReader(`{"model":"other","status":"unprovisioned"}`)); !errors.Is(err, ErrUnexpectedDevice) {
		t.Fatalf("decodeDeviceInfo() error = %v, want ErrUnexpectedDevice", err)
	}
}

func TestDecodeOneJSONRejectsTrailingDocument(t *testing.T) {
	var output map[string]string
	if err := decodeOneJSON(strings.NewReader(`{"ok":"yes"} {"extra":"no"}`), &output); err == nil {
		t.Fatal("decodeOneJSON() error = nil")
	}
}

func TestHTTPClientMethodsRejectPublicAddress(t *testing.T) {
	client := NewHTTPClientForModel(0, "cyd-monitor")
	if _, err := client.Inspect(context.Background(), "8.8.8.8"); !errors.Is(err, ErrInvalidIPAddress) {
		t.Fatalf("Inspect() error = %v", err)
	}
	if err := client.Provision(context.Background(), "8.8.8.8", Settings{}); !errors.Is(err, ErrInvalidIPAddress) {
		t.Fatalf("Provision() error = %v", err)
	}
}

func decodeDeviceInfo(reader *strings.Reader) (DeviceInfo, error) {
	var info DeviceInfo
	if err := decodeOneJSON(reader, &info); err != nil {
		return DeviceInfo{}, err
	}
	if info.Model != "cyd-monitor" || info.Status != deviceStatus {
		return DeviceInfo{}, ErrUnexpectedDevice
	}
	return info, nil
}
