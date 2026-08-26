package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
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

// TestTorrentInfo_NeverSilentlyAsksAnotherProvider pins that a metadata
// preview stays with the provider it was aimed at.
//
// It briefly fell back to any provider that supported the lookup, on the
// reasoning that a magnet's metadata belongs to the torrent rather than an
// account. True of the data, but the query still goes somewhere specific —
// so a hash intended for one provider ended up disclosed to another, with
// nothing visible to say so. The add fallbacks are a different trade: they
// prevent a download from failing and surface as a download filed under
// whoever took it.
func TestTorrentInfo_NeverSilentlyAsksAnotherProvider(t *testing.T) {
	noPreview := &fakeProvider{providerName: "alldebrid", torrentInfoErr: debrid.ErrTorrentInfoUnsupported}
	hasPreview := &fakeProvider{providerName: "torbox", torrentInfoResp: debrid.TorrentInfo{Name: "Some Release"}}
	srv := twoTorrentServer(t, noPreview, hasPreview)

	rec := getInfo(t, srv, "/api/v1/downloads/torrent/info?hash=abc")
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)

	if got["available"] != false {
		t.Errorf("available = %v, want false — the default can't preview and no other provider should be asked", got["available"])
	}
	if hasPreview.torrentInfoHash != "" {
		t.Error("the other provider was queried; a hash aimed at one provider must not reach another")
	}
	if msg, _ := got["error"].(string); !strings.Contains(msg, "alldebrid") {
		t.Errorf("error = %q, want it to name the provider that can't preview", msg)
	}
}

func twoTorrentServer(t *testing.T, providers ...*fakeProvider) *Server {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("database.Open() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })
	registry := debrid.NewRegistry()
	for _, p := range providers {
		td := debrid.NewDynamicTorrentProvider(p.providerName)
		td.Set(p)
		registry.Register(p.providerName, td, nil, nil)
	}
	registry.SetDefault(providers[0].providerName)
	return NewServer("dev", db, registry, &fakeSettings{})
}

func getInfo(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestAddEndpoints_AcceptEitherEncoding pins that all three add endpoints
// take multipart and urlencoded alike.
//
// They used to disagree — torrent and usenet accepted only multipart because
// they take file uploads, web downloads only urlencoded because it has none.
// Sending the wrong one got a 400, and for web downloads that 400 read "link
// is required" on a request that carried a link, blaming the caller's data
// for a Content-Type mismatch.
func TestAddEndpoints_AcceptEitherEncoding(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c"

	cases := []struct {
		path, field, value string
	}{
		{"/api/v1/downloads/torrent", "magnet", magnet},
		{"/api/v1/downloads/usenet", "url", "https://example.com/x.nzb"},
		{"/api/v1/downloads/webdl", "link", "https://example.com/f.zip"},
	}

	for _, tc := range cases {
		for _, encoding := range []string{"urlencoded", "multipart"} {
			t.Run(tc.path+"/"+encoding, func(t *testing.T) {
				p := &fakeProvider{addID: "id-1"}
				srv, _ := newTestServerWithProviders(t, p, p, p, &fakeSettings{})

				var req *http.Request
				if encoding == "urlencoded" {
					form := url.Values{tc.field: {tc.value}}
					req = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(form.Encode()))
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				} else {
					var buf bytes.Buffer
					mw := multipart.NewWriter(&buf)
					if err := mw.WriteField(tc.field, tc.value); err != nil {
						t.Fatalf("WriteField() error = %v", err)
					}
					mw.Close()
					req = httptest.NewRequest(http.MethodPost, tc.path, &buf)
					req.Header.Set("Content-Type", mw.FormDataContentType())
				}
				req.Header.Set("Authorization", "Bearer secret")

				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)

				if rec.Code != http.StatusCreated {
					t.Fatalf("status = %d, want 201 — %s should be accepted: %s",
						rec.Code, encoding, rec.Body.String())
				}
				// The field has to have actually been read, not just the
				// body parsed: a silently empty form was exactly the old
				// web-download failure.
				if p.addedMagnet == "" && p.addedURL == "" && p.addedLink == "" {
					t.Errorf("%s body parsed but no field reached the provider", encoding)
				}
			})
		}
	}
}

// TestNormalizeMagnet covers adding a torrent by bare infohash.
//
// A hash on its own is what an indexer or another client will often show
// you, and GET /downloads/torrent/info already previewed one directly — so
// previewing by hash worked while adding the same hash failed, with TorBox
// answering "Invalid Magnet Link". Measured before fixing.
func TestNormalizeMagnet(t *testing.T) {
	const v1 = "08ada5a7a6183aae1e09d831df6748d566095a10"
	v2 := strings.Repeat("a", 64)

	t.Run("wraps a bare infohash", func(t *testing.T) {
		if got := normalizeMagnet(v1); got != "magnet:?xt=urn:btih:"+v1 {
			t.Errorf("normalizeMagnet(v1) = %q", got)
		}
		if got := normalizeMagnet(strings.ToUpper(v1)); got != "magnet:?xt=urn:btih:"+v1 {
			t.Errorf("uppercase hash not lowercased: %q", got)
		}
		if got := normalizeMagnet(v2); got != "magnet:?xt=urn:btih:"+v2 {
			t.Errorf("normalizeMagnet(v2) = %q", got)
		}
	})

	// Anything already a magnet, or plainly not a hash, has to pass through
	// untouched — this runs on every torrent add, including file uploads
	// where the field is empty.
	t.Run("leaves everything else alone", func(t *testing.T) {
		for _, in := range []string{
			"",
			"magnet:?xt=urn:btih:" + v1,
			"https://example.com/x.torrent",
			v1[:39],  // one short
			v1 + "a", // one long
			"08ada5a7a6183aae1e09d831df6748d566095g10", // non-hex
		} {
			if got := normalizeMagnet(in); got != in {
				t.Errorf("normalizeMagnet(%q) = %q, want it unchanged", in, got)
			}
		}
	})
}

// TestAddTorrent_AcceptsBareInfohash proves it end to end through the
// handler, not just the helper.
func TestAddTorrent_AcceptsBareInfohash(t *testing.T) {
	const hash = "08ada5a7a6183aae1e09d831df6748d566095a10"
	p := &fakeProvider{addID: "t-1"}
	srv, _ := newTestServerWithProviders(t, p, nil, nil, &fakeSettings{})

	form := url.Values{"magnet": {hash}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/downloads/torrent", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if p.addedMagnet != "magnet:?xt=urn:btih:"+hash {
		t.Errorf("provider received %q, want a wrapped magnet — a bare hash is rejected by real providers", p.addedMagnet)
	}
}
