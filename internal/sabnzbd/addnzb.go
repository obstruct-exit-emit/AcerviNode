package sabnzbd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/acervinode/acervinode/internal/database"
	"github.com/acervinode/acervinode/internal/debrid"
)

// handleAddURL implements mode=addurl: *arr apps pass the NZB's URL directly
// (the "name" parameter, despite the misleading name — this is SABnzbd's
// actual field name for the URL in addurl mode) plus a category.
func (s *Server) handleAddURL(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nzbURL := r.FormValue("name")
	if nzbURL == "" {
		writeJSON(w, map[string]any{"status": false, "error": "no URL given"})
		return
	}
	category := r.FormValue("cat")
	displayName := r.FormValue("nzbname")

	p := s.defaultUsenet()
	if p == nil {
		writeJSON(w, map[string]any{"status": false, "error": "no usenet-capable provider configured"})
		return
	}
	// Fetch it here, as real SABnzbd does -- see fetchNZB -- and upload it like
	// an addfile. Only if that fails does the link itself go to the provider.
	var id debrid.ProviderDownloadID
	var err error
	if data, filename, fetchErr := s.fetchNZB(ctx, nzbURL); fetchErr == nil {
		displayName = nzbJobName(displayName, filename)
		id, err = p.AddNZBFile(ctx, filename, data, debrid.AddOptions{Name: displayName})
	} else {
		slog.Warn("sabnzbd: could not fetch the NZB here, handing the provider the link instead", "error", fetchErr)
		id, err = p.AddNZBURL(ctx, nzbURL, debrid.AddOptions{Name: displayName})
	}
	if err != nil {
		slog.Error("sabnzbd: add nzb url failed", "error", err)
		// The provider may have taken it even though the reply did not
		// arrive. Usenet has no client-side hash, so this can only be
		// matched on name -- best-effort, and better than the item being
		// adopted as Manual and never imported.
		s.recordPendingAdd(ctx, displayName, category)
		writeJSON(w, map[string]any{"status": false, "error": err.Error()})
		return
	}

	nzoID, err := s.storeNewDownload(ctx, id, displayName, category, nzbURL)
	if err != nil {
		slog.Error("sabnzbd: store new download failed", "error", err)
		writeJSON(w, map[string]any{"status": false, "error": "internal error"})
		return
	}
	s.categories.add(category)
	writeJSON(w, map[string]any{"status": true, "nzo_ids": []string{nzoID}})
}

// nzbFetchTimeout bounds fetching an addurl link here. The clients that send
// addurl give the whole request 30 seconds (CantiNode's and LibriNode's
// SABnzbd clients), and the provider add still has to fit after it.
const nzbFetchTimeout = 15 * time.Second

// maxNZBBytes caps what an addurl fetch will read. NZBs for the largest
// releases run to tens of megabytes; this is well past that and still bounded.
const maxNZBBytes = 100 << 20

// nzbHTTPClient fetches addurl links. No client-level timeout: fetchNZB puts
// its own deadline on the context.
var nzbHTTPClient = &http.Client{}

// fetchNZB downloads an addurl link from this machine and returns the NZB and
// the file name it goes by.
//
// Real SABnzbd fetches an addurl link itself. Handing it to the provider
// instead asks a debrid service's cloud servers to fetch it, and the link a
// self-hosted app sends is usually a LAN one -- typically Prowlarr's download
// proxy -- which they can never reach. Found live: CantiNode fell back to
// addurl with a 192.168.1.x Prowlarr link and AcerviNode did not answer within
// its 30-second timeout. Sonarr and Radarr never hit it because they upload the
// file themselves (addfile).
//
// Anything that is not plainly an NZB is an error, so the caller falls back to
// the link rather than uploading an indexer's HTML error page. The link carries
// the indexer's API key, so no error here includes it.
func (s *Server) fetchNZB(ctx context.Context, rawURL string) (data []byte, filename string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, "", errors.New("not an http(s) link")
	}
	ctx, cancel := context.WithTimeout(ctx, nzbFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", errors.New("could not build the request")
	}
	resp, err := nzbHTTPClient.Do(req)
	if err != nil {
		// *url.Error repeats the link, key and all; keep only its cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, "", fmt.Errorf("fetching from %s: %w", u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetching from %s: HTTP %d", u.Host, resp.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxNZBBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading from %s: %w", u.Host, err)
	}
	if len(data) > maxNZBBytes {
		return nil, "", fmt.Errorf("response from %s is larger than any NZB", u.Host)
	}
	if !looksLikeNZB(data) {
		return nil, "", fmt.Errorf("response from %s is not an NZB", u.Host)
	}
	return data, nzbFileName(resp.Header.Get("Content-Disposition"), u), nil
}

