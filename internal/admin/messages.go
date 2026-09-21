package admin

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// message is what survives a redirect after a mutating request (add/remove
// device), passed as an opaque one-time token in the redirect URL instead
// of the message ITSELF - see messageStore's doc comment for why.
type message struct {
	Flash  *flashView
	Notice string
	Error  string
}

// messageStore hands out a one-time token for a message and lets exactly
// one subsequent read consume it. This exists to implement the
// Post/Redirect/Get pattern: every mutating handler (handleAddDevice,
// handleRemoveDevice) MUST redirect rather than render its own response
// directly - responding to the POST itself leaves that POST as the
// browser's "current page", so reloading (or navigating back and
// resubmitting) resends it. Provision() rotates an existing device's
// Mosquitto password as a side effect, so a resent POST silently
// invalidating a working device's credential is exactly the incident this
// closes off entirely, not just the single case DeviceExists() already
// guards.
//
// The token, not the message, goes in the redirect's query string
// specifically so a just-generated device password never appears in a URL
// - URLs end up in browser history, referrer headers and access logs in a
// way a short-lived server-side token deliberately avoids.
type messageStore struct {
	mu    sync.Mutex
	items map[string]storedMessage
}

type storedMessage struct {
	message
	expires time.Time
}

const messageTTL = 5 * time.Minute

func newMessageStore() *messageStore {
	return &messageStore{items: make(map[string]storedMessage)}
}

// put stores msg and returns a token that retrieves it exactly once, via
// take. Also opportunistically sweeps expired, never-read entries so a
// long-running admin process with abandoned redirects doesn't leak memory.
func (s *messageStore) put(msg message) string {
	token := randomToken()
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, stored := range s.items {
		if now.After(stored.expires) {
			delete(s.items, key)
		}
	}
	s.items[token] = storedMessage{message: msg, expires: now.Add(messageTTL)}
	return token
}

// take returns the message for token and deletes it - a second call with
// the same token (e.g. a stale bookmark, or the redirect page itself being
// reloaded) finds nothing, which is the whole point.
func (s *messageStore) take(token string) (message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.items[token]
	if !ok {
		return message{}, false
	}
	delete(s.items, token)
	if time.Now().After(stored.expires) {
		return message{}, false
	}
	return stored.message, true
}

func randomToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read failing is effectively unrecoverable on any
		// real OS; a predictable fallback here would defeat the point of
		// an unguessable token, so this is the one place in the package
		// that deliberately panics instead of returning an error.
		panic("admin: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
