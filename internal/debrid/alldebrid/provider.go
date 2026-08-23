package alldebrid

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/acervinode/acervinode/internal/debrid"
)

// Provider adapts Client to debrid.TorrentProvider.
type Provider struct {
	client *Client
	name   string
}

// NewProvider builds a torrent provider for apiKey. name is the registry
// entry this provider was configured under, not the service — two accounts
// on AllDebrid are two providers with different names, and each has to
// report its own so a download resolves back to the right one (see
// database.Download.Provider).
func NewProvider(name, apiKey string, opts ...Option) *Provider {
	return &Provider{client: NewClient(apiKey, opts...), name: name}
}

var _ debrid.TorrentProvider = (*Provider)(nil)

func (p *Provider) Name() string { return p.name }

// magnet is one entry from /v4.1/magnet/status.
type magnet struct {
	ID         int64  `json:"id"`
	Filename   string `json:"filename"`
	Size       int64  `json:"size"`
	Hash       string `json:"hash"`
	Status     string `json:"status"`
	StatusCode int    `json:"statusCode"`
	// Downloaded/Seeders/DownloadSpeed are only present while a magnet is
	// actually transferring (statusCode 0-3); a cached magnet arrives
	// already Ready with none of them.
	Downloaded    int64 `json:"downloaded"`
	Seeders       int64 `json:"seeders"`
	DownloadSpeed int64 `json:"downloadSpeed"`
}

// uploadedMagnet is one entry from /v4/magnet/upload, which reports a
// different shape from status: name rather than filename, and no status.
type uploadedMagnet struct {
	ID    int64  `json:"id"`
	Hash  string `json:"hash"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Ready bool   `json:"ready"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// AddMagnet uploads a magnet and returns its AllDebrid id.
func (p *Provider) AddMagnet(ctx context.Context, magnetURI string, _ debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	var out struct {
		Magnets []uploadedMagnet `json:"magnets"`
	}
	form := url.Values{}
	form.Add("magnets[]", magnetURI)
	if err := p.client.do(ctx, http.MethodPost, "/v4/magnet/upload", form, &out); err != nil {
		return "", fmt.Errorf("alldebrid: add magnet: %w", err)
	}
	if len(out.Magnets) == 0 {
		return "", fmt.Errorf("alldebrid: add magnet: no magnet in response")
	}
	// Per-magnet errors ride inside a successful envelope, since the
	// endpoint accepts a batch — an unsuccessful add is not an unsuccessful
	// request. With one magnet in, one error out means the add failed.
	if e := out.Magnets[0].Error; e != nil {
		return "", fmt.Errorf("alldebrid: add magnet: %w", &APIError{Code: e.Code, Message: e.Message})
	}
	return debrid.ProviderDownloadID(strconv.FormatInt(out.Magnets[0].ID, 10)), nil
}

// AddTorrentFile uploads a .torrent file.
//
// AllDebrid's own upload endpoint takes multipart file uploads, but every
// .torrent reduces to a magnet by infohash and AcerviNode already
// reconstructs magnets that way elsewhere (see torbox.magnetFromHash).
// Rather than carry a second upload path that can't be exercised without
// real torrent files, this is deliberately unsupported: internal/api's add
// endpoint surfaces the error, and the magnet path covers the same ground.
func (p *Provider) AddTorrentFile(context.Context, string, []byte, debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	return "", fmt.Errorf("alldebrid: adding a .torrent file is not supported, use a magnet link")
}

// List returns every magnet on the account.
func (p *Provider) List(ctx context.Context) ([]debrid.DownloadStatus, error) {
	var out struct {
		Magnets []magnet `json:"magnets"`
	}
	if err := p.client.do(ctx, http.MethodGet, "/v4.1/magnet/status", nil, &out); err != nil {
		return nil, fmt.Errorf("alldebrid: list: %w", err)
	}
	statuses := make([]debrid.DownloadStatus, 0, len(out.Magnets))
	for _, m := range out.Magnets {
		statuses = append(statuses, magnetToStatus(m))
	}
	return statuses, nil
}

// Status looks up one magnet directly, which is dramatically cheaper than
// listing the whole account for one row — the same reasoning as
// torbox.GetTorrent, and what internal/importer's fast poll relies on.
func (p *Provider) Status(ctx context.Context, id debrid.ProviderDownloadID) (debrid.DownloadStatus, error) {
	var out struct {
		Magnets []magnet `json:"magnets"`
	}
	form := url.Values{}
	form.Set("id", string(id))
	if err := p.client.do(ctx, http.MethodGet, "/v4.1/magnet/status", form, &out); err != nil {
		return debrid.DownloadStatus{}, fmt.Errorf("alldebrid: status: %w", err)
	}
	if len(out.Magnets) == 0 {
		return debrid.DownloadStatus{}, fmt.Errorf("alldebrid: status: magnet %s not found", id)
	}
	return magnetToStatus(out.Magnets[0]), nil
}

// magnetFile is one entry from /v4/magnet/files. A folder has no link and
// carries its children in Entries instead, so the tree has to be walked
// rather than read as a flat list.
type magnetFile struct {
	Name    string       `json:"n"`
	Size    int64        `json:"s"`
	Link    string       `json:"l"`
	Entries []magnetFile `json:"e"`
}

// Files lists a magnet's files.
//
// The "link" on each file is itself locked — it is an alldebrid.com/f/...
// URL, not something that can be downloaded from. It becomes a real URL
// only through RequestDownloadLink, which is why that link is what gets
// used as the file id here.
func (p *Provider) Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error) {
	var out struct {
		Magnets []struct {
			ID    any          `json:"id"`
			Files []magnetFile `json:"files"`
		} `json:"magnets"`
	}
	form := url.Values{}
	form.Add("id[]", string(id))
	if err := p.client.do(ctx, http.MethodPost, "/v4/magnet/files", form, &out); err != nil {
		return nil, fmt.Errorf("alldebrid: files: %w", err)
	}
	if len(out.Magnets) == 0 {
		return nil, fmt.Errorf("alldebrid: files: magnet %s not found", id)
	}
	var files []debrid.DownloadFile
	flattenFiles(out.Magnets[0].Files, "", &files)
	return files, nil
}

