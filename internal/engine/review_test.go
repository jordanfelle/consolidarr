package engine

import (
	"testing"

	"github.com/jordanfelle/consolidarr/internal/order"
)

// Real file names: the Tenchi Muyo! Ryo-Ohki pack that was grabbed for season 4 but is the "2nd Season" (Sonarr S02).
func tenchiEps() (target, all []order.Ep) {
	s2 := []string{"Hello Baby", "Sasami and Tsunami", "I Love Tenchi", "The Advent of the Goddess", "Zero Ryoko", "Here Comes Jurai"}
	for i, t := range s2 {
		all = append(all, order.Ep{ID: 20 + i, Season: 2, Number: i + 1, Title: t})
	}
	target = []order.Ep{{ID: 40, Season: 4, Number: 1, Title: "The Night-Before Party"}, {ID: 41, Season: 4, Number: 2, Title: "The Fate of the Masaki"}}
	return target, append(all, target...)
}

func TestAPackOfAnotherSeasonIsAMismatchByEpisodeTitle(t *testing.T) {
	target, all := tenchiEps()
	names := []string{
		"[ZetaRebel] Tenchi Muyo! Ryo-Ohki 2nd Season (REMUX AVC 1080p TrueHD)/[ZetaRebel] Tenchi Muyo! Ryo-Ohki 2nd Season - 01 - Hello Baby! (REMUX AVC 1080p TrueHD).mkv.!qB",
		"[ZetaRebel] Tenchi Muyo! Ryo-Ohki 2nd Season (REMUX AVC 1080p TrueHD)/[ZetaRebel] Tenchi Muyo! Ryo-Ohki 2nd Season - 02 - Sasami and Tsunami (REMUX AVC 1080p TrueHD).mkv.!qB",
		"[ZetaRebel] Tenchi Muyo! Ryo-Ohki 2nd Season (REMUX AVC 1080p TrueHD)/[ZetaRebel] Tenchi Muyo! Ryo-Ohki 2nd Season - 03 - I Love Tenchi (REMUX AVC 1080p TrueHD).mkv.!qB",
		"[ZetaRebel] Tenchi Muyo! Ryo-Ohki 2nd Season (REMUX AVC 1080p TrueHD)/[ZetaRebel] Tenchi Muyo! Ryo-Ohki 2nd Season - 04 - The Advent of the Goddess (REMUX AVC 1080p TrueHD).mkv.!qB",
	}
	if v, why := reviewCheck(names, target, all, 4); v != reviewMismatch {
		t.Fatalf("verdict %s (%s), want mismatch", v, why)
	}
}

func TestAPackNamedForTheSeasonMatches(t *testing.T) {
	var names []string
	for _, n := range []string{"01", "02", "03"} {
		names = append(names, "Medalist.S02.1080p.BluRay/Medalist.S02E"+n+".1080p.BluRay.Remux.AVC.Dual-Audio.FLAC.2.0-Sylvar.mkv")
	}
	target := []order.Ep{{ID: 1, Season: 2, Number: 1, Title: "A"}, {ID: 2, Season: 2, Number: 2, Title: "B"}, {ID: 3, Season: 2, Number: 3, Title: "C"}}
	if v, why := reviewCheck(names, target, target, 2); v != reviewMatch {
		t.Fatalf("verdict %s (%s), want match", v, why)
	}
	if v, _ := reviewCheck(names, target, target, 3); v != reviewUnknown {
		t.Fatalf("S02 numbering for season 3 may just be scene numbering: a human decides, got %s", v)
	}
}

// A relabelled pack is "S01" by definition, so S01 in its file names must never condemn it for season 2+.
func TestS01FileNamesSayNothingAboutALaterSeason(t *testing.T) {
	names := []string{"P/Show.S01E01.1080p.mkv", "P/Show.S01E02.1080p.mkv"}
	target := []order.Ep{{ID: 1, Season: 3, Number: 1, Title: "Alpha Episode"}, {ID: 2, Season: 3, Number: 2, Title: "Beta Episode"}}
	if v, why := reviewCheck(names, target, target, 3); v != reviewUnknown {
		t.Fatalf("verdict %s (%s), want unknown", v, why)
	}
}

func TestAPackWithTooFewFilesOrNoNamesNeedsAHuman(t *testing.T) {
	target := []order.Ep{{ID: 1, Season: 3, Number: 1, Title: "Alpha Episode"}, {ID: 2, Season: 3, Number: 2, Title: "Beta Episode"}}
	if v, _ := reviewCheck([]string{"p/one.mkv"}, target, target, 3); v != reviewUnknown {
		t.Fatalf("fewer files than episodes proves nothing (specials, merged episodes): %s", v)
	}
	if v, _ := reviewCheck([]string{"p/01.mkv", "p/02.mkv"}, target, target, 3); v != reviewUnknown {
		t.Fatalf("names that say nothing: %s", v)
	}
	if v, _ := reviewCheck([]string{"p/readme.txt", "p/Extras/01.mkv"}, target, target, 3); v != reviewUnknown {
		t.Fatalf("no real video: %s", v)
	}
}
