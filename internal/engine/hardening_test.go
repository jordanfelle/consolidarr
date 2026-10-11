package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
)

func TestInsideFolderCleansBeforeComparing(t *testing.T) {
	for path, want := range map[string]bool{
		"/dl/pack/a.mkv":            true,
		"/dl/pack":                  true,
		"/dl/pack/sub/../a.mkv":     true,
		"/dl/pack/../../etc/passwd": false,
		"/dl/packx/a.mkv":           false,
		"/dl/pack/../other/a.mkv":   false,
		"dl/pack/a.mkv":             false,
		"-i":                        false,
		"/dl/pack/../../../../x":    false,
	} {
		if got := insideFolder(path, "/dl/pack"); got != want {
			t.Errorf("insideFolder(%q) = %v, want %v", path, got, want)
		}
	}
	if insideFolder("/dl/pack/a.mkv", "relative") || insideFolder("relative/a.mkv", "relative") {
		t.Error("a relative folder is never a safe anchor")
	}
}

func TestAdoptRefusesADowngradeUnlessForced(t *testing.T) {
	item := stuckItem(10, 1, 3, "Not an upgrade for existing episode file(s).")
	item["quality"] = map[string]any{"quality": map[string]any{"name": "WEBDL-720p", "source": "web", "resolution": 720}}
	v := attentionEnv(t, item)
	var q sonarr.Quality
	q.Quality.Name, q.Quality.Source, q.Quality.Resolution = "Bluray-1080p", "bluray", 1080
	v.fake.filesAfter = []sonarr.EpisodeFile{{ID: 11, SeasonNumber: 3, Quality: q}, {ID: 12, SeasonNumber: 3, Quality: q}, {ID: 13, SeasonNumber: 3, Quality: q}}

	if _, err := v.e.Adopt(context.Background(), v.inst, 1, 3, false); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("a 720p pack over 1080p bluray files must need force, got %v", err)
	}
	if js, _ := v.st.Jobs(true, 10); len(js) != 0 {
		t.Fatalf("a refused adopt must not leave a job behind: %+v", js)
	}
	if _, err := v.e.Adopt(context.Background(), v.inst, 1, 3, true); err != nil {
		t.Fatalf("forced adopt must go through: %v", err)
	}
}
