package qbittorrent

import (
	"sort"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
)

func TestServer_CategoriesAndAddCategory(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s := NewServer(newFakeProvider(), db, staticAPIKey("test-api-key"))

	if got := s.Categories(); len(got) != 1 || got[0] != defaultCategory {
		t.Errorf("Categories() = %v, want just [%s] before anything else is added", got, defaultCategory)
	}

	s.AddCategory("movies")
	s.categories.add("tv-sonarr", "/downloads/tv") // simulating an *arr app's own createCategory call

	got := s.Categories()
	want := []string{defaultCategory, "movies", "tv-sonarr"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("Categories() = %v, want %v (sorted)", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("Categories()[%d] = %q, want %q", i, got[i], name)
		}
	}
}
