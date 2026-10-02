package devicev2

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// PairingSuite is deliberately fixed for v1. A future suite requires a new
// protocol version instead of a silent fallback.
const PairingSuite = "X25519-HKDF-SHA256-AES-256-GCM"

// IdentityVerifier keeps the gateway's session transport independent from a
// particular device issuer/trust-store implementation. Ed25519 is the v1
// default, but the caller owns the admission policy for a public key.
type IdentityVerifier interface {
	Verify(publicKey, message, signature []byte) bool
}

type Ed25519Verifier struct{}

func (Ed25519Verifier) Verify(publicKey, message, signature []byte) bool {
	return len(publicKey) == ed25519.PublicKeySize && len(signature) == ed25519.SignatureSize && ed25519.Verify(ed25519.PublicKey(publicKey), message, signature)
}

// SessionClient performs the device-facing half of Pairing session v1. It
// never persists, returns or logs plaintext MQTT credentials.
type SessionClient struct {
	HTTPClient *http.Client
	Random     io.Reader
	Verifier   IdentityVerifier
	Now        func() time.Time
}

type pairingRequest struct {
	Version                   int    `json:"version"`
	Suite                     string `json:"suite"`
	GatewayNonce              string `json:"gateway_nonce"`
	GatewayEphemeralPublicKey string `json:"gateway_ephemeral_public_key"`
}

type pairingResponse struct {
	Version                  int    `json:"version"`
	Suite                    string `json:"suite"`
	SessionID                string `json:"session_id"`
	DeviceNonce              string `json:"device_nonce"`
	DeviceEphemeralPublicKey string `json:"device_ephemeral_public_key"`
	ExpiresInMS              uint64 `json:"expires_in_ms"`
	IdentitySignature        string `json:"identity_signature"`
}

