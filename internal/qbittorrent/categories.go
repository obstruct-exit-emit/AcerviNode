package qbittorrent

import (
	"encoding/json"
	"net/http"
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
