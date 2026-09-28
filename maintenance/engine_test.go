package maintenance_test

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chris64233/go-maintenance-window/maintenance"
)

var base = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

// fakeClock is a goroutine-safe manually controlled clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type countingNotifier struct {
	mu      sync.Mutex
	records []maintenance.Record
}

func (n *countingNotifier) Notify(r maintenance.Record) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.records = append(n.records, r)
}

func (n *countingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.records)
}

func newEngine(t *testing.T, clock *fakeClock, n maintenance.Notifier) *maintenance.Engine {
	t.Helper()
	opts := []maintenance.Option{maintenance.WithClock(clock.now)}
	if n != nil {
		opts = append(opts, maintenance.WithNotifier(n))
	}
	eng, err := maintenance.NewEngine(nil, opts...)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	for _, id := range []string{"r1", "r2", "r3"} {
		if err := eng.RegisterResource(id, "resource "+id); err != nil {
			t.Fatalf("RegisterResource(%s): %v", id, err)
		}
	}
	return eng
}

func register(t *testing.T, eng *maintenance.Engine, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := eng.RegisterResource(id, id); err != nil {
			t.Fatalf("RegisterResource(%s): %v", id, err)
		}
	}
}

func mustCreate(t *testing.T, eng *maintenance.Engine, spec maintenance.RequestSpec) *maintenance.Request {
	t.Helper()
	r, err := eng.CreateRequest(spec)
	if err != nil {
		t.Fatalf("CreateRequest(%s): %v", spec.ID, err)
	}
	return r
}

func mustModify(t *testing.T, eng *maintenance.Engine, id string, spec maintenance.ModifySpec) *maintenance.Request {
	t.Helper()
	r, err := eng.ModifyRequest(id, spec)
	if err != nil {
		t.Fatalf("ModifyRequest(%s): %v", id, err)
	}
	return r
}

func recordsFor(records []maintenance.Record, id string) int {
	n := 0
	for _, r := range records {
		if r.RequestID == id {
			n++
		}
	}
	return n
}

func notesFor(n *countingNotifier, id string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, r := range n.records {
		if r.RequestID == id {
			c++
		}
	}
	return c
}

func TestCreateRequestValidation(t *testing.T) {
	clock := &fakeClock{t: base}
	eng := newEngine(t, clock, nil)

	_, err := eng.CreateRequest(maintenance.RequestSpec{ID: "unknown-resource", Resources: []string{"rX"}, Start: base.Add(time.Hour), Duration: time.Hour})
	if !errors.Is(err, maintenance.ErrResourceNotFound) {
		t.Fatalf("unknown resource: want ErrResourceNotFound, got %v", err)
	}

	if _, err := eng.CreateRequest(maintenance.RequestSpec{ID: "no-resources", Start: base.Add(time.Hour), Duration: time.Hour}); err == nil {
		t.Fatalf("empty resource list: expected an error, got nil")
	}

	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "mw-1", Resources: []string{"r1"},
		Start: base.Add(time.Hour), Duration: time.Hour,
	})
	if _, err := eng.CreateRequest(maintenance.RequestSpec{
		ID: "mw-1", Resources: []string{"r2"},
		Start: base.Add(2 * time.Hour), Duration: time.Hour,
	}); !errors.Is(err, maintenance.ErrRequestExists) {
		t.Fatalf("duplicate id: want ErrRequestExists, got %v", err)
	}

	// Zero duration.
	if _, err := eng.CreateRequest(maintenance.RequestSpec{
		ID: "mw-bad-d", Resources: []string{"r2"},
		Start: base.Add(time.Hour), Duration: 0,
	}); !errors.Is(err, maintenance.ErrInvalidWindow) {
		t.Fatalf("zero duration: want ErrInvalidWindow, got %v", err)
	}

	// Window ending before now.
	if _, err := eng.CreateRequest(maintenance.RequestSpec{
		ID: "mw-past", Resources: []string{"r2"},
		Start: base.Add(-2 * time.Hour), Duration: time.Hour,
	}); !errors.Is(err, maintenance.ErrInvalidWindow) {
		t.Fatalf("elapsed window: want ErrInvalidWindow, got %v", err)
	}

	// Overlap on the same resource is rejected.
	if _, err := eng.CreateRequest(maintenance.RequestSpec{
		ID: "mw-overlap", Resources: []string{"r1"},
		Start: base.Add(90 * time.Minute), Duration: time.Hour, // 11:30-12:30 vs 11:00-12:00
	}); !errors.Is(err, maintenance.ErrWindowOverlap) {
		t.Fatalf("overlapping window: want ErrWindowOverlap, got %v", err)
	}

	// Adjacent windows (half-open intervals) are allowed.
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "mw-adj", Resources: []string{"r1"},
		Start: base.Add(2 * time.Hour), Duration: time.Hour,
	})
	// Same time on a different resource is allowed.
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "mw-other", Resources: []string{"r2", "r3"},
		Start: base.Add(time.Hour), Duration: time.Hour,
	})

	// Duplicate resources in the spec are normalized, not rejected.
	r := mustCreate(t, eng, maintenance.RequestSpec{
		ID: "mw-dup-rsrc", Resources: []string{"r2", "r2"},
		Start: base.Add(3 * time.Hour), Duration: time.Hour,
	})
	if len(r.Resources) != 1 || r.Resources[0] != "r2" {
		t.Fatalf("duplicate resources not normalized: %v", r.Resources)
	}
}

