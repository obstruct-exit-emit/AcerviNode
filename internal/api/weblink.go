package api

import (
	"regexp"
	"strings"
)

// Link shapes a provider will not accept, rewritten into the ones it will.
//
// Purely syntactic: nothing here fetches anything, and a link this does not
// recognise comes back untouched. It runs on both the add and the check-cached
// path, which have to agree — TorBox keys a web download's cache entry on an
// MD5 of the link itself, so normalising one and not the other would answer
// "is this cached?" about a different string than the one being added.

var (
	// MEGA's pre-2020 links carry the node handle and decryption key in the
	// URL fragment, separated by "!". The modern form moved the handle into
	// the path and left only the key in the fragment.
	//
	// Both still open in a browser, because mega.nz's own front end rewrites
	// the old shape client-side. A debrid provider parses the URL instead of
	// running that JavaScript, and a parser reading the path finds nothing
	// there to work with — which is why the legacy form has to be converted
	// before it is handed over.
	//
	// Folder is matched before file only for readability; the two cannot both
	// match, since "#F!" and "#!" differ at the first character after the
	// fragment marker.
	//
	// Only the scheme and host are case-folded. Hostnames are case-insensitive
	// so MEGA.NZ has to match, but the "F" marking a folder link is MEGA's own
	// and is always uppercase — folding it too would let "#f!" be read as a
	// folder share when it is not one.
	megaLegacyFolder = regexp.MustCompile(`^(?i:https?://mega(?:\.co)?\.nz)/#F!([A-Za-z0-9_-]+)!([A-Za-z0-9_-]+)$`)
	megaLegacyFile   = regexp.MustCompile(`^(?i:https?://mega(?:\.co)?\.nz)/#!([A-Za-z0-9_-]+)!([A-Za-z0-9_-]+)$`)

	// The pre-2016 domain. It still resolves, but by redirecting, and a
	// provider fetching server-side may not follow that.
	megaOldDomain = regexp.MustCompile(`^(?i:https?://mega\.co\.nz)(/.*)?$`)
)

// normalizeWebLink rewrites a web download link into the form providers
// accept, or returns it unchanged when there is nothing to do.
//
// Deliberately narrow. A legacy MEGA folder link may carry a third "!"
// segment naming a node inside the folder, and that case is left alone: the
// modern equivalent is either .../folder/ID#KEY/file/NODE or
// .../folder/ID#KEY/folder/NODE depending on what the node actually is, and
// nothing in the URL says which. Guessing wrong would silently point the
// download at the wrong thing, whereas leaving it untouched fails loudly with
// the provider's own error — the better outcome of the two.
func normalizeWebLink(in string) string {
	link := strings.TrimSpace(in)
	// The trimmed value, not the original: the callers test the result against
	// "" to decide whether a link was supplied at all, and handing back a
	// whitespace-only string would sail past that check.
	if link == "" {
		return link
	}
	if m := megaLegacyFolder.FindStringSubmatch(link); m != nil {
		return "https://mega.nz/folder/" + m[1] + "#" + m[2]
	}
	if m := megaLegacyFile.FindStringSubmatch(link); m != nil {
		return "https://mega.nz/file/" + m[1] + "#" + m[2]
	}
	if m := megaOldDomain.FindStringSubmatch(link); m != nil {
		return "https://mega.nz" + m[1]
	}
	return link
}
