package sabnzbd

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

	s := NewServer(newFakeProvider(), db, staticAPIKey(testAPIKey))

	// "*" is a protocol requirement (see categoryStore's doc comment) that
	// Categories() deliberately filters back out — it's not something
	// anyone declares or manages, so the settings API shouldn't surface it.
	if got := s.Categories(); len(got) != 1 || got[0] != defaultCategory {
		t.Errorf("Categories() = %v, want just [%s] before anything else is added", got, defaultCategory)
	}
	if raw := s.categories.list(); len(raw) != 2 {
		t.Errorf("internal categoryStore.list() = %v, want 2 (\"*\" plus the default, for the real SABnzbd protocol surface)", raw)
	}

	s.AddCategory("movies")
	s.categories.add("tv-sonarr") // simulating an *arr app's own implicit category declaration

	got := s.Categories()
	want := []string{defaultCategory, "movies", "tv-sonarr"}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("Categories() = %v, want %v (sorted, \"*\" excluded)", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("Categories()[%d] = %q, want %q", i, got[i], name)
		}
	}
}
