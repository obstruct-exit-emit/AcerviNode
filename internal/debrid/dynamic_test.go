package debrid

import (
	"context"
	"errors"
	"testing"
	"time"
)

var (
	_ TorrentProvider     = (*DynamicTorrentProvider)(nil)
	_ UsenetProvider      = (*DynamicUsenetProvider)(nil)
	_ WebDownloadProvider = (*DynamicWebDownloadProvider)(nil)
)

type stubTorrentProvider struct{ name string }

func (s *stubTorrentProvider) Name() string { return s.name }
func (s *stubTorrentProvider) AddMagnet(context.Context, string, AddOptions) (ProviderDownloadID, error) {
	return "id-1", nil
}
func (s *stubTorrentProvider) AddTorrentFile(context.Context, string, []byte, AddOptions) (ProviderDownloadID, error) {
	return "", nil
}
func (s *stubTorrentProvider) Status(context.Context, ProviderDownloadID) (DownloadStatus, error) {
	return DownloadStatus{State: StateDownloading}, nil
}
func (s *stubTorrentProvider) List(context.Context) ([]DownloadStatus, error) { return nil, nil }
func (s *stubTorrentProvider) Files(context.Context, ProviderDownloadID) ([]DownloadFile, error) {
	return nil, nil
}
func (s *stubTorrentProvider) RequestDownloadLink(context.Context, ProviderDownloadID, string) (string, error) {
	return "https://example.test/file", nil
}
func (s *stubTorrentProvider) RequestZipDownloadLink(context.Context, ProviderDownloadID) (string, error) {
	return "https://example.test/all.zip", nil
}
func (s *stubTorrentProvider) Delete(context.Context, ProviderDownloadID, bool) error { return nil }
func (s *stubTorrentProvider) CheckCached(context.Context, []string) (map[string]bool, error) {
	return nil, nil
}

func TestDynamicTorrentProvider_ErrorsBeforeSet(t *testing.T) {
	d := NewDynamicTorrentProvider("torbox")
	if d.Configured() {
		t.Error("Configured() = true before Set(), want false")
	}
	if d.Name() != "torbox" {
		t.Errorf("Name() = %q, want torbox even before Set()", d.Name())
	}
	if _, err := d.AddMagnet(context.Background(), "magnet:?xt=x", AddOptions{}); !errors.Is(err, ErrNoProvider) {
		t.Errorf("AddMagnet() error = %v, want ErrNoProvider", err)
	}
	if _, err := d.Status(context.Background(), "x"); !errors.Is(err, ErrNoProvider) {
		t.Errorf("Status() error = %v, want ErrNoProvider", err)
	}
}

func TestDynamicTorrentProvider_DelegatesAfterSet(t *testing.T) {
	d := NewDynamicTorrentProvider("torbox")
	d.Set(&stubTorrentProvider{name: "torbox"})

	if !d.Configured() {
		t.Error("Configured() = false after Set(), want true")
	}
	id, err := d.AddMagnet(context.Background(), "magnet:?xt=x", AddOptions{})
	if err != nil {
		t.Fatalf("AddMagnet() error = %v", err)
	}
	if id != "id-1" {
		t.Errorf("id = %q, want id-1", id)
	}

	status, err := d.Status(context.Background(), "id-1")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.State != StateDownloading {
		t.Errorf("state = %q, want downloading", status.State)
	}
}

func TestDynamicTorrentProvider_SwapReplacesProvider(t *testing.T) {
	d := NewDynamicTorrentProvider("torbox")
	d.Set(&stubTorrentProvider{name: "torbox-old-key"})
	d.Set(&stubTorrentProvider{name: "torbox-new-key"})

	// Name() reports the fixed slot name, not the delegate's — confirms the
	// wrapper's own identity survives a swap.
	if d.Name() != "torbox" {
		t.Errorf("Name() = %q, want torbox (fixed, not delegate's)", d.Name())
	}
}

type stubUsenetProvider struct{}