func TestCreateDoesNotChangeResourceState(t *testing.T) {
	clock := &fakeClock{t: base}
	eng := newEngine(t, clock, nil)

	// A window without preparation steps becomes READY at once, but the
	// resource must remain free until Start.
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "mw-1", Resources: []string{"r1"},
		Start: base, Duration: time.Hour,
	})
	for _, res := range eng.Resources() {
		if res.OccupiedBy != "" {
			t.Fatalf("resource %s occupied before start: %q", res.ID, res.OccupiedBy)
		}
	}
	if err := eng.Start("mw-1"); err != nil {
		t.Fatalf("Start at scheduled time: %v", err)
	}
	occupied := map[string]string{}
	for _, res := range eng.Resources() {
		occupied[res.ID] = res.OccupiedBy
	}
	if occupied["r1"] != "mw-1" {
		t.Fatalf("r1 should be held by mw-1, got %q", occupied["r1"])
	}
	if occupied["r2"] != "" || occupied["r3"] != "" {
		t.Fatalf("unrelated resources were touched: %v", occupied)
	}
}

func TestStartTooEarlyLeavesResourcesUntouched(t *testing.T) {
	clock := &fakeClock{t: base}
	eng := newEngine(t, clock, nil)
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "mw-future", Resources: []string{"r1", "r2"},
		Start: base.Add(time.Hour), Duration: time.Hour,
	})
	if err := eng.Start("mw-future"); !errors.Is(err, maintenance.ErrTooEarly) {
		t.Fatalf("early start: want ErrTooEarly, got %v", err)
	}
	for _, res := range eng.Resources() {
		if res.OccupiedBy != "" {
			t.Fatalf("resource %s occupied after rejected early start", res.ID)
		}
	}
	r, _ := eng.Request("mw-future")
	if r.Status != maintenance.StatusReady {
		t.Fatalf("rejected early start changed status: %s", r.Status)
	}
}

