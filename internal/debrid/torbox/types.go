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

// RequestTorrentZipDownloadLink resolves a single URL for every file in a
// torrent at once, zipped server-side by TorBox — confirmed live against a
// real account: omitting file_id and adding zip_link=true returns a working
// .zip URL (Content-Type: application/zip, correct total size), not
// documented anywhere we found, discovered by testing directly against the
// real API.
func (c *Client) RequestTorrentZipDownloadLink(ctx context.Context, torrentID string) (string, error) {
	q := url.Values{
		"token":      {c.apiKey},
		"torrent_id": {torrentID},
		"zip_link":   {"true"},
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
	// Airlocked reports whether this download is in TorBox AirLock, its
	// permanent storage: an airlocked item is exempt from the 30-day
	// retention policy that otherwise removes an inactive download from the
	// account. Added to all three mylist responses in TorBox v9 (2026-07-01)
	// and not yet in the published SDK model docs, which still lag that
	// release. Purely informational here — AcerviNode never sets it (that
	// needs the separate edit endpoints) and never acts on it; it's
	// surfaced so the web UI can show which Manual downloads are actually
	// safe from expiry and which are on the retention clock, the same thing
	// vanished-download detection exists to catch after the fact (see
	// database.handleMissingFromProvider).
	Airlocked bool `json:"airlocked"`
	// Seeds/Peers/DownloadSpeed are real, documented fields on TorBox's own
	// SDK response schema (torbox-sdk-js's GetTorrentListOkResponseData)
	// that weren't modeled here until the qBittorrent shim needed something
	// to report for real qBittorrent's own num_seeds/num_leechs/dlspeed —
	// found live: watching a real, genuinely uncached torrent download (not
	// TorBox's own instant-cache path) made it obvious this swarm
	// visibility was simply never being passed through anywhere.
	Seeds         float64 `json:"seeds"`
	Peers         float64 `json:"peers"`
	DownloadSpeed float64 `json:"download_speed"`
}

// listLimit caps how many rows mylist/getqueued return per call, matching
// rdt-client's own TorBox client (TorBox.NET v2.1.0's Torrents.GetCurrentAsync,
// which always sends limit=1000 alongside bypass_cache — this project doesn't
// send one at all otherwise). Confirmed live against a real account,
// byte-identical response either way (a small account, well under this cap),
// but omitting it was consistently 2-4x slower across repeated back-to-back
// calls — TorBox's server evidently does more work per request without a
// LIMIT clause, not a payload-size effect.
const listLimit = "1000"

// ListTorrents returns every torrent on the account. TorBox's own docs note
// mylist "only gets updated every 600 seconds" server-side unless asked to
// bypass that cache — confirmed live (a freshly added torrent was simply
// absent from an un-bypassed mylist response) — so this always sets
// bypass_cache, since AcerviNode's whole polling model depends on this
// endpoint reflecting current state promptly.
func (c *Client) ListTorrents(ctx context.Context) ([]Torrent, error) {
	var env envelope[[]Torrent]
	if err := c.doGet(ctx, "/torrents/mylist", url.Values{"bypass_cache": {"true"}, "limit": {listLimit}}, &env); err != nil {
		return nil, err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// GetTorrent returns a single torrent's current status via mylist's own
// id-filter — confirmed against TorBox's official SDK docs (torbox-sdk-js's
// TorrentsService: passing id "will return an object rather than list", not
// guessed or inferred from behavior). Used by internal/importer's fast
// per-download poll (see docs/providers.md): checking one specific download
// this way is far cheaper than ListTorrents, since TorBox only has to look up
// a single row instead of listing the whole account. A zero-value Torrent
// (ID == 0) means TorBox has no mylist entry for id at all — same "not found"
// signal ListTorrents' own linear scan would produce, callers should fall
// back to ListQueued exactly as they already do for that case.
func (c *Client) GetTorrent(ctx context.Context, id string) (Torrent, error) {
	var env envelope[Torrent]
	if err := c.doGet(ctx, "/torrents/mylist", url.Values{"id": {id}, "bypass_cache": {"true"}}, &env); err != nil {
		return Torrent{}, err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return Torrent{}, err
	}
	return env.Data, nil
}

type cachedAvailabilityData struct {
	Hash string `json:"hash"`
}

// CheckCachedTorrents reports, per hash, whether TorBox already has it
// cached — see checkCached's own doc comment for the shared mechanics.
func (c *Client) CheckCachedTorrents(ctx context.Context, hashes []string) (map[string]bool, error) {
	return c.checkCached(ctx, "/torrents/checkcached", hashes)
}

// checkCached backs CheckCachedTorrents/CheckCachedUsenet/
// CheckCachedWebDownloads — identical mechanics across all three, just a
// different endpoint and a different notion of "hash" (a real BitTorrent
// infohash for torrents; an MD5 of the link, or of the uploaded file's own
// bytes, for usenet/webdl — see each caller's own doc comment). TorBox's
// checkcached endpoints (format=object, the default) return a map keyed by
// whichever hashes are cached — hashes absent from the response are not
// cached.
//
// Sends one repeated hash= query param per hash, not the single
// comma-separated value the docs describe — confirmed live against the real
// API: a comma-joined value consistently timed out (curl exit 28, twice in a
// row), while repeated hash= params correctly returned every cached hash
// requested, even for two hashes at once. Docs were wrong here; the live
// behavior is what this follows.
func (c *Client) checkCached(ctx context.Context, endpoint string, hashes []string) (map[string]bool, error) {
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
	if err := c.doGet(ctx, endpoint, q, &env); err != nil {
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

// TorrentInfoResultFile is one file within a TorrentInfoResult.
type TorrentInfoResultFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// TorrentInfoResult is torrentinfo's response — a preview of a torrent's
// metadata straight from the BitTorrent network, by hash alone, before ever
// adding it. Confirmed live: needs no API key at all (TorBox's own docs say
// so too — "Authorization: None required" — sent anyway for consistency
// with every other call, harmless either way), and a torrent it can't find
// enough peers for within timeoutSeconds comes back a plain HTTP 500 with a
// real Detail message (surfaced by doGet/checkSuccess exactly like any other
// failed call), not a 200 with empty data.
type TorrentInfoResult struct {
	Name  string                  `json:"name"`
	Hash  string                  `json:"hash"`
	Size  int64                   `json:"size"`
	Seeds int64                   `json:"seeds"`
	Peers int64                   `json:"peers"`
	Files []TorrentInfoResultFile `json:"files"`
}

// TorrentInfo previews a torrent's metadata by hash — TorBox's own
// torrentinfo endpoint. timeoutSeconds bounds how long TorBox searches the
// BitTorrent network before giving up; 0 uses TorBox's own default (10s).
func (c *Client) TorrentInfo(ctx context.Context, hash string, timeoutSeconds int) (TorrentInfoResult, error) {
	q := url.Values{"hash": {hash}}
	if timeoutSeconds > 0 {
		q.Set("timeout", strconv.Itoa(timeoutSeconds))
	}
	var env envelope[TorrentInfoResult]
	if err := c.doGet(ctx, "/torrents/torrentinfo", q, &env); err != nil {
		return TorrentInfoResult{}, err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return TorrentInfoResult{}, err
	}
	return env.Data, nil
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
// consistent with what ListUsenetDownloads' own numeric id produces.
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

// RequestUsenetZipDownloadLink is RequestTorrentZipDownloadLink's usenet
// counterpart. Unlike the torrent version, this specific call is NOT
// confirmed live — by the time it was written, every usenet download on the
// test account had expired from mylist entirely (0 items), leaving nothing
// to test zip_link against. It mirrors the torrent endpoint's exact shape,
// which every other usenet/torrent pair in this client has matched so far
// (see docs/providers.md), but treat it as unverified until confirmed
// against a real, live usenet download.
func (c *Client) RequestUsenetZipDownloadLink(ctx context.Context, usenetID string) (string, error) {
	q := url.Values{
		"token":     {c.apiKey},
		"usenet_id": {usenetID},
		"zip_link":  {"true"},
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
	// OriginalURL is the NZB URL this download was submitted with — present
	// (confirmed live against a real account) when added via a URL, null
	// when added via an uploaded .nzb file (nothing to record in that case).
	// Not in any published TorBox docs; found by inspecting a real response.
	OriginalURL string `json:"original_url"`
	// DownloadPresent and Active are documented in TorBox's official SDK
	// field lists (torbox-sdk-js's UsenetService) but weren't modeled here
	// until mapUsenetState needed them — see usenet_provider.go.
	// DownloadPresent is TorBox's own authoritative "the files are actually
	// retrievable now" signal, more reliable than download_finished alone:
	// a usenet download can be download_finished (every article fetched)
	// while TorBox is still running its own SABnzbd-style post-processing
	// (par2 verify/repair, archive extraction) server-side, well before the
	// result is actually servable.
	DownloadPresent bool `json:"download_present"`
	Active          bool `json:"active"`
	// DownloadSpeed is another real, documented SDK field (bytes/sec) that
	// went unmodeled the same way DownloadPresent/Active did — SABnzbd's own
	// mode=queue reports an aggregate speed across every active download,
	// not a per-item one (confirmed against SABnzbd's real API docs), so
	// this feeds a sum rather than a per-row field — see
	// internal/sabnzbd/queue.go's handleQueue.
	DownloadSpeed float64 `json:"download_speed"`
	// Airlocked is TorBox AirLock — see Torrent.Airlocked.
	Airlocked bool `json:"airlocked"`
}

// ListUsenetDownloads returns every usenet download on the account. Same
// bypass_cache reasoning as ListTorrents.
func (c *Client) ListUsenetDownloads(ctx context.Context) ([]UsenetDownload, error) {
	var env envelope[[]UsenetDownload]
	if err := c.doGet(ctx, "/usenet/mylist", url.Values{"bypass_cache": {"true"}, "limit": {listLimit}}, &env); err != nil {
		return nil, err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// GetUsenetDownload is ListUsenetDownloads' single-item counterpart — same
// id-filter/cost reasoning as GetTorrent, confirmed for this endpoint too via
// TorBox's official SDK docs (UsenetService's own id parameter description).
func (c *Client) GetUsenetDownload(ctx context.Context, id string) (UsenetDownload, error) {
	var env envelope[UsenetDownload]
	if err := c.doGet(ctx, "/usenet/mylist", url.Values{"id": {id}, "bypass_cache": {"true"}}, &env); err != nil {
		return UsenetDownload{}, err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return UsenetDownload{}, err
	}
	return env.Data, nil
}

// CheckCachedUsenet reports, per hash, whether TorBox already has a usenet
// download cached — see checkCached's own doc comment for the shared
// mechanics. Per TorBox's docs, the hash here isn't TorBox's own — it's an
// MD5 of the NZB's link (URL-based add) or of the uploaded .nzb file's own
// bytes (file-based add); computing it is the caller's job (see
// internal/api's md5Hex).
func (c *Client) CheckCachedUsenet(ctx context.Context, hashes []string) (map[string]bool, error) {
	return c.checkCached(ctx, "/usenet/checkcached", hashes)
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
	if err := c.doGet(ctx, "/queued/getqueued", url.Values{"type": {kind}, "bypass_cache": {"true"}, "limit": {listLimit}}, &env); err != nil {
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

// --- Web Downloads -----------------------------------------------------------
//
// TorBox's "Web Downloads" service debrids direct links from ~160 supported
// hosters (Mega, 1Fichier, Mediafire, PixelDrain, and more — TorBox's own
// /webdl/hosters endpoint lists them, deliberately not modeled here since
// nothing in AcerviNode consumes it), confirmed live against the real
// account: Mega itself is
// active right now, and a real (if since-expired) Mega folder download
// already existed in the account's own history, confirming the shape below
// against real data rather than SDK docs alone — see docs/providers.md.
// Genuinely link-only, unlike createtorrent/createusenetdownload — no file
// upload option exists for this endpoint.

// CreateWebDownloadRequest mirrors createwebdownload's form fields (per
// TorBox's real OpenAPI spec, application/x-www-form-urlencoded — confirmed
// directly, not assumed from the SDK's docs). Link is the only required
// field; Name overrides the display name TorBox would otherwise infer.
type CreateWebDownloadRequest struct {
	Link string
	Name string
}

type createWebDownloadData struct {
	WebdownloadID float64 `json:"webdownload_id"`
	Hash          string  `json:"hash"`
}

// CreateWebDownload submits a hoster link. Returns TorBox's assigned id and
// hash the same way CreateTorrent/CreateUsenetDownload do. webdownload_id
// comes back as a JSON number — confirmed live against a real account
// (documented as a string in the SDK's own docs, the same mismatch
// usenetdownload_id turned out to have — see CreateUsenetDownload).
func (c *Client) CreateWebDownload(ctx context.Context, req CreateWebDownloadRequest) (id string, hash string, err error) {
	form := url.Values{"link": {req.Link}}
	if req.Name != "" {
		form.Set("name", req.Name)
	}
	var env envelope[createWebDownloadData]
	if err := c.doPostForm(ctx, "/webdl/createwebdownload", form, &env); err != nil {
		return "", "", err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return "", "", err
	}
	return formatID(env.Data.WebdownloadID), env.Data.Hash, nil
}

// ControlWebDownload performs delete on a web download — the only control
// operation that's meaningful here, unlike torrents/usenet (no pause/resume/
// reannounce concept for a direct-link fetch). webID is sent as a JSON
// string in the body; TorBox's real OpenAPI spec declares webdl_id as an
// integer, same as torrent_id/usenet_id, but both of those have been sent as
// strings in this client all along without issue (confirmed by every real
// delete this session) — TorBox's backend evidently coerces it.
func (c *Client) ControlWebDownload(ctx context.Context, webID, operation string) error {
	body := map[string]any{"webdl_id": webID, "operation": operation}
	var env envelope[any]
	if err := c.doPostJSON(ctx, "/webdl/controlwebdownload", body, &env); err != nil {
		return err
	}
	return checkSuccess(env.Success, env.Detail)
}

// RequestWebDownloadLink resolves a real CDN URL for one file of a web download.
func (c *Client) RequestWebDownloadLink(ctx context.Context, webID, fileID string) (string, error) {
	q := url.Values{
		"token":   {c.apiKey},
		"web_id":  {webID},
		"file_id": {fileID},
	}
	var env envelope[string]
	if err := c.doGet(ctx, "/webdl/requestdl", q, &env); err != nil {
		return "", err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return "", err
	}
	return env.Data, nil
}

// RequestWebDownloadZipDownloadLink mirrors RequestTorrentZipDownloadLink's
// zip_link=true trick — confirmed live against a real web download (a small
// public-domain archive.org file): the returned URL served a real
// Content-Type: application/zip with the correct content-disposition.
func (c *Client) RequestWebDownloadZipDownloadLink(ctx context.Context, webID string) (string, error) {
	q := url.Values{
		"token":    {c.apiKey},
		"web_id":   {webID},
		"zip_link": {"true"},
	}
	var env envelope[string]
	if err := c.doGet(ctx, "/webdl/requestdl", q, &env); err != nil {
		return "", err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return "", err
	}
	return env.Data, nil
}

// WebDownloadFile is one file within a web download, per mylist's embedded
// file list — confirmed live (a real Mega folder download's files carried
// exactly these fields, including a file id of 0, which formatID handles
// fine).
type WebDownloadFile struct {
	ID   float64 `json:"id"`
	Name string  `json:"name"`
	Size float64 `json:"size"`
}

// WebDownload is one entry from ListWebDownloads.
type WebDownload struct {
	ID               float64           `json:"id"`
	Hash             string            `json:"hash"`
	Name             string            `json:"name"`
	Size             float64           `json:"size"`
	DownloadState    string            `json:"download_state"`
	Progress         float64           `json:"progress"`
	DownloadFinished bool              `json:"download_finished"`
	Eta              float64           `json:"eta"`
	Files            []WebDownloadFile `json:"files"`
	// OriginalURL is the hoster link this download was submitted with —
	// confirmed live against a real account (a pre-existing Mega folder
	// download's original_url was the real mega.nz link). Not in any
	// published TorBox docs.
	OriginalURL string `json:"original_url"`
	// Airlocked is TorBox AirLock — see Torrent.Airlocked.
	Airlocked bool `json:"airlocked"`
}

// ListWebDownloads returns every web download on the account. Same
// bypass_cache reasoning as ListTorrents/ListUsenetDownloads.
func (c *Client) ListWebDownloads(ctx context.Context) ([]WebDownload, error) {
	var env envelope[[]WebDownload]
	if err := c.doGet(ctx, "/webdl/mylist", url.Values{"bypass_cache": {"true"}, "limit": {listLimit}}, &env); err != nil {
		return nil, err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// GetWebDownload is ListWebDownloads' single-item counterpart — same
// id-filter/cost reasoning as GetTorrent, confirmed for this endpoint too via
// TorBox's official SDK docs (WebDownloadsDebridService's own id parameter
// description).
func (c *Client) GetWebDownload(ctx context.Context, id string) (WebDownload, error) {
	var env envelope[WebDownload]
	if err := c.doGet(ctx, "/webdl/mylist", url.Values{"id": {id}, "bypass_cache": {"true"}}, &env); err != nil {
		return WebDownload{}, err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return WebDownload{}, err
	}
	return env.Data, nil
}

// CheckCachedWebDownloads reports, per hash, whether TorBox already has a
// web download cached — see checkCached's own doc comment for the shared
// mechanics. Per TorBox's docs, the hash is an MD5 of the link itself;
// computing it is the caller's job (see internal/api's md5Hex).
func (c *Client) CheckCachedWebDownloads(ctx context.Context, hashes []string) (map[string]bool, error) {
	return c.checkCached(ctx, "/webdl/checkcached", hashes)
}

// --- User -------------------------------------------------------------------

// UserData is a subset of GET /user/me's response — just what AcerviNode's
// own account-status display needs. Confirmed live against the real
// account: the actual response has many more fields than either the
// official SDK's docs or its own Go types declare (e.g. total_bytes_downloaded,
// torrents_downloaded, web_downloads_downloaded weren't documented anywhere
// found) — this only models what's actually used, using the field names
// confirmed from that real response.
type UserData struct {
	Plan                 float64 `json:"plan"` // 0 Free, 1 Essential, 2 Pro, 3 Standard
	IsSubscribed         bool    `json:"is_subscribed"`
	PremiumExpiresAt     string  `json:"premium_expires_at"`
	TotalBytesDownloaded float64 `json:"total_bytes_downloaded"`
	// CooldownUntil is a real field on /user/me's response, not documented
	// anywhere found. Found live, while investigating a real "everything
	// looks frozen" report: with this set to a future time, every one of
	// TorBox's own listing endpoints (torrents/usenet mylist, bypass_cache
	// or not) returned a 200 with zero items instead of erroring —
	// confirmed by replicating AcerviNode's exact request by hand and
	// watching it come back empty the same way. That specific mechanism
	// (this field being the direct cause, as opposed to a coincidental
	// correlation with some other account-level restriction) isn't
	// independently confirmed from TorBox's own side — there's no official
	// documentation of what this field means — but the correlation was
	// exact and repeatable in the moment it was found. Surfaced as-is
	// rather than acted on: AcerviNode doesn't change polling behavior
	// based on it, only displays it (see debrid.AccountStatus.CooldownUntil).
	CooldownUntil string `json:"cooldown_until"`
}

// GetUserData returns the current account's plan/usage info.
func (c *Client) GetUserData(ctx context.Context) (UserData, error) {
	var env envelope[UserData]
	if err := c.doGet(ctx, "/user/me", nil, &env); err != nil {
		return UserData{}, err
	}
	if err := checkSuccess(env.Success, env.Detail); err != nil {
		return UserData{}, err
	}
	return env.Data, nil
}
