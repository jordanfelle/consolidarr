package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jordanfelle/consolidarr/internal/sonarr"
	"github.com/jordanfelle/consolidarr/internal/store"
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

func queueFor(j store.Job) []sonarr.QueueItem {
	return []sonarr.QueueItem{{SeriesID: 1, SeasonNumber: 3, Status: "completed", DownloadID: "d", OutputPath: "/dl/pack", Title: j.Release}}
}

// runKilled runs fn and swallows the simulated process death (a panic at a kill point).
func runKilled(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(killed); !ok {
				panic(r)
			}
		}
	}()
	fn()
}

// resume is a brand-new engine (no memory of the dead one) working a job from the database alone.
func resume(t *testing.T, v *env, j store.Job) store.Job {
	t.Helper()
	fresh := New(v.st)
	for i := 0; i < 6; i++ {
		cur, _ := v.st.Job(j.ID)
		if cur.Status == "done" || cur.Status == "failed" {
			break
		}
		fresh.advance(context.Background(), v.inst, v.c, live(), cur, queueFor(j))
	}
	return v.status(t, j.ID)
}

// A SIGKILL, OOM or power cut can land at any step of a replacement. Sonarr is modelled honestly: a delete removes
// the files, an import is refused while the old files are still there. For every step the replacement must finish
// from the database alone with the old files deleted exactly once and ONE import.
func TestKillNineAtEveryStepOfAReplacementIsRecovered(t *testing.T) {
	for _, stage := range []string{"before-record", "after-record", "after-delete", "after-import-start"} {
		for _, deleteLanded := range []bool{false, true} {
			if deleteLanded && stage != "after-record" {
				continue // stages after the delete always had it land
			}
			name := stage
			if stage == "after-record" {
				name += map[bool]string{false: "/delete-never-sent", true: "/delete-reached-sonarr"}[deleteLanded]
			}
			t.Run(name, func(t *testing.T) {
				v := setup(t, []int{1, 2, 3})
				v.fake.realistic = true
				j := readyJob(t, v)
				v.e.killpoint = func(s string) {
					if s == stage {
						if deleteLanded { // the request was on the wire: Sonarr carried it out although we died
							v.fake.mu.Lock()
							v.fake.existing = map[int]bool{}
							for n := range v.fake.seasonEps {
								v.fake.seasonEps[n] = 0
							}
							v.fake.mu.Unlock()
						}
						panic(killed{})
					}
				}
				runKilled(func() { v.e.replace(context.Background(), v.inst, v.c, &j, okVerdict(), completed(), "test") })
				got := resume(t, v, j)
				if got.Status != "done" {
					t.Fatalf("kill at %s: must recover to done, got %s %q", name, got.Status, got.Message)
				}
				if len(v.fake.deleted) > 1 {
					t.Fatalf("kill at %s: old files deleted more than once: %v", name, v.fake.deleted)
				}
				if v.fake.imports != 1 || v.fake.rejected != 0 {
					t.Fatalf("kill at %s: want exactly one import and none refused, got imports=%d refused=%d", name, v.fake.imports, v.fake.rejected)
				}
				if len(v.fake.existing) != 0 {
					t.Fatalf("kill at %s: old files must be gone at the end: %v", name, v.fake.existing)
				}
			})
		}
	}
}

// Killed inside finishImport, after Sonarr accepted the import but before its id was saved: the resumed job must
// find that command and not start a second scan.
func TestKillBetweenImportAcceptedAndSavedDoesNotImportTwice(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.realistic = true
	j := readyJob(t, v)
	now := time.Now().Unix()
	j.Status, j.Deleted, j.ImportAt, j.DownloadID, j.Expected, j.ExpectedEps, j.ImportCmd = "importing", now, now, "d", 3, []int{1, 2, 3}, 0
	_ = v.st.UpdateJob(j)
	v.fake.existing = map[int]bool{} // the delete had already landed
	for n := range v.fake.seasonEps {
		v.fake.seasonEps[n] = 0
	}
	first := New(v.st)
	first.killpoint = func(s string) {
		if s == "fi-after-import-start" {
			panic(killed{})
		}
	}
	runKilled(func() {
		cur, _ := v.st.Job(j.ID)
		first.advance(context.Background(), v.inst, v.c, live(), cur, queueFor(j))
	})
	if v.fake.imports != 1 {
		t.Fatalf("setup: the first run should have started one import, got %d", v.fake.imports)
	}
	v.fake.mu.Lock()
	for n := range v.fake.seasonEps {
		v.fake.seasonEps[n] = 0 // Sonarr is still working on the import we started
	}
	v.fake.mu.Unlock()
	fresh := New(v.st)
	cur, _ := v.st.Job(j.ID)
	fresh.advance(context.Background(), v.inst, v.c, live(), cur, queueFor(j))
	after := v.status(t, j.ID)
	if v.fake.imports != 1 || after.ImportCmd == 0 {
		t.Fatalf("a restart must adopt the import Sonarr already has, not start another: imports=%d cmd=%d %q", v.fake.imports, after.ImportCmd, after.Message)
	}
	v.fake.mu.Lock()
	for _, n := range []int{1, 2, 3} {
		v.fake.seasonEps[n] = 900 + n // the import completes
	}
	v.fake.mu.Unlock()
	if got := resume(t, v, j); got.Status != "done" || v.fake.imports != 1 {
		t.Fatalf("job should finish with the single import: %s imports=%d", got.Status, v.fake.imports)
	}
}