func TestPrepReceiptsOutOfOrderDuplicatesAndVersion(t *testing.T) {
	clock := &fakeClock{t: base}
	eng := newEngine(t, clock, nil)
	r := mustCreate(t, eng, maintenance.RequestSpec{
		ID: "mw-1", Resources: []string{"r1"},
		Start: base.Add(time.Hour), Duration: time.Hour,
		PrepSteps: []string{"backup", "notify"},
	})
	if r.Version != 1 || r.Status != maintenance.StatusPending {
		t.Fatalf("new request: v%d status %s", r.Version, r.Status)
	}

	// Out of order.
	if err := eng.ReportPrep("mw-1", 1, "notify"); err != nil {
		t.Fatalf("ReportPrep: %v", err)
	}
	// Duplicate.
	if err := eng.ReportPrep("mw-1", 1, "notify"); err != nil {
		t.Fatalf("duplicate receipt: %v", err)
	}
	if r, _ := eng.Request("mw-1"); r.Status != maintenance.StatusPending {
		t.Fatalf("partial receipts must not make it ready, got %s", r.Status)
	}
	// Unknown step rejected.
	if err := eng.ReportPrep("mw-1", 1, "nope"); !errors.Is(err, maintenance.ErrUnknownStep) {
		t.Fatalf("unknown step: want ErrUnknownStep, got %v", err)
	}
	// Final step of v1.
	if err := eng.ReportPrep("mw-1", 1, "backup"); err != nil {
		t.Fatalf("ReportPrep: %v", err)
	}
	if r, _ := eng.Request("mw-1"); r.Status != maintenance.StatusReady {
		t.Fatalf("want READY after all v1 receipts, got %s", r.Status)
	}

	// Modify the window: version bumps and receipts are reset.
	r = mustModify(t, eng, "mw-1", maintenance.ModifySpec{
		Resources: []string{"r1", "r2"},
		Start:     base.Add(2 * time.Hour),
		Duration:  time.Hour,
	})
	if r.Version != 2 || r.Status != maintenance.StatusPending || len(r.DoneSteps) != 0 {
		t.Fatalf("after modify: v%d status %s done=%v", r.Version, r.Status, r.DoneSteps)
	}

	// A late v1 receipt must be ignored and must not advance v2.
	if err := eng.ReportPrep("mw-1", 1, "backup"); err != nil {
		t.Fatalf("stale receipt should be ignored, got %v", err)
	}
	if r, _ := eng.Request("mw-1"); r.Status != maintenance.StatusPending {
		t.Fatalf("stale v1 receipt advanced v2: %s", r.Status)
	}
	// A future-version receipt is ignored as well.
	if err := eng.ReportPrep("mw-1", 3, "backup"); err != nil {
		t.Fatalf("future-version receipt should be ignored, got %v", err)
	}
	// Only the full v2 set makes it ready.
	if err := eng.ReportPrep("mw-1", 2, "backup"); err != nil {
		t.Fatalf("ReportPrep: %v", err)
	}
	if r, _ := eng.Request("mw-1"); r.Status != maintenance.StatusPending {
		t.Fatalf("one of two v2 receipts made it ready: %s", r.Status)
	}
	if err := eng.ReportPrep("mw-1", 2, "notify"); err != nil {
		t.Fatalf("ReportPrep: %v", err)
	}
	if r, _ := eng.Request("mw-1"); r.Status != maintenance.StatusReady {
		t.Fatalf("want READY after all v2 receipts, got %s", r.Status)
	}
}

func TestModifyValidation(t *testing.T) {
	clock := &fakeClock{t: base}
	eng := newEngine(t, clock, nil)
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "mw-a", Resources: []string{"r1"},
		Start: base.Add(time.Hour), Duration: time.Hour,
	})
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "mw-b", Resources: []string{"r2"},
		Start: base.Add(3 * time.Hour), Duration: time.Hour,
	})

	// Moving mw-a onto r2 during mw-b's window is an overlap.
	if _, err := eng.ModifyRequest("mw-a", maintenance.ModifySpec{
		Resources: []string{"r2"},
		Start:     base.Add(3 * time.Hour),
		Duration:  time.Hour,
	}); !errors.Is(err, maintenance.ErrWindowOverlap) {
		t.Fatalf("modify into overlap: want ErrWindowOverlap, got %v", err)
	}

	// A running request cannot be modified.
	clock.advance(time.Hour)
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "mw-c", Resources: []string{"r3"},
		Start: base.Add(time.Hour), Duration: 2 * time.Hour,
	})
	if err := eng.Start("mw-c"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := eng.ModifyRequest("mw-c", maintenance.ModifySpec{
		Resources: []string{"r3"},
		Start:     base.Add(2 * time.Hour),
		Duration:  time.Hour,
	}); !errors.Is(err, maintenance.ErrInvalidState) {
		t.Fatalf("modify running: want ErrInvalidState, got %v", err)
	}
}

