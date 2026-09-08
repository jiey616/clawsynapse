package adapter

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"clawsynapse/internal/store"
)

// sessionMapper keeps adapter session continuations in memory and batches
// their persistence to the FSStore (T2.3).
//
// Every read and write hits the in-memory map only; disk writes are
// deferred to a flush triggered by (a) a 30s timer scheduled after each
// mutation and (b) Close() on process exit. This removes the per-delivery
// load+save file I/O the hermes adapter previously did on the hot path
// (audit 6.1.4 write amplification).
//
// Deletions are tombstoned so a key deleted in memory stays invisible
// until the flush removes its file; a flush failure keeps the entry dirty
// for the next flush instead of losing the mutation.
type sessionMappingEntry struct {
	sessionID   string
	createdAtMs int64
	dirty       bool
	deleted     bool // tombstone: remove the persisted file on flush
}

type sessionMapper struct {
	adapterName string
	store       *store.FSStore
	log         *slog.Logger

	mu         sync.Mutex
	entries    map[string]*sessionMappingEntry
	timer      *time.Timer
	stopped    bool
	flushDelay time.Duration // overridable in tests
}

func newSessionMapper(adapterName string, st *store.FSStore, log *slog.Logger) *sessionMapper {
	return &sessionMapper{
		adapterName: adapterName,
		store:       st,
		log:         log,
		entries:     map[string]*sessionMappingEntry{},
		flushDelay:  30 * time.Second,
	}
}

// load returns the continuation id for sessionKey. Memory first; on a miss
// the persisted state is loaded once and cached (clean, so it never
// triggers a write). Deleted tombstones read as empty.
func (m *sessionMapper) load(sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if m == nil || m.store == nil || sessionKey == "" {
		return ""
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[sessionKey]; ok {
		if e.deleted {
			// Tombstone: hidden immediately, even before the flush
			// removes the persisted file. Never fall through to disk.
			return ""
		}
		return strings.TrimSpace(e.sessionID)
	}

	st, ok, err := m.store.LoadSessionState(m.adapterName, sessionKey)
	if err != nil {
		m.warn("load session mapping failed", sessionKey, err)
		return ""
	}
	if !ok {
		return ""
	}
	id := strings.TrimSpace(st.SessionID)
	if id == "" {
		return ""
	}
	// Cache the disk value so repeated loads stay off disk. Clean entry:
	// it matches the persisted state, nothing to flush.
	m.entries[sessionKey] = &sessionMappingEntry{sessionID: id, createdAtMs: st.CreatedAtMs}
	return id
}

// save records the continuation id in memory and schedules a flush. An
// unchanged value is a no-op. The original creation time is preserved
// across updates.
func (m *sessionMapper) save(sessionKey string, sessionID string) {
	sessionKey = strings.TrimSpace(sessionKey)
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || m.store == nil || sessionKey == "" || sessionID == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.entries[sessionKey]; ok && !e.deleted {
		if strings.TrimSpace(e.sessionID) == sessionID {
			return
		}
		e.sessionID = sessionID
		e.dirty = true
		e.deleted = false
		m.scheduleFlushLocked()
		return
	}

	// Not cached (or tombstoned): consult disk for the original creation
	// time and the no-op check.
	createdAt := time.Now().UnixMilli()
	if e, ok := m.entries[sessionKey]; ok && e.deleted {
		createdAt = e.createdAtMs
	} else if st, ok, err := m.store.LoadSessionState(m.adapterName, sessionKey); err != nil {
		m.warn("load session mapping failed", sessionKey, err)
	} else if ok {
		if strings.TrimSpace(st.SessionID) == sessionID {
			m.entries[sessionKey] = &sessionMappingEntry{sessionID: sessionID, createdAtMs: st.CreatedAtMs}
			return
		}
		if st.CreatedAtMs > 0 {
			createdAt = st.CreatedAtMs
		}
	}

	m.entries[sessionKey] = &sessionMappingEntry{
		sessionID:   sessionID,
		createdAtMs: createdAt,
		dirty:       true,
	}
	m.scheduleFlushLocked()
}

// delete hides the key immediately (memory) and tombstones it so the
// flush removes the persisted file. The tombstone is kept until the
// delete succeeds so a failed flush retries.
func (m *sessionMapper) delete(sessionKey string) {
	sessionKey = strings.TrimSpace(sessionKey)
	if m == nil || m.store == nil || sessionKey == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	createdAt := int64(0)
	if e, ok := m.entries[sessionKey]; ok {
		createdAt = e.createdAtMs
	}
	m.entries[sessionKey] = &sessionMappingEntry{createdAtMs: createdAt, dirty: true, deleted: true}
	m.scheduleFlushLocked()
}

// Flush persists every dirty entry. Safe to call concurrently; the lock
// also serializes flush against mutations so no mutation is lost or
// double-written.
func (m *sessionMapper) Flush() {
	if m == nil || m.store == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for key, e := range m.entries {
		if !e.dirty {
			continue
		}
		if e.deleted {
			if err := m.store.DeleteSessionState(m.adapterName, key); err != nil {
				m.warn("delete session mapping failed", key, err)
				continue // stay dirty for the next flush
			}
			delete(m.entries, key)
			continue
		}
		st := store.SessionState{
			Adapter:     m.adapterName,
			SessionKey:  key,
			SessionID:   e.sessionID,
			CreatedAtMs: e.createdAtMs,
			UpdatedAtMs: time.Now().UnixMilli(),
		}
		if err := m.store.SaveSessionState(st); err != nil {
			m.warn("save session mapping failed", key, err)
			continue // stay dirty for the next flush
		}
		e.dirty = false
	}
}

// Close stops the deferred flush timer and writes out anything pending.
// Called on process exit so the last mutations are not lost to the 30s
// window.
func (m *sessionMapper) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if m.stopped {
		return
	}
	m.stopped = true

	for key, e := range m.entries {
		if !e.dirty {
			continue
		}
		if e.deleted {
			if err := m.store.DeleteSessionState(m.adapterName, key); err != nil {
				m.warn("delete session mapping failed", key, err)
				continue
			}
			delete(m.entries, key)
			continue
		}
		st := store.SessionState{
			Adapter:     m.adapterName,
			SessionKey:  key,
			SessionID:   e.sessionID,
			CreatedAtMs: e.createdAtMs,
			UpdatedAtMs: time.Now().UnixMilli(),
		}
		if err := m.store.SaveSessionState(st); err != nil {
			m.warn("save session mapping failed", key, err)
		} else {
			e.dirty = false
		}
	}
}

// scheduleFlushLocked (re)arms the 30s deferred flush. AfterFunc + Reset
// may race with an already-fired callback, but Flush is idempotent and
// lock-serialized, so a spurious extra flush is harmless.
func (m *sessionMapper) scheduleFlushLocked() {
	if m.stopped {
		return
	}
	if m.timer == nil {
		m.timer = time.AfterFunc(m.flushDelay, m.Flush)
		return
	}
	m.timer.Reset(m.flushDelay)
}

func (m *sessionMapper) warn(msg, sessionKey string, err error) {
	if m.log == nil {
		return
	}
	m.log.Warn(msg,
		slog.String("adapter", m.adapterName),
		slog.String("sessionKey", sessionKey),
		slog.String("error", err.Error()),
	)
}