// looksLikeNZB reports whether data opens with an NZB document: an <nzb
// element within the first few KB, past any XML declaration and DOCTYPE.
func looksLikeNZB(data []byte) bool {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	return bytes.Contains(bytes.ToLower(head), []byte("<nzb"))
}

// nzbFileName is the name a fetched NZB goes by: the Content-Disposition
// filename when the server gives one, as indexers and Prowlarr do, or else
// the link's last path segment. Always ending in .nzb, so nzbJobName and the
// provider both treat it as the upload it now is.
func nzbFileName(contentDisposition string, u *url.URL) string {
	name := ""
	if _, params, err := mime.ParseMediaType(contentDisposition); err == nil {
		name = strings.TrimSpace(params["filename"])
	}
	if name == "" {
		name = path.Base(u.Path)
	}
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if name == "" || name == "." || name == "/" {
		name = "download"
	}
	if !strings.HasSuffix(strings.ToLower(name), ".nzb") {
		name += ".nzb"
	}
	return name
}

// handleAddFile implements mode=addfile: a multipart upload where "name" is
// the NZB file part itself, matching SABnzbd's real (slightly confusing)
// field-naming convention.
func (s *Server) handleAddFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.MultipartForm == nil {
		writeJSON(w, map[string]any{"status": false, "error": "no file given"})
		return
	}
	headers := r.MultipartForm.File["name"]
	if len(headers) == 0 {
		writeJSON(w, map[string]any{"status": false, "error": "no file given"})
		return
	}
	header := headers[0]
	f, err := header.Open()
	if err != nil {
		writeJSON(w, map[string]any{"status": false, "error": "could not read file"})
		return
	}
	data, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		writeJSON(w, map[string]any{"status": false, "error": "could not read file"})
		return
	}

	category := r.FormValue("cat")
	// Sonarr and Radarr never send nzbname -- see nzbJobName.
	displayName := nzbJobName(r.FormValue("nzbname"), header.Filename)

	p := s.defaultUsenet()
	if p == nil {
		writeJSON(w, map[string]any{"status": false, "error": "no usenet-capable provider configured"})
		return
	}
	id, err := p.AddNZBFile(ctx, header.Filename, data, debrid.AddOptions{Name: displayName})
	if err != nil {
		slog.Error("sabnzbd: add nzb file failed", "error", err)
		// The provider may have taken it even though the reply did not
		// arrive. Usenet has no client-side hash, so this can only be
		// matched on name -- best-effort, and better than the item being
		// adopted as Manual and never imported.
		s.recordPendingAdd(ctx, displayName, category)
		writeJSON(w, map[string]any{"status": false, "error": err.Error()})
		return
	}

	nzoID, err := s.storeNewDownload(ctx, id, displayName, category, "")
	if err != nil {
		slog.Error("sabnzbd: store new download failed", "error", err)
		writeJSON(w, map[string]any{"status": false, "error": "internal error"})
		return
	}
	s.categories.add(category)
	writeJSON(w, map[string]any{"status": true, "nzo_ids": []string{nzoID}})
}

