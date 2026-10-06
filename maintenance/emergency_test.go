package maintenance

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func emergencyInput(key string, start time.Time, d time.Duration, reason string, resources ...string) EmergencyInput {
	return EmergencyInput{
		IdempotencyKey: key,
		ResourceIDs:    resources,
		Reason:         reason,
		Start:          start,
		Duration:       d,
	}
}

func TestEmergencyConfirmDisplacesAndReschedule(t *testing.T) {
	env := newTestEnv(t, "db-1", "db-2")
	normal, _ := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-1"))
	e, err := env.svc.CreateEmergency(emergencyInput("E1", base.Add(time.Hour), 30*time.Minute, "hotfix", "db-1", "db-2"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Status != EmergencyDraft {
		t.Fatalf("status = %s, want draft", e.Status)
	}
	if len(e.Affected) != 1 || e.Affected[0].RequestID != normal.ID {
		t.Fatalf("affected = %+v, want snapshot of %s", e.Affected, normal.ID)
	}
	env.advance(time.Hour)
	if _, err := env.svc.ConfirmEmergency(e.ID); err != nil {
		t.Fatal(err)
	}
	// 受影响窗口转为待重排，保留原计划时间与重排原因
	got, _ := env.svc.GetRequest(normal.ID)
	if got.Status != StatusReschedule {
		t.Fatalf("normal status = %s, want reschedule", got.Status)
	}
	if got.OriginalStart == nil || !got.OriginalStart.Equal(base.Add(time.Hour)) {
		t.Fatalf("original start = %v, want %v", got.OriginalStart, base.Add(time.Hour))
	}
	if got.OriginalDuration != time.Hour {
		t.Fatalf("original duration = %v, want 1h", got.OriginalDuration)
	}
	if !strings.Contains(got.RescheduleReason, e.ID) {
		t.Fatalf("reschedule reason %q should reference emergency %s", got.RescheduleReason, e.ID)
	}
	for _, r := range env.svc.Resources() {
		if r.Status != ResourceMaintenance {
			t.Fatalf("resource %s status = %s, want maintenance", r.ID, r.Status)
		}
	}
	// 查询：排期前后对比、受影响窗口、资源占用、重排原因
	report, err := env.svc.EmergencyReport(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Before) != 1 || report.Before[0].RequestID != normal.ID {
		t.Fatalf("before = %+v, want normal window", report.Before)
	}
	if len(report.Occupancy) != 2 {
		t.Fatalf("occupancy = %+v, want 2 active windows", report.Occupancy)
	}
	if len(report.Affected) != 1 || report.Affected[0].Status != StatusReschedule ||
		report.Affected[0].RescheduleReason == "" {
		t.Fatalf("affected = %+v", report.Affected)
	}
	// 重排：只改时间，原计划时间保留，准备进度不变
	if _, err := env.svc.Reschedule(normal.ID, base.Add(-time.Hour)); !errors.Is(err, ErrInvalidTimeRange) {
		t.Fatalf("want ErrInvalidTimeRange for past reschedule, got %v", err)
	}
	rescheduled, err := env.svc.Reschedule(normal.ID, base.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rescheduled.Status != StatusReady || !rescheduled.Start.Equal(base.Add(3*time.Hour)) {
		t.Fatalf("rescheduled = %+v", rescheduled)
	}
	if rescheduled.OriginalStart == nil || rescheduled.Version != 1 {
		t.Fatalf("original plan must be preserved, version unchanged: %+v", rescheduled)
	}
	if _, err := env.svc.Reschedule(normal.ID, base.Add(4*time.Hour)); !errors.Is(err, ErrNotReschedulable) {
		t.Fatalf("want ErrNotReschedulable after reschedule, got %v", err)
	}
	if err := env.svc.CompleteEmergency(e.ID); err != nil {
		t.Fatal(err)
	}
	for _, r := range env.svc.Resources() {
		if r.Status != ResourceRunning {
			t.Fatalf("resource %s still %s after complete", r.ID, r.Status)
		}
	}
}

func TestEmergencyConfirmFailsWhenResourceBusy(t *testing.T) {
	env := newTestEnv(t, "db-1", "db-2")
	// db-1 上有执行中的维护，db-2 上有待开始的排期
	running, _ := env.svc.CreateRequest(input(base.Add(time.Hour), 2*time.Hour, "db-1"))
	planned, _ := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-2"))
	env.advance(time.Hour)
	if err := env.svc.Start(running.ID); err != nil {
		t.Fatal(err)
	}
	e, err := env.svc.CreateEmergency(emergencyInput("", base.Add(90*time.Minute), 30*time.Minute, "hotfix", "db-1", "db-2"))
	if err != nil {
		t.Fatal(err)
	}
	env.advance(30 * time.Minute)
	// 无法一次取得全部资源：整体失败，原排期完全保留
	if _, err := env.svc.ConfirmEmergency(e.ID); !errors.Is(err, ErrResourceBusy) {
		t.Fatalf("want ErrResourceBusy, got %v", err)
	}
	got, _ := env.svc.GetRequest(planned.ID)
	if got.Status != StatusReady {
		t.Fatalf("planned status = %s, want ready (排期不得被部分挪走)", got.Status)
	}
	if got.OriginalStart != nil {
		t.Fatalf("planned window must not be rewritten: %+v", got)
	}
	eGot, _ := env.svc.GetEmergency(e.ID)
	if eGot.Status != EmergencyDraft {
		t.Fatalf("emergency status = %s, want draft", eGot.Status)
	}
	// 占用记录只有执行中的 db-1 窗口
	wins := env.svc.Calendar(base, base.Add(3*time.Hour))
	var active int
	for _, w := range wins {
		if w.Kind == "active" {
			active++
			if w.RequestID != running.ID {
				t.Fatalf("unexpected active window: %+v", w)
			}
		}
	}
	if active != 1 {
		t.Fatalf("active windows = %d, want 1", active)
	}
}

func TestEmergencySnapshotStaleOnWindowChange(t *testing.T) {
	env := newTestEnv(t, "db-1")
	normal, _ := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-1"))
	e, _ := env.svc.CreateEmergency(emergencyInput("", base.Add(time.Hour), 30*time.Minute, "hotfix", "db-1"))
	// 受影响窗口版本变化 → 原申请失效
	if _, err := env.svc.ModifyRequest(normal.ID, RequestInput{
		ResourceIDs: []string{"db-1"},
		Start:       base.Add(2 * time.Hour),
		Duration:    time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	env.advance(time.Hour)
	if _, err := env.svc.ConfirmEmergency(e.ID); !errors.Is(err, ErrStaleEmergency) {
		t.Fatalf("want ErrStaleEmergency, got %v", err)
	}
	if got, _ := env.svc.GetEmergency(e.ID); got.Status != EmergencyStale {
		t.Fatalf("emergency status = %s, want stale", got.Status)
	}
}

func TestEmergencyStaleOnResourceVersionChange(t *testing.T) {
	env := newTestEnv(t, "db-1")
	// 另一段普通窗口与紧急窗口首尾错开，不属于受影响快照；
	// 但它的开始/完成会切换资源状态并推进资源版本。
	earlier, _ := env.svc.CreateRequest(input(base.Add(5*time.Minute), 5*time.Minute, "db-1"))
	e, _ := env.svc.CreateEmergency(emergencyInput("", base.Add(time.Hour), 30*time.Minute, "hotfix", "db-1"))
	if len(e.Affected) != 0 {
		t.Fatalf("affected = %+v, want none (non-overlapping windows)", e.Affected)
	}
	env.advance(5 * time.Minute)
	if err := env.svc.Start(earlier.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.Complete(earlier.ID, "done"); err != nil {
		t.Fatal(err)
	}
	// 资源版本已随状态切换递增，紧急窗口仍按原时间到来，但原申请必须失效
	env.advance(55 * time.Minute)
	if _, err := env.svc.ConfirmEmergency(e.ID); !errors.Is(err, ErrStaleEmergency) {
		t.Fatalf("want ErrStaleEmergency, got %v", err)
	}
	if r := env.svc.Resources()[0]; r.Status != ResourceRunning {
		t.Fatalf("stale confirm touched resource: %s", r.Status)
	}
}

func TestEmergencyHalfOpenBoundaryDoesNotDisplace(t *testing.T) {
	env := newTestEnv(t, "db-1")
	// 普通窗口 [10:30,11:00)，紧急窗口 [10:00,10:30)：end==start 不冲突
	normal, _ := env.svc.CreateRequest(input(base.Add(90*time.Minute), 30*time.Minute, "db-1"))
	e, err := env.svc.CreateEmergency(emergencyInput("", base.Add(time.Hour), 30*time.Minute, "hotfix", "db-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Affected) != 0 {
		t.Fatalf("half-open boundary windows must not be affected: %+v", e.Affected)
	}
	if Overlaps(base, base.Add(time.Hour), base.Add(time.Hour), base.Add(2*time.Hour)) {
		t.Fatal("Overlaps unexpectedly true for touching [a,b) and [b,c)")
	}
	env.advance(time.Hour)
	if _, err := env.svc.ConfirmEmergency(e.ID); err != nil {
		t.Fatalf("back-to-back window should confirm: %v", err)
	}
	got, _ := env.svc.GetRequest(normal.ID)
	if got.Status != StatusReady || got.OriginalStart != nil {
		t.Fatalf("back-to-back window must stay untouched: %+v", got)
	}
}

func TestEmergencyStaleWhenAffectedWindowCompleted(t *testing.T) {
	env := newTestEnv(t, "db-1")
	normal, _ := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-1"))
	e, _ := env.svc.CreateEmergency(emergencyInput("", base.Add(time.Hour), 30*time.Minute, "hotfix", "db-1"))
	// 受影响窗口先开始并完成：已完成的窗口不能被回写，原申请失效
	env.advance(time.Hour)
	if err := env.svc.Start(normal.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.Complete(normal.ID, "done early"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.ConfirmEmergency(e.ID); !errors.Is(err, ErrStaleEmergency) {
		t.Fatalf("want ErrStaleEmergency, got %v", err)
	}
	got, _ := env.svc.GetRequest(normal.ID)
	if got.Status != StatusCompleted || got.OriginalStart != nil {
		t.Fatalf("completed window must not be rewritten: %+v", got)
	}
}

func TestEmergencyIdempotentReplayAndConflict(t *testing.T) {
	env := newTestEnv(t, "db-1", "db-2")
	in := emergencyInput("K-1", base.Add(time.Hour), 30*time.Minute, "hotfix", "db-1")
	first, err := env.svc.CreateEmergency(in)
	if err != nil {
		t.Fatal(err)
	}
	// 相同申请号 + 相同内容 → 返回原结果
	replay, err := env.svc.CreateEmergency(in)
	if err != nil {
		t.Fatal(err)
	}
	if replay.ID != first.ID {
		t.Fatalf("replay returned %s, want original %s", replay.ID, first.ID)
	}
	// 时长变化 → 冲突
	longer := in
	longer.Duration = time.Hour
	if _, err := env.svc.CreateEmergency(longer); !errors.Is(err, ErrEmergencyConflict) {
		t.Fatalf("want ErrEmergencyConflict for duration change, got %v", err)
	}
	// 资源变化 → 冲突
	wider := in
	wider.ResourceIDs = []string{"db-1", "db-2"}
	if _, err := env.svc.CreateEmergency(wider); !errors.Is(err, ErrEmergencyConflict) {
		t.Fatalf("want ErrEmergencyConflict for resource change, got %v", err)
	}
	// 不同申请号不受限
	if _, err := env.svc.CreateEmergency(emergencyInput("K-2", base.Add(time.Hour), time.Hour, "other", "db-2")); err != nil {
		t.Fatal(err)
	}
}

func TestEmergencyConcurrentConfirmAndNormalStart(t *testing.T) {
	env := newTestEnv(t, "r1", "r2")
	start := base.Add(time.Hour)
	// 三方竞争同一批资源：两个紧急插入 + 一个普通窗口启动
	e1, _ := env.svc.CreateEmergency(emergencyInput("", start, time.Hour, "e1", "r1"))
	e2, _ := env.svc.CreateEmergency(emergencyInput("", start, time.Hour, "e2", "r1", "r2"))
	normal, _ := env.svc.CreateRequest(input(start, time.Hour, "r1"))
	env.advance(time.Hour)
	var wg sync.WaitGroup
	errs := make([]error, 3)
	ops := []func() error{
		func() error { _, err := env.svc.ConfirmEmergency(e1.ID); return err },
		func() error { _, err := env.svc.ConfirmEmergency(e2.ID); return err },
		func() error { return env.svc.Start(normal.ID) },
	}
	for i, op := range ops {
		wg.Add(1)
		go func(i int, op func() error) {
			defer wg.Done()
			errs[i] = op()
		}(i, op)
	}
	wg.Wait()
	// 只能形成一个资源占用结果
	var succeeded int
	for _, err := range errs {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("succeeded = %d, want exactly 1 (errs=%v)", succeeded, errs)
	}
	wins := env.svc.Calendar(start, start.Add(time.Hour))
	active := map[string]int{}
	for _, w := range wins {
		if w.Kind == "active" {
			active[w.ResourceID]++
		}
	}
	if active["r1"] != 1 {
		t.Fatalf("r1 active windows = %d, want exactly 1: %v", active["r1"], active)
	}
	// 若胜者是 e2（占 r1+r2），则 r2 也有占用；否则 r2 必须空闲
	if errs[1] == nil && active["r2"] != 1 {
		t.Fatalf("e2 won but r2 not occupied: %v", active)
	}
	if errs[1] != nil && active["r2"] != 0 {
		t.Fatalf("e2 lost but r2 occupied: %v", active)
	}
}

func TestEmergencyLateCancelDoesNotReleaseOthers(t *testing.T) {
	env := newTestEnv(t, "db-1")
	e, _ := env.svc.CreateEmergency(emergencyInput("", base.Add(time.Hour), 30*time.Minute, "hotfix", "db-1"))
	env.advance(time.Hour)
	if _, err := env.svc.ConfirmEmergency(e.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.CompleteEmergency(e.ID); err != nil {
		t.Fatal(err)
	}
	// 紧急窗口完成后，普通窗口接手同一资源
	normal, _ := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-1"))
	if err := env.svc.Start(normal.ID); err != nil {
		t.Fatal(err)
	}
	// 迟到的取消不得释放别人的资源
	if err := env.svc.CancelEmergency(e.ID); !errors.Is(err, ErrEmergencyFinal) {
		t.Fatalf("want ErrEmergencyFinal, got %v", err)
	}
	if r := env.svc.Resources()[0]; r.Status != ResourceMaintenance {
		t.Fatalf("late cancel released someone's resource: %s", r.Status)
	}
	got, _ := env.svc.GetRequest(normal.ID)
	if got.Status != StatusExecuting {
		t.Fatalf("normal status = %s, want executing", got.Status)
	}
}

func TestEmergencyCancelDraft(t *testing.T) {
	env := newTestEnv(t, "db-1")
	e, _ := env.svc.CreateEmergency(emergencyInput("", base.Add(time.Hour), 30*time.Minute, "hotfix", "db-1"))
	if err := env.svc.CancelEmergency(e.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := env.svc.GetEmergency(e.ID); got.Status != EmergencyCancelled {
		t.Fatalf("status = %s, want cancelled", got.Status)
	}
	// 取消幂等；取消后不得再确认
	if err := env.svc.CancelEmergency(e.ID); err != nil {
		t.Fatalf("second cancel should be idempotent: %v", err)
	}
	env.advance(time.Hour)
	if _, err := env.svc.ConfirmEmergency(e.ID); !errors.Is(err, ErrEmergencyFinal) {
		t.Fatalf("want ErrEmergencyFinal confirming cancelled, got %v", err)
	}
	if r := env.svc.Resources()[0]; r.Status != ResourceRunning {
		t.Fatalf("cancelled emergency touched resource: %s", r.Status)
	}
}

func TestEmergencyExpireOverrun(t *testing.T) {
	env := newTestEnv(t, "db-1")
	e, _ := env.svc.CreateEmergency(emergencyInput("", base.Add(time.Hour), 30*time.Minute, "hotfix", "db-1"))
	env.advance(time.Hour)
	if _, err := env.svc.ConfirmEmergency(e.ID); err != nil {
		t.Fatal(err)
	}
	env.advance(time.Hour) // 超过预计时长
	expired := env.svc.Expire()
	if len(expired) != 1 || expired[0] != e.ID {
		t.Fatalf("expired = %v, want [%s]", expired, e.ID)
	}
	if got, _ := env.svc.GetEmergency(e.ID); got.Status != EmergencyExpired {
		t.Fatalf("status = %s, want expired", got.Status)
	}
	if r := env.svc.Resources()[0]; r.Status != ResourceRunning {
		t.Fatalf("overrun emergency resource not released: %s", r.Status)
	}
}
