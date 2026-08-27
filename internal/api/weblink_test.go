package api

import "testing"

func TestNormalizeWebLink(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// The case that prompted this: providers do not understand the
			// legacy fragment shape, and the key is identical in both forms.
			name: "legacy folder link",
			in:   "https://mega.nz/#F!5zRGyQZR!u91UYP1weBd6gaLRJDBCMg",
			want: "https://mega.nz/folder/5zRGyQZR#u91UYP1weBd6gaLRJDBCMg",
		},
		{
			name: "legacy file link",
			in:   "https://mega.nz/#!aB3dEfGh!kEyMaTeRiAl_1234567890abcd",
			want: "https://mega.nz/file/aB3dEfGh#kEyMaTeRiAl_1234567890abcd",
		},
		{
			name: "legacy link on the pre-2016 domain",
			in:   "https://mega.co.nz/#F!5zRGyQZR!u91UYP1weBd6gaLRJDBCMg",
			want: "https://mega.nz/folder/5zRGyQZR#u91UYP1weBd6gaLRJDBCMg",
		},
		{
			name: "modern link on the pre-2016 domain",
			in:   "https://mega.co.nz/folder/5zRGyQZR#u91UYP1weBd6gaLRJDBCMg",
			want: "https://mega.nz/folder/5zRGyQZR#u91UYP1weBd6gaLRJDBCMg",
		},
		{
			// http rather than https, which the rewrite upgrades because it
			// is rebuilding the URL anyway.
			name: "legacy link over http",
			in:   "http://mega.nz/#F!5zRGyQZR!u91UYP1weBd6gaLRJDBCMg",
			want: "https://mega.nz/folder/5zRGyQZR#u91UYP1weBd6gaLRJDBCMg",
		},
		{
			name: "keys using the base64url alphabet survive intact",
			in:   "https://mega.nz/#!Ab-Cd_Ef!key-with_both-chars_here",
			want: "https://mega.nz/file/Ab-Cd_Ef#key-with_both-chars_here",
		},
		{
			name: "modern folder link is already correct",
			in:   "https://mega.nz/folder/5zRGyQZR#u91UYP1weBd6gaLRJDBCMg",
			want: "https://mega.nz/folder/5zRGyQZR#u91UYP1weBd6gaLRJDBCMg",
		},
		{
			name: "modern file link is already correct",
			in:   "https://mega.nz/file/aB3dEfGh#kEyMaTeRiAl",
			want: "https://mega.nz/file/aB3dEfGh#kEyMaTeRiAl",
		},
		{
			// A node inside a shared folder. The modern form needs to say
			// whether that node is a file or a folder, and the legacy URL
			// does not carry that, so this is deliberately left alone rather
			// than guessed at. See normalizeWebLink's doc comment.
			name: "legacy folder link naming an inner node is left alone",
			in:   "https://mega.nz/#F!5zRGyQZR!u91UYP1weBd6gaLRJDBCMg!xYz12345",
			want: "https://mega.nz/#F!5zRGyQZR!u91UYP1weBd6gaLRJDBCMg!xYz12345",
		},
		{
			// No key means nothing can decrypt it either way; rewriting the
			// path would only disguise that.
			name: "legacy link with no key is left alone",
			in:   "https://mega.nz/#F!5zRGyQZR",
			want: "https://mega.nz/#F!5zRGyQZR",
		},
		{
			name: "other hosts are untouched",
			in:   "https://1fichier.com/?abc123",
			want: "https://1fichier.com/?abc123",
		},
		{
			// Guards the anchoring: a mega link inside another URL's query
			// must not be pulled out and rewritten as the whole thing.
			name: "a mega link embedded in another URL is untouched",
			in:   "https://example.com/go?to=https://mega.nz/#F!abc!def",
			want: "https://example.com/go?to=https://mega.nz/#F!abc!def",
		},
		{
			name: "empty stays empty",
			in:   "",
			want: "",
		},
		{
			// Regression: this returned the original, untrimmed, so the
			// handlers' `link == ""` check saw a non-empty string and let a
			// blank link through to the provider.
			name: "whitespace-only normalises to empty, not to itself",
			in:   "   ",
			want: "",
		},
		{
			name: "an uppercased host still matches",
			in:   "https://MEGA.NZ/#F!5zRGyQZR!u91UYP1weBd6gaLRJDBCMg",
			want: "https://mega.nz/folder/5zRGyQZR#u91UYP1weBd6gaLRJDBCMg",
		},
		{
			name: "a mixed-case old domain still matches",
			in:   "HTTPS://Mega.Co.Nz/folder/5zRGyQZR#u91UYP1weBd6gaLRJDBCMg",
			want: "https://mega.nz/folder/5zRGyQZR#u91UYP1weBd6gaLRJDBCMg",
		},
		{
			// The "F" is MEGA's own marker and is always uppercase. Folding it
			// would read a non-folder link as a folder share.
			name: "a lowercase f is not a folder marker",
			in:   "https://mega.nz/#f!5zRGyQZR!u91UYP1weBd6gaLRJDBCMg",
			want: "https://mega.nz/#f!5zRGyQZR!u91UYP1weBd6gaLRJDBCMg",
		},
		{
			name: "surrounding whitespace is trimmed",
			in:   "  https://mega.nz/#F!5zRGyQZR!u91UYP1weBd6gaLRJDBCMg  ",
			want: "https://mega.nz/folder/5zRGyQZR#u91UYP1weBd6gaLRJDBCMg",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeWebLink(tc.in); got != tc.want {
				t.Errorf("normalizeWebLink(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// The add and check-cached paths must agree, because TorBox keys a web
// download's cache entry on an MD5 of the link. If only one of them
// normalised, the cached answer would describe a different string than the
// one actually added.
func TestNormalizeWebLinkIsStable(t *testing.T) {
	legacy := "https://mega.nz/#F!5zRGyQZR!u91UYP1weBd6gaLRJDBCMg"
	once := normalizeWebLink(legacy)
	twice := normalizeWebLink(once)
	if once != twice {
		t.Fatalf("not idempotent: %q then %q", once, twice)
	}
	if md5Hex(once) != md5Hex(twice) {
		t.Fatal("cache hash differs between one and two passes")
	}
}
