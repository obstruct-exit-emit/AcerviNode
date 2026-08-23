package alldebrid

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/acervinode/acervinode/internal/debrid"
)

// WebDownloadProvider adapts Client to debrid.WebDownloadProvider — hoster
// links (Mega, 1fichier, and whatever else the account's plan covers; see
// /v4/user/hosts, which is per-plan rather than global).
//
// AllDebrid's model here is quite different from a service that creates a
// tracked "web download" object you poll until it finishes. Unlocking is
// synchronous: one call validates the link, resolves the host and hands
// back a direct URL immediately. There is no progress to report and nothing
// to wait for.
//
// What makes it fit AcerviNode's add/poll/fetch shape anyway is the saved
// links list (/v4/user/links): saving a link gives it a durable presence on
// the account that List can enumerate, Status can look up and Delete can
// remove. So an added link is simply born complete — which is honest, since
// by then AllDebrid really has resolved it and the bytes are one unlock
// away.
type WebDownloadProvider struct {
	client *Client
	name   string
}

// NewWebDownloadProvider builds a web-download provider for apiKey. name is
// the registry entry, not the service — see NewProvider.
func NewWebDownloadProvider(name, apiKey string, opts ...Option) *WebDownloadProvider {
	return &WebDownloadProvider{client: NewClient(apiKey, opts...), name: name}
}

var _ debrid.WebDownloadProvider = (*WebDownloadProvider)(nil)

func (p *WebDownloadProvider) Name() string { return p.name }