func TestStartAcquiresAllOrNothingAndRollsBack(t *testing.T) {
	clock := &fakeClock{t: base}
	eng := newEngine(t, clock, nil)

	// A holds r2 during [10:00,11:00) and is running.
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "a", Resources: []string{"r2"},
		Start: base, Duration: time.Hour,
	})
	if err := eng.Start("a"); err != nil {
		t.Fatalf("Start a: %v", err)
	}

	// B needs r1 and r2 during [11:00,12:00): creation succeeds (schedules do
	// not overlap), but when B tries to start at 11:01 A has overrun its
	// window and still holds r2 (not yet swept).
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "b", Resources: []string{"r1", "r2"},
		Start: base.Add(time.Hour), Duration: time.Hour,
	})
	clock.advance(time.Hour + time.Minute)

	if err := eng.Start("b"); !errors.Is(err, maintenance.ErrResourceBusy) {
		t.Fatalf("Start b: want ErrResourceBusy, got %v", err)
	}
	// Rollback: the partially acquired r1 must not stay locked.
	free := map[string]string{}
	for _, res := range eng.Resources() {
		free[res.ID] = res.OccupiedBy
	}
	if free["r1"] != "" {
		t.Fatalf("r1 left locked after failed start: %q (full rollback required)", free["r1"])
	}
	if free["r2"] != "a" {
		t.Fatalf("r2 should still be held by a, got %q", free["r2"])
	}
	r, _ := eng.Request("b")
	if r.Status != maintenance.StatusReady {
		t.Fatalf("failed start changed b's status: %s", r.Status)
	}

	// Timeout sweep finalizes a and releases r2; then b acquires everything.
	if err := eng.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if err := eng.Start("b"); err != nil {
		t.Fatalf("Start b after sweep: %v", err)
	}
	got := map[string]string{}
	for _, res := range eng.Resources() {
		got[res.ID] = res.OccupiedBy
	}
	if got["r1"] != "b" || got["r2"] != "b" {
		t.Fatalf("after start b: %v", got)
	}
}

func TestConcurrentStartVsFinalize(t *testing.T) {
	clock := &fakeClock{t: base}
	notes := &countingNotifier{}
	eng := newEngine(t, clock, notes)

	// A on {r1,r2} running [10:00,11:00); B on {r2,r3} ready [11:00,12:00).
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "a", Resources: []string{"r1", "r2"},
		Start: base, Duration: time.Hour,
	})
	if err := eng.Start("a"); err != nil {
		t.Fatalf("Start a: %v", err)
	}
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "b", Resources: []string{"r2", "r3"},
		Start: base.Add(time.Hour), Duration: time.Hour,
	})
	clock.advance(time.Hour + time.Minute)

	// Concurrently: a may be completed, aborted, or swept by the timeout, and
	// b keeps trying to start. Exactly one finalization of a is allowed.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(4)
		go func() { defer wg.Done(); _ = eng.Complete("a", "completed by operator") }()
		go func() { defer wg.Done(); _ = eng.Abort("a", "aborted by operator") }()
		go func() { defer wg.Done(); _ = eng.Tick() }()
		go func() {
			defer wg.Done()
			for try := 0; try < 1000; try++ {
				if err := eng.Start("b"); err == nil {
					return
				}
				time.Sleep(time.Microsecond)
			}
		}()
	}
	wg.Wait()

	if n := recordsFor(eng.History(), "a"); n != 1 {
		t.Fatalf("a finalized %d times, want exactly 1", n)
	}
	if n := notesFor(notes, "a"); n != 1 {
		t.Fatalf("a notified %d times, want exactly 1", n)
	}
	rb, err := eng.Request("b")
	if err != nil || rb.Status != maintenance.StatusRunning {
		t.Fatalf("b should eventually be RUNNING, got status=%v err=%v", rb, err)
	}
	for _, res := range eng.Resources() {
		switch res.ID {
		case "r1":
			if res.OccupiedBy != "" {
				t.Fatalf("r1 not released after a finalized: %q", res.OccupiedBy)
			}
		case "r2", "r3":
			if res.OccupiedBy != "b" {
				t.Fatalf("%s: want held by b, got %q", res.ID, res.OccupiedBy)
			}
		}
	}
}

func TestFinalizeRaceProducesSingleRecord(t *testing.T) {
	clock := &fakeClock{t: base}
	notes := &countingNotifier{}
	eng := newEngine(t, clock, notes)

	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "mw-1", Resources: []string{"r1"},
		Start: base, Duration: time.Hour,
	})
	if err := eng.Start("mw-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Move past the end so Tick competes with Complete/Abort as a timeout.
	clock.advance(2 * time.Hour)

	var wg sync.WaitGroup
	ops := []func() error{
		func() error { return eng.Complete("mw-1", "finished") },
		func() error { return eng.Abort("mw-1", "cancelled") },
		func() error { return eng.Tick() },
	}
	for round := 0; round < 20; round++ {
		for _, op := range ops {
			wg.Add(1)
			go func(f func() error) { defer wg.Done(); _ = f() }(op)
		}
	}
	wg.Wait()

	if n := recordsFor(eng.History(), "mw-1"); n != 1 {
		t.Fatalf("history records for mw-1: %d, want 1", n)
	}
	if notes.count() != 1 {
		t.Fatalf("notifications: %d, want 1", notes.count())
	}
	r, _ := eng.Request("mw-1")
	if !r.Status.Final() {
		t.Fatalf("status: %s, want final", r.Status)
	}
	if r.Reason == "" {
		t.Fatalf("final reason must be recorded")
	}
	for _, res := range eng.Resources() {
		if res.OccupiedBy != "" {
			t.Fatalf("resource %s still occupied: %q", res.ID, res.OccupiedBy)
		}
	}
	// Second sweep must not add another record.
	if err := eng.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n := recordsFor(eng.History(), "mw-1"); n != 1 {
		t.Fatalf("history records after repeat Tick: %d, want 1", n)
	}
	if notes.count() != 1 {
		t.Fatalf("notifications after repeat Tick: %d, want 1", notes.count())
	}
}