func (s *stubUsenetProvider) Name() string { return "torbox" }
func (s *stubUsenetProvider) AddNZBFile(context.Context, string, []byte, AddOptions) (ProviderDownloadID, error) {
	return "nzb-1", nil
}
func (s *stubUsenetProvider) AddNZBURL(context.Context, string, AddOptions) (ProviderDownloadID, error) {
	return "nzb-2", nil
}
func (s *stubUsenetProvider) Status(context.Context, ProviderDownloadID) (DownloadStatus, error) {
	return DownloadStatus{State: StateCompleted}, nil
}
func (s *stubUsenetProvider) List(context.Context) ([]DownloadStatus, error) { return nil, nil }
func (s *stubUsenetProvider) Files(context.Context, ProviderDownloadID) ([]DownloadFile, error) {
	return nil, nil
}
func (s *stubUsenetProvider) RequestDownloadLink(context.Context, ProviderDownloadID, string) (string, error) {
	return "https://example.test/nzb", nil
}
func (s *stubUsenetProvider) RequestZipDownloadLink(context.Context, ProviderDownloadID) (string, error) {
	return "https://example.test/all.zip", nil
}
func (s *stubUsenetProvider) Delete(context.Context, ProviderDownloadID, bool) error { return nil }
func (s *stubUsenetProvider) CheckCached(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{"stub-usenet-hash": true}, nil
}

func TestDynamicUsenetProvider_ErrorsBeforeSetAndDelegatesAfter(t *testing.T) {
	d := NewDynamicUsenetProvider("torbox")
	if _, err := d.AddNZBURL(context.Background(), "https://example.test/x.nzb", AddOptions{}); !errors.Is(err, ErrNoProvider) {
		t.Errorf("AddNZBURL() error = %v, want ErrNoProvider", err)
	}

	d.Set(&stubUsenetProvider{})
	if !d.Configured() {
		t.Error("Configured() = false after Set(), want true")
	}
	id, err := d.AddNZBURL(context.Background(), "https://example.test/x.nzb", AddOptions{})
	if err != nil {
		t.Fatalf("AddNZBURL() error = %v", err)
	}
	if id != "nzb-2" {
		t.Errorf("id = %q, want nzb-2", id)
	}

	result, err := d.CheckCached(context.Background(), []string{"stub-usenet-hash"})
	if err != nil {
		t.Fatalf("CheckCached() error = %v", err)
	}
	if !result["stub-usenet-hash"] {
		t.Errorf("CheckCached() result = %+v, want delegated to the inner provider", result)
	}
}

type stubWebDownloadProvider struct{}

func (s *stubWebDownloadProvider) Name() string { return "torbox" }
func (s *stubWebDownloadProvider) AddLink(context.Context, string, AddOptions) (ProviderDownloadID, error) {
	return "web-1", nil
}
func (s *stubWebDownloadProvider) Status(context.Context, ProviderDownloadID) (DownloadStatus, error) {
	return DownloadStatus{State: StateCompleted}, nil
}
func (s *stubWebDownloadProvider) List(context.Context) ([]DownloadStatus, error) { return nil, nil }
func (s *stubWebDownloadProvider) Files(context.Context, ProviderDownloadID) ([]DownloadFile, error) {
	return nil, nil
}
func (s *stubWebDownloadProvider) RequestDownloadLink(context.Context, ProviderDownloadID, string) (string, error) {
	return "https://example.test/webdl", nil
}
func (s *stubWebDownloadProvider) RequestZipDownloadLink(context.Context, ProviderDownloadID) (string, error) {
	return "https://example.test/webdl.zip", nil
}
func (s *stubWebDownloadProvider) Delete(context.Context, ProviderDownloadID, bool) error { return nil }
func (s *stubWebDownloadProvider) CheckCached(context.Context, []string) (map[string]bool, error) {
	return map[string]bool{"stub-webdl-hash": true}, nil
}

func TestDynamicWebDownloadProvider_ErrorsBeforeSetAndDelegatesAfter(t *testing.T) {
	d := NewDynamicWebDownloadProvider("torbox")
	if d.Configured() {
		t.Error("Configured() = true before Set(), want false")
	}
	if _, err := d.AddLink(context.Background(), "https://example.test/file", AddOptions{}); !errors.Is(err, ErrNoProvider) {
		t.Errorf("AddLink() error = %v, want ErrNoProvider", err)
	}

	d.Set(&stubWebDownloadProvider{})
	if !d.Configured() {
		t.Error("Configured() = false after Set(), want true")
	}
	id, err := d.AddLink(context.Background(), "https://example.test/file", AddOptions{})
	if err != nil {
		t.Fatalf("AddLink() error = %v", err)
	}
	if id != "web-1" {
		t.Errorf("id = %q, want web-1", id)
	}

	status, err := d.Status(context.Background(), "web-1")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.State != StateCompleted {
		t.Errorf("state = %q, want completed", status.State)
	}

	result, err := d.CheckCached(context.Background(), []string{"stub-webdl-hash"})
	if err != nil {
		t.Fatalf("CheckCached() error = %v", err)
	}
	if !result["stub-webdl-hash"] {
		t.Errorf("CheckCached() result = %+v, want delegated to the inner provider", result)
	}
}

