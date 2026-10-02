package devicev2

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSessionClientPairsProvisionsAndConsumesSession(t *testing.T) {
	devicePublic, devicePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	info := DeviceInfo{
		Protocol:          "iot-device-v1",
		DeviceUID:         "led-unit-01",
		Model:             "esp32c3-led",
		FirmwareVersion:   "2.0.0",
		ManifestSHA256:    strings.Repeat("a", 64),
		IdentityPublicKey: base64.StdEncoding.EncodeToString(devicePublic),
		PairingRequired:   true,
	}
	info.IdentitySignature = sign64(devicePrivate, info.DeviceUID+"|"+info.Model+"|"+info.FirmwareVersion+"|"+info.ManifestSHA256)
	var pair pairingResponse
	var sessionKey []byte
	var receivedPassword string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/pair":
			var request pairingRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatalf("decode pair request: %v", err)
			}
			curve := ecdh.X25519()
			private, err := curve.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			gatewayBytes, err := base64.StdEncoding.DecodeString(request.GatewayEphemeralPublicKey)
			if err != nil {
				t.Fatal(err)
			}
			gatewayPublic, err := curve.NewPublicKey(gatewayBytes)
			if err != nil {
				t.Fatal(err)
			}
			secret, err := private.ECDH(gatewayPublic)
			if err != nil {
				t.Fatal(err)
			}
			pair = pairingResponse{
				Version:                  1,
				Suite:                    PairingSuite,
				SessionID:                base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
				DeviceNonce:              base64.RawURLEncoding.EncodeToString(bytesOf(32, 7)),
				DeviceEphemeralPublicKey: base64.StdEncoding.EncodeToString(private.PublicKey().Bytes()),
				ExpiresInMS:              120000,
			}
			pair.IdentitySignature = sign64(devicePrivate, pairSignatureTranscript(info, request, pair))
			sessionKey = deriveSessionKey(secret, request.GatewayNonce, pair.DeviceNonce, pairOpenTranscript(info, request.GatewayNonce, pair.DeviceNonce, request.GatewayEphemeralPublicKey, pair.SessionID))
			_ = json.NewEncoder(w).Encode(pair)
		case "/v1/provision":
			var envelope provisionEnvelope
			if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
				t.Fatalf("decode provision envelope: %v", err)
			}
			block, err := aes.NewCipher(sessionKey)
			if err != nil {
				t.Fatal(err)
			}
			aead, err := cipher.NewGCM(block)
			if err != nil {
				t.Fatal(err)
			}
			nonce, _ := base64.StdEncoding.DecodeString(envelope.Nonce)
			ciphertext, _ := base64.StdEncoding.DecodeString(envelope.Ciphertext)
			tag, _ := base64.StdEncoding.DecodeString(envelope.Tag)
			plaintext, err := aead.Open(nil, nonce, append(ciphertext, tag...), []byte(provisionAAD(info, envelope.SessionID)))
			if err != nil {
				t.Fatalf("decrypt provision envelope: %v", err)
			}
			var decoded struct {
				DeviceID string `json:"device_id"`
				MQTT     struct {
					Password string `json:"password"`
				} `json:"mqtt"`
			}
			if err := json.Unmarshal(plaintext, &decoded); err != nil {
				t.Fatal(err)
			}
			receivedPassword = decoded.MQTT.Password
			sum := sha256.Sum256(plaintext)
			confirmation := provisionConfirmation{Status: "persisted", DeviceID: decoded.DeviceID, ManifestSHA256: info.ManifestSHA256, SessionID: envelope.SessionID, ProvisionSHA256: hex.EncodeToString(sum[:])}
			confirmation.IdentitySignature = sign64(devicePrivate, "iot-device-provision-confirm-v1|"+info.DeviceUID+"|"+decoded.DeviceID+"|"+info.ManifestSHA256+"|"+envelope.SessionID+"|"+confirmation.ProvisionSHA256)
			_ = json.NewEncoder(w).Encode(confirmation)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	host, port := serverAddress(t, server)
	announcement := Announcement{DeviceUID: info.DeviceUID, Host: host, Port: port, Model: info.Model, Protocol: "iot-device-v1", ManifestSHA256: info.ManifestSHA256}
	client := SessionClient{HTTPClient: server.Client(), Now: func() time.Time { return time.Unix(1, 0).UTC() }}
	session, err := client.Pair(context.Background(), announcement, info)
	if err != nil {
		t.Fatalf("Pair() error: %v", err)
	}
	if err := client.Provision(context.Background(), session, ProvisionSettings{DeviceID: "led-sala", ManifestSHA256: info.ManifestSHA256, MQTTHost: "mqtt.local", MQTTPort: 8883, MQTTUsername: "led-sala", MQTTPassword: "not-in-result"}); err != nil {
		t.Fatalf("Provision() error: %v", err)
	}
	if receivedPassword != "not-in-result" {
		t.Fatalf("device received password %q", receivedPassword)
	}
	if session.key != nil || !session.used {
		t.Fatalf("session was not consumed: %#v", session)
	}
}

func TestSessionClientRejectsInvalidDeviceInfoSignature(t *testing.T) {
	info := DeviceInfo{DeviceUID: "uid", Model: "model", FirmwareVersion: "1", ManifestSHA256: strings.Repeat("a", 64), IdentityPublicKey: base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)), IdentitySignature: base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)), PairingRequired: true}
	announcement := Announcement{DeviceUID: info.DeviceUID, Host: "127.0.0.1", Port: 1, Model: info.Model, Protocol: "iot-device-v1", ManifestSHA256: info.ManifestSHA256}
	if _, err := (SessionClient{}).Pair(context.Background(), announcement, info); err == nil {
		t.Fatal("Pair() accepted an invalid static identity signature")
	}
}

func sign64(private ed25519.PrivateKey, material string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(private, []byte(material)))
}

func bytesOf(n int, value byte) []byte {
	return bytes.Repeat([]byte{value}, n)
}

func serverAddress(t *testing.T, server *httptest.Server) (string, uint16) {
	t.Helper()
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	return host, uint16(port)
}
