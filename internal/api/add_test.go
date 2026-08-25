package api

import (
	"context"
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
func twoWebDLServer(t *testing.T, providers ...*fakeProvider) (*Server, *database.DB) {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	registry := debrid.NewRegistry()
	for _, p := range providers {
		wd := debrid.NewDynamicWebDownloadProvider(p.providerName)
		// A nil fake stands for a registered-but-unconfigured provider,
		// which the fallback must skip rather than try.
		if !p.unconfigured {
			wd.Set(p)
		}
		registry.Register(p.providerName, nil, nil, wd)
	}
	registry.SetDefault(providers[0].providerName)
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

// TestAddWebDownload_FallbackRecordsTheProviderThatAccepted is the one that
// matters most. Identity here is the (provider, provider_download_id) pair,
// and every later status poll, link request and delete routes back through
// it — so a row that names the routed provider while the download actually
// lives on another would be polled against an account that has never heard
// of it, then eventually flagged "no longer found in the provider's
// account". Exactly the cross-provider failure the importer already had.
func TestAddWebDownload_FallbackRecordsTheProviderThatAccepted(t *testing.T) {
	refuses := &fakeProvider{providerName: "alldebrid", addErr: fmt.Errorf("no: %w", debrid.ErrHostNotSupported)}
	accepts := &fakeProvider{providerName: "torbox", addID: "wd-42"}
	srv, db := twoWebDLServer(t, refuses, accepts)

	rec := postWebDL(t, srv, url.Values{"link": {"https://mediafire.com/file/x"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	rows, err := db.ListDownloads(context.Background(), database.KindWebDL)
	if err != nil {
		t.Fatalf("ListDownloads() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Provider != "torbox" {
		t.Errorf("row Provider = %q, want torbox — the row must name whoever actually took it", rows[0].Provider)
	}
	if rows[0].ProviderDownloadID != "wd-42" {
		t.Errorf("row ProviderDownloadID = %q, want the id the accepting provider returned", rows[0].ProviderDownloadID)
	}
}

// TestAddWebDownload_FallbackSkipsUnconfiguredProviders proves a registered
// but keyless provider isn't tried. It would fail with ErrNoProvider, which
// isn't a host error, so treating it as a candidate would abort the search
// and report "no provider configured" instead of trying the one that works.
func TestAddWebDownload_FallbackSkipsUnconfiguredProviders(t *testing.T) {
	refuses := &fakeProvider{providerName: "alldebrid", addErr: fmt.Errorf("no: %w", debrid.ErrHostNotSupported)}
	keyless := &fakeProvider{providerName: "premiumize", unconfigured: true}
	accepts := &fakeProvider{providerName: "torbox", addID: "wd-7"}
	srv, _ := twoWebDLServer(t, refuses, keyless, accepts)

	rec := postWebDL(t, srv, url.Values{"link": {"https://mediafire.com/file/x"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — an unconfigured provider should be skipped, not abort the search: %s", rec.Code, rec.Body.String())
	}
	if accepts.addedLink == "" {
		t.Error("the configured fallback provider was never reached")
	}
}

// TestAddWebDownload_FallbackWalksPastSeveralRefusals proves the search
// continues rather than giving up after one alternative.
func TestAddWebDownload_FallbackWalksPastSeveralRefusals(t *testing.T) {
	a := &fakeProvider{providerName: "a", addErr: fmt.Errorf("no: %w", debrid.ErrHostNotSupported)}
	b := &fakeProvider{providerName: "b", addErr: fmt.Errorf("no: %w", debrid.ErrHostNotSupported)}
	c := &fakeProvider{providerName: "c", addID: "wd-3"}
	srv, _ := twoWebDLServer(t, a, b, c)

	rec := postWebDL(t, srv, url.Values{"link": {"https://host/x"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if b.addedLink == "" || c.addedLink == "" {
		t.Error("the search stopped early instead of walking every candidate")
	}
}

// TestAddWebDownload_SingleProviderDoesNotLoop guards the degenerate case:
// with nothing to fall back to, the original refusal is reported once and
// the provider is not retried.
func TestAddWebDownload_SingleProviderDoesNotLoop(t *testing.T) {
	only := &fakeProvider{providerName: "alldebrid", addErr: fmt.Errorf("alldebrid says no: %w", debrid.ErrHostNotSupported)}
	srv, _ := twoWebDLServer(t, only)

	rec := postWebDL(t, srv, url.Values{"link": {"https://mediafire.com/file/x"}})
	if rec.Code == http.StatusCreated {
		t.Fatalf("status = 201, want a failure with only an unsupported provider configured")
	}
	if body := rec.Body.String(); !strings.Contains(body, "alldebrid says no") {
		t.Errorf("body = %q, want the provider's own explanation", body)
	}
}

// TestTorrentInfo_FallsBackToAProviderThatSupportsIt proves the metadata
// preview isn't hostage to which provider happens to be default.
//
// A magnet's name, size and file list are properties of the torrent, not of
// an account, so any provider offering the lookup answers the same. Asking
// only the default meant the "+ Add" preview stopped working whenever the
// default was a provider without the feature — AllDebrid has no equivalent
// of TorBox's torrentinfo — with a capable provider configured beside it.
// Observed live: identical request, different answer, purely from which
// provider was default.
func TestTorrentInfo_FallsBackToAProviderThatSupportsIt(t *testing.T) {
	noPreview := &fakeProvider{providerName: "alldebrid", torrentInfoErr: debrid.ErrTorrentInfoUnsupported}
	hasPreview := &fakeProvider{providerName: "torbox", torrentInfoResp: debrid.TorrentInfo{Name: "Some Release", SizeBytes: 42}}

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	registry := debrid.NewRegistry()
	for _, p := range []*fakeProvider{noPreview, hasPreview} {
		td := debrid.NewDynamicTorrentProvider(p.providerName)
		td.Set(p)
		registry.Register(p.providerName, td, nil, nil)
	}
	registry.SetDefault("alldebrid")
	srv := NewServer("dev", db, registry, &fakeSettings{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/downloads/torrent/info?hash=abc", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["available"] != true {
		t.Fatalf("available = %v, want true — the capable provider should have answered: %s", got["available"], rec.Body.String())
	}
	if got["name"] != "Some Release" {
		t.Errorf("name = %v, want the capable provider's answer", got["name"])
	}
}