// stubAccountingTorrentProvider is a stubTorrentProvider that also implements
// AccountProvider, used to prove DynamicTorrentProvider.Account delegates via
// type assertion when the inner provider supports it.
type stubAccountingTorrentProvider struct {
	stubTorrentProvider
}

func (s *stubAccountingTorrentProvider) Account(context.Context) (AccountStatus, error) {
	return AccountStatus{PlanName: "Pro", IsSubscribed: true, TotalBytesDownloaded: 1024}, nil
}

func TestDynamicTorrentProvider_Account(t *testing.T) {
	d := NewDynamicTorrentProvider("torbox")

	if _, err := d.Account(context.Background()); !errors.Is(err, ErrNoProvider) {
		t.Errorf("Account() error = %v, want ErrNoProvider before Set()", err)
	}

	d.Set(&stubTorrentProvider{name: "torbox"})
	if _, err := d.Account(context.Background()); err == nil {
		t.Error("Account() error = nil, want an error since the inner provider doesn't implement AccountProvider")
	}

	d.Set(&stubAccountingTorrentProvider{stubTorrentProvider{name: "torbox"}})
	status, err := d.Account(context.Background())
	if err != nil {
		t.Fatalf("Account() error = %v", err)
	}
	if status.PlanName != "Pro" || !status.IsSubscribed || status.TotalBytesDownloaded != 1024 {
		t.Errorf("status = %+v", status)
	}
}

// stubTorrentInfoProvider is a stubTorrentProvider that also implements
// TorrentInfoProvider, used to prove DynamicTorrentProvider.TorrentInfo
// delegates via type assertion when the inner provider supports it — same
// pattern as stubAccountingTorrentProvider/TestDynamicTorrentProvider_Account
// above.
type stubTorrentInfoProvider struct {
	stubTorrentProvider
}

func (s *stubTorrentInfoProvider) TorrentInfo(context.Context, string) (TorrentInfo, error) {
	return TorrentInfo{Name: "Preview.Me", SizeBytes: 999}, nil
}

func TestDynamicTorrentProvider_TorrentInfo(t *testing.T) {
	d := NewDynamicTorrentProvider("torbox")

	if _, err := d.TorrentInfo(context.Background(), "hash"); !errors.Is(err, ErrNoProvider) {
		t.Errorf("TorrentInfo() error = %v, want ErrNoProvider before Set()", err)
	}

	d.Set(&stubTorrentProvider{name: "torbox"})
	if _, err := d.TorrentInfo(context.Background(), "hash"); err == nil {
		t.Error("TorrentInfo() error = nil, want an error since the inner provider doesn't implement TorrentInfoProvider")
	}

	d.Set(&stubTorrentInfoProvider{stubTorrentProvider{name: "torbox"}})
	info, err := d.TorrentInfo(context.Background(), "hash")
	if err != nil {
		t.Fatalf("TorrentInfo() error = %v", err)
	}
	if info.Name != "Preview.Me" || info.SizeBytes != 999 {
		t.Errorf("info = %+v", info)
	}
}