type provisionEnvelope struct {
	Version    int    `json:"version"`
	Suite      string `json:"suite"`
	SessionID  string `json:"session_id"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
	Tag        string `json:"tag"`
}

type provisionConfirmation struct {
	Status            string `json:"status"`
	DeviceID          string `json:"device_id"`
	ManifestSHA256    string `json:"manifest_sha256"`
	SessionID         string `json:"session_id"`
	ProvisionSHA256   string `json:"provision_sha256"`
	IdentitySignature string `json:"identity_signature"`
}

// ProvisionSettings is intentionally local to the admin process. Password is
// only marshalled into an AEAD plaintext and is never part of a result type.
type ProvisionSettings struct {
	DeviceID       string
	ManifestSHA256 string
	MQTTHost       string
	MQTTPort       uint16
	MQTTUsername   string
	MQTTPassword   string
}

// SecureProvisioner is the narrow capability the privileged registration
// coordinator needs. It intentionally exposes neither session keys nor MQTT
// passwords after the device has confirmed persistence.
type SecureProvisioner interface {
	PairAndProvision(context.Context, Announcement, DeviceInfo, ProvisionSettings) error
}

// Session is short-lived and single-use. Its key is wiped after Provision,
// whether the device accepts or rejects the request.
type Session struct {
	announcement Announcement
	info         DeviceInfo
	key          []byte
	response     pairingResponse
	expiresAt    time.Time
	used         bool
}

func (s *Session) Close() {
	for i := range s.key {
		s.key[i] = 0
	}
	s.key = nil
	s.used = true
}

func (c SessionClient) Pair(ctx context.Context, announcement Announcement, info DeviceInfo) (*Session, error) {
	if !info.PairingRequired {
		return nil, errors.New("device does not require pairing")
	}
	if err := validatePairIdentity(announcement, info, c.verifier()); err != nil {
		return nil, err
	}
	nonce := make([]byte, 32)
	if _, err := io.ReadFull(c.random(), nonce); err != nil {
		return nil, fmt.Errorf("generate pairing nonce: %w", err)
	}
	curve := ecdh.X25519()
	private, err := curve.GenerateKey(c.random())
	if err != nil {
		return nil, fmt.Errorf("generate X25519 key: %w", err)
	}
	request := pairingRequest{
		Version:                   1,
		Suite:                     PairingSuite,
		GatewayNonce:              base64.RawURLEncoding.EncodeToString(nonce),
		GatewayEphemeralPublicKey: base64.StdEncoding.EncodeToString(private.PublicKey().Bytes()),
	}
	var response pairingResponse
	if err := c.postJSON(ctx, announcement, "/v1/pair", request, &response); err != nil {
		return nil, err
	}
	if err := validatePairResponse(info, request, response, c.verifier()); err != nil {
		return nil, err
	}
	devicePublicBytes, _ := base64.StdEncoding.DecodeString(response.DeviceEphemeralPublicKey)
	devicePublic, err := curve.NewPublicKey(devicePublicBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid device X25519 key: %w", err)
	}
	secret, err := private.ECDH(devicePublic)
	if err != nil {
		return nil, fmt.Errorf("derive X25519 secret: %w", err)
	}
	transcript := pairOpenTranscript(info, request.GatewayNonce, response.DeviceNonce, request.GatewayEphemeralPublicKey, response.SessionID)
	key := deriveSessionKey(secret, request.GatewayNonce, response.DeviceNonce, transcript)
	for i := range secret {
		secret[i] = 0
	}
	if len(key) != 32 {
		return nil, errors.New("invalid derived session key")
	}
	if response.ExpiresInMS == 0 || response.ExpiresInMS > 120000 {
		for i := range key {
			key[i] = 0
		}
		return nil, errors.New("invalid pairing session lifetime")
	}
	return &Session{announcement: announcement, info: info, key: key, response: response, expiresAt: c.now().Add(time.Duration(response.ExpiresInMS) * time.Millisecond)}, nil
}

// Provision encrypts the exact deterministic JSON plaintext required by the
// device core. A session is consumed even if the request fails in transit:
// callers must pair again before retrying.
func (c SessionClient) Provision(ctx context.Context, session *Session, settings ProvisionSettings) error {
	if session == nil || session.used || len(session.key) != 32 {
		return errors.New("pairing session is unavailable")
	}
	defer session.Close()
	if !c.now().Before(session.expiresAt) {
		return errors.New("pairing session expired")
	}
	if settings.DeviceID == "" || settings.ManifestSHA256 != session.info.ManifestSHA256 || settings.MQTTHost == "" || settings.MQTTPort == 0 || settings.MQTTUsername == "" || settings.MQTTPassword == "" {
		return errors.New("invalid provisioning settings")
	}
	plaintext, err := json.Marshal(struct {
		DeviceID       string `json:"device_id"`
		ManifestSHA256 string `json:"manifest_sha256"`
		MQTT           struct {
			Host     string `json:"host"`
			Port     uint16 `json:"port"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"mqtt"`
	}{DeviceID: settings.DeviceID, ManifestSHA256: settings.ManifestSHA256, MQTT: struct {
		Host     string `json:"host"`
		Port     uint16 `json:"port"`
		Username string `json:"username"`
		Password string `json:"password"`
	}{settings.MQTTHost, settings.MQTTPort, settings.MQTTUsername, settings.MQTTPassword}})
	if err != nil {
		return fmt.Errorf("marshal provisioning plaintext: %w", err)
	}
	block, err := aes.NewCipher(session.key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(c.random(), nonce); err != nil {
		return fmt.Errorf("generate AEAD nonce: %w", err)
	}
	aad := []byte(provisionAAD(session.info, session.response.SessionID))
	sealed := aead.Seal(nil, nonce, plaintext, aad)
	tagSize := aead.Overhead()
	envelope := provisionEnvelope{Version: 1, Suite: PairingSuite, SessionID: session.response.SessionID, Nonce: base64.StdEncoding.EncodeToString(nonce), Ciphertext: base64.StdEncoding.EncodeToString(sealed[:len(sealed)-tagSize]), Tag: base64.StdEncoding.EncodeToString(sealed[len(sealed)-tagSize:])}
	var confirmation provisionConfirmation
	if err := c.postJSON(ctx, session.announcement, "/v1/provision", envelope, &confirmation); err != nil {
		return err
	}
	if err := validateConfirmation(session.info, settings.DeviceID, session.response.SessionID, plaintext, confirmation, c.verifier()); err != nil {
		return err
	}
	return nil
}

// PairAndProvision is the one-shot coordinator-facing operation. Any failure
// after Pair consumes the local key through Session.Close, so a retry always
// starts from a new physical pairing operation.
func (c SessionClient) PairAndProvision(ctx context.Context, announcement Announcement, info DeviceInfo, settings ProvisionSettings) error {
	session, err := c.Pair(ctx, announcement, info)
	if err != nil {
		return err
	}
	return c.Provision(ctx, session, settings)
}

func (c SessionClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 5 * time.Second}
}
func (c SessionClient) random() io.Reader {
	if c.Random != nil {
		return c.Random
	}
	return rand.Reader
}
func (c SessionClient) verifier() IdentityVerifier {
	if c.Verifier != nil {
		return c.Verifier
	}
	return Ed25519Verifier{}
}
func (c SessionClient) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c SessionClient) postJSON(ctx context.Context, a Announcement, path string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	url := deviceURL(a, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("device %s returned %s", path, resp.Status)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

func validatePairIdentity(a Announcement, info DeviceInfo, verifier IdentityVerifier) error {
	if a.DeviceUID == "" || a.DeviceUID != info.DeviceUID || a.Model != info.Model || info.ManifestSHA256 != a.ManifestSHA256 {
		return errors.New("pairing identity does not match discovery")
	}
	key, err := base64.StdEncoding.DecodeString(info.IdentityPublicKey)
	if err != nil {
		return errors.New("invalid device identity public key")
	}
	signature, err := base64.StdEncoding.DecodeString(info.IdentitySignature)
	if err != nil || !verifier.Verify(key, []byte(info.DeviceUID+"|"+info.Model+"|"+info.FirmwareVersion+"|"+info.ManifestSHA256), signature) {
		return errors.New("device-info identity signature is invalid")
	}
	return nil
}

func validatePairResponse(info DeviceInfo, request pairingRequest, response pairingResponse, verifier IdentityVerifier) error {
	if response.Version != 1 || response.Suite != PairingSuite || len(response.SessionID) != 22 || len(response.DeviceNonce) != 43 || response.ExpiresInMS == 0 || response.ExpiresInMS > 120000 {
		return errors.New("invalid pairing response")
	}
	if _, err := base64.RawURLEncoding.DecodeString(response.SessionID); err != nil {
		return errors.New("invalid pairing session id")
	}
	if _, err := base64.RawURLEncoding.DecodeString(response.DeviceNonce); err != nil {
		return errors.New("invalid pairing device nonce")
	}
	if key, err := base64.StdEncoding.DecodeString(response.DeviceEphemeralPublicKey); err != nil || len(key) != 32 {
		return errors.New("invalid device ephemeral key")
	}
	publicKey, err := base64.StdEncoding.DecodeString(info.IdentityPublicKey)
	if err != nil {
		return errors.New("invalid device identity public key")
	}
	signature, err := base64.StdEncoding.DecodeString(response.IdentitySignature)
	if err != nil || !verifier.Verify(publicKey, []byte(pairSignatureTranscript(info, request, response)), signature) {
		return errors.New("pairing identity signature is invalid")
	}
	return nil
}

func validateConfirmation(info DeviceInfo, deviceID, sessionID string, plaintext []byte, confirmation provisionConfirmation, verifier IdentityVerifier) error {
	sha := sha256.Sum256(plaintext)
	provisionSHA := hex.EncodeToString(sha[:])
	if confirmation.Status != "persisted" || confirmation.DeviceID != deviceID || confirmation.ManifestSHA256 != info.ManifestSHA256 || confirmation.SessionID != sessionID || confirmation.ProvisionSHA256 != provisionSHA {
		return errors.New("invalid provisioning confirmation")
	}
	key, err := base64.StdEncoding.DecodeString(info.IdentityPublicKey)
	if err != nil {
		return errors.New("invalid device identity public key")
	}
	signature, err := base64.StdEncoding.DecodeString(confirmation.IdentitySignature)
	if err != nil || !verifier.Verify(key, []byte("iot-device-provision-confirm-v1|"+info.DeviceUID+"|"+deviceID+"|"+info.ManifestSHA256+"|"+sessionID+"|"+provisionSHA), signature) {
		return errors.New("provision confirmation signature is invalid")
	}
	return nil
}

func pairSignatureTranscript(info DeviceInfo, request pairingRequest, response pairingResponse) string {
	return "iot-device-pair-v1|" + info.DeviceUID + "|" + info.Model + "|" + info.FirmwareVersion + "|" + info.ManifestSHA256 + "|" + PairingSuite + "|" + response.SessionID + "|" + request.GatewayNonce + "|" + response.DeviceNonce + "|" + request.GatewayEphemeralPublicKey + "|" + response.DeviceEphemeralPublicKey
}

func pairOpenTranscript(info DeviceInfo, gatewayNonce, deviceNonce, gatewayPublicKey, sessionID string) string {
	return "iot-device-pair-open-v1|" + info.DeviceUID + "|" + info.ManifestSHA256 + "|" + PairingSuite + "|" + sessionID + "|" + gatewayNonce + "|" + deviceNonce + "|" + gatewayPublicKey
}

func provisionAAD(info DeviceInfo, sessionID string) string {
	return "iot-device-provision-v1|" + info.DeviceUID + "|" + info.ManifestSHA256 + "|" + sessionID + "|" + PairingSuite
}

func deriveSessionKey(secret []byte, gatewayNonce, deviceNonce, info string) []byte {
	saltHash := sha256.Sum256([]byte("iot-device-pair-salt-v1|" + gatewayNonce + "|" + deviceNonce))
	extract := hmac.New(sha256.New, saltHash[:])
	_, _ = extract.Write(secret)
	prk := extract.Sum(nil)
	expand := hmac.New(sha256.New, prk)
	_, _ = expand.Write([]byte(info))
	_, _ = expand.Write([]byte{1})
	return append([]byte(nil), expand.Sum(nil)[:32]...)
}
