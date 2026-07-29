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
	if got := s.Categories(); len(got) != 0 {
		t.Errorf("Categories() = %v, want empty before anything is added", got)
	}
	if raw := s.categories.list(); len(raw) != 1 {
		t.Errorf("internal categoryStore.list() = %v, want 1 (just \"*\", for the real SABnzbd protocol surface)", raw)
	}

	s.AddCategory("movies")
	s.categories.add("tv-sonarr") // simulating an *arr app's own implicit category declaration

	got := s.Categories()
	want := []string{"movies", "tv-sonarr"}
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
