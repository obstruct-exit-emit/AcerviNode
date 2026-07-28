package sabnzbd

import (
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

	// The "*" default category always exists, matching a real SABnzbd install.
	if got := s.Categories(); len(got) != 1 || got[0] != "*" {
		t.Errorf("Categories() = %v, want just the default \"*\" before anything is added", got)
	}

	s.AddCategory("movies")
	s.categories.add("tv-sonarr") // simulating an *arr app's own implicit category declaration

	got := s.Categories()
	want := []string{"*", "movies", "tv-sonarr"}
	if len(got) != len(want) {
		t.Fatalf("Categories() = %v, want %v (sorted)", got, want)
	}
	for i, name := range want {
		if got[i] != name {
			t.Errorf("Categories()[%d] = %q, want %q", i, got[i], name)
		}
	}
}
