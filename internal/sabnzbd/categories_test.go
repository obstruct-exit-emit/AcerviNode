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

	s := NewServer(testRegistry(newFakeProvider()), db, staticAPIKey(testAPIKey))

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

func TestServer_RemoveCategory(t *testing.T) {
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s := NewServer(testRegistry(newFakeProvider()), db, staticAPIKey(testAPIKey))
	s.AddCategory("movies")
	s.AddCategory("tv-sonarr")

	s.RemoveCategory("movies")

	got := s.Categories()
	if len(got) != 1 || got[0] != "tv-sonarr" {
		t.Errorf("Categories() after RemoveCategory(movies) = %v, want [tv-sonarr]", got)
	}

	// "*" is a protocol requirement, never removable — see categoryStore's
	// doc comment.
	s.RemoveCategory("*")
	if raw := s.categories.list(); len(raw) != 2 { // "*" plus tv-sonarr
		t.Errorf("internal categoryStore.list() after RemoveCategory(*) = %v, want \"*\" to survive", raw)
	}
}