// If the write-ahead record cannot be saved, nothing may be deleted: a crash after an unrecorded delete loses the job.
func TestFailedWriteAheadRecordMeansNothingIsDeleted(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	j := readyJob(t, v)
	before := v.status(t, j.ID)
	if _, err := v.st.Exec(`create trigger no_update before update on jobs begin select raise(abort, 'disk full'); end`); err != nil {
		t.Skip("sqlite triggers unavailable: ", err)
	}
	v.e.replace(context.Background(), v.inst, v.c, &j, okVerdict(), completed(), "test")
	if len(v.fake.deleted) != 0 || v.fake.imports != 0 {
		t.Fatalf("an unrecorded replacement must not touch Sonarr: deleted=%v imports=%d", v.fake.deleted, v.fake.imports)
	}
	if j.Status != before.Status {
		t.Fatalf("the in-memory job must be rolled back, got %s want %s", j.Status, before.Status)
	}
}

// The import command is lost (Sonarr restarted / host was down) while the pack is still queued: ask again instead of
// leaving a human to click Retry, but only a couple of times.
func TestLostImportCommandIsReissuedThenGivesUp(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.realistic = true
	j := readyJob(t, v)
	long := time.Now().Add(-2 * time.Hour).Unix()
	j.Status, j.Deleted, j.ImportAt, j.DownloadID, j.Expected, j.ExpectedEps, j.ImportCmd = "importing", long, long, "d", 3, []int{1, 2, 3}, 77
	_ = v.st.UpdateJob(j)
	v.fake.existing = map[int]bool{}
	for n := range v.fake.seasonEps {
		v.fake.seasonEps[n] = 0
	}
	cur, _ := v.st.Job(j.ID)
	New(v.st).advance(context.Background(), v.inst, v.c, live(), cur, queueFor(j)) // command 77 is unknown to Sonarr now
	cur = v.status(t, j.ID)
	if cur.Status != "importing" || cur.ImportCmd != 0 {
		t.Fatalf("a lost import command must be reissued, got %s cmd=%d %q", cur.Status, cur.ImportCmd, cur.Message)
	}
	if got := resume(t, v, j); got.Status != "done" {
		t.Fatalf("the reissued import should finish the job, got %s %q", got.Status, got.Message)
	}
}

func seedScan(v *env, id int, status string, queuedAgo time.Duration) {
	v.fake.mu.Lock()
	defer v.fake.mu.Unlock()
	v.fake.scans = append(v.fake.scans, map[string]any{"id": id, "name": "DownloadedEpisodesScan", "status": status,
		"queued": time.Now().Add(-queuedAgo).UTC().Format(time.RFC3339Nano), "body": map[string]string{"path": "/dl/pack", "downloadClientId": "d"}})
}

func importingNoFiles(t *testing.T, v *env, importAgo time.Duration, cmd int) store.Job {
	t.Helper()
	j := readyJob(t, v)
	at := time.Now().Add(-importAgo).Unix()
	j.Status, j.Deleted, j.ImportAt, j.DownloadID, j.Expected, j.ExpectedEps, j.ImportCmd = "importing", at, at, "d", 3, []int{1, 2, 3}, cmd
	_ = v.st.UpdateJob(j)
	v.fake.existing = map[int]bool{} // the delete had landed
	for n := range v.fake.seasonEps {
		v.fake.seasonEps[n] = 0
	}
	return j
}

// An older scan of the same folder (an earlier attempt, a manual scan) is not this attempt's: it must not be adopted,
// or a needed import would never be sent.
func TestAnOlderScanOfTheSameFolderIsNotAdopted(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.realistic = true
	j := importingNoFiles(t, v, 0, 0)
	seedScan(v, 7, "failed", 3*time.Hour)
	cur, _ := v.st.Job(j.ID)
	New(v.st).advance(context.Background(), v.inst, v.c, live(), cur, queueFor(j))
	if got := v.status(t, j.ID); v.fake.imports != 1 || got.ImportCmd == 7 {
		t.Fatalf("a stale failed scan must be ignored and a fresh import sent: imports=%d cmd=%d", v.fake.imports, got.ImportCmd)
	}
}

// The case the re-ask exists for: the command finished with episodes missing and Sonarr still lists it. The re-ask
// must send a NEW import, not re-adopt the finished one.
func TestReaskSendsANewImportEvenWhileSonarrStillListsTheOldCommand(t *testing.T) {
	v := setup(t, []int{1, 2, 3})
	v.fake.realistic = true
	v.fake.cmdStatus = "failed"
	j := importingNoFiles(t, v, 2*time.Hour, 42)
	seedScan(v, 42, "failed", 2*time.Hour+time.Minute)
	fresh := New(v.st)
	cur, _ := v.st.Job(j.ID)
	fresh.advance(context.Background(), v.inst, v.c, live(), cur, queueFor(j)) // re-ask: ImportCmd back to 0
	if got := v.status(t, j.ID); got.ImportCmd != 0 || v.fake.imports != 0 {
		t.Fatalf("first the job must reset for a re-ask: cmd=%d imports=%d %q", got.ImportCmd, v.fake.imports, got.Message)
	}
	cur, _ = v.st.Job(j.ID)
	fresh.advance(context.Background(), v.inst, v.c, live(), cur, queueFor(j)) // ...then ask Sonarr again
	if got := v.status(t, j.ID); v.fake.imports != 1 || got.ImportCmd == 0 || got.ImportCmd == 42 {
		t.Fatalf("the re-ask must send a new import: imports=%d cmd=%d", v.fake.imports, got.ImportCmd)
	}
}