// flattenFiles walks the file tree, joining folder names into each file's
// path so a nested torrent lands on disk with its structure intact — the
// same shape TorBox reports directly.
func flattenFiles(entries []magnetFile, prefix string, out *[]debrid.DownloadFile) {
	for _, e := range entries {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if len(e.Entries) > 0 {
			flattenFiles(e.Entries, path, out)
			continue
		}
		// A folder with no children and no link is not a file; skipping it
		// keeps empty directories out of the fetch list entirely.
		if e.Link == "" {
			continue
		}
		*out = append(*out, debrid.DownloadFile{
			ProviderFileID: e.Link,
			Path:           path,
			SizeBytes:      e.Size,
		})
	}
}

// RequestDownloadLink turns a file's locked link into a direct URL.
//
// fileID is the locked alldebrid.com/f/... link Files reported, not an
// opaque id — AllDebrid identifies a file by that link and has no separate
// per-file identifier to use instead.
func (p *Provider) RequestDownloadLink(ctx context.Context, _ debrid.ProviderDownloadID, fileID string) (string, error) {
	var out struct {
		Link string `json:"link"`
	}
	form := url.Values{}
	form.Set("link", fileID)
	if err := p.client.do(ctx, http.MethodPost, "/v4/link/unlock", form, &out); err != nil {
		return "", fmt.Errorf("alldebrid: unlock link: %w", err)
	}
	if out.Link == "" {
		return "", fmt.Errorf("alldebrid: unlock link: empty link in response")
	}
	return out.Link, nil
}

// RequestZipDownloadLink is not supported: AllDebrid unlocks one link at a
// time and has no server-side archive endpoint. Reported as an error rather
// than faked locally, matching debrid.TorrentProvider's own guidance —
// internal/api surfaces it and the per-file path still works.
func (p *Provider) RequestZipDownloadLink(context.Context, debrid.ProviderDownloadID) (string, error) {
	return "", fmt.Errorf("alldebrid: zipped downloads are not supported, fetch files individually")
}

// Delete removes a magnet from the account. deleteFiles is ignored:
// AllDebrid has no concept of keeping a magnet while discarding its files,
// so a delete is always both.
func (p *Provider) Delete(ctx context.Context, id debrid.ProviderDownloadID, _ bool) error {
	form := url.Values{}
	form.Set("id", string(id))
	if err := p.client.do(ctx, http.MethodPost, "/v4/magnet/delete", form, nil); err != nil {
		return fmt.Errorf("alldebrid: delete: %w", err)
	}
	return nil
}

// CheckCached reports whether each hash is already available.
//
// AllDebrid removed its instant-availability endpoint (/v4/magnet/instant
// returns 404 against the live API), so this answers from what is actually
// on the account: a magnet already uploaded and Ready is cached in every
// sense that matters to a caller deciding whether to add it. Anything else
// reports false rather than guessing — see debrid.TorrentProvider.CheckCached,
// which explicitly allows a provider with no cache-check endpoint to do this.
func (p *Provider) CheckCached(ctx context.Context, hashes []string) (map[string]bool, error) {
	result := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		result[strings.ToLower(h)] = false
	}
	statuses, err := p.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, st := range statuses {
		h := strings.ToLower(st.Hash)
		if _, wanted := result[h]; wanted && st.State == debrid.StateCompleted {
			result[h] = true
		}
	}
	return result, nil
}

