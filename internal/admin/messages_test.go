package admin

import (
	"testing"
	"time"
)

func TestMessageStorePutTakeIsOneTime(t *testing.T) {
	store := newMessageStore()
	token := store.put(message{Notice: "hello"})

	got, ok := store.take(token)
	if !ok || got.Notice != "hello" {
		t.Fatalf("take() = %#v, %v; want {Notice: hello}, true", got, ok)
	}

	// The whole point: a second take (e.g. the redirect page being
	// reloaded, or a resubmitted form landing on an old token) finds
	// nothing rather than replaying the original message.
	_, ok = store.take(token)
	if ok {
		t.Fatalf("take() after first take = ok, want not found")
	}
}

func TestMessageStoreUnknownTokenNotFound(t *testing.T) {
	store := newMessageStore()
	if _, ok := store.take("does-not-exist"); ok {
		t.Fatalf("take() of unknown token = ok, want not found")
	}
}

func TestMessageStoreExpiredEntryNotFound(t *testing.T) {
	store := newMessageStore()
	token := randomToken()
	store.items[token] = storedMessage{
		message: message{Notice: "stale"},
		expires: time.Now().Add(-time.Second),
	}

	if _, ok := store.take(token); ok {
		t.Fatalf("take() of expired token = ok, want not found")
	}
}

func TestMessageStoreSweepsExpiredEntriesOnPut(t *testing.T) {
	store := newMessageStore()
	staleToken := randomToken()
	store.items[staleToken] = storedMessage{
		message: message{Notice: "stale"},
		expires: time.Now().Add(-time.Minute),
	}

	store.put(message{Notice: "fresh"})

	if _, exists := store.items[staleToken]; exists {
		t.Errorf("expired entry survived a put() sweep")
	}
}
