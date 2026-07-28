package torbox

import (
	"context"
	"net/url"
	"strconv"
)

// Control operations, per the TorBox API docs (torrents/controltorrent and
// usenet/controlusenetdownload both take an "operation" string).
const (
	OpReannounce = "reannounce" // torrents only
	OpResume     = "resume"
	OpPause      = "pause" // usenet only
	OpDelete     = "delete"
)

// --- Torrents -------------------------------------------------------------

// CreateTorrentRequest mirrors createtorrent's multipart form fields. Send
// either Magnet or File+Filename, not both.
type CreateTorrentRequest struct {
	Magnet   string
	File     []byte
	Filename string
	Name     string // optional display name override
	Seed     string // "1" auto, "2" seed, "3" don't seed — see TorBox docs
}

type createTorrentData struct {
	TorrentID float64 `json:"torrent_id"`
	Hash      string  `json:"hash"`
	QueuedID  float64 `json:"queued_id"`
}

// CreateTorrent submits a magnet or torrent file. Returns TorBox's assigned
// torrent ID (as a string — TorBox's own API uses a numeric ID, formatted
// here since debrid.ProviderDownloadID is opaque to callers) and infohash.
func (c *Client) CreateTorrent(ctx context.Context, req CreateTorrentRequest) (id string, hash string, err error) {
	fields := map[string]string{
		"magnet": req.Magnet,
		"name":   req.Name,
		"seed":   req.Seed,
	}
	var env envelope[createTorrentData]
	if err := c.doMultipart(ctx, "/torrents/createtorrent", fields, "file", req.Filename, req.File, &env); err != nil {
		return "", "", err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return "", "", err
	}
	return formatID(env.Data.TorrentID), env.Data.Hash, nil
}

// ControlTorrent performs delete/reannounce/resume on a torrent.
func (c *Client) ControlTorrent(ctx context.Context, torrentID, operation string) error {
	body := map[string]any{"torrent_id": torrentID, "operation": operation}
	var env envelope[any]
	if err := c.doPostJSON(ctx, "/torrents/controltorrent", body, &env); err != nil {
		return err
	}
	return checkSuccess(env.Success, env.Detail)
}

// RequestTorrentDownloadLink resolves a real CDN URL for one file of a torrent.
func (c *Client) RequestTorrentDownloadLink(ctx context.Context, torrentID, fileID string) (string, error) {
	q := url.Values{
		"token":      {c.apiKey},
		"torrent_id": {torrentID},
		"file_id":    {fileID},
	}
	var env envelope[string]
	if err := c.doGet(ctx, "/torrents/requestdl", q, &env); err != nil {
		return "", err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return "", err
	}
	return env.Data, nil
}

// TorrentFile is one file within a torrent, per mylist's embedded file list.
type TorrentFile struct {
	ID   float64 `json:"id"`
	Name string  `json:"name"`
	Size float64 `json:"size"`
}

// Torrent is one entry from ListTorrents.
type Torrent struct {
	ID               float64       `json:"id"`
	Hash             string        `json:"hash"`
	Name             string        `json:"name"`
	Size             float64       `json:"size"`
	DownloadState    string        `json:"download_state"`
	Progress         float64       `json:"progress"`
	DownloadFinished bool          `json:"download_finished"`
	Eta              float64       `json:"eta"`
	Files            []TorrentFile `json:"files"`
}

// ListTorrents returns every torrent on the account. TorBox's own docs note
// mylist "only gets updated every 600 seconds" server-side unless asked to
// bypass that cache — confirmed live (a freshly added torrent was simply
// absent from an un-bypassed mylist response) — so this always sets
// bypass_cache, since AcerviNode's whole polling model depends on this
// endpoint reflecting current state promptly.
func (c *Client) ListTorrents(ctx context.Context) ([]Torrent, error) {
	var env envelope[[]Torrent]
	if err := c.doGet(ctx, "/torrents/mylist", url.Values{"bypass_cache": {"true"}}, &env); err != nil {
		return nil, err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return nil, err
	}
	return env.Data, nil
}

type cachedAvailabilityData struct {
	Hash string `json:"hash"`
}

// CheckCachedTorrents reports, per hash, whether TorBox already has it cached.
// TorBox's checkcached endpoint (format=object, the default) returns a map
// keyed by whichever hashes are cached — hashes absent from the response are
// not cached.
func (c *Client) CheckCachedTorrents(ctx context.Context, hashes []string) (map[string]bool, error) {
	result := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		result[h] = false
	}
	if len(hashes) == 0 {
		return result, nil
	}

	q := url.Values{}
	for _, h := range hashes {
		q.Add("hash", h)
	}
	var env envelope[map[string]cachedAvailabilityData]
	if err := c.doGet(ctx, "/torrents/checkcached", q, &env); err != nil {
		return nil, err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return nil, err
	}
	for h := range env.Data {
		result[h] = true
	}
	return result, nil
}

// --- Usenet -----------------------------------------------------------------

