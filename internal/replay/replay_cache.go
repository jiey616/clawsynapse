// Package replay provides a persisted seen-key cache shared by the auth
// handshake (nonce/message replay protection) and the messaging inbox
// (duplicate envelope suppression).
package replay

import (
	"sync"
	"time"

	"clawsynapse/internal/store"
)

// ReplayGuard remembers keys until their expiry. The persisted entry value
// is the expiry unix-millisecond timestamp, so entries with different TTLs
// (auth nonces vs inbox message ids) can coexist in one shared state file.
//
// Pre-1.0 state files stored the insertion timestamp instead; those values
// are interpreted as already-expired on load and gc'd away harmlessly.
type ReplayGuard struct {
	mu         sync.Mutex
	store      *store.FSStore
	entries    map[string]int64 // key -> expireAtUnixMs
	maxEntries int
	defaultTTL time.Duration
}

func NewReplayGuard(fs *store.FSStore, maxEntries int, ttl time.Duration) (*ReplayGuard, error) {
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}

	st, err := fs.LoadReplayState()
	if err != nil {
		return nil, err
	}

	r := &ReplayGuard{
		store:      fs,
		entries:    st.Entries,
		maxEntries: maxEntries,
		defaultTTL: ttl,
	}
	r.gc(time.Now().UnixMilli())
	if err := r.persist(); err != nil {
		return nil, err
	}
	return r, nil
}

// CheckAndRemember reports whether key is being seen for the first time
// within its ttl (true = fresh, remembered now; false = duplicate).
// ttl <= 0 falls back to the constructor default.
func (r *ReplayGuard) CheckAndRemember(key string, ttl time.Duration) bool {
	if ttl <= 0 {
		ttl = r.defaultTTL
	}
	nowMs := time.Now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()

	r.gc(nowMs)

	if _, exists := r.entries[key]; exists {
		return false
	}

	r.entries[key] = nowMs + ttl.Milliseconds()
	if len(r.entries) > r.maxEntries {
		r.evictOldest()
	}

	// Persist synchronously: a restart must not forget seen keys, or a
	// redelivered envelope/nonce would be processed twice. Message and
	// handshake frequency is low enough that a full-file write per check
	// is acceptable (spec T2.2 allows the simplification).
	return r.persistLocked() == nil
}

func (r *ReplayGuard) gc(nowMs int64) {
	for k, expireAt := range r.entries {
		if expireAt < nowMs {
			delete(r.entries, k)
		}
	}
}

func (r *ReplayGuard) evictOldest() {
	for len(r.entries) > r.maxEntries {
		var oldestKey string
		var oldestTs int64
		first := true
		for k, ts := range r.entries {
			if first || ts < oldestTs {
				oldestKey = k
				oldestTs = ts
				first = false
			}
		}
		if oldestKey == "" {
			return
		}
		delete(r.entries, oldestKey)
	}
}

func (r *ReplayGuard) persist() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.persistLocked()
}

func (r *ReplayGuard) persistLocked() error {
	cp := make(map[string]int64, len(r.entries))
	for k, v := range r.entries {
		cp[k] = v
	}
	return r.store.SaveReplayState(store.ReplayState{
		SchemaVersion: 2,
		Entries:       cp,
	})
}