func TestTickExpiresPendingAndTimesOutRunning(t *testing.T) {
	clock := &fakeClock{t: base}
	notes := &countingNotifier{}
	eng := newEngine(t, clock, notes)

	// Pending with incomplete prep steps.
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "pending", Resources: []string{"r1"},
		Start: base, Duration: 30 * time.Minute,
		PrepSteps: []string{"backup"},
	})
	// Running.
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "running", Resources: []string{"r2"},
		Start: base, Duration: time.Hour,
	})
	if err := eng.Start("running"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Ready but not yet due to start: should be untouched by the sweep.
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "future", Resources: []string{"r3"},
		Start: base.Add(2 * time.Hour), Duration: time.Hour,
	})

	clock.advance(90 * time.Minute)
	if err := eng.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	rp, _ := eng.Request("pending")
	if rp.Status != maintenance.StatusExpired {
		t.Fatalf("pending: want EXPIRED, got %s", rp.Status)
	}
	rr, _ := eng.Request("running")
	if rr.Status != maintenance.StatusAborted || rr.Reason == "" {
		t.Fatalf("running: want ABORTED with reason, got %s (%q)", rr.Status, rr.Reason)
	}
	rf, _ := eng.Request("future")
	if rf.Status != maintenance.StatusReady {
		t.Fatalf("future: want READY, got %s", rf.Status)
	}
	for _, res := range eng.Resources() {
		if res.OccupiedBy != "" {
			t.Fatalf("resource %s not released: %q", res.ID, res.OccupiedBy)
		}
	}
	if n := recordsFor(eng.History(), "pending"); n != 1 {
		t.Fatalf("pending history records: %d, want 1", n)
	}
	if n := recordsFor(eng.History(), "running"); n != 1 {
		t.Fatalf("running history records: %d, want 1", n)
	}
	if notes.count() != 2 {
		t.Fatalf("notifications: %d, want 2", notes.count())
	}
}

func TestCompleteReleasesResourcesAndRecordsReason(t *testing.T) {
	clock := &fakeClock{t: base}
	eng := newEngine(t, clock, nil)
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "mw-1", Resources: []string{"r1", "r2"},
		Start: base, Duration: time.Hour,
	})
	if err := eng.Start("mw-1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.Complete("mw-1", "all checks passed"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for _, res := range eng.Resources() {
		if res.OccupiedBy != "" {
			t.Fatalf("resource %s still occupied: %q", res.ID, res.OccupiedBy)
		}
	}
	rec := eng.History()[0]
	if rec.Outcome != maintenance.StatusCompleted || rec.Reason != "all checks passed" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	// Cannot complete twice.
	if err := eng.Complete("mw-1", "again"); !errors.Is(err, maintenance.ErrInvalidState) {
		t.Fatalf("double complete: want ErrInvalidState, got %v", err)
	}
}

