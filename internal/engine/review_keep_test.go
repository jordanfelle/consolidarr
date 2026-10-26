package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jordanfelle/consolidarr/internal/order"
)

func manyFiles(n int, season int) []string {
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("Complete/Show - %03d.mkv", i))
	}
	return out
}

func eps(n, season int) []order.Ep {
	var out []order.Ep
	for i := 1; i <= n; i++ {
		out = append(out, order.Ep{ID: i, Season: season, Number: i, Title: fmt.Sprintf("Title Number %d", i)})
	}
	return out
}

// Live (#88): a 52-file pack for a 13-episode season 2, and a ~500-file "Complete" for a 24-episode season.
func TestAPackWithFarMoreFilesThanTheSeasonIsAMultiSeasonPack(t *testing.T) {
	for _, c := range []struct{ files, season int }{{52, 13}, {500, 24}} {
		if v, why := reviewCheck(manyFiles(c.files, 1), eps(c.season, 2), eps(c.season, 2), 2); v != reviewMismatch {
			t.Fatalf("%d files for a %d-episode season: %s (%s)", c.files, c.season, v, why)
		}
	}
	// ... but a pack with a few extra files (a recap, specials) is not condemned
	if v, _ := reviewCheck(manyFiles(16, 1), eps(13, 2), eps(13, 2), 2); v == reviewMismatch {
		t.Fatal("16 files for 13 episodes is an ordinary pack")
	}
}

// The operator's rule: a torrent with data is never deleted. A multi-season mismatch goes to the seed category.
func TestAWrongPackIsMovedToTheSeedCategoryAndNeverDeleted(t *testing.T) {
	q := newFakeQbit(t, "btn", 0.5, manyFiles(12, 3)...)
	v, j := reviewEnv(t, q)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "failed" || !strings.Contains(got.Message, "were kept") || !strings.Contains(got.Message, "seed-btn") {
		t.Fatalf("got %s %q", got.Status, got.Message)
	}
	l := q.log()
	if !strings.Contains(l, "park:consolidarr-review") || !strings.Contains(l, "park:seed-btn") || strings.Contains(l, "remove") {
		t.Fatalf("qBittorrent changes: %q", l)
	}
	q.mu.Lock()
	cat := q.torrent["category"]
	q.mu.Unlock()
	if cat != "seed-btn" {
		t.Fatalf("category = %v", cat)
	}
}

func TestWithoutASeedCategoryTheTorrentStaysWhereItIsStillNotDeleted(t *testing.T) {
	q := newFakeQbit(t, "btn", 0.5, manyFiles(12, 3)...)
	delete(q.cats, "seed-btn")
	v, j := reviewEnv(t, q)
	v.e.advance(context.Background(), v.inst, v.c, live(), j, nil)
	if got := v.status(t, j.ID); got.Status != "failed" || !strings.Contains(got.Message, "left in category") {
		t.Fatalf("got %s %q", got.Status, got.Message)
	}
	if q.torrent == nil || strings.Contains(q.log(), "remove") {
		t.Fatal("the torrent must still exist")
	}
}
