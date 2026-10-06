package importer

import (
	"testing"

	"github.com/acervinode/acervinode/internal/debrid"
)

func sized(path string, mb int64) debrid.DownloadFile {
	return debrid.DownloadFile{ProviderFileID: path, Path: path, SizeBytes: mb << 20}
}

func paths(files []debrid.DownloadFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path
	}
	return out
}

// TestDropSamples pins what counts as a sample. Skipping a real file is far
// worse than fetching a sample -- a sample costs some bandwidth, a skipped
// episode is a failed import -- so a file is dropped only when all three
// hold: it is a video, "sample" is a whole word in its name or it sits in a
// sample folder, and the same download has a larger video that is not one.
func TestDropSamples(t *testing.T) {
	tests := []struct {
		name  string
		files []debrid.DownloadFile
		want  []string
	}{
		{"a Sample folder beside the movie",
			[]debrid.DownloadFile{sized("Movie.2020.1080p/Movie.2020.1080p.mkv", 8000), sized("Movie.2020.1080p/Sample/movie.2020.1080p-sample.mkv", 60)},
			[]string{"Movie.2020.1080p/Movie.2020.1080p.mkv"}},
		{"sample as a word in the file name",
			[]debrid.DownloadFile{sized("Movie.mkv", 4000), sized("Movie.sample.mkv", 40), sized("sample-movie.mp4", 30), sized("Movie_SAMPLE.avi", 20)},
			[]string{"Movie.mkv"}},
		{"a lowercase samples folder in a season pack",
			[]debrid.DownloadFile{sized("S01/Show.S01E01.mkv", 900), sized("S01/Show.S01E02.mkv", 900), sized("S01/samples/Show.S01E01.mkv", 50)},
			[]string{"S01/Show.S01E01.mkv", "S01/Show.S01E02.mkv"}},
		{"everything else in the release is kept",
			[]debrid.DownloadFile{sized("M/M.mkv", 4000), sized("M/M.nfo", 0), sized("M/M.en.srt", 0), sized("M/Sample/m-sample.mkv", 50)},
			[]string{"M/M.mkv", "M/M.nfo", "M/M.en.srt"}},
		{"a film called Sample is the main file, so it stays",
			[]debrid.DownloadFile{sized("Sample.2019.1080p.WEB/Sample.2019.1080p.WEB.mkv", 5000)},
			[]string{"Sample.2019.1080p.WEB/Sample.2019.1080p.WEB.mkv"}},
		{"a sample larger than every other video is not dropped",
			[]debrid.DownloadFile{sized("x.sample.mkv", 900), sized("extras/featurette.mkv", 300)},
			[]string{"x.sample.mkv", "extras/featurette.mkv"}},
		{"a download that is only a sample is kept whole",
			[]debrid.DownloadFile{sized("Movie.Sample.mkv", 60)},
			[]string{"Movie.Sample.mkv"}},
		{"a song called Sample is not a video",
			[]debrid.DownloadFile{sized("Album/01 - Intro.flac", 30), sized("Album/02 - Sample.flac", 40), sized("Album/video.mkv", 500)},
			[]string{"Album/01 - Intro.flac", "Album/02 - Sample.flac", "Album/video.mkv"}},
		{"only whole words count: Sampler, Samplers and Resample stay",
			[]debrid.DownloadFile{sized("Main.mkv", 4000), sized("The.Sampler.mkv", 40), sized("Samplers/a.mkv", 40), sized("Resample.mkv", 40)},
			[]string{"Main.mkv", "The.Sampler.mkv", "Samplers/a.mkv", "Resample.mkv"}},
		{"extension and folder case do not matter",
			[]debrid.DownloadFile{sized("Main.MKV", 4000), sized("SAMPLE/clip.MP4", 40)},
			[]string{"Main.MKV"}},
		{"a Windows-style path from the provider",
			// "samples" only counts as a folder name, so this needs the path split.
			[]debrid.DownloadFile{sized(`Movie\Movie.mkv`, 4000), sized(`Movie\samples\clip.mkv`, 40)},
			[]string{`Movie\Movie.mkv`}},
		{"nothing to drop", nil, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := paths(dropSamples(tc.files))
			if len(got) != len(tc.want) {
				t.Fatalf("dropSamples() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("dropSamples() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestFilterFiles_SkipSamples -- wired into the one place that decides what
// gets fetched, on unless switched off, and alongside the operator's own
// filters rather than instead of them.
func TestFilterFiles_SkipSamples(t *testing.T) {
	files := []debrid.DownloadFile{sized("M/M.mkv", 4000), sized("M/Sample/m.mkv", 50), sized("M/M.nfo", 0)}

	im := &Importer{}
	im.SetSkipSamples(true)
	if got := paths(im.filterFiles(files)); len(got) != 2 || got[0] != "M/M.mkv" || got[1] != "M/M.nfo" {
		t.Errorf("skip on: filterFiles() = %v, want the sample dropped and the rest kept", got)
	}

	im.SetSkipSamples(false)
	if got := im.filterFiles(files); len(got) != 3 {
		t.Errorf("skip off: filterFiles() = %v, want every file", paths(got))
	}

	im.SetSkipSamples(true)
	im.SetFileFilters(1, 0, nil, nil) // the operator's own minimum size, which drops the 0-byte .nfo
	if got := paths(im.filterFiles(files)); len(got) != 1 || got[0] != "M/M.mkv" {
		t.Errorf("both on: filterFiles() = %v, want only the movie", got)
	}
}
