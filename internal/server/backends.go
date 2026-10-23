package server

import (
	"slices"

	"pircd/internal/engine"
)

// backendSet is the configured backends. The first is the default for new
// sessions. Session IDs are UUIDs in every backend, so one ID space serves
// them all; SessionInfo.Engine records which backend owns a session.
type backendSet []engine.Backend

// get returns the backend called name, or the default for "".
func (bs backendSet) get(name string) engine.Backend {
	if name == "" && len(bs) > 0 {
		return bs[0]
	}
	for _, b := range bs {
		if b.Name() == name {
			return b
		}
	}
	return nil
}

func (bs backendSet) names() []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Name()
	}
	return out
}

// List merges every backend's sessions, most recently modified first.
func (bs backendSet) List() []engine.SessionInfo {
	var out []engine.SessionInfo
	for _, b := range bs {
		for _, info := range b.List() {
			info.Engine = b.Name()
			out = append(out, info)
		}
	}
	if len(bs) > 1 {
		slices.SortStableFunc(out, func(a, b engine.SessionInfo) int { return b.Modified.Compare(a.Modified) })
	}
	return out
}

// Find looks a session up in every backend. Cached lists are checked
// before any backend's own Find, which may rescan its storage.
func (bs backendSet) Find(id string) (engine.SessionInfo, bool) {
	for _, info := range bs.List() {
		if info.ID == id {
			return info, true
		}
	}
	for _, b := range bs {
		if info, ok := b.Find(id); ok {
			info.Engine = b.Name()
			return info, true
		}
	}
	return engine.SessionInfo{}, false
}
