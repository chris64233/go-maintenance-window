package maintenance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestResourceStatusReflectsRemainingOccupancy 覆盖两个关键回滚/定案场景：
//  1. 相邻（半开区间首尾相接、互不重叠）的执行中维护并存时，一个申请开始失败
//     回滚后，另一个超时未清扫的执行中占用不能被误释放；
//  2. 定案释放资源时，若资源上还有其它执行中窗口，资源必须保持 maintenance。
func TestResourceStatusReflectsRemainingOccupancy(t *testing.T) {
	env := newTestEnv(t, "r1", "r2")
	// A：[base+1h, base+2h) 占 r1；C：[base+2h, base+3h) 占 r1+r2（与 A 首尾相接）；
	// D：[base+2h, base+3h) 占 r2，用于让 C 在 r2 上冲突。
	a, _ := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "r1"))
	c, _ := env.svc.CreateRequest(input(base.Add(2*time.Hour), time.Hour, "r1", "r2"))
	d, _ := env.svc.CreateRequest(input(base.Add(2*time.Hour), time.Hour, "r2"))

	env.advance(time.Hour)
	if err := env.svc.Start(a.ID); err != nil {
		t.Fatal(err)
	}
	env.advance(time.Hour) // 现在 2h：A 已超过窗口结束但尚未被 Expire 清扫，仍执行中
	if err := env.svc.Start(d.ID); err != nil {
		t.Fatal(err)
	}

	// C 的窗口与 A 相邻（不重叠），先取得 r1，随后在 r2 上与 D 冲突 → 整体回滚。
	if err := env.svc.Start(c.ID); !errors.Is(err, ErrResourceBusy) {
		t.Fatalf("want ErrResourceBusy, got %v", err)
	}
	statuses := map[string]ResourceStatus{}
	for _, r := range env.svc.Resources() {
		statuses[r.ID] = r.Status
	}
	if statuses["r1"] != ResourceMaintenance {
		t.Fatalf("r1 = %s, want maintenance (A still active after rollback)", statuses["r1"])
	}
	if statuses["r2"] != ResourceMaintenance {
		t.Fatalf("r2 = %s, want maintenance (D still active)", statuses["r2"])
	}
	if got, _ := env.svc.GetRequest(c.ID); got.Status != StatusReady {
		t.Fatalf("c = %s, want ready after failed start", got.Status)
	}

	// D 释放 r2 后，C 可以与超时未清扫的 A 相邻开始。
	if err := env.svc.Abort(d.ID, "postpone"); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.Start(c.ID); err != nil {
		t.Fatalf("adjacent start should succeed: %v", err)
	}
	// 此时 A 与 C 都持有 r1 的（互不重叠）窗口；A 定案不得把 r1 置回 running。
	if err := env.svc.Complete(a.ID, "done"); err != nil {
		t.Fatal(err)
	}
	for _, r := range env.svc.Resources() {
		if r.Status != ResourceMaintenance {
			t.Fatalf("resource %s = %s, want maintenance (C still active)", r.ID, r.Status)
		}
	}
	if err := env.svc.Complete(c.ID, "done too"); err != nil {
		t.Fatal(err)
	}
	for _, r := range env.svc.Resources() {
		if r.Status != ResourceRunning {
			t.Fatalf("resource %s = %s, want running after all finals", r.ID, r.Status)
		}
	}
}

