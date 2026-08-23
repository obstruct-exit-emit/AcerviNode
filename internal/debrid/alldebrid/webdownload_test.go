package alldebrid

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acervinode/acervinode/internal/debrid"
)

// newTestWebDL serves handler as the AllDebrid API.
func newTestWebDL(t *testing.T, handler http.HandlerFunc) *WebDownloadProvider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewWebDownloadProvider("alldebrid", "test-key", WithBaseURL(srv.URL))
}

// TestAddLink_ReturnsTheStoredLinkNotTheSubmittedOne is the regression for
// AllDebrid rewriting links on save: a Mega link submitted as
// mega.nz/file/ID#KEY comes back from /v4/user/links as
// mega.co.nz/#!ID!KEY. Returning the submitted link as the id would mean no
// later listing ever matched it, so every web download would look like it
// had vanished from the account moments after being added.
func TestAddLink_ReturnsTheStoredLinkNotTheSubmittedOne(t *testing.T) {
	const submitted = "https://mega.nz/file/axBTkJjY#4xZrOpjWHPsrurX6aq"
	const stored = "https://mega.co.nz/#!axBTkJjY!4xZrOpjWHPsrurX6aq"

	p := newTestWebDL(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v4/link/unlock":
			json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
				"link": "https://dl.example/direct", "filename": "Mega test.txt", "filesize": 7, "host": "mega",
			}})
		case "/v4/user/links/save":
			json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"message": "1 links saved"}})
		case "/v4/user/links":
			json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
				"links": []map[string]any{{"link": stored, "filename": "Mega test.txt", "size": 7, "host": "mega", "date": 100}},
			}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	id, err := p.AddLink(context.Background(), submitted, debrid.AddOptions{})
	if err != nil {
		t.Fatalf("AddLink() error = %v", err)
	}
	if string(id) != stored {
		t.Errorf("id = %q, want the stored form %q — the submitted link never matches a later listing", id, stored)
	}
}

// An unsupported host must fail at the add, not be saved as a link that can
// never resolve.
func TestAddLink_UnsupportedHostFailsBeforeSaving(t *testing.T) {
	saved := false
	p := newTestWebDL(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v4/user/links/save" {
			saved = true
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": "error",
			"error":  map[string]any{"code": "LINK_HOST_NOT_SUPPORTED", "message": "This host or link is not supported"},
		})
	})

	if _, err := p.AddLink(context.Background(), "https://nope.example/x", debrid.AddOptions{}); err == nil {
		t.Fatal("AddLink() error = nil, want the unsupported host reported")
	}
	if saved {
		t.Error("an unsupported link was saved to the account anyway")
	}
}

// A saved link is already resolved, so it reports complete — there is no
// in-progress state for one to be in.
func TestList_SavedLinksAreComplete(t *testing.T) {
	p := newTestWebDL(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
			"links": []map[string]any{{"link": "https://mega.co.nz/#!a!b", "filename": "f.txt", "size": 7, "host": "mega", "date": 1}},
		}})
	})

	got, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List() = %d entries, want 1", len(got))
	}
	if got[0].State != debrid.StateCompleted || got[0].Progress != 1 {
		t.Errorf("state = %v progress = %v, want completed/1", got[0].State, got[0].Progress)
	}
	if string(got[0].ID) != "https://mega.co.nz/#!a!b" {
		t.Errorf("id = %q, want the stored link", got[0].ID)
	}
}

// RequestDownloadLink unlocks every time rather than reusing a stored URL:
// AllDebrid's direct links are short-lived, so a cached one would work now
// and quietly fail later.
func TestRequestDownloadLink_UnlocksEachTime(t *testing.T) {
	unlocks := 0
	p := newTestWebDL(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v4/link/unlock" {
			unlocks++
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
			"link": "https://dl.example/fresh", "filename": "f.txt", "filesize": 7,
		}})
	})

	for i := 0; i < 2; i++ {
		url, err := p.RequestDownloadLink(context.Background(), "https://mega.co.nz/#!a!b", "")
		if err != nil {
			t.Fatalf("RequestDownloadLink() error = %v", err)
		}
		if url != "https://dl.example/fresh" {
			t.Errorf("url = %q, want the unlocked direct link", url)
		}
	}
	if unlocks != 2 {
		t.Errorf("unlock calls = %d, want 2 — links must not be cached", unlocks)
	}
}

// AllDebrid has no usenet service, so nothing here should ever claim to be
// a usenet provider. Guards against a future edit wiring one up by accident.
func TestWebDownloadProvider_ImplementsOnlyWebDL(t *testing.T) {
	var p any = NewWebDownloadProvider("alldebrid", "k")
	if _, isUsenet := p.(debrid.UsenetProvider); isUsenet {
		t.Error("the web-download provider also satisfies UsenetProvider — AllDebrid has no usenet service")
	}
}
