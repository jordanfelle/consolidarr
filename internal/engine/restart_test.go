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
