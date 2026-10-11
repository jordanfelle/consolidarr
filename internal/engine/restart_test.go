package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
)

func okVerdict() verdict {
	return verdict{State: "ok", DownloadID: "d", OutputPath: "/dl/pack", Expected: 3, ExpectedEps: []int{1, 2, 3}, Items: completed(), At: time.Now()}
}

// Once shutdown has begun nothing may start deleting: the job must be exactly as it was, ready for the next start.
func TestNoReplacementStartsOnceDraining(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := readyJob(t, v)
	before := v.status(t, j.ID)
	v.e.draining.Store(true)
	v.e.replace(context.Background(), v.inst, v.c, &j, okVerdict(), completed(), "test")
	if got := v.status(t, j.ID); got.Status != before.Status || got.Deleted != 0 || len(v.fake.deleted) != 0 || v.fake.imports != 0 {
		t.Fatalf("a draining engine must not touch anything: %+v deleted=%v imports=%d", got, v.fake.deleted, v.fake.imports)
	}
	if v.e.inflight.Load() != 0 {
		t.Fatal("the in-flight counter must come back to zero")
	}
}

// A SIGTERM that lands mid-replacement must not cut it between the delete and the import, and WaitIdle must
// hold the shutdown until it is through.
func TestShutdownMidReplacementStillFinishesAndWaitIdleWaits(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := readyJob(t, v)
	idleDuring := true
	v.fake.onDelete = func() {
		v.e.draining.Store(true) // SIGTERM arrives while the delete is in flight
		idleDuring = v.e.WaitIdle(20 * time.Millisecond)
	}
	v.e.replace(context.Background(), v.inst, v.c, &j, okVerdict(), completed(), "test")
	if idleDuring {
		t.Fatal("WaitIdle must report busy while a replacement is between delete and import")
	}
	if got := v.status(t, j.ID); got.Status != "importing" || got.ImportCmd == 0 || v.fake.imports != 1 {
		t.Fatalf("the replacement must run to 'import started' despite the shutdown: %+v imports=%d", got, v.fake.imports)
	}
	if !v.e.WaitIdle(time.Second) {
		t.Fatal("WaitIdle must return once the replacement is done")
	}
}

// Every point the process can die at leaves a job a brand-new Engine (no memory of the old one) can finish.
func TestACrashBeforeTheDeleteResumesOnAFreshEngine(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := readyJob(t, v)
	now := time.Now().Unix()
	// state replace() wrote just before the delete request, then the process died
	j.Status, j.Deleted, j.ImportAt, j.DownloadID, j.Expected, j.ExpectedEps, j.ImportCmd = "importing", now, now, "d", 3, []int{1, 2, 3}, 0
	_ = v.st.UpdateJob(j)
	fresh := New(v.st)
	q := []sonarr.QueueItem{{SeriesID: 1, SeasonNumber: 3, Status: "completed", DownloadID: "d", OutputPath: "/dl/pack", Title: j.Release}}
	cur, _ := v.st.Job(j.ID)
	fresh.advance(context.Background(), v.inst, v.c, live(), cur, q)
	if after := v.status(t, j.ID); after.ImportCmd != 42 || v.fake.imports != 1 || len(v.fake.deleted) != 0 {
		t.Fatalf("a fresh engine must start the import without deleting anything again: %+v imports=%d deleted=%v", after, v.fake.imports, v.fake.deleted)
	}
}

func TestStartLogsWhatItResumes(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	_ = v.job(t, time.Time{})
	v.e.logResume()
	evs, _ := v.st.Events(10)
	for _, e := range evs {
		if strings.Contains(e.Msg, "resuming 1 active jobs") {
			return
		}
	}
	t.Fatal("a start with active jobs must say so")
}

type killed struct{}

// A SIGKILL can land at any step of a replacement. For every step: the process dies there, a brand-new engine (no
// memory of the old one) takes over from the database alone, and the season must end done, with no further delete
// and no second delete of files the first run already removed.
func TestKillNineAtEveryStepOfAReplacementIsRecovered(t *testing.T) {
	for _, stage := range []string{"before-record", "after-record", "after-delete", "after-import-start"} {
		for _, sonarrFinishedAnyway := range []bool{false, true} {
			t.Run(stage, func(t *testing.T) {
				v := setup(t, []int{1, 2, 3})
				j := readyJob(t, v)
				v.e.killpoint = func(s string) {
					if s == stage {
						panic(killed{})
					}
				}
				func() {
					defer func() {
						if r := recover(); r != nil {
							if _, ok := r.(killed); !ok {
								panic(r)
							}
						}
					}()
					v.e.replace(context.Background(), v.inst, v.c, &j, okVerdict(), completed(), "test")
				}()
				deletedBefore := len(v.fake.deleted)
				// what Sonarr did meanwhile: if the delete went through the old files are gone; if its import
				// was started (or it finished the import on its own) the new files appear
				deleteLanded := stage == "after-delete" || stage == "after-import-start" || (stage == "after-record" && sonarrFinishedAnyway)
				if deleteLanded {
					v.fake.existing = map[int]bool{}
				}
				fresh := New(v.st)
				q := []sonarr.QueueItem{{SeriesID: 1, SeasonNumber: 3, Status: "completed", DownloadID: "d", OutputPath: "/dl/pack", Title: j.Release}}
				for i := 0; i < 6; i++ {
					cur, _ := v.st.Job(j.ID)
					if cur.Status == "done" {
						break
					}
					fresh.advance(context.Background(), v.inst, v.c, live(), cur, q)
					if cur2, _ := v.st.Job(j.ID); cur2.ImportCmd != 0 || v.fake.imports > 0 {
						v.fake.seasonEps = map[int]int{1: 91, 2: 92, 3: 93} // Sonarr finishes the import
					}
				}
				got := v.status(t, j.ID)
				if got.Status != "done" {
					t.Fatalf("kill at %s: the job must recover to done, got %s %q", stage, got.Status, got.Message)
				}
				// nothing was recorded before "before-record", so the job is still an untouched 'ready' one and the new
				// engine is entitled to do the (single) replacement itself; from "after-record" on it must only resume
				if stage != "before-record" && len(v.fake.deleted) != deletedBefore {
					t.Fatalf("kill at %s: the recovering engine must never delete again (before=%d after=%d)", stage, deletedBefore, len(v.fake.deleted))
				}
				if len(v.fake.deleted) > 1 {
					t.Fatalf("kill at %s: old files must be deleted at most once, got %v", stage, v.fake.deleted)
				}
			})
		}
	}
}
