// Package p1s speaks the Bambu P1-series local protocols: MQTT state/commands
// on :8883 and the chamber-camera stream on :6000.
package p1s

import "sync"

// StateCache merges partial "print" reports into a full picture. After the
// initial pushall dump the printer only sends changed fields.
type StateCache struct {
	mu        sync.Mutex
	fields    map[string]any
	connected bool
}

func NewStateCache() *StateCache {
	return &StateCache{fields: map[string]any{}}
}

func (s *StateCache) Merge(fields map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deepMerge(s.fields, fields)
}

// deepMerge recursively merges src into dst. Nested JSON objects are merged
// key-by-key so a partial report doesn't wipe sibling fields an earlier fuller
// report set — e.g. an "ams" delta that omits tray_now (the loaded tray) must
// not erase it. Arrays and scalars replace wholesale: the printer resends whole
// arrays (like the tray list), so element-wise merging isn't needed.
func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, ok := dst[k].(map[string]any); ok {
				deepMerge(dstMap, srcMap)
				continue
			}
		}
		dst[k] = v
	}
}

func (s *StateCache) SetConnected(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = v
}

// Snapshot returns a copy of the merged fields plus the connection flag.
func (s *StateCache) Snapshot() (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]any, len(s.fields))
	for k, v := range s.fields {
		out[k] = v
	}
	return out, s.connected
}
