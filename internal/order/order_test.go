package order

import "testing"

var sealab = []Ep{
	{ID: 1, Season: 2, Number: 1, Title: "In The Closet", TvdbID: 101},
	{ID: 2, Season: 2, Number: 2, Title: "Stimutacs", TvdbID: 102},
	{ID: 3, Season: 2, Number: 3, Title: "Swimming in Oblivion", TvdbID: 103},
	{ID: 4, Season: 2, Number: 4, Title: "Der Dieb", TvdbID: 104},
}

func f(name string, eps ...int) File { return NewFile("/dl/p/"+name+".mp4", eps) }

func TestResolveTitlesDespiteWrongSonarrMapping(t *testing.T) {
	// Sonarr's scene mapping shifted every file by three; the titles say otherwise.
	files := []File{
		f("Sealab.2021.S02E01.In.The.Closet.HBOMAX.WEB-DL.1080p", 4),
		f("Sealab.2021.S02E02.Stimutacs.HBOMAX.WEB-DL.1080p", 5),
		f("Sealab.2021.S02E03.Swimming.in.Oblivion.HBOMAX.WEB-DL.1080p", 6),
		f("Sealab.2021.S02E04.Der.Dieb.HBOMAX.WEB-DL.1080p", 7),
	}
	if c := Conflicts(files, sealab); len(c) != 4 {
		t.Fatalf("conflicts = %v", c)
	}
	p, why := Resolve(files, sealab, map[int]bool{1: true, 2: true, 3: true, 4: true}, nil)
	if why != "" {
		t.Fatal(why)
	}
	if len(p.Files) != 4 || p.Files[files[0].Path][0] != 1 || p.Files[files[3].Path][0] != 4 || len(p.Eps) != 4 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestNoConflictWhenSonarrAgrees(t *testing.T) {
	files := []File{f("Sealab.2021.S02E01.In.The.Closet.WEB", 1), f("Sealab.2021.S02E04.Der.Dieb.WEB", 4)}
	if c := Conflicts(files, sealab); len(c) != 0 {
		t.Fatal(c)
	}
}

func TestResolveRefusesWhenAnEpisodeIsNotIdentified(t *testing.T) {
	files := []File{f("Sealab.2021.S02E01.In.The.Closet.WEB"), f("Sealab.2021.S02E02.Stimutacs.WEB")}
	_, why := Resolve(files, sealab, map[int]bool{1: true, 2: true, 3: true}, nil)
	if why == "" {
		t.Fatal("must not resolve with episode 3 unidentified")
	}
}

func TestResolveRefusesTwoFilesForOneEpisode(t *testing.T) {
	files := []File{f("Sealab.S02E01.In.The.Closet.WEB"), f("Sealab.S02E09.In.The.Closet.PROPER.WEB")}
	_, why := Resolve(files, sealab[:1], map[int]bool{1: true}, nil)
	if why == "" {
		t.Fatal("two files for one episode is ambiguous")
	}
}

func TestLongestTitleWinsAndSharedTitlesAreAmbiguous(t *testing.T) {
	eps := []Ep{{ID: 1, Title: "Pilot"}, {ID: 2, Title: "Pilot Part 2"}, {ID: 3, Title: "Twin"}, {ID: 4, Title: "Twin"}}
	if e, ok := TitleOf(f("Show.S01E02.Pilot.Part.2.WEB"), eps); !ok || e.ID != 2 {
		t.Fatalf("got %v %v", e, ok)
	}
	if _, ok := TitleOf(f("Show.S01E03.Twin.WEB"), eps); ok {
		t.Fatal("a title shared by two episodes is not evidence")
	}
}

func TestPlaceholderTitlesAreNoEvidence(t *testing.T) {
	eps := []Ep{{ID: 1, Title: "TBA"}, {ID: 2, Title: "Episode 2"}}
	if _, ok := TitleOf(f("Show.S01E01.TBA.WEB"), eps); ok {
		t.Fatal("TBA is not a title")
	}
	if _, ok := TitleOf(f("Show.S01E02.Episode.2.WEB"), eps); ok {
		t.Fatal("'Episode 2' is not a title")
	}
}

func TestTitleMustStartNearTheFront(t *testing.T) {
	eps := []Ep{{ID: 1, Title: "Tires"}}
	if _, ok := TitleOf(f("Show.S01E01.1080p.WEB-DL.DDP2.0.H.264-Tires"), eps); ok {
		t.Fatal("a title buried in the release tags (a group name) must not match")
	}
}

func TestApostrophesAndProductionCodes(t *testing.T) {
	eps := []Ep{{ID: 9, Title: "Let 'Em Eat Corn"}, {ID: 8, Title: "Take My Siblings, Please"}}
	if e, ok := TitleOf(f("Sealab.S03E12.Let.Em.Eat.Corn.WEB"), eps); !ok || e.ID != 9 {
		t.Fatal("apostrophes must not matter")
	}
	if e, ok := TitleOf(NewFile("/dl/2e66a-Take My Siblings Please.mkv", nil), eps); !ok || e.ID != 8 {
		t.Fatal("a production code before the title must be skipped")
	}
}

func TestAltOrderWhenSonarrTitlesDiffer(t *testing.T) {
	// Sonarr calls them by other titles; the pack follows the DVD order and the DVD order's titles confirm it.
	eps := []Ep{{ID: 1, Season: 1, Number: 1, Title: "Alpha", TvdbID: 11}, {ID: 2, Season: 1, Number: 2, Title: "Beta", TvdbID: 12}}
	alt := []Alt{{Name: "DVD", Eps: []AltEp{
		{Season: 1, Number: 1, Title: "Beta (DVD cut)", TvdbID: 12},
		{Season: 1, Number: 2, Title: "Alpha (DVD cut)", TvdbID: 11},
	}}}
	files := []File{f("Show.S01E01.Beta.DVD.Cut.WEB", 1), f("Show.S01E02.Alpha.DVD.Cut.WEB", 2)}
	p, why := Resolve(files, eps, map[int]bool{1: true, 2: true}, alt)
	if why != "" {
		t.Fatal(why)
	}
	if p.Files[files[0].Path][0] != 2 || p.Files[files[1].Path][0] != 1 {
		t.Fatalf("plan = %+v", p)
	}
}

func TestAltOrderIsNotTrustedWithoutTitles(t *testing.T) {
	eps := []Ep{{ID: 1, Season: 1, Number: 1, Title: "Alpha", TvdbID: 11}}
	alt := []Alt{{Name: "DVD", Eps: []AltEp{{Season: 1, Number: 1, Title: "Alpha", TvdbID: 11}}}}
	if _, why := Resolve([]File{f("Show.S01E01.1080p.WEB")}, eps, map[int]bool{1: true}, alt); why == "" {
		t.Fatal("numbers alone are not evidence")
	}
}

func TestPrefixTitlesAndTagWordsAreNotEvidence(t *testing.T) {
	eps := []Ep{{ID: 1, Season: 1, Number: 1, Title: "Home"}, {ID: 2, Season: 1, Number: 2, Title: "Home Is Where the Heart Is (2)"}, {ID: 3, Season: 1, Number: 3, Title: "Web"}, {ID: 4, Season: 1, Number: 4, Title: "Monster"}}
	for _, name := range []string{
		"Show.S01E02.Home.Is.Where.the.Heart.Is.Part.2.1080p",
		"Show.S01E04.1080p.WEB.h264-GRP",
	} {
		if e, ok := TitleOf(f(name), eps); ok && (e.ID == 1 || e.ID == 3) {
			t.Fatalf("%s must not match %q", name, e.Title)
		}
	}
	if c := Conflicts([]File{f("Show.S01E02.Home.Is.Where.the.Heart.Is.Part.2.1080p", 2), f("Show.S01E04.1080p.WEB.h264-GRP", 4)}, eps); len(c) != 0 {
		t.Fatalf("correctly mapped files must never be conflicts: %v", c)
	}
	if _, ok := TitleOf(NewFile("/dl/[Grp] Monster - 02 [1080p].mkv", nil), eps); ok {
		t.Fatal("a series name followed by an episode number is not an episode title")
	}
}

func TestExtrasMultiEpisodeAndNotNeededFilesAreNeverPlaced(t *testing.T) {
	eps := []Ep{{ID: 1, Season: 1, Number: 1, Title: "Home"}, {ID: 2, Season: 1, Number: 2, Title: "Away"}}
	need := map[int]bool{1: true}
	// The pack lacks "Home" itself but has an episode that merely starts with its title, plus a multi-episode file.
	if _, why := Resolve([]File{f("Show.S01E02.Home.Is.Where.the.Heart.Is.Part.2.mkv"), f("Show.S01E01E02.Home.1080p.WEB.mkv")}, eps, need, nil); why == "" {
		t.Fatal("must refuse: nothing in the pack is identified as Home")
	}
	// A file for an episode that has no file today is left alone, never imported.
	p, why := Resolve([]File{f("Show.S01E01.Home.1080p.WEB.mkv"), f("Show.S01E02.Away.1080p.WEB.mkv")}, eps, need, nil)
	if why != "" || len(p.Files) != 1 || len(p.Spare) != 1 {
		t.Fatalf("only the episode you have is replaced: %+v %q", p, why)
	}
}

func TestTailOfALongerTitleIsNotTheShorterTitle(t *testing.T) {
	eps := []Ep{{ID: 3, Season: 1, Number: 3, Title: "Revenge"}, {ID: 8, Season: 1, Number: 8, Title: "Sweet Revenge (1)"}}
	if _, why := Resolve([]File{f("Show.S01E08.Sweet.Revenge.720p.WEB", 8)}, eps, map[int]bool{3: true}, nil); why == "" {
		t.Fatal("a file for another episode must not be placed on the one you have")
	}
	if c := Conflicts([]File{f("Show.S01E08.Sweet.Revenge.720p.WEB", 8)}, eps); len(c) != 0 {
		t.Fatalf("not a conflict: %v", c)
	}
	// ordinary words that are also tags in some releases do not end a title
	if _, ok := TitleOf(f("Show.S01E01.Code.Red.1080p.WEB"), []Ep{{ID: 1, Title: "Code"}}); ok {
		t.Fatal("'Red' is a word, not a tag")
	}
}

func TestFileFlagsNoSxEAndMulti(t *testing.T) {
	if f("Show.S00E01.Unaired.Pilot.720p").HasSxE != true || f("Show.2x05.Halloween.avi").HasSxE {
		t.Fatal("HasSxE must tell 'special' from 'no number'")
	}
	eps := []Ep{{ID: 1, Season: 1, Number: 1, Title: "Bee Story", TvdbID: 12}}
	alt := []Alt{{Name: "DVD", Eps: []AltEp{{Season: 1, Number: 1, Title: "Bee Story", TvdbID: 12}}}}
	if _, why := Resolve([]File{f("Show.S01E01E02.Bee.Story.720p.mkv", 1, 2)}, eps, map[int]bool{1: true}, alt); why == "" {
		t.Fatal("a multi-episode file must not be placed through an alternative order either")
	}
}