// savedLink is one entry from /v4/user/links.
type savedLink struct {
	Link     string `json:"link"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	Host     string `json:"host"`
	Date     int64  `json:"date"`
}

// unlocked is /v4/link/unlock's response — the direct URL plus what
// AllDebrid worked out about the link on the way.
type unlocked struct {
	Link     string `json:"link"`
	Filename string `json:"filename"`
	Filesize int64  `json:"filesize"`
	Host     string `json:"host"`
}

// unlock resolves link to a direct download URL, which also serves as the
// validity check: an unsupported host or a dead link fails here rather than
// being saved and failing later.
func (p *WebDownloadProvider) unlock(ctx context.Context, link string) (unlocked, error) {
	var out unlocked
	form := url.Values{}
	form.Set("link", link)
	if err := p.client.do(ctx, http.MethodPost, "/v4/link/unlock", form, &out); err != nil {
		return unlocked{}, err
	}
	return out, nil
}

// savedLinks lists everything currently saved on the account.
func (p *WebDownloadProvider) savedLinks(ctx context.Context) ([]savedLink, error) {
	var out struct {
		Links []savedLink `json:"links"`
	}
	if err := p.client.do(ctx, http.MethodGet, "/v4/user/links", nil, &out); err != nil {
		return nil, err
	}
	return out.Links, nil
}

// AddLink saves a hoster link and returns the id everything afterwards uses
// to reach it.
//
// That id is the link *as AllDebrid stores it*, which is not necessarily
// the link that went in. Saving normalises: a Mega link submitted as
// "mega.nz/file/ID#KEY" comes back from /v4/user/links as
// "mega.co.nz/#!ID!KEY". Confirmed live, and not mentioned in AllDebrid's
// documentation.
//
// That detail is load-bearing. Returning the caller's original link as the
// id would mean every later List reported a link that matched nothing
// tracked locally, so each of these downloads would look like it had
// vanished from the account moments after being added — and
// database.handleMissingFromProvider would eventually flag them all as
// gone. Matching back to the stored form is what avoids that, and is why
// this makes a second call rather than just returning what it was handed.
func (p *WebDownloadProvider) AddLink(ctx context.Context, link string, _ debrid.AddOptions) (debrid.ProviderDownloadID, error) {
	// Unlock first: it validates the host and link, so an unsupported one
	// fails here instead of being saved as a link that can never resolve.
	info, err := p.unlock(ctx, link)
	if err != nil {
		return "", fmt.Errorf("alldebrid: add link: %w", err)
	}

	form := url.Values{}
	form.Add("links[]", link)
	if err := p.client.do(ctx, http.MethodPost, "/v4/user/links/save", form, nil); err != nil {
		return "", fmt.Errorf("alldebrid: add link: %w", err)
	}

	// Find how it was actually stored. Matched on filename and size, which
	// the unlock above already told us, rather than on the link text that
	// may have been rewritten.
	links, err := p.savedLinks(ctx)
	if err != nil {
		return "", fmt.Errorf("alldebrid: add link: %w", err)
	}
	var best savedLink
	for _, l := range links {
		if l.Filename == info.Filename && l.Size == info.Filesize && l.Date >= best.Date {
			best = l
		}
	}
	if best.Link == "" {
		// Saved, but not recognisable in the listing. Falling back to the
		// original link keeps the add working; the download may later look
		// missing, which is strictly better than failing an add that did in
		// fact succeed.
		slog.Warn("alldebrid: saved link not found in listing, using the submitted link as its id",
			"provider", p.name, "filename", info.Filename)
		return debrid.ProviderDownloadID(link), nil
	}
	return debrid.ProviderDownloadID(best.Link), nil
}

// toStatus maps a saved link to AcerviNode's own status shape. Always
// complete: a saved link has already been resolved by AllDebrid, so there
// is no in-progress state for one to be in.
func (p *WebDownloadProvider) toStatus(l savedLink) debrid.DownloadStatus {
	return debrid.DownloadStatus{
		ID:          debrid.ProviderDownloadID(l.Link),
		Name:        l.Filename,
		SizeBytes:   l.Size,
		Progress:    1,
		State:       debrid.StateCompleted,
		RawState:    "saved",
		OriginalURL: l.Link,
	}
}

// List returns every saved link on the account.
func (p *WebDownloadProvider) List(ctx context.Context) ([]debrid.DownloadStatus, error) {
	links, err := p.savedLinks(ctx)
	if err != nil {
		return nil, fmt.Errorf("alldebrid: list web downloads: %w", err)
	}
	out := make([]debrid.DownloadStatus, 0, len(links))
	for _, l := range links {
		out = append(out, p.toStatus(l))
	}
	return out, nil
}

// Status looks one saved link up by id. AllDebrid has no per-link endpoint,
// so this filters the listing — the same call List makes, which the shared
// listing cache means is usually already paid for.
func (p *WebDownloadProvider) Status(ctx context.Context, id debrid.ProviderDownloadID) (debrid.DownloadStatus, error) {
	links, err := p.savedLinks(ctx)
	if err != nil {
		return debrid.DownloadStatus{}, fmt.Errorf("alldebrid: web download status: %w", err)
	}
	for _, l := range links {
		if l.Link == string(id) {
			return p.toStatus(l), nil
		}
	}
	return debrid.DownloadStatus{}, fmt.Errorf("alldebrid: web download %s not found", id)
}

// Files reports the single file behind a saved link. A hoster link is one
// file by definition — there is no archive to enumerate the way a torrent
// has — so the file id is the link itself, which is what
// RequestDownloadLink then unlocks.
func (p *WebDownloadProvider) Files(ctx context.Context, id debrid.ProviderDownloadID) ([]debrid.DownloadFile, error) {
	st, err := p.Status(ctx, id)
	if err != nil {
		return nil, err
	}
	return []debrid.DownloadFile{{
		ProviderFileID: string(id),
		Path:           st.Name,
		SizeBytes:      st.SizeBytes,
	}}, nil
}

// RequestDownloadLink unlocks the saved link into a direct URL.
//
// Unlocked every time rather than cached: AllDebrid's direct URLs are
// short-lived and tied to one session, so a stored one would work now and
// quietly 404 later.
func (p *WebDownloadProvider) RequestDownloadLink(ctx context.Context, id debrid.ProviderDownloadID, _ string) (string, error) {
	info, err := p.unlock(ctx, string(id))
	if err != nil {
		return "", fmt.Errorf("alldebrid: web download link: %w", err)
	}
	if info.Link == "" {
		return "", fmt.Errorf("alldebrid: web download link: empty link in response")
	}
	return info.Link, nil
}

// RequestZipDownloadLink is not supported: AllDebrid unlocks one link at a
// time and has no endpoint that bundles several. A web download is a single
// file anyway, so this would have nothing to bundle even if it existed —
// see debrid.WebDownloadProvider.RequestZipDownloadLink, which allows a
// provider to say so rather than faking it locally.
func (p *WebDownloadProvider) RequestZipDownloadLink(context.Context, debrid.ProviderDownloadID) (string, error) {
	return "", fmt.Errorf("alldebrid: zipped downloads are not supported, fetch the file directly")
}

// Delete removes a saved link from the account. deleteFiles is ignored:
// nothing is stored on AllDebrid's side to delete separately — a saved link
// is a reference, and removing it is the whole operation.
func (p *WebDownloadProvider) Delete(ctx context.Context, id debrid.ProviderDownloadID, _ bool) error {
	form := url.Values{}
	form.Set("link", string(id))
	if err := p.client.do(ctx, http.MethodPost, "/v4/user/links/delete", form, nil); err != nil {
		return fmt.Errorf("alldebrid: delete web download: %w", err)
	}
	return nil
}

// CheckCached reports, per hash, whether that link is already saved.
//
// The hashes are MD5s of the link text (internal/api's md5Hex), not
// anything AllDebrid knows about, so this hashes the account's own saved
// links and compares. AllDebrid has no cache-check endpoint of its own —
// /v4/magnet/instant is gone, and there was never a web-download
// equivalent — and a link that is already saved is available in the only
// sense the caller is asking about.
func (p *WebDownloadProvider) CheckCached(ctx context.Context, hashes []string) (map[string]bool, error) {
	result := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		result[strings.ToLower(h)] = false
	}

	links, err := p.savedLinks(ctx)
	if err != nil {
		return nil, fmt.Errorf("alldebrid: check cached web downloads: %w", err)
	}
	for _, l := range links {
		sum := md5.Sum([]byte(l.Link))
		h := hex.EncodeToString(sum[:])
		if _, wanted := result[h]; wanted {
			result[h] = true
		}
	}
	return result, nil
}
