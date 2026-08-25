package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// twoWebDLServer registers two web-download providers so a link one refuses
// can be tried against the other.
func twoWebDLServer(t *testing.T, first, second *fakeProvider) (*Server, *database.DB) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	registry := debrid.NewRegistry()
	for _, p := range []*fakeProvider{first, second} {
		wd := debrid.NewDynamicWebDownloadProvider(p.providerName)
		wd.Set(p)
		registry.Register(p.providerName, nil, nil, wd)
	}
	registry.SetDefault(first.providerName)
	return NewServer("dev", db, registry, &fakeSettings{}), db
}

func postWebDL(t *testing.T, srv *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/downloads/webdl", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestAddWebDownload_FallsBackWhenTheHostIsUnsupported proves a link the
// routed provider can't handle is tried against another configured one.
//
// Which hosts a service covers varies a lot, and on AllDebrid varies by plan
// — a trial covers five against TorBox's ~160 — so this is an ordinary
// situation with two providers, not an edge case. Before, the add simply
// failed while a capable provider sat unused beside it.
func TestAddWebDownload_FallsBackWhenTheHostIsUnsupported(t *testing.T) {
	refuses := &fakeProvider{providerName: "alldebrid", addErr: fmt.Errorf("no: %w", debrid.ErrHostNotSupported)}
	accepts := &fakeProvider{providerName: "torbox", addID: "wd-1"}
	srv, _ := twoWebDLServer(t, refuses, accepts)

	rec := postWebDL(t, srv, url.Values{"link": {"https://mediafire.com/file/x"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["provider"] != "torbox" {
		t.Errorf("provider = %v, want torbox — the add should have moved to the provider that takes this host", got["provider"])
	}
	if accepts.addedLink != "https://mediafire.com/file/x" {
		t.Error("the fallback provider never saw the link")
	}
}

// TestAddWebDownload_ExplicitProviderIsNeverRedirected is the guard that
// makes the fallback safe. Naming a provider is a request for that account;
// silently using another would put the download somewhere the caller didn't
// choose, which is the same reason readd refuses to migrate a download.
func TestAddWebDownload_ExplicitProviderIsNeverRedirected(t *testing.T) {
	refuses := &fakeProvider{providerName: "alldebrid", addErr: fmt.Errorf("no: %w", debrid.ErrHostNotSupported)}
	accepts := &fakeProvider{providerName: "torbox", addID: "wd-1"}
	srv, _ := twoWebDLServer(t, refuses, accepts)

	rec := postWebDL(t, srv, url.Values{
		"link":     {"https://mediafire.com/file/x"},
		"provider": {"alldebrid"},
	})
	if rec.Code == http.StatusCreated {
		t.Fatalf("status = 201, want a failure — an explicitly named provider must not be swapped out")
	}
	if accepts.addedLink != "" {
		t.Error("the add was redirected to another provider despite naming one explicitly")
	}
}

// TestAddWebDownload_OtherErrorsAreNotRetried keeps the fallback narrow. Any
// failure other than an unsupported host might mean the add partly landed,
// and re-sending it elsewhere would risk a second copy of the download.
func TestAddWebDownload_OtherErrorsAreNotRetried(t *testing.T) {
	refuses := &fakeProvider{providerName: "alldebrid", addErr: errors.New("upstream exploded")}
	accepts := &fakeProvider{providerName: "torbox", addID: "wd-1"}
	srv, _ := twoWebDLServer(t, refuses, accepts)

	rec := postWebDL(t, srv, url.Values{"link": {"https://mediafire.com/file/x"}})
	if rec.Code == http.StatusCreated {
		t.Fatalf("status = 201, want a failure — a non-host error must not be retried elsewhere")
	}
	if accepts.addedLink != "" {
		t.Error("a generic provider failure was retried on another provider; that risks a duplicate download")
	}
}

// TestAddWebDownload_NobodySupportsTheHost reports the first provider's
// explanation rather than the last one's, since they all say the same thing
// and the routed provider is the one the caller was implicitly using.
func TestAddWebDownload_NobodySupportsTheHost(t *testing.T) {
	first := &fakeProvider{providerName: "alldebrid", addErr: fmt.Errorf("alldebrid says no: %w", debrid.ErrHostNotSupported)}
	second := &fakeProvider{providerName: "torbox", addErr: fmt.Errorf("torbox says no: %w", debrid.ErrHostNotSupported)}
	srv, _ := twoWebDLServer(t, first, second)

	rec := postWebDL(t, srv, url.Values{"link": {"https://nowhere.example/file/x"}})
	if rec.Code == http.StatusCreated {
		t.Fatalf("status = 201, want a failure when no provider handles the host")
	}
	if body := rec.Body.String(); !strings.Contains(body, "alldebrid says no") {
		t.Errorf("body = %q, want the routed provider's own explanation", body)
	}
}
