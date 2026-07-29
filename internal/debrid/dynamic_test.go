package debrid

import (
	"context"
	"errors"
	"testing"
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
