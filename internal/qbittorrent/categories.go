package qbittorrent

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
)

// categoryStore tracks categories *arr apps have declared, purely so
// /api/v2/torrents/categories has something to report back — AcerviNode
// doesn't interpret categories itself (see docs/configuration.md).
type categoryStore struct {
	mu    sync.Mutex
	names map[string]string // name -> save path
}

func newCategoryStore() *categoryStore {
	return &categoryStore{names: map[string]string{}}
}

func (c *categoryStore) add(name, savePath string) {
	if name == "" {
		return
	}
	c.mu.Lock()
	c.names[name] = savePath
	c.mu.Unlock()
}

// Categories lists every category name currently known — populated
// reactively as *arr apps declare them (see handleCreateCategory), or
// manually via AddCategory (see the settings API). Sorted for a stable,
// predictable order in the settings UI.
func (s *Server) Categories() []string {
	s.categories.mu.Lock()
	defer s.categories.mu.Unlock()
	names := make([]string, 0, len(s.categories.names))
	for name := range s.categories.names {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// AddCategory manually registers a category name, the same way an *arr
// app's own createCategory call does — see internal/api's settings
// endpoints, which let a category be pre-declared from the web UI (e.g. to
// populate the "Add Download" form's category field) without needing
// Sonarr/Radarr to have done it first. No save path: that's an *arr-app
// concept AcerviNode itself never interprets (see docs/configuration.md).
func (s *Server) AddCategory(name string) {
	s.categories.add(name, "")
}

type categoryResponse struct {
	Name     string `json:"name"`
	SavePath string `json:"savePath"`
}

func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	s.categories.mu.Lock()
	out := make(map[string]categoryResponse, len(s.categories.names))
	for name, savePath := range s.categories.names {
		out[name] = categoryResponse{Name: name, SavePath: savePath}
	}
	s.categories.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeText(w, http.StatusBadRequest, "")
		return
	}
	s.categories.add(r.FormValue("category"), r.FormValue("savePath"))
	writeText(w, http.StatusOK, "")
}
