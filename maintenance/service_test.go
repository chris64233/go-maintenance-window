package maintenance

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var base = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

// clock 是并发安全的可控时钟。
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testEnv 提供可控时钟的服务与已登记资源。
type testEnv struct {
	svc *Service
	clk *clock
}

func newTestEnv(t *testing.T, resources ...string) *testEnv {
	t.Helper()
	clk := &clock{now: base}
	svc := NewService(mustStore(t, ""), WithClock(clk.Now))
	env := &testEnv{svc: svc, clk: clk}
	for _, r := range resources {
		if _, err := svc.RegisterResource(r, r); err != nil {
			t.Fatalf("register %s: %v", r, err)
		}
	}
	return env
}

func mustStore(t *testing.T, path string) *Store {
	t.Helper()
	st, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

func (e *testEnv) advance(d time.Duration) { e.clk.advance(d) }

func input(start time.Time, d time.Duration, resources ...string) RequestInput {
	return RequestInput{ResourceIDs: resources, Start: start, Duration: d}
}

func TestCreateValidation(t *testing.T) {
	env := newTestEnv(t, "db-1", "db-2")

	if _, err := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour)); !errors.Is(err, ErrEmptyResourceSet) {
		t.Fatalf("want ErrEmptyResourceSet, got %v", err)
	}
	if _, err := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-1", "ghost")); !errors.Is(err, ErrResourceNotFound) {
		t.Fatalf("want ErrResourceNotFound, got %v", err)
	}
	if _, err := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-1", "db-1")); !errors.Is(err, ErrDuplicateResource) {
		t.Fatalf("want ErrDuplicateResource, got %v", err)
	}
	if _, err := env.svc.CreateRequest(input(base.Add(time.Hour), 0, "db-1")); !errors.Is(err, ErrInvalidTimeRange) {
		t.Fatalf("want ErrInvalidTimeRange for zero duration, got %v", err)
	}
	if _, err := env.svc.CreateRequest(input(base.Add(-time.Hour), time.Hour, "db-1")); !errors.Is(err, ErrInvalidTimeRange) {
		t.Fatalf("want ErrInvalidTimeRange for past start, got %v", err)
	}
}

func TestCreateDoesNotTouchResourceState(t *testing.T) {
	env := newTestEnv(t, "db-1")
	if _, err := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-1")); err != nil {
		t.Fatal(err)
	}
	for _, r := range env.svc.Resources() {
		if r.Status != ResourceRunning {
			t.Fatalf("resource %s status = %s before start, want running", r.ID, r.Status)
		}
	}
}

