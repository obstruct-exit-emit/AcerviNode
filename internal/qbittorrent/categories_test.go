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

	s := NewServer(testRegistry(newFakeProvider()), db, staticAPIKey("test-api-key"))

	if got := s.Categories(); len(got) != 0 {
		t.Errorf("Categories() = %v, want empty before anything is added", got)
	}

	s.AddCategory("movies")
	s.categories.add("tv-sonarr", "/downloads/tv") // simulating an *arr app's own createCategory call

	got := s.Categories()
	want := []string{"movies", "tv-sonarr"}
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

func TestServer_RemoveCategory(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s := NewServer(testRegistry(newFakeProvider()), db, staticAPIKey("test-api-key"))
	s.AddCategory("movies")
	s.AddCategory("tv-sonarr")

	s.RemoveCategory("movies")

	got := s.Categories()
	if len(got) != 1 || got[0] != "tv-sonarr" {
		t.Errorf("Categories() after RemoveCategory(movies) = %v, want [tv-sonarr]", got)
	}

	// Removing something never added, or removing twice, is a routine no-op.
	s.RemoveCategory("movies")
	s.RemoveCategory("never-added")
	if got := s.Categories(); len(got) != 1 || got[0] != "tv-sonarr" {
		t.Errorf("Categories() after redundant removes = %v, want [tv-sonarr]", got)
	}
}
