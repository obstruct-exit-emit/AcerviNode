package qbittorrent

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"math/rand"
	"strings"
	"testing"
)

// A minimal but real-shaped single-file torrent. The info dictionary is the
// substring between "4:info" and the final "e" — an infohash is the SHA-1 of
// exactly those raw bytes, which is why extraction has to work on byte offsets
// rather than on a re-encoding of the parsed structure.
const testInfoDict = "d6:lengthi1024e4:name8:test.bin12:piece lengthi16384e6:pieces0:e"
const testTorrent = "d8:announce19:http://tracker.test4:info" + testInfoDict + "e"

func wantHashOf(t *testing.T, infoDict string) string {
	t.Helper()
	sum := sha1.Sum([]byte(infoDict))
	return hex.EncodeToString(sum[:])
}

// TestInfohashFromTorrentFile is the core of it: Sonarr and Radarr compute a
// .torrent's infohash themselves and use it as the DownloadId they track the
// grab by, so it is also what we have to key on.
func TestInfohashFromTorrentFile(t *testing.T) {
	got := infohashFromTorrentFile([]byte(testTorrent))
	want := wantHashOf(t, testInfoDict)
	if got != want {
		t.Errorf("infohashFromTorrentFile() = %q, want %q", got, want)
	}
	if len(got) != 40 {
		t.Errorf("infohash length = %d, want 40 hex characters", len(got))
	}
	if got != strings.ToLower(got) {
		t.Errorf("infohash = %q, want lowercase to match the rest of the codebase", got)
	}
}

// TestInfohashFromTorrentFile_KeyOrderIndependent — "info" is not always last,
// and a dictionary that puts it first must hash identically.
func TestInfohashFromTorrentFile_KeyOrderIndependent(t *testing.T) {
	infoFirst := "d4:info" + testInfoDict + "8:announce19:http://tracker.teste"
	if got, want := infohashFromTorrentFile([]byte(infoFirst)), wantHashOf(t, testInfoDict); got != want {
		t.Errorf("info-first torrent hashed to %q, want %q", got, want)
	}
}

// TestInfohashFromTorrentFile_NestedInfoKeyIsNotMistaken — only the top-level
// "info" counts. A nested dictionary with its own "info" key must not win.
func TestInfohashFromTorrentFile_NestedInfoKeyIsNotMistaken(t *testing.T) {
	nested := "d4:infod6:lengthi7e4:name3:abc4:infod6:lengthi9eee8:announce5:hellooe"
	// Deliberately malformed tail aside, the point is the outer info must be
	// the one hashed. Build a clean version instead:
	nested = "d8:metadatad4:info5:decoye4:info" + testInfoDict + "e"
	if got, want := infohashFromTorrentFile([]byte(nested)), wantHashOf(t, testInfoDict); got != want {
		t.Errorf("nested-decoy torrent hashed to %q, want the top-level info %q", got, want)
	}
}

// TestInfohashFromTorrentFile_RejectsGarbage — a wrong hash is worse than no
// hash, so anything not clearly a torrent returns empty rather than a guess.
func TestInfohashFromTorrentFile_RejectsGarbage(t *testing.T) {
	cases := map[string]string{
		"empty":                    "",
		"not bencode":              "hello world",
		"not a dict at top level":  "l4:infoe",
		"dict but no info key":     "d8:announce5:helloe",
		"info is not a dict":       "d4:info5:helloe",
		"truncated mid-info":       "d4:infod6:length",
		"unterminated string":      "d4:info99:short",
		"negative string length":   "d4:info-1:xe",
		"bare integer":             "i42e",
		"dict key is not a string": "d" + "i1e" + "4:infoe",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if got := infohashFromTorrentFile([]byte(in)); got != "" {
				t.Errorf("infohashFromTorrentFile(%q) = %q, want empty", in, got)
			}
		})
	}
}

// TestInfohashFromTorrentFile_NeverPanics: this parses attacker-influenced
// bytes (whatever an indexer served), so the only acceptable failure is "".
// Deterministic seed so a failure is reproducible.
func TestInfohashFromTorrentFile_NeverPanics(t *testing.T) {
	rng := rand.New(rand.NewSource(20260929))
	alphabet := []byte("dile0123456789:-e")

	for i := 0; i < 20000; i++ {
		n := rng.Intn(64)
		buf := make([]byte, n)
		for j := range buf {
			// Mostly bencode-ish bytes, sometimes arbitrary ones.
			if rng.Intn(4) == 0 {
				buf[j] = byte(rng.Intn(256))
			} else {
				buf[j] = alphabet[rng.Intn(len(alphabet))]
			}
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on input %q: %v", buf, r)
				}
			}()
			if got := infohashFromTorrentFile(buf); got != "" && len(got) != 40 {
				t.Fatalf("input %q produced a non-empty, non-infohash result %q", buf, got)
			}
		}()
	}
}

// TestInfohashFromTorrentFile_DeeplyNestedDoesNotBlowTheStack — a few bytes of
// input can otherwise ask for unbounded recursion.
func TestInfohashFromTorrentFile_DeeplyNestedDoesNotBlowTheStack(t *testing.T) {
	deep := "d4:info" + strings.Repeat("l", 100000)
	done := make(chan string, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- "panic"
				return
			}
		}()
		done <- infohashFromTorrentFile([]byte(deep))
	}()
	if got := <-done; got != "" {
		t.Errorf("deeply nested input = %q, want empty (a depth cap, not a crash)", got)
	}
}

// TestInfohashFromTorrentFile_BinaryPiecesSurvive — real torrents carry raw
// SHA-1 piece hashes, which contain NULs and every other byte value. A parser
// that treated the payload as text would corrupt the hash.
func TestInfohashFromTorrentFile_BinaryPiecesSurvive(t *testing.T) {
	pieces := make([]byte, 20)
	for i := range pieces {
		pieces[i] = byte(i * 11)
	}
	var info bytes.Buffer
	info.WriteString("d6:lengthi1024e4:name8:test.bin12:piece lengthi16384e6:pieces20:")
	info.Write(pieces)
	info.WriteString("e")

	var torrent bytes.Buffer
	torrent.WriteString("d8:announce19:http://tracker.test4:info")
	torrent.Write(info.Bytes())
	torrent.WriteString("e")

	sum := sha1.Sum(info.Bytes())
	want := hex.EncodeToString(sum[:])
	if got := infohashFromTorrentFile(torrent.Bytes()); got != want {
		t.Errorf("binary-pieces torrent = %q, want %q", got, want)
	}
}