func TestCalendarQuery(t *testing.T) {
	clock := &fakeClock{t: base}
	eng := newEngine(t, clock, nil)
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "w1", Resources: []string{"r1"},
		Start: base.Add(time.Hour), Duration: time.Hour,
	})
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "w2", Resources: []string{"r2"},
		Start: base.Add(3 * time.Hour), Duration: time.Hour,
	})
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "w3", Resources: []string{"r3"},
		Start: base.Add(5 * time.Hour), Duration: time.Hour,
	})

	got := eng.Calendar(base.Add(90*time.Minute), base.Add(4*time.Hour))
	if len(got) != 2 || got[0].RequestID != "w1" || got[1].RequestID != "w2" {
		t.Fatalf("calendar window wrong: %+v", got)
	}
	// Touching at an endpoint does not include the window.
	got = eng.Calendar(base.Add(2*time.Hour), base.Add(3*time.Hour))
	if len(got) != 0 {
		t.Fatalf("half-open endpoint query returned %d windows", len(got))
	}
	// Finalized windows remain visible on the calendar.
	clock.advance(time.Hour)
	if err := eng.Start("w1"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.Complete("w1", "done"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got = eng.Calendar(base, base.Add(2*time.Hour))
	if len(got) != 1 || got[0].Status != maintenance.StatusCompleted {
		t.Fatalf("completed window missing from calendar: %+v", got)
	}
}

func TestFileStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	clock := &fakeClock{t: base}

	eng1, err := maintenance.NewEngine(maintenance.NewFileStore(path),
		maintenance.WithClock(clock.now))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	register(t, eng1, "r1", "r2")

	mustCreate(t, eng1, maintenance.RequestSpec{
		ID: "a", Resources: []string{"r1"},
		Start: base, Duration: 2 * time.Hour,
	})
	if err := eng1.Start("a"); err != nil {
		t.Fatalf("Start a: %v", err)
	}
	mustCreate(t, eng1, maintenance.RequestSpec{
		ID: "b", Resources: []string{"r2"},
		Start: base.Add(2 * time.Hour), Duration: time.Hour,
		PrepSteps: []string{"backup"},
	})
	if err := eng1.ReportPrep("b", 1, "backup"); err != nil {
		t.Fatalf("ReportPrep: %v", err)
	}

	// Reload from the same file: occupancy, request states and version survive.
	eng2, err := maintenance.NewEngine(maintenance.NewFileStore(path),
		maintenance.WithClock(clock.now))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	var held string
	for _, res := range eng2.Resources() {
		if res.ID == "r1" {
			held = res.OccupiedBy
		}
	}
	if held != "a" {
		t.Fatalf("r1 occupancy not persisted: %q", held)
	}
	if ra, _ := eng2.Request("a"); ra.Status != maintenance.StatusRunning {
		t.Fatalf("a status after reload: %s", ra.Status)
	}
	if rb, _ := eng2.Request("b"); rb.Status != maintenance.StatusReady || rb.Version != 1 {
		t.Fatalf("b after reload: %+v", rb)
	}

	// Finalizing on the reloaded engine persists the history record.
	if err := eng2.Complete("a", "done after reload"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	eng3, err := maintenance.NewEngine(maintenance.NewFileStore(path),
		maintenance.WithClock(clock.now))
	if err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if n := recordsFor(eng3.History(), "a"); n != 1 {
		t.Fatalf("history after reload: %d records, want 1", n)
	}
	for _, res := range eng3.Resources() {
		if res.OccupiedBy != "" {
			t.Fatalf("resource %s occupied after reload: %q", res.ID, res.OccupiedBy)
		}
	}
}

func TestRegisterResourceDuplicate(t *testing.T) {
	clock := &fakeClock{t: base}
	eng := newEngine(t, clock, nil)
	if err := eng.RegisterResource("r1", "again"); !errors.Is(err, maintenance.ErrResourceExists) {
		t.Fatalf("duplicate resource: want ErrResourceExists, got %v", err)
	}
}

func TestAbortPendingReleasesAndRecords(t *testing.T) {
	clock := &fakeClock{t: base}
	notes := &countingNotifier{}
	eng := newEngine(t, clock, notes)
	mustCreate(t, eng, maintenance.RequestSpec{
		ID: "mw-1", Resources: []string{"r1"},
		Start: base.Add(time.Hour), Duration: time.Hour,
		PrepSteps: []string{"s1"},
	})
	if err := eng.Abort("mw-1", "plan cancelled"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	r, _ := eng.Request("mw-1")
	if r.Status != maintenance.StatusAborted || r.Reason != "plan cancelled" {
		t.Fatalf("after abort: %s (%q)", r.Status, r.Reason)
	}
	if notes.count() != 1 {
		t.Fatalf("notifications: %d, want 1", notes.count())
	}
	// A second abort is rejected and creates no extra notification.
	if err := eng.Abort("mw-1", "again"); !errors.Is(err, maintenance.ErrInvalidState) {
		t.Fatalf("double abort: want ErrInvalidState, got %v", err)
	}
	if notes.count() != 1 {
		t.Fatalf("notifications after double abort: %d, want 1", notes.count())
	}
}
