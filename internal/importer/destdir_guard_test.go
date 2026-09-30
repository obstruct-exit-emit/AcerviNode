package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
)

// TestRemoveLocalFiles_RefusesPathNotEndingInTheDownloadsName is the safety net
// under os.RemoveAll.
//
// resolveDestDir's contract, stated in its own doc comment and relied on by
// three others, is that the destination always ends with the download's own
// name — that is what makes removing it safe. The *arr-supplied save_path
// branch broke that silently, and the only existing guard (an empty Name)
// does not catch it: the row has a perfectly good name, the path just is not
// namespaced by it, so RemoveAll took the shared directory and every sibling
// download in it with it.
//
// Enforcing the invariant at the dangerous operation covers rows written before
// the add path was fixed, which no amount of care at insert time can reach. A
// skipped removal orphans one download's files; the alternative destroys
// everyone else's.
func TestRemoveLocalFiles_RefusesPathNotEndingInTheDownloadsName(t *testing.T) {
	shared := t.TempDir()

	// Two downloads that were both handed the same explicit save_path, the way
	// a client sending savepath= produced before namespacing.
	sibling := filepath.Join(shared, "sibling-file.mkv")
	if err := os.WriteFile(sibling, []byte("another download's data"), 0o644); err != nil {
		t.Fatalf("seed sibling file: %v", err)
	}

	im := &Importer{}
	d := &database.Download{
		ID: "dl-1", Name: "Some.Release", Category: "tv-sonarr", SavePath: shared,
	}

	err := im.RemoveLocalFiles(d)
	if err == nil {
		t.Error("RemoveLocalFiles() = nil, want a refusal: the path does not end in the download's own name")
	}
	if _, statErr := os.Stat(sibling); os.IsNotExist(statErr) {
		t.Fatal("RemoveLocalFiles() deleted a sibling download's files from a shared directory")
	}
	if _, statErr := os.Stat(shared); os.IsNotExist(statErr) {
		t.Fatal("RemoveLocalFiles() deleted the shared directory itself")
	}
}

// TestRemoveLocalFiles_StillRemovesAProperlyNamespacedDirectory is the other
// half: the guard must not break the normal case it is protecting.
func TestRemoveLocalFiles_StillRemovesAProperlyNamespacedDirectory(t *testing.T) {
	base := t.TempDir()
	dest := filepath.Join(base, "Some.Release")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	inside := filepath.Join(dest, "episode.mkv")
	if err := os.WriteFile(inside, []byte("this download's data"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	im := &Importer{}
	d := &database.Download{
		ID: "dl-1", Name: "Some.Release", Category: "tv-sonarr", SavePath: dest,
	}

	if err := im.RemoveLocalFiles(d); err != nil {
		t.Fatalf("RemoveLocalFiles() error = %v, want the namespaced directory removed", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("destination still present after RemoveLocalFiles(); stat err = %v", err)
	}
	// The parent it sat in is emphatically not ours to remove.
	if _, err := os.Stat(base); err != nil {
		t.Errorf("RemoveLocalFiles() removed the parent directory too: %v", err)
	}
}

// TestRemoveLocalFiles_StillRefusesAnEmptyName pins the pre-existing guard so
// the new one does not quietly replace it.
func TestRemoveLocalFiles_StillRefusesAnEmptyName(t *testing.T) {
	im := &Importer{}
	if err := im.RemoveLocalFiles(&database.Download{ID: "dl-1", SavePath: t.TempDir()}); err == nil {
		t.Error("RemoveLocalFiles() with no Name = nil, want a refusal")
	}
}
