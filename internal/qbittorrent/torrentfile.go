package qbittorrent

import (
	"crypto/sha1"
	"encoding/hex"
)

// Reading a .torrent's infohash out of the file itself.
//
// Sonarr and Radarr compute this the same way and use it as the DownloadId they
// track a grab by, so it is what we have to key on too. Without it a
// file-upload add had no hash at all until a poll backfilled one from the
// provider, and — more importantly — a lost add reply could only ever be
// reconciled by name, which is the best-effort half of the "a pending *arr add
// is claimed exactly once" invariant.
//
// An infohash is the SHA-1 of the *raw bytes* of the torrent's top-level "info"
// dictionary, exactly as they appear in the file. It cannot be computed by
// parsing the structure and re-encoding it: bencode is canonical in theory and
// files in the wild are not, and any difference changes the hash. So this walks
// the encoding purely to find where that dictionary starts and ends, and hashes
// the original slice.
//
// A wrong hash is worse than no hash — it would attach a grab to the wrong
// download — so every failure path returns "" rather than a guess.

// maxBencodeDepth bounds nesting. The format allows arbitrarily deep lists and
// dictionaries, so a few bytes of input can otherwise ask for unbounded
// recursion; real torrents nest a handful of levels at most.
const maxBencodeDepth = 100

// infohashFromTorrentFile returns the lowercase hex v1 infohash of a .torrent
// file's contents, or "" if data is not a torrent this can read with certainty.
func infohashFromTorrentFile(data []byte) string {
	start, end, ok := topLevelInfoDict(data)
	if !ok {
		return ""
	}
	sum := sha1.Sum(data[start:end])
	return hex.EncodeToString(sum[:])
}

// topLevelInfoDict locates the byte range of the top-level "info" value.
// Deliberately top-level only: a nested dictionary may carry its own "info"
// key, and hashing that would produce a confident, wrong answer.
func topLevelInfoDict(data []byte) (start, end int, ok bool) {
	if len(data) == 0 || data[0] != 'd' {
		return 0, 0, false
	}
	i := 1
	for i < len(data) && data[i] != 'e' {
		// A dictionary key is always a bencoded string. Comparing the decoded
		// bytes — rather than searching the file for a literal "4:info" — is
		// what stops a piece of binary payload being mistaken for a key.
		keyStart, keyEnd, failed := bencodeString(data, i)
		if failed {
			return 0, 0, false
		}
		valueStart := keyEnd
		valueEnd, failed := bencodeValueEnd(data, valueStart, 0)
		if failed {
			return 0, 0, false
		}
		if string(data[keyStart:keyEnd]) == "info" {
			// The value has to be a dictionary; anything else is not a torrent.
			if valueStart >= len(data) || data[valueStart] != 'd' {
				return 0, 0, false
			}
			return valueStart, valueEnd, true
		}
		i = valueEnd
	}
	return 0, 0, false
}

// bencodeString returns the byte range of a bencoded string's *contents*
// starting at i (<length>:<bytes>), and whether parsing failed.
func bencodeString(data []byte, i int) (start, end int, failed bool) {
	colon := -1
	for j := i; j < len(data); j++ {
		if data[j] == ':' {
			colon = j
			break
		}
		if data[j] < '0' || data[j] > '9' {
			return 0, 0, true
		}
	}
	// No digits, or no terminator.
	if colon <= i {
		return 0, 0, true
	}
	n := 0
	for _, c := range data[i:colon] {
		d := int(c - '0')
		// Bound the length as it is built, so a long run of digits cannot
		// overflow into a small positive number.
		if n > (1<<31)/10 {
			return 0, 0, true
		}
		n = n*10 + d
	}
	contentStart := colon + 1
	contentEnd := contentStart + n
	if contentEnd < contentStart || contentEnd > len(data) {
		return 0, 0, true
	}
	return contentStart, contentEnd, false
}

// bencodeValueEnd returns the offset one past the value starting at i.
func bencodeValueEnd(data []byte, i, depth int) (end int, failed bool) {
	if i >= len(data) || depth > maxBencodeDepth {
		return 0, true
	}
	switch c := data[i]; {
	case c == 'i':
		for j := i + 1; j < len(data); j++ {
			if data[j] == 'e' {
				if j == i+1 {
					return 0, true // "ie" is not an integer
				}
				return j + 1, false
			}
			if (data[j] < '0' || data[j] > '9') && data[j] != '-' {
				return 0, true
			}
		}
		return 0, true

	case c >= '0' && c <= '9':
		_, contentEnd, err := bencodeString(data, i)
		if err {
			return 0, true
		}
		return contentEnd, false

	case c == 'l' || c == 'd':
		isDict := c == 'd'
		j := i + 1
		for {
			if j >= len(data) {
				return 0, true
			}
			if data[j] == 'e' {
				return j + 1, false
			}
			if isDict {
				// Keys must be strings.
				_, keyEnd, err := bencodeString(data, j)
				if err {
					return 0, true
				}
				j = keyEnd
				if j >= len(data) {
					return 0, true
				}
			}
			next, err := bencodeValueEnd(data, j, depth+1)
			if err {
				return 0, true
			}
			j = next
		}

	default:
		return 0, true
	}
}
