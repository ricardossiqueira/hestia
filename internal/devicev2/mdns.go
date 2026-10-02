package devicev2

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

const deviceService = "_iot-device._tcp"

// rebrowseInterval bounds how stale a discovery sighting can get. zeroconf
// only delivers an entry the first time it sees a given service instance -
// it never re-emits one for that same instance after its TXT/address record
// changes (e.g. a device reboots with new firmware), so a single long-lived
// Browse call would freeze every device's Inbox state at its first sighting
// forever. Restarting Browse from scratch on this interval re-discovers
// everything currently on the network instead. It must stay under the
// Inbox's TTL (NewInbox's default is 90s) so an online device's entry is
// refreshed before List() would otherwise demote it to Offline.
const rebrowseInterval = 60 * time.Second

// MDNSBrowser turns untrusted DNS-SD announcements into inspected inbox
// entries. It owns no registry or credential capability.
type MDNSBrowser struct {
	Inspector Inspector
}

// Run blocks until ctx is cancelled, repeating discovery every
// rebrowseInterval (see its doc comment for why). Repeated advertisements
// are harmless: Inbox Observe preserves the already-inspected state and
// inspection confirms the current endpoint before an operator can pair it.
func (b MDNSBrowser) Run(ctx context.Context, inbox *Inbox) error {
	if inbox == nil || b.Inspector == nil {
		return errors.New("mDNS browser requires inbox and inspector")
	}
	for ctx.Err() == nil {
		if err := b.browseOnce(ctx, inbox); err != nil {
			return err
		}
	}
	return nil
}

// browseOnce runs a single bounded discovery cycle: it blocks for up to
// rebrowseInterval (or until ctx is cancelled), feeding every entry it sees
// to inbox, then returns so Run can start a fresh cycle.
func (b MDNSBrowser) browseOnce(ctx context.Context, inbox *Inbox) error {
	resolver, err := zeroconf.NewResolver()
	if err != nil {
		return err
	}
	cycleCtx, cancel := context.WithTimeout(ctx, rebrowseInterval)
	defer cancel()
	entries := make(chan *zeroconf.ServiceEntry)
	if err := resolver.Browse(cycleCtx, deviceService, "local.", entries); err != nil {
		return err
	}
	for entry := range entries {
		a, err := announcementFromMDNS(entry)
		if err != nil || inbox.Observe(a) != nil {
			continue
		}
		info, _, _, inspectErr := b.Inspector.Inspect(ctx, a)
		inbox.MarkInspected(a.DeviceUID, info, inspectErr)
	}
	return nil
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