// CreateUsenetDownloadRequest mirrors createusenetdownload's multipart form
// fields. Send either Link or File+Filename, not both.
type CreateUsenetDownloadRequest struct {
	Link     string
	File     []byte
	Filename string
	Name     string
}

type createUsenetDownloadData struct {
	UsenetDownloadID float64 `json:"usenetdownload_id"`
	Hash             string  `json:"hash"`
}

// CreateUsenetDownload submits an NZB link or file. usenetdownload_id comes
// back as a JSON number, the same as a torrent's torrent_id — confirmed live
// against a real account (the official SDK's docs describe it as a string,
// which doesn't match reality and caused a real decode failure here: "json:
// cannot unmarshal number into Go struct field ...usenetdownload_id of type
// string"). Formatted the same way torrent IDs are (formatID) so it's
// consistent with what ListUsenetDownloads' own numeric id produces — see
// idMatches in usenet_provider.go, which no longer needs to assume the two
// match, now that both are derived the same way.
func (c *Client) CreateUsenetDownload(ctx context.Context, req CreateUsenetDownloadRequest) (id string, hash string, err error) {
	fields := map[string]string{
		"link": req.Link,
		"name": req.Name,
	}
	var env envelope[createUsenetDownloadData]
	if err := c.doMultipart(ctx, "/usenet/createusenetdownload", fields, "file", req.Filename, req.File, &env); err != nil {
		return "", "", err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return "", "", err
	}
	return formatID(env.Data.UsenetDownloadID), env.Data.Hash, nil
}

// ControlUsenetDownload performs delete/pause/resume on a usenet download.
func (c *Client) ControlUsenetDownload(ctx context.Context, usenetID, operation string) error {
	body := map[string]any{"usenet_id": usenetID, "operation": operation}
	var env envelope[any]
	if err := c.doPostJSON(ctx, "/usenet/controlusenetdownload", body, &env); err != nil {
		return err
	}
	return checkSuccess(env.Success, env.Detail)
}

// RequestUsenetDownloadLink resolves a real CDN URL for one file of a usenet download.
func (c *Client) RequestUsenetDownloadLink(ctx context.Context, usenetID, fileID string) (string, error) {
	q := url.Values{
		"token":     {c.apiKey},
		"usenet_id": {usenetID},
		"file_id":   {fileID},
	}
	var env envelope[string]
	if err := c.doGet(ctx, "/usenet/requestdl", q, &env); err != nil {
		return "", err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return "", err
	}
	return env.Data, nil
}

// UsenetFile is one file within a usenet download, per mylist's embedded file list.
type UsenetFile struct {
	ID   float64 `json:"id"`
	Name string  `json:"name"`
	Size float64 `json:"size"`
}

// UsenetDownload is one entry from ListUsenetDownloads.
type UsenetDownload struct {
	ID               float64      `json:"id"`
	Hash             string       `json:"hash"`
	Name             string       `json:"name"`
	Size             float64      `json:"size"`
	DownloadState    string       `json:"download_state"`
	Progress         float64      `json:"progress"`
	DownloadFinished bool         `json:"download_finished"`
	Eta              float64      `json:"eta"`
	Files            []UsenetFile `json:"files"`
}

// ListUsenetDownloads returns every usenet download on the account. Same
// bypass_cache reasoning as ListTorrents.
func (c *Client) ListUsenetDownloads(ctx context.Context) ([]UsenetDownload, error) {
	var env envelope[[]UsenetDownload]
	if err := c.doGet(ctx, "/usenet/mylist", url.Values{"bypass_cache": {"true"}}, &env); err != nil {
		return nil, err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// --- Queue ------------------------------------------------------------------

// QueuedDownload is one entry from /queued/getqueued — a torrent or usenet
// download TorBox has accepted but hasn't started processing yet (e.g. an
// account concurrency limit or backend load). Confirmed live: it does not
// appear in mylist/usenet/mylist at all until it's promoted out of this
// queue, so without checking this endpoint too, a backlogged download is
// indistinguishable from one TorBox has never heard of. No progress, state,
// or size is available for it yet — only proof it exists and is pending;
// found by inspecting a comparable open-source debrid client's own polling
// code (RDT-Client), which checks both endpoints where AcerviNode previously
// only checked mylist — see docs/providers.md.
type QueuedDownload struct {
	ID   float64 `json:"id"`
	Hash string  `json:"hash"`
	Name string  `json:"name"`
}

// ListQueued returns downloads of the given kind ("torrent" or "usenet")
// still waiting in TorBox's pre-processing queue, per the account. Shares
// the bypass_cache reasoning of ListTorrents/ListUsenetDownloads.
func (c *Client) ListQueued(ctx context.Context, kind string) ([]QueuedDownload, error) {
	var env envelope[[]QueuedDownload]
	if err := c.doGet(ctx, "/queued/getqueued", url.Values{"type": {kind}, "bypass_cache": {"true"}}, &env); err != nil {
		return nil, err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return nil, err
	}
	return env.Data, nil
}

func formatID(n float64) string {
	return strconv.FormatInt(int64(n), 10)
}
