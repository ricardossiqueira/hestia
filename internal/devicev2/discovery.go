package devicev2

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Announcement is the trusted-minimum projection of an mDNS TXT record. It
// is not identity proof: callers must inspect and pair before registration.
type Announcement struct {
	DeviceUID      string
	Host           string
	Port           uint16
	Model          string
	Protocol       string
	Firmware       string
	ManifestSHA256 string
	Pairing        bool
	Status         string
	Path           string
	SeenAt         time.Time
}

type DiscoveryState string

const (
	Seen            DiscoveryState = "seen"
	Inspected       DiscoveryState = "inspected"
	PairingRequired DiscoveryState = "pairing_required"
	ReadyToRegister DiscoveryState = "ready_to_register"
	Registered      DiscoveryState = "registered"
	Rejected        DiscoveryState = "rejected"
	Offline         DiscoveryState = "offline"
)

type DiscoveryEntry struct {
	Announcement
	State DiscoveryState
	Info  *DeviceInfo
	Error string
}

// Inbox holds expiring mDNS sightings. It intentionally has no persistence:
// a discovery entry is never an active or trusted device.
type Inbox struct {
	mu      sync.Mutex
	entries map[string]DiscoveryEntry
	ttl     time.Duration
	now     func() time.Time
}

func NewInbox(ttl time.Duration) *Inbox {
	if ttl <= 0 {
		ttl = 90 * time.Second
	}
	return &Inbox{entries: map[string]DiscoveryEntry{}, ttl: ttl, now: func() time.Time { return time.Now().UTC() }}
}
func (i *Inbox) Observe(a Announcement) error {
	if strings.TrimSpace(a.DeviceUID) == "" || strings.TrimSpace(a.Host) == "" || a.Port == 0 || a.Protocol != "iot-device-v1" || len(a.ManifestSHA256) != 64 {
		return errors.New("invalid iot-device mDNS announcement")
	}
	if a.SeenAt.IsZero() {
		a.SeenAt = i.now()
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	previous, exists := i.entries[a.DeviceUID]
	entry := DiscoveryEntry{Announcement: a, State: Seen}
	if exists {
		entry.State = previous.State
		entry.Info = previous.Info
		entry.Error = previous.Error
	}
	if entry.State == Offline {
		entry.State = Seen
	}
	i.entries[a.DeviceUID] = entry
	return nil
}
func (i *Inbox) MarkInspected(uid string, info DeviceInfo, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	entry, ok := i.entries[uid]
	if !ok {
		return
	}
	if err != nil {
		entry.State = Rejected
		entry.Error = err.Error()
	} else {
		entry.Info = &info
		entry.Error = ""
		if info.PairingRequired {
			entry.State = PairingRequired
		} else {
			entry.State = ReadyToRegister
		}
	}
	i.entries[uid] = entry
}
func (i *Inbox) MarkRegistered(uid string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	entry, ok := i.entries[uid]
	if !ok {
		return
	}
	entry.State = Registered
	entry.Error = ""
	i.entries[uid] = entry
}
func (i *Inbox) List() []DiscoveryEntry {
	i.mu.Lock()
	defer i.mu.Unlock()
	now := i.now()
	out := make([]DiscoveryEntry, 0, len(i.entries))
	for uid, entry := range i.entries {
		if entry.State != Registered && now.Sub(entry.SeenAt) > i.ttl {
			entry.State = Offline
			i.entries[uid] = entry
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].DeviceUID < out[b].DeviceUID })
	return out
}

type DeviceInfo struct {
	Protocol          string          `json:"protocol"`
	DeviceUID         string          `json:"device_uid"`
	Model             string          `json:"model"`
	FirmwareVersion   string          `json:"firmware_version"`
	Manifest          json.RawMessage `json:"manifest"`
	ManifestSHA256    string          `json:"manifest_sha256"`
	IdentityPublicKey string          `json:"identity_public_key"`
	IdentitySignature string          `json:"identity_signature"`
	PairingRequired   bool            `json:"pairing_required"`
	ProvisioningState string          `json:"provisioning_state"`
}

// Inspector is the only network dependency discovery registration needs.
type Inspector interface {
	Inspect(context.Context, Announcement) (DeviceInfo, Manifest, string, error)
}
type HTTPInspector struct{ Client *http.Client }

func (h HTTPInspector) Inspect(ctx context.Context, a Announcement) (DeviceInfo, Manifest, string, error) {
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	path := a.Path
	if path == "" {
		path = "/v1/device-info"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a.Host+fmt.Sprintf(":%d", a.Port)+path, nil)
	if err != nil {
		return DeviceInfo{}, Manifest{}, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return DeviceInfo{}, Manifest{}, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return DeviceInfo{}, Manifest{}, "", fmt.Errorf("device-info returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return DeviceInfo{}, Manifest{}, "", err
	}
	var info DeviceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return DeviceInfo{}, Manifest{}, "", fmt.Errorf("decode device-info: %w", err)
	}
	if info.Protocol != "iot-device-v1" || info.DeviceUID != a.DeviceUID || info.Model != a.Model || strings.TrimSpace(info.IdentityPublicKey) == "" {
		return DeviceInfo{}, Manifest{}, "", errors.New("device-info does not match discovery identity")
	}
	manifest, canonical, hash, err := Parse(string(info.Manifest))
	if err != nil {
		return DeviceInfo{}, Manifest{}, "", err
	}
	if manifest.Model != info.Model || hash != info.ManifestSHA256 || hash != a.ManifestSHA256 {
		return DeviceInfo{}, Manifest{}, "", fmt.Errorf("device-info manifest hash does not match: computed=%s info=%s announcement=%s", hash, info.ManifestSHA256, a.ManifestSHA256)
	}
	return info, manifest, canonical, nil
}

// ManifestHash is exported for fixtures and adapter tests that need to form
// an announcement before a gateway has inspected its source device.
func ManifestHash(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}
