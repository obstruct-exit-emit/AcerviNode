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

// ListTorrents returns every torrent on the account.
func (c *Client) ListTorrents(ctx context.Context) ([]Torrent, error) {
	var env envelope[[]Torrent]
	if err := c.doGet(ctx, "/torrents/mylist", nil, &env); err != nil {
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
	UsenetDownloadID string `json:"usenetdownload_id"`
	Hash             string `json:"hash"`
}

// CreateUsenetDownload submits an NZB link or file. Unlike torrent IDs,
// TorBox's usenet download IDs are already strings.
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
	return env.Data.UsenetDownloadID, env.Data.Hash, nil
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

// ListUsenetDownloads returns every usenet download on the account.
func (c *Client) ListUsenetDownloads(ctx context.Context) ([]UsenetDownload, error) {
	var env envelope[[]UsenetDownload]
	if err := c.doGet(ctx, "/usenet/mylist", nil, &env); err != nil {
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