// storeNewDownload fetches the provider's own view of a just-added download
// and records it under a fresh AcerviNode-assigned id, which doubles as the
// nzo_id handed back to the *arr app — SABnzbd's real nzo_id has no fixed
// format, so there's nothing to preserve from the provider side (contrast
// with the qBittorrent shim, which must expose a real infohash).
// recordPendingAdd notes an *arr add that failed after the request went out,
// so discovery can recognise the item as Managed if the provider took it
// anyway. Best-effort by design -- see database.RecordPendingArrAdd.
// nzbJobName is the name an addfile job goes by: the nzbname the client
// supplied, or else the uploaded file's own name without its .nzb extension.
//
// The fallback is what real SABnzbd does, and it is not an edge case: Sonarr
// and Radarr never send nzbname (confirmed in both apps' SabnzbdProxy
// .DownloadNzb, which sends only the file, category and priority), so without
// it every job they added through here had no name of ours at all. That was
// harmless for display only because the provider picked a name on its own. It
// was fatal to reconciliation: a failed add is recorded in pending_arr_adds so
// discovery can reclaim the item as Managed if the provider took it anyway,
// and usenet has no hash -- the name is the only thing that can match. It was
// recorded empty, so for a Sonarr or Radarr usenet grab that reconciliation
// could never fire.
//
// The name is also passed to the provider explicitly, so what we record and
// what it later reports are the same string by construction rather than by
// hoping its own derivation agrees with ours. (TorBox's does, observed live:
// an uploaded "acervinode-test.nzb" came back as "acervinode-test".)
//
// Only the last path segment is used, split on either separator: the
// uploading client's OS decides which one it sent, not ours.
func nzbJobName(nzbName, filename string) string {
	if n := strings.TrimSpace(nzbName); n != "" {
		return n
	}
	base := strings.TrimSpace(filename)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if strings.HasSuffix(strings.ToLower(base), ".nzb") {
		base = base[:len(base)-len(".nzb")]
	}
	return strings.TrimSpace(base)
}

func (s *Server) recordPendingAdd(ctx context.Context, name, category string) {
	if err := s.db.RecordPendingArrAdd(ctx, &database.PendingArrAdd{
		Provider: s.registry.DefaultNameFor(debrid.KindUsenet),
		Kind:     database.KindUsenet,
		Name:     name,
		Category: category,
	}); err != nil {
		slog.Error("sabnzbd: could not record a failed add for later reconciliation", "error", err)
	}
}

func (s *Server) storeNewDownload(ctx context.Context, id debrid.ProviderDownloadID, fallbackName, category, source string) (nzoID string, err error) {
	p := s.defaultUsenet()
	if p == nil {
		return "", debrid.ErrNoProvider
	}
	status, statusErr := p.Status(ctx, id)
	if statusErr != nil {
		slog.Warn("sabnzbd: provider status not yet available after add, using fallback", "id", id, "error", statusErr)
		status = debrid.DownloadStatus{ID: id, Name: fallbackName, State: debrid.StateQueued}
	}
	if status.Name == "" {
		status.Name = fallbackName
	}

	d := &database.Download{
		ID:                 uuid.NewString(),
		Provider:           p.Name(),
		ProviderDownloadID: string(id),
		Kind:               database.KindUsenet,
		Name:               status.Name,
		Category:           category,
		SizeBytes:          status.SizeBytes,
		State:              database.LocalStateFromProvider(status.State),
		// A provider can report a failure the instant it accepts an add,
		// and a row born in StateError needs its reason like any other --
		// see database.ProviderErrorMessage.
		ErrorMessage: database.ProviderErrorMessage(status.State, status.RawState),
		Progress:     status.Progress,
		// Source is the NZB URL itself for a URL-based add, empty for a
		// .nzb file upload — see database.Download.Source and ReAddDownload.
		Source: source,
		// AddedViaArr, not AddedViaManual: this shim only exists for *arr
		// apps, which need the files to land on local disk for their own
		// import step — see database.AddedVia.
		AddedVia: database.AddedViaArr,
	}
	// Not a plain InsertDownload: a row for this provider id may already
	// exist (TorBox dedupes by content, and the importer's discovery pass
	// can adopt a just-added item first), in which case that row is claimed
	// for *arr rather than colliding with it — see InsertOrClaimForArr. The
	// returned row's id is what must go back as the nzo_id: for a claimed
	// row that's the existing row's id, not the one generated above.
	stored, err := s.db.InsertOrClaimForArr(ctx, d)
	if err != nil {
		return "", err
	}
	return stored.ID, nil
}
