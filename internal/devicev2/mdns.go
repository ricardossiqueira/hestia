package devicev2

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"

	"github.com/grandcat/zeroconf"
)

const deviceService = "_iot-device._tcp"

// MDNSBrowser turns untrusted DNS-SD announcements into inspected inbox
// entries. It owns no registry or credential capability.
type MDNSBrowser struct {
	Inspector Inspector
}

// Run blocks until ctx is cancelled. Repeated advertisements are harmless:
// Inbox Observe preserves the already-inspected state and inspection confirms
// the current endpoint before an operator can pair it.
func (b MDNSBrowser) Run(ctx context.Context, inbox *Inbox) error {
	if inbox == nil || b.Inspector == nil {
		return errors.New("mDNS browser requires inbox and inspector")
	}
	resolver, err := zeroconf.NewResolver()
	if err != nil {
		return err
	}
	entries := make(chan *zeroconf.ServiceEntry)
	if err := resolver.Browse(ctx, deviceService, "local.", entries); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case entry, ok := <-entries:
			if !ok {
				return nil
			}
			a, err := announcementFromMDNS(entry)
			if err != nil || inbox.Observe(a) != nil {
				continue
			}
			info, _, _, inspectErr := b.Inspector.Inspect(ctx, a)
			inbox.MarkInspected(a.DeviceUID, info, inspectErr)
		}
	}
}

func announcementFromMDNS(entry *zeroconf.ServiceEntry) (Announcement, error) {
	if entry == nil || entry.Port <= 0 || entry.Port > 65535 {
		return Announcement{}, errors.New("invalid mDNS device entry")
	}
	txt := map[string]string{}
	for _, value := range entry.Text {
		key, value, ok := strings.Cut(value, "=")
		if ok {
			txt[key] = value
		}
	}
	host := firstAddress(entry.AddrIPv4, entry.AddrIPv6)
	if host == "" {
		return Announcement{}, errors.New("mDNS device entry has no address")
	}
	pairing, err := strconv.ParseBool(txt["pairing"])
	if err != nil {
		return Announcement{}, errors.New("invalid mDNS pairing flag")
	}
	return Announcement{DeviceUID: txt["uid"], Host: host, Port: uint16(entry.Port), Model: txt["model"], Protocol: txt["protocol"], Firmware: txt["firmware"], ManifestSHA256: txt["manifest_sha256"], Pairing: pairing, Status: txt["status"], Path: txt["path"]}, nil
}

func firstAddress(v4, v6 []net.IP) string {
	if len(v4) > 0 {
		return v4[0].String()
	}
	if len(v6) > 0 {
		return v6[0].String()
	}
	return ""
}
