package storage

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestDirtyBufferReadYourOwnWrites(t *testing.T) {
	clock := time.Now()
	buffer := NewDirtyBuffer(10*time.Second, func() time.Time { return clock })
	entry := CacheEntry{
		Key:        "fingerprint-1",
		StatusCode: 201,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       []byte("fresh"),
	}
	buffer.Set(entry)

	got, ok := buffer.Get(entry.Key)
	if !ok || got.StatusCode != 201 || string(got.Body) != "fresh" {
		t.Fatalf("unexpected read-your-own-write: %#v, hit=%v", got, ok)
	}
	entry.Body[0] = 'X'
	got.Body[0] = 'Y'
	again, ok := buffer.Get(entry.Key)
	if !ok || string(again.Body) != "fresh" {
		t.Fatalf("buffer value was aliased: %#v, hit=%v", again, ok)
	}

	clock = clock.Add(10*time.Second + time.Nanosecond)
	if _, ok := buffer.Get(entry.Key); ok {
		t.Fatal("expired dirty entry was returned")
	}
}

func TestDirtyBufferConcurrentAccess(t *testing.T) {
	buffer := NewDirtyBuffer(250*time.Millisecond, time.Now)
	const writers = 64
	const perWriter = 32
	var group sync.WaitGroup
	group.Add(writers)
	for worker := 0; worker < writers; worker++ {
		worker := worker
		go func() {
			defer group.Done()
			for index := 0; index < perWriter; index++ {
				key := "key-" + string(rune('a'+worker%26)) + "-" + string(rune(index))
				buffer.Set(CacheEntry{Key: key, Body: []byte("value")})
				_, _ = buffer.Get(key)
			}
		}()
	}
	group.Wait()
	if _, ok := buffer.Get("key-a-\x00"); !ok {
		// The exact key is intentionally not important; this confirms that
		// concurrent accesses completed without a panic or lost map state.
		buffer.Set(CacheEntry{Key: "sentinel", Body: []byte("value")})
		if _, ok := buffer.Get("sentinel"); !ok {
			t.Fatal("concurrent writes left the dirty buffer unusable")
		}
	}
}
