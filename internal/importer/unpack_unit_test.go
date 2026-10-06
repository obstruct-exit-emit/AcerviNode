package importer

import (
	"testing"

	"github.com/acervinode/acervinode/internal/debrid"
)

func filesNamed(names ...string) []debrid.DownloadFile {
	out := make([]debrid.DownloadFile, len(names))
	for i, n := range names {
		out[i] = debrid.DownloadFile{ProviderFileID: n, Path: n}
	}
	return out
}

// TestPackedOnly pins the rule. Failing a download is not free -- Sonarr
// blocklists the release -- so the rule errs toward "this has content": any
// file that is not plainly an archive or a recovery/metadata file, images
// included, means the download is fetched as normal.
func TestPackedOnly(t *testing.T) {
	tests := []struct {
		name  string
		files []debrid.DownloadFile
		want  bool
	}{
		{"the Amphibia post: rar parts, par2, nzb, srr, nfo", filesNamed(
			"a/x.part01.rar", "a/x.part02.rar", "a/x.par2", "a/x.vol00+01.par2", "a/x.nzb", "a/x.srr", "a/x.nfo"), true},
		{"old-style .rar/.r00 set", filesNamed("x.rar", "x.r00", "x.r01", "x.sfv"), true},
		{"split archive .001/.002", filesNamed("x.001", "x.002", "x.par2"), true},
		{"a lone 7z", filesNamed("x.7z"), true},
		{"a lone zip", filesNamed("x.zip", "x.nfo"), true},
		{"extension case does not matter", filesNamed("X.PART01.RAR", "X.PAR2"), true},
		{"unpacked: video plus leftovers", filesNamed("x.mkv", "x.nfo", "x.srr"), false},
		{"unpacked: video alongside its archives (post_processing 2)", filesNamed("x.mkv", "x.rar", "x.par2"), false},
		{"content that is not video still counts: an ebook", filesNamed("x.epub", "x.nfo"), false},
		{"images count as content, conservatively", filesNamed("x.rar", "cover.jpg"), false},
		{"no archive at all is not 'packed'", filesNamed("x.par2", "x.nzb", "x.nfo"), false},
		{"empty", filesNamed(), false},
		{"an mp3 is not a three-digit split part", filesNamed("track.mp3"), false},
	}
	// A release's sample clip travels with the post and is not the content.
	// Found live: German episodes TorBox left packed came back as RARs plus
	// Sample/x.sample.mkv, and the clip alone made them look unpacked, so
	// they were fetched instead of failed.
	sampleCases := []struct {
		name  string
		files []debrid.DownloadFile
		want  bool
	}{
		{"rars, par2 and a sample clip are still packed",
			[]debrid.DownloadFile{sized("x/x.part01.rar", 500), sized("x/x.part02.rar", 500), sized("x/x.par2", 1), sized("x/Sample/x.sample.mkv", 40)}, true},
		{"a sample beside the real, unpacked video is not packed",
			[]debrid.DownloadFile{sized("x/x.mkv", 1000), sized("x/x.rar", 900), sized("x/Sample/x.sample.mkv", 40)}, false},
		{"a 'sample' at least as big as the archives is content, conservatively",
			[]debrid.DownloadFile{sized("x/x.rar", 100), sized("x/x.sample.mkv", 150)}, false},
		{"a sample and recovery files without an archive are not packed",
			[]debrid.DownloadFile{sized("x/x.par2", 1), sized("x/Sample/x.sample.mkv", 40)}, false},
		{"a song called Sample is still content",
			[]debrid.DownloadFile{sized("x/x.rar", 500), sized("x/Sample.flac", 40)}, false},
	}
	for _, tc := range sampleCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := packedOnly(tc.files); got != tc.want {
				t.Errorf("packedOnly(%v) = %v, want %v", paths(tc.files), got, tc.want)
			}
		})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := packedOnly(tc.files); got != tc.want {
				t.Errorf("packedOnly(%v) = %v, want %v", tc.files, got, tc.want)
			}
		})
	}
}

func TestPackedOnly_SummaryCountsByExtension(t *testing.T) {
	summary, ok := packedOnly(filesNamed("a.part01.rar", "a.part02.rar", "a.par2", "a.vol0+1.PAR2", "a.nzb"))
	if !ok {
		t.Fatal("packedOnly() = false, want true")
	}
	if want := "1 .nzb, 2 .par2, 2 .rar"; summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
}
