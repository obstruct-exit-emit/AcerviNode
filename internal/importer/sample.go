package importer

import (
	"path/filepath"
	"strings"

	"github.com/acervinode/acervinode/internal/debrid"
)

// sampleVideoExt is every extension a release's sample clip comes in. Samples
// are a video-release convention; nothing else is ever treated as one, so an
// album track or audiobook chapter called "Sample" is safe.
var sampleVideoExt = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true,
	".wmv": true, ".ts": true, ".m2ts": true, ".mpg": true, ".mpeg": true,
	".webm": true, ".flv": true, ".vob": true,
}

// dropSamples removes a release's sample clips from files, keeping order.
//
// A file is a sample only when all three hold:
//   - it is a video (sampleVideoExt);
//   - "sample" is a whole word in its name ("Movie.sample.mkv",
//     "sample-movie.mp4"), or it sits in a "sample"/"samples" folder;
//   - the same download has a larger video that is not itself one.
//
// The last is what makes it safe to have on by default. Skipping a real file
// is far worse than fetching a sample -- one costs some bandwidth, the other
// is a failed import -- and it guarantees the main file is never dropped: a
// film actually called "Sample", a download that is nothing but a sample, or a
// sample larger than everything else beside it are all kept whole. Whole
// words only, so "Sampler" and "Resample" are not samples.
//
// Sonarr and Radarr skip samples on import anyway, so this saves fetching
// them, rather than changing what gets imported.
func dropSamples(files []debrid.DownloadFile) []debrid.DownloadFile {
	marked := make([]bool, len(files))
	var largestMain int64 = -1
	anyMarked := false
	for i, f := range files {
		if !isVideoFile(f.Path) {
			continue
		}
		if namedLikeSample(f.Path) {
			marked[i], anyMarked = true, true
		} else if f.SizeBytes > largestMain {
			largestMain = f.SizeBytes
		}
	}
	if !anyMarked {
		return files
	}
	out := make([]debrid.DownloadFile, 0, len(files))
	for i, f := range files {
		if marked[i] && f.SizeBytes < largestMain {
			continue
		}
		out = append(out, f)
	}
	return out
}

func isVideoFile(path string) bool {
	return sampleVideoExt[strings.ToLower(filepath.Ext(slashed(path)))]
}

// namedLikeSample reports whether path's file name has "sample" as a whole
// word, or one of its folders is named "sample" or "samples".
func namedLikeSample(path string) bool {
	segments := strings.Split(slashed(path), "/")
	for _, dir := range segments[:len(segments)-1] {
		switch strings.ToLower(strings.TrimSpace(dir)) {
		case "sample", "samples":
			return true
		}
	}
	name := segments[len(segments)-1]
	name = strings.TrimSuffix(name, filepath.Ext(name))
	for _, word := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if word == "sample" {
			return true
		}
	}
	return false
}

// slashed normalises a provider path to forward slashes. Which separator a
// provider uses is its own choice, not this machine's.
func slashed(path string) string {
	return strings.ReplaceAll(path, `\`, "/")
}