// TestDynamicProviders_EveryCallErrorsCleanlyBeforeSet walks every method on
// all three wrappers with no inner provider set.
//
// This is the seam the whole application routes through, and "no provider
// configured yet" is a completely ordinary state — it is what a fresh
// install is before anyone pastes a key, and what an entry becomes when its
// key is cleared. Every one of these has to answer ErrNoProvider rather than
// dereference a nil provider, because a panic here takes down the process
// serving both compat shims and the web UI.
//
// Written after coverage showed most of these delegations had no test at all
// — the two that did (AddMagnet, Status) were the two that happened to get
// one, not the two that mattered most.
func TestDynamicProviders_EveryCallErrorsCleanlyBeforeSet(t *testing.T) {
	ctx := context.Background()

	check := func(t *testing.T, name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrNoProvider) {
			t.Errorf("%s error = %v, want ErrNoProvider", name, err)
		}
	}

	t.Run("torrent", func(t *testing.T) {
		d := NewDynamicTorrentProvider("p")
		_, err := d.AddMagnet(ctx, "magnet:?xt=x", AddOptions{})
		check(t, "AddMagnet", err)
		_, err = d.AddTorrentFile(ctx, "f.torrent", []byte("x"), AddOptions{})
		check(t, "AddTorrentFile", err)
		_, err = d.Status(ctx, "id")
		check(t, "Status", err)
		_, err = d.List(ctx)
		check(t, "List", err)
		_, _, err = d.ListCached(ctx)
		check(t, "ListCached", err)
		_, _, err = d.ListFresh(ctx)
		check(t, "ListFresh", err)
		_, err = d.Files(ctx, "id")
		check(t, "Files", err)
		_, err = d.RequestDownloadLink(ctx, "id", "0")
		check(t, "RequestDownloadLink", err)
		_, err = d.RequestZipDownloadLink(ctx, "id")
		check(t, "RequestZipDownloadLink", err)
		check(t, "Delete", d.Delete(ctx, "id", true))
		_, err = d.CheckCached(ctx, []string{"abc"})
		check(t, "CheckCached", err)
		_, err = d.Account(ctx)
		check(t, "Account", err)
		_, err = d.TorrentInfo(ctx, "abc")
		check(t, "TorrentInfo", err)
		// Must not panic on an unconfigured wrapper either.
		d.SetListCacheTTL(time.Second)
	})

	t.Run("usenet", func(t *testing.T) {
		d := NewDynamicUsenetProvider("p")
		_, err := d.AddNZBFile(ctx, "f.nzb", []byte("x"), AddOptions{})
		check(t, "AddNZBFile", err)
		_, err = d.AddNZBURL(ctx, "http://x/f.nzb", AddOptions{})
		check(t, "AddNZBURL", err)
		_, err = d.Status(ctx, "id")
		check(t, "Status", err)
		_, err = d.List(ctx)
		check(t, "List", err)
		_, _, err = d.ListCached(ctx)
		check(t, "ListCached", err)
		_, _, err = d.ListFresh(ctx)
		check(t, "ListFresh", err)
		_, err = d.Files(ctx, "id")
		check(t, "Files", err)
		_, err = d.RequestDownloadLink(ctx, "id", "0")
		check(t, "RequestDownloadLink", err)
		_, err = d.RequestZipDownloadLink(ctx, "id")
		check(t, "RequestZipDownloadLink", err)
		check(t, "Delete", d.Delete(ctx, "id", true))
		_, err = d.CheckCached(ctx, []string{"abc"})
		check(t, "CheckCached", err)
		_, err = d.Account(ctx)
		check(t, "Account", err)
		d.SetListCacheTTL(time.Second)
	})

	t.Run("webdl", func(t *testing.T) {
		d := NewDynamicWebDownloadProvider("p")
		_, err := d.AddLink(ctx, "https://host/f", AddOptions{})
		check(t, "AddLink", err)
		_, err = d.Status(ctx, "id")
		check(t, "Status", err)
		_, err = d.List(ctx)
		check(t, "List", err)
		_, _, err = d.ListCached(ctx)
		check(t, "ListCached", err)
		_, _, err = d.ListFresh(ctx)
		check(t, "ListFresh", err)
		_, err = d.Files(ctx, "id")
		check(t, "Files", err)
		_, err = d.RequestDownloadLink(ctx, "id", "0")
		check(t, "RequestDownloadLink", err)
		_, err = d.RequestZipDownloadLink(ctx, "id")
		check(t, "RequestZipDownloadLink", err)
		check(t, "Delete", d.Delete(ctx, "id", true))
		_, err = d.CheckCached(ctx, []string{"abc"})
		check(t, "CheckCached", err)
		_, err = d.Account(ctx)
		check(t, "Account", err)
		d.SetListCacheTTL(time.Second)
	})
}

// TestDynamicProviders_ClearingAKeyReturnsToErrNoProvider proves Set(nil)
// genuinely unconfigures a wrapper rather than leaving the previous provider
// in place. This is what an empty api_key does through the settings API, and
// silently keeping the old credentials would be the worst possible reading
// of "clear this provider".
func TestDynamicProviders_ClearingAKeyReturnsToErrNoProvider(t *testing.T) {
	d := NewDynamicTorrentProvider("p")
	d.Set(&stubTorrentProvider{name: "p"})
	if !d.Configured() {
		t.Fatal("Configured() = false after Set(), want true")
	}

	d.Set(nil)
	if d.Configured() {
		t.Error("Configured() = true after Set(nil), want false")
	}
	if _, err := d.List(context.Background()); !errors.Is(err, ErrNoProvider) {
		t.Errorf("List() error = %v after Set(nil), want ErrNoProvider", err)
	}
}
