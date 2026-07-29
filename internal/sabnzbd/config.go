package sabnzbd

import (
	"net/http"
	"sort"
	"sync"
)

// fakeVersion is a fixed, plausible SABnzbd version string — *arr apps check
// this on "Test" but don't require a specific value.
const fakeVersion = "4.3.1"

// categoryStore tracks categories *arr apps have declared, purely so
// mode=get_config has something to report back — AcerviNode doesn't
// interpret categories itself (see docs/configuration.md). The "*" default
// category always exists, matching a real SABnzbd install — a protocol
// requirement, not a user-visible one (see Server.Categories, which filters
// it back out for the settings UI). Otherwise starts empty; Sonarr/Radarr
// populate it themselves the moment they declare a category.
type categoryStore struct {
	mu    sync.Mutex
	names map[string]bool
}

func newCategoryStore() *categoryStore {
	return &categoryStore{names: map[string]bool{"*": true}}
}

func (c *categoryStore) add(name string) {
	if name == "" {
		return
	}
	c.mu.Lock()
	c.names[name] = true
	c.mu.Unlock()
}

func (c *categoryStore) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.names))
	for name := range c.names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Categories lists every category name currently known, including the
// always-present "*" default — populated reactively as *arr apps declare
// them, or manually via AddCategory (see the settings API).
// Categories lists every category name currently known, excluding the
// always-present "*" — that's a protocol requirement every real SABnzbd
// install reports (see categoryStore's doc comment), not something anyone
// declared or manages, so callers of this API shouldn't have to know to
// filter it back out themselves.
func (s *Server) Categories() []string {
	names := s.categories.list()
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name != "*" {
			out = append(out, name)
		}
	}
	return out
}

// AddCategory manually registers a category name, the same way an *arr
// app's own mode=addurl/addfile "cat" field does implicitly — see
// internal/qbittorrent's identical AddCategory for the rationale.
func (s *Server) AddCategory(name string) {
	s.categories.add(name)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"version": fakeVersion})
}

type sabCategory struct {
	Name string `json:"name"`
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	names := s.categories.list()
	cats := make([]sabCategory, len(names))
	for i, name := range names {
		cats[i] = sabCategory{Name: name}
	}
	writeJSON(w, map[string]any{
		"config": map[string]any{
			"categories": cats,
			"misc": map[string]any{
				"complete_dir": "",
			},
		},
	})
}

func (s *Server) handleFullStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"status": map[string]any{
			"version": fakeVersion,
			"paused":  false,
		},
	})
}