// magnetToStatus maps AllDebrid's own magnet shape into AcerviNode's
// provider-agnostic one.
func magnetToStatus(m magnet) debrid.DownloadStatus {
	return debrid.DownloadStatus{
		ID:                 debrid.ProviderDownloadID(strconv.FormatInt(m.ID, 10)),
		Name:               m.Filename,
		Hash:               strings.ToLower(m.Hash),
		SizeBytes:          m.Size,
		Progress:           magnetProgress(m),
		State:              mapStatusCode(m.StatusCode),
		RawState:           m.Status,
		Seeders:            m.Seeders,
		DownloadSpeedBytes: m.DownloadSpeed,
		// AllDebrid records the magnet it was added with, so a discovered
		// download still supports Re-add — see
		// debrid.DownloadStatus.OriginalURL. Reconstructed from the hash
		// rather than stored, the same way torbox does it.
		OriginalURL: magnetFromHash(m.Hash),
	}
}

// magnetProgress derives 0..1 from bytes, since AllDebrid reports no
// percentage of its own. A Ready magnet is 1 regardless: a cached one
// arrives with downloaded at 0 having never transferred anything, and
// reporting that as 0% would show a finished download as untouched.
func magnetProgress(m magnet) float64 {
	if mapStatusCode(m.StatusCode) == debrid.StateCompleted {
		return 1
	}
	if m.Size <= 0 || m.Downloaded <= 0 {
		return 0
	}
	if m.Downloaded >= m.Size {
		return 1
	}
	return float64(m.Downloaded) / float64(m.Size)
}

// mapStatusCode translates AllDebrid's numeric status.
//
// 0-3 are the stages of getting a magnet onto the account (queued,
// downloading, compressing/moving, uploading) and are all "still working".
// 4 is Ready. Everything from 5 up is a terminal failure of some kind —
// upload failed, unpacking failed, no peers, took too long — and there is
// nothing a caller can do differently for one versus another, so they all
// map to error rather than being enumerated into distinctions AcerviNode
// would never act on. An unrecognised code is treated as an error too:
// a code this doesn't know about is not something to report as healthy.
func mapStatusCode(code int) debrid.DownloadState {
	switch {
	case code == 4:
		return debrid.StateCompleted
	case code >= 0 && code <= 3:
		return debrid.StateDownloading
	default:
		return debrid.StateError
	}
}

// magnetFromHash reconstructs a resubmittable magnet URI from an infohash —
// see torbox.magnetFromHash, which does the same for the same reason.
func magnetFromHash(hash string) string {
	if hash == "" {
		return ""
	}
	return "magnet:?xt=urn:btih:" + strings.ToLower(hash)
}

// user is the subset of /v4/user AcerviNode's account panel needs.
type user struct {
	Username     string `json:"username"`
	IsPremium    bool   `json:"isPremium"`
	IsTrial      bool   `json:"isTrial"`
	IsSubscribed bool   `json:"isSubscribed"`
	IsUltimate   bool   `json:"isUltimate"`
	// PremiumUntil is a Unix timestamp, not an RFC 3339 string like
	// TorBox's — converted in Account below so the shared shape stays one
	// format.
	PremiumUntil int64 `json:"premiumUntil"`
}

var _ debrid.AccountProvider = (*Provider)(nil)

// Account reports AllDebrid's own view of the account, for the settings
// panel. Nothing here is used to make decisions — see debrid.AccountStatus.
//
// TotalBytesDownloaded stays zero: AllDebrid reports current storage usage
// rather than a lifetime transfer counter, and showing "space used" under a
// label that means "downloaded ever" on every other provider would be worse
// than showing nothing. CooldownUntil is TorBox-specific and has no
// AllDebrid equivalent.
func (p *Provider) Account(ctx context.Context) (debrid.AccountStatus, error) {
	var out struct {
		User user `json:"user"`
	}
	if err := p.client.do(ctx, http.MethodGet, "/v4/user", nil, &out); err != nil {
		return debrid.AccountStatus{}, fmt.Errorf("alldebrid: account: %w", err)
	}
	u := out.User
	var expires string
	if u.PremiumUntil > 0 {
		expires = time.Unix(u.PremiumUntil, 0).UTC().Format(time.RFC3339)
	}
	return debrid.AccountStatus{
		PlanName:         planName(u),
		IsSubscribed:     u.IsSubscribed,
		PremiumExpiresAt: expires,
	}, nil
}

// planName describes the tier in AllDebrid's own vocabulary, most specific
// first — an Ultimate account is also premium, so checking premium first
// would flatten every tier into one label.
func planName(u user) string {
	switch {
	case u.IsUltimate && u.IsTrial:
		return "Ultimate (trial)"
	case u.IsUltimate:
		return "Ultimate"
	case u.IsPremium && u.IsTrial:
		return "Premium (trial)"
	case u.IsPremium:
		return "Premium"
	default:
		return "Free"
	}
}