func TestPrepReceiptsDuplicateAndOutOfOrder(t *testing.T) {
	env := newTestEnv(t, "db-1")
	req, err := env.svc.CreateRequest(RequestInput{
		ResourceIDs: []string{"db-1"},
		Start:       base.Add(time.Hour),
		Duration:    time.Hour,
		PrepSteps:   []string{"backup", "notify", "drain"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != StatusPending {
		t.Fatalf("status = %s, want pending", req.Status)
	}
	// 乱序 + 重复回报
	for _, step := range []string{"drain", "backup", "drain", "notify", "backup"} {
		if err := env.svc.ReportPrep(req.ID, 1, step); err != nil {
			t.Fatalf("report %s: %v", step, err)
		}
	}
	got, _ := env.svc.GetRequest(req.ID)
	if got.Status != StatusReady {
		t.Fatalf("status = %s, want ready", got.Status)
	}
	if err := env.svc.ReportPrep(req.ID, 1, "nonexistent"); !errors.Is(err, ErrUnknownPrepStep) {
		t.Fatalf("want ErrUnknownPrepStep, got %v", err)
	}
}

func TestVersionBumpInvalidatesOldReceipts(t *testing.T) {
	env := newTestEnv(t, "db-1", "db-2")
	req, err := env.svc.CreateRequest(RequestInput{
		ResourceIDs: []string{"db-1"},
		Start:       base.Add(time.Hour),
		Duration:    time.Hour,
		PrepSteps:   []string{"backup"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.svc.ReportPrep(req.ID, 1, "backup"); err != nil {
		t.Fatal(err)
	}
	if got, _ := env.svc.GetRequest(req.ID); got.Status != StatusReady {
		t.Fatalf("status = %s, want ready", got.Status)
	}

	// 修改资源集合 → 版本递增，准备状态重置
	updated, err := env.svc.ModifyRequest(req.ID, RequestInput{
		ResourceIDs: []string{"db-1", "db-2"},
		Start:       base.Add(2 * time.Hour),
		Duration:    time.Hour,
		PrepSteps:   []string{"backup"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 {
		t.Fatalf("version = %d, want 2", updated.Version)
	}
	if updated.Status != StatusPending {
		t.Fatalf("status = %s after modify, want pending", updated.Status)
	}

	// 旧版本回执不得推进新版本
	if err := env.svc.ReportPrep(req.ID, 1, "backup"); !errors.Is(err, ErrStalePrepVersion) {
		t.Fatalf("want ErrStalePrepVersion, got %v", err)
	}
	if got, _ := env.svc.GetRequest(req.ID); got.Status != StatusPending {
		t.Fatalf("stale receipt advanced request to %s", got.Status)
	}
	// 新版本回执正常推进
	if err := env.svc.ReportPrep(req.ID, 2, "backup"); err != nil {
		t.Fatal(err)
	}
	if got, _ := env.svc.GetRequest(req.ID); got.Status != StatusReady {
		t.Fatalf("status = %s, want ready", got.Status)
	}
}

func TestStartRequiresReadyAndWindow(t *testing.T) {
	env := newTestEnv(t, "db-1")
	req, _ := env.svc.CreateRequest(RequestInput{
		ResourceIDs: []string{"db-1"},
		Start:       base.Add(time.Hour),
		Duration:    time.Hour,
		PrepSteps:   []string{"backup"},
	})
	if err := env.svc.Start(req.ID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("want ErrNotReady, got %v", err)
	}
	if err := env.svc.ReportPrep(req.ID, 1, "backup"); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.Start(req.ID); !errors.Is(err, ErrWindowNotReached) {
		t.Fatalf("want ErrWindowNotReached, got %v", err)
	}
	env.advance(time.Hour)
	if err := env.svc.Start(req.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := env.svc.GetRequest(req.ID)
	if got.Status != StatusExecuting {
		t.Fatalf("status = %s, want executing", got.Status)
	}
	if r := env.svc.Resources()[0]; r.Status != ResourceMaintenance {
		t.Fatalf("resource status = %s, want maintenance", r.Status)
	}
}

func TestConcurrentStartAtomicAcquisition(t *testing.T) {
	env := newTestEnv(t, "db-1", "db-2", "db-3")
	start := base.Add(time.Hour)
	// A 占用 db-1+db-2，B 占用 db-2+db-3：两者竞争 db-2，只有一个能开始。
	mk := func(resources ...string) string {
		req, err := env.svc.CreateRequest(input(start, time.Hour, resources...))
		if err != nil {
			t.Fatal(err)
		}
		return req.ID
	}
	a, b := mk("db-1", "db-2"), mk("db-2", "db-3")
	env.advance(time.Hour)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []string{a, b} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			errs[i] = env.svc.Start(id)
		}(i, id)
	}
	wg.Wait()

	var started, failed int
	for _, err := range errs {
		if err == nil {
			started++
		} else if errors.Is(err, ErrResourceBusy) {
			failed++
		}
	}
	if started != 1 || failed != 1 {
		t.Fatalf("started=%d failed=%d, want exactly one of each (errs=%v)", started, failed, errs)
	}
	// 失败方不得留下任何占用：统计每个资源的 active 窗口
	wins := env.svc.Calendar(start, start.Add(time.Hour))
	count := map[string]int{}
	for _, w := range wins {
		if w.Kind == "active" {
			count[w.ResourceID]++
		}
	}
	if count["db-1"]+count["db-2"]+count["db-3"] != 2 {
		t.Fatalf("partial acquisition leaked: %v", count)
	}
	// 失败方仍是 ready，可在胜方完成后重试
	var loser string
	if errs[0] != nil {
		loser = a
	} else {
		loser = b
	}
	if got, _ := env.svc.GetRequest(loser); got.Status != StatusReady {
		t.Fatalf("loser status = %s, want ready", got.Status)
	}
}

func TestFailedStartRollsBackPartialAcquisition(t *testing.T) {
	env := newTestEnv(t, "db-1", "db-2")
	start := base.Add(time.Hour)
	first, _ := env.svc.CreateRequest(input(start, time.Hour, "db-2"))
	env.advance(time.Hour)
	if err := env.svc.Start(first.ID); err != nil {
		t.Fatal(err)
	}
	// 第二个申请需要先占 db-1 再占 db-2；db-2 冲突时必须回滚 db-1。
	second, _ := env.svc.CreateRequest(input(start, time.Hour, "db-1", "db-2"))
	if err := env.svc.Start(second.ID); !errors.Is(err, ErrResourceBusy) {
		t.Fatalf("want ErrResourceBusy, got %v", err)
	}
	for _, r := range env.svc.Resources() {
		want := ResourceRunning
		if r.ID == "db-2" {
			want = ResourceMaintenance
		}
		if r.Status != want {
			t.Fatalf("resource %s status = %s, want %s (rollback failed)", r.ID, r.Status, want)
		}
	}
	// db-1 未被锁住：单独的 db-1 申请可以开始
	third, _ := env.svc.CreateRequest(input(start, time.Hour, "db-1"))
	if err := env.svc.Start(third.ID); err != nil {
		t.Fatalf("db-1 should be free after rollback: %v", err)
	}
}

func TestCompleteAbortRaceSingleFinalRecord(t *testing.T) {
	env := newTestEnv(t, "db-1")
	var mu sync.Mutex
	var notified []Notification
	env.svc.notifiers = append(env.svc.notifiers, NotifierFunc(func(n Notification) {
		mu.Lock()
		notified = append(notified, n)
		mu.Unlock()
	}))

	req, _ := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-1"))
	env.advance(time.Hour)
	if err := env.svc.Start(req.ID); err != nil {
		t.Fatal(err)
	}

	// 完成、中止、超时并发竞争
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i {
			case 0:
				_ = env.svc.Complete(req.ID, "done")
			case 1:
				_ = env.svc.Abort(req.ID, "operator abort")
			case 2:
				env.advance(2 * time.Hour)
				env.svc.Expire()
			}
		}(i)
	}
	wg.Wait()

	if got := len(env.svc.History()); got != 1 {
		t.Fatalf("final records = %d, want exactly 1", got)
	}
	if got := len(env.svc.Notifications()); got != 1 {
		t.Fatalf("notifications = %d, want exactly 1", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := len(notified); got != 1 {
		t.Fatalf("external notifications = %d, want exactly 1", got)
	}
	if r := env.svc.Resources()[0]; r.Status != ResourceRunning {
		t.Fatalf("resource not released: %s", r.Status)
	}
	rec := env.svc.History()[0]
	if rec.Reason == "" {
		t.Fatal("final record must carry a reason")
	}
}

func TestCompleteReleasesResourcesAndRecordsReason(t *testing.T) {
	env := newTestEnv(t, "db-1", "db-2")
	req, _ := env.svc.CreateRequest(input(base.Add(time.Hour), 2*time.Hour, "db-1", "db-2"))
	env.advance(time.Hour)
	if err := env.svc.Start(req.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.Complete(req.ID, "patched"); err != nil {
		t.Fatal(err)
	}
	got, _ := env.svc.GetRequest(req.ID)
	if got.Status != StatusCompleted || got.Reason != "patched" {
		t.Fatalf("got %+v", got)
	}
	for _, r := range env.svc.Resources() {
		if r.Status != ResourceRunning {
			t.Fatalf("resource %s still %s", r.ID, r.Status)
		}
	}
	if wins := env.svc.Calendar(base, base.Add(3*time.Hour)); len(wins) != 0 {
		t.Fatalf("occupancy not released: %+v", wins)
	}
	// 终态后再次完成/中止报错
	if err := env.svc.Complete(req.ID, "again"); !errors.Is(err, ErrAlreadyFinal) {
		t.Fatalf("want ErrAlreadyFinal, got %v", err)
	}
}

func TestExpireMissedWindowAndOverrun(t *testing.T) {
	env := newTestEnv(t, "db-1", "db-2")
	missed, _ := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-1"))
	running, _ := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-2"))
	env.advance(time.Hour)
	if err := env.svc.Start(running.ID); err != nil {
		t.Fatal(err)
	}
	env.advance(2 * time.Hour) // 两个窗口都已结束
	expired := env.svc.Expire()
	if len(expired) != 2 {
		t.Fatalf("expired = %v, want 2 requests", expired)
	}
	for _, id := range []string{missed.ID, running.ID} {
		got, _ := env.svc.GetRequest(id)
		if got.Status != StatusExpired {
			t.Fatalf("%s status = %s, want expired", id, got.Status)
		}
	}
	if r := env.svc.Resources()[1]; r.Status != ResourceRunning {
		t.Fatalf("overrun resource not released: %s", r.Status)
	}
	// 再次 Expire 幂等
	if again := env.svc.Expire(); len(again) != 0 {
		t.Fatalf("second Expire returned %v", again)
	}
	if got := len(env.svc.History()); got != 2 {
		t.Fatalf("history = %d, want 2", got)
	}
}

func TestCalendar(t *testing.T) {
	env := newTestEnv(t, "db-1", "db-2")
	scheduled, _ := env.svc.CreateRequest(input(base.Add(2*time.Hour), time.Hour, "db-1"))
	active, _ := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-2"))
	env.advance(time.Hour)
	if err := env.svc.Start(active.ID); err != nil {
		t.Fatal(err)
	}
	wins := env.svc.Calendar(base, base.Add(4*time.Hour))
	if len(wins) != 2 {
		t.Fatalf("calendar windows = %d, want 2: %+v", len(wins), wins)
	}
	byKind := map[string]string{}
	for _, w := range wins {
		byKind[w.Kind] = w.RequestID
	}
	if byKind["active"] != active.ID || byKind["scheduled"] != scheduled.ID {
		t.Fatalf("unexpected calendar: %+v", wins)
	}
	// 范围外查询为空
	if wins := env.svc.Calendar(base.Add(10*time.Hour), base.Add(11*time.Hour)); len(wins) != 0 {
		t.Fatalf("out-of-range calendar not empty: %+v", wins)
	}
}

func TestPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "store.json")
	now := base
	svc := NewService(mustStore(t, path), WithClock(func() time.Time { return now }))
	if _, err := svc.RegisterResource("db-1", "db-1"); err != nil {
		t.Fatal(err)
	}
	req, err := svc.CreateRequest(RequestInput{
		ResourceIDs: []string{"db-1"},
		Start:       base.Add(time.Hour),
		Duration:    time.Hour,
		PrepSteps:   []string{"backup"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ReportPrep(req.ID, 1, "backup"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := svc.Start(req.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Complete(req.ID, "done"); err != nil {
		t.Fatal(err)
	}

	// 重新加载：占用、历史、通知、资源状态都应保留
	svc2 := NewService(mustStore(t, path), WithClock(func() time.Time { return now }))
	got, err := svc2.GetRequest(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCompleted || got.Version != 1 || got.Reason != "done" {
		t.Fatalf("reloaded request = %+v", got)
	}
	if len(svc2.History()) != 1 || len(svc2.Notifications()) != 1 {
		t.Fatalf("history/notifications not persisted")
	}
	if r := svc2.Resources()[0]; r.Status != ResourceRunning {
		t.Fatalf("resource status = %s after reload", r.Status)
	}
}

func TestConcurrentStartsStress(t *testing.T) {
	env := newTestEnv(t, "r1", "r2", "r3", "r4")
	start := base.Add(time.Hour)
	// 8 个申请两两共享资源，全部并发开始；最终每个资源上至多一个 active 窗口。
	var ids []string
	combos := [][]string{{"r1", "r2"}, {"r2", "r3"}, {"r3", "r4"}, {"r1", "r4"}, {"r1", "r3"}, {"r2", "r4"}, {"r1"}, {"r4"}}
	for _, c := range combos {
		req, err := env.svc.CreateRequest(input(start, time.Hour, c...))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, req.ID)
	}
	env.advance(time.Hour)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_ = env.svc.Start(id)
		}(id)
	}
	wg.Wait()

	wins := env.svc.Calendar(start, start.Add(time.Hour))
	seen := map[string]bool{}
	for _, w := range wins {
		if w.Kind != "active" {
			continue
		}
		if seen[w.ResourceID] {
			t.Fatalf("resource %s has overlapping active windows", w.ResourceID)
		}
		seen[w.ResourceID] = true
	}
	// 所有执行中的申请都能正常完成，资源全部释放
	for _, id := range ids {
		_ = env.svc.Complete(id, "done")
	}
	for _, r := range env.svc.Resources() {
		if r.Status != ResourceRunning {
			t.Fatalf("resource %s leaked status %s", r.ID, r.Status)
		}
	}
}

func ExampleService() {
	now := base
	svc := NewService(mustStoreForExample(), WithClock(func() time.Time { return now }))
	svc.RegisterResource("db-1", "primary database")
	req, _ := svc.CreateRequest(RequestInput{
		ResourceIDs: []string{"db-1"},
		Start:       now.Add(time.Hour),
		Duration:    30 * time.Minute,
		PrepSteps:   []string{"backup"},
	})
	svc.ReportPrep(req.ID, req.Version, "backup")
	now = now.Add(time.Hour)
	svc.Start(req.ID)
	svc.Complete(req.ID, "schema migrated")
	got, _ := svc.GetRequest(req.ID)
	fmt.Println(got.Status, got.Reason)
	// Output: completed schema migrated
}

func mustStoreForExample() *Store {
	st, _ := OpenStore("")
	return st
}