// TestModifyWithoutChangesKeepsVersion 校验无实际变化的修改不递增版本、
// 不重置准备进度；有变化时才递增。
func TestModifyWithoutChangesKeepsVersion(t *testing.T) {
	env := newTestEnv(t, "db-1", "db-2")
	in := RequestInput{
		ResourceIDs: []string{"db-1"},
		Start:       base.Add(time.Hour),
		Duration:    time.Hour,
		PrepSteps:   []string{"backup"},
	}
	req, _ := env.svc.CreateRequest(in)
	if err := env.svc.ReportPrep(req.ID, 1, "backup"); err != nil {
		t.Fatal(err)
	}
	got, _ := env.svc.GetRequest(req.ID)
	if got.Status != StatusReady {
		t.Fatalf("status = %s, want ready", got.Status)
	}

	// 内容完全相同：版本与 ready 状态保持不变，回执仍被认可。
	same, err := env.svc.ModifyRequest(req.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if same.Version != 1 || same.Status != StatusReady {
		t.Fatalf("no-op modify: version=%d status=%s, want 1/ready", same.Version, same.Status)
	}
	if err := env.svc.ReportPrep(req.ID, 1, "backup"); err != nil {
		t.Fatalf("receipt after no-op modify: %v", err)
	}

	// 修改开始时间：版本递增、回到 pending，旧回执失效。
	changed, err := env.svc.ModifyRequest(req.ID, RequestInput{
		ResourceIDs: []string{"db-1"},
		Start:       base.Add(2 * time.Hour),
		Duration:    time.Hour,
		PrepSteps:   []string{"backup"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Version != 2 || changed.Status != StatusPending {
		t.Fatalf("changed modify: version=%d status=%s, want 2/pending", changed.Version, changed.Status)
	}
	if err := env.svc.ReportPrep(req.ID, 1, "backup"); !errors.Is(err, ErrStalePrepVersion) {
		t.Fatalf("want ErrStalePrepVersion, got %v", err)
	}
}

// TestDuplicatePrepStepsRejected 校验重复声明的准备步骤在创建与修改时都被拒绝。
func TestDuplicatePrepStepsRejected(t *testing.T) {
	env := newTestEnv(t, "db-1")
	in := RequestInput{
		ResourceIDs: []string{"db-1"},
		Start:       base.Add(time.Hour),
		Duration:    time.Hour,
		PrepSteps:   []string{"backup", "backup"},
	}
	if _, err := env.svc.CreateRequest(in); !errors.Is(err, ErrDuplicatePrepStep) {
		t.Fatalf("want ErrDuplicatePrepStep, got %v", err)
	}

	valid := in
	valid.PrepSteps = []string{"backup"}
	req, _ := env.svc.CreateRequest(valid)
	if err := env.svc.ReportPrep(req.ID, 1, "backup"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.ModifyRequest(req.ID, in); !errors.Is(err, ErrDuplicatePrepStep) {
		t.Fatalf("modify want ErrDuplicatePrepStep, got %v", err)
	}
	got, _ := env.svc.GetRequest(req.ID)
	if got.Version != 1 || got.Status != StatusReady {
		t.Fatalf("rejected modify should leave request untouched: v=%d %s", got.Version, got.Status)
	}
}

// TestScheduledOverlapAllowedActiveWins 校验排期阶段允许窗口重叠，
// 冲突延迟到开始时仲裁，失败方不留下任何占用。
func TestScheduledOverlapAllowedActiveWins(t *testing.T) {
	env := newTestEnv(t, "db-1")
	start := base.Add(time.Hour)
	a, _ := env.svc.CreateRequest(input(start, time.Hour, "db-1"))
	b, _ := env.svc.CreateRequest(input(start, time.Hour, "db-1"))

	wins := env.svc.Calendar(base, base.Add(3*time.Hour))
	if len(wins) != 2 {
		t.Fatalf("scheduled windows = %d, want 2 (overlap allowed before start)", len(wins))
	}
	env.advance(time.Hour)
	if err := env.svc.Start(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.Start(b.ID); !errors.Is(err, ErrResourceBusy) {
		t.Fatalf("want ErrResourceBusy, got %v", err)
	}
	if got, _ := env.svc.GetRequest(b.ID); got.Status != StatusReady {
		t.Fatalf("loser = %s, want ready", got.Status)
	}
	active := 0
	for _, w := range env.svc.Calendar(start, start.Add(time.Hour)) {
		if w.Kind == "active" {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("active windows = %d, want 1", active)
	}
}

// TestReadyRequestExpiresWhenWindowMissed 校验就绪但错过窗口的申请被 Expire 定案。
func TestReadyRequestExpiresWhenWindowMissed(t *testing.T) {
	env := newTestEnv(t, "db-1")
	req, _ := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-1"))
	env.advance(2 * time.Hour)
	expired := env.svc.Expire()
	if len(expired) != 1 || expired[0] != req.ID {
		t.Fatalf("expired = %v, want [%s]", expired, req.ID)
	}
	got, _ := env.svc.GetRequest(req.ID)
	if got.Status != StatusExpired || got.Reason == "" || got.FinalizedAt == nil {
		t.Fatalf("expired request = %+v", got)
	}
	if recs := env.svc.History(); len(recs) != 1 || recs[0].Outcome != StatusExpired {
		t.Fatalf("history = %+v, want one expired record", recs)
	}
}

// TestStartIdempotent 校验重复 Start 幂等，不会产生重复占用。
func TestStartIdempotent(t *testing.T) {
	env := newTestEnv(t, "db-1")
	req, _ := env.svc.CreateRequest(input(base.Add(time.Hour), time.Hour, "db-1"))
	env.advance(time.Hour)
	if err := env.svc.Start(req.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.svc.Start(req.ID); err != nil {
		t.Fatalf("second start: %v", err)
	}
	wins := env.svc.Calendar(base, base.Add(3*time.Hour))
	if len(wins) != 1 {
		t.Fatalf("occupancy windows = %d, want 1", len(wins))
	}
}

// TestHTTPAPIEndToEnd 通过 httptest 走完整 HTTP 生命周期。
func TestHTTPAPIEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	srv := httptest.NewServer(NewHandler(env.svc))
	defer srv.Close()

	post := func(path string, body any) (int, map[string]any) {
		t.Helper()
		data, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	if code, _ := post("/resources", map[string]string{"id": "db-1", "name": "primary"}); code != http.StatusOK {
		t.Fatalf("register resource code = %d", code)
	}
	start := base.Add(time.Hour).Format(time.RFC3339)
	code, out := post("/requests", map[string]any{
		"resource_ids":     []string{"db-1"},
		"start":            start,
		"duration_minutes": 60,
		"prep_steps":       []string{"backup"},
	})
	if code != http.StatusOK {
		t.Fatalf("create request code = %d body=%v", code, out)
	}
	id, _ := out["id"].(string)

	if code, _ := post(fmt.Sprintf("/requests/%s/prep", id), map[string]any{"version": 2, "step": "backup"}); code != http.StatusBadRequest {
		t.Fatalf("stale prep code = %d, want 400", code)
	}
	if code, _ := post(fmt.Sprintf("/requests/%s/prep", id), map[string]any{"version": 1, "step": "backup"}); code != http.StatusOK {
		t.Fatalf("prep code = %d, want 200", code)
	}
	// 窗口未到，开始被拒绝。
	if code, body := post(fmt.Sprintf("/requests/%s/start", id), nil); code != http.StatusBadRequest || body["error"] == "" {
		t.Fatalf("early start code = %d body=%v", code, body)
	}

	env.advance(time.Hour)
	if code, _ := post(fmt.Sprintf("/requests/%s/start", id), nil); code != http.StatusOK {
		t.Fatalf("start code = %d", code)
	}
	if code, _ := post(fmt.Sprintf("/requests/%s/complete", id), map[string]string{"reason": "patched"}); code != http.StatusOK {
		t.Fatalf("complete code = %d", code)
	}
	// 终态后重复定案被拒绝。
	if code, _ := post(fmt.Sprintf("/requests/%s/abort", id), map[string]string{"reason": "again"}); code != http.StatusBadRequest {
		t.Fatalf("second final code = %d, want 400", code)
	}

	resp, err := http.Get(srv.URL + "/history")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var recs []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&recs); err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0]["outcome"] != "completed" || recs[0]["reason"] != "patched" {
		t.Fatalf("history = %+v", recs)
	}
}
