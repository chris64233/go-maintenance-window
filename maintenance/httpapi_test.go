package maintenance

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func newAPITestServer(t *testing.T) (*httptest.Server, *clock) {
	t.Helper()
	clk := &clock{now: base}
	svc := NewService(mustStore(t, ""), WithClock(clk.Now))
	return httptest.NewServer(NewHandler(svc)), clk
}

func postJSON(t *testing.T, srv *httptest.Server, path string, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return resp.StatusCode, out
}

func TestHTTPAPIFullLifecycle(t *testing.T) {
	srv, clk := newAPITestServer(t)
	defer srv.Close()

	if code, _ := postJSON(t, srv, "/resources", `{"id":"db-1","name":"primary"}`); code != http.StatusOK {
		t.Fatalf("register resource status = %d", code)
	}
	code, out := postJSON(t, srv, "/requests", `{
		"resource_ids":["db-1"],
		"start":"2026-09-28T11:00:00Z",
		"duration_minutes":60,
		"prep_steps":["backup"]
	}`)
	if code != http.StatusOK {
		t.Fatalf("create request status = %d: %v", code, out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("missing request id: %v", out)
	}
	if got := out["status"]; got != string(StatusPending) {
		t.Fatalf("status = %v, want pending", got)
	}

	// 创建/准备阶段资源保持 running
	resp, _ := http.Get(srv.URL + "/resources")
	var resources []map[string]any
	json.NewDecoder(resp.Body).Decode(&resources)
	resp.Body.Close()
	if resources[0]["status"] != string(ResourceRunning) {
		t.Fatalf("resource changed before start: %v", resources[0])
	}

	if code, _ := postJSON(t, srv, "/requests/"+id+"/prep", `{"version":1,"step":"backup"}`); code != http.StatusOK {
		t.Fatal("prep receipt failed")
	}
	// 旧版本回执被拒
	if code, _ := postJSON(t, srv, "/requests/"+id+"/prep", `{"version":9,"step":"backup"}`); code != http.StatusBadRequest {
		t.Fatalf("stale receipt status = %d, want 400", code)
	}

	clk.advance(time.Hour)
	if code, _ := postJSON(t, srv, "/requests/"+id+"/start", ""); code != http.StatusOK {
		t.Fatal("start failed")
	}
	if code, out := postJSON(t, srv, "/requests/"+id+"/complete", `{"reason":"patched"}`); code != http.StatusOK {
		t.Fatalf("complete failed: %d %v", code, out)
	}

	resp, _ = http.Get(srv.URL + "/requests/" + id)
	var final Request
	json.NewDecoder(resp.Body).Decode(&final)
	resp.Body.Close()
	if final.Status != StatusCompleted || final.Reason != "patched" {
		t.Fatalf("final request = %+v", final)
	}

	resp, _ = http.Get(srv.URL + "/history")
	var history []map[string]any
	json.NewDecoder(resp.Body).Decode(&history)
	resp.Body.Close()
	if len(history) != 1 || history[0]["reason"] != "patched" {
		t.Fatalf("history = %v", history)
	}

	// 终态后再次定案被拒
	if code, _ := postJSON(t, srv, "/requests/"+id+"/abort", `{"reason":"late"}`); code != http.StatusBadRequest {
		t.Fatalf("abort after final status = %d, want 400", code)
	}
}

func TestHTTPAPIVersionBump(t *testing.T) {
	srv, clk := newAPITestServer(t)
	defer srv.Close()
	postJSON(t, srv, "/resources", `{"id":"db-1","name":"primary"}`)
	postJSON(t, srv, "/resources", `{"id":"db-2","name":"replica"}`)
	_, out := postJSON(t, srv, "/requests", `{
		"resource_ids":["db-1"],
		"start":"2026-09-28T11:00:00Z",
		"duration_minutes":60,
		"prep_steps":["backup"]
	}`)
	id := out["id"].(string)
	postJSON(t, srv, "/requests/"+id+"/prep", `{"version":1,"step":"backup"}`)

	// 修改资源集合 → 版本 2，旧回执失效
	if code, mod := postJSON(t, srv, "/requests/"+id+"/modify", `{
		"resource_ids":["db-1","db-2"],
		"start":"2026-09-28T11:00:00Z",
		"duration_minutes":60,
		"prep_steps":["backup"]
	}`); code != http.StatusOK || mod["version"].(float64) != 2 {
		t.Fatalf("modify: code=%d body=%v", code, mod)
	}
	if code, _ := postJSON(t, srv, "/requests/"+id+"/prep", `{"version":1,"step":"backup"}`); code != http.StatusBadRequest {
		t.Fatalf("old-version receipt status = %d, want 400", code)
	}

	// 无变化的修改保留版本与 ready 之前的状态
	if code, mod := postJSON(t, srv, "/requests/"+id+"/modify", `{
		"resource_ids":["db-1","db-2"],
		"start":"2026-09-28T11:00:00Z",
		"duration_minutes":60,
		"prep_steps":["backup"]
	}`); code != http.StatusOK || mod["version"].(float64) != 2 {
		t.Fatalf("no-op modify: code=%d body=%v", code, mod)
	}

	clk.advance(time.Hour)
	// 未完成新版准备步骤不能开始
	if code, _ := postJSON(t, srv, "/requests/"+id+"/start", ""); code != http.StatusBadRequest {
		t.Fatalf("start before prep status = %d, want 400", code)
	}
	postJSON(t, srv, "/requests/"+id+"/prep", `{"version":2,"step":"backup"}`)
	if code, _ := postJSON(t, srv, "/requests/"+id+"/start", ""); code != http.StatusOK {
		t.Fatal("start after current-version prep failed")
	}
}

func TestHTTPAPIConcurrentStartOnlyOneAcquires(t *testing.T) {
	srv, clk := newAPITestServer(t)
	defer srv.Close()
	postJSON(t, srv, "/resources", `{"id":"r1","name":"r1"}`)
	postJSON(t, srv, "/resources", `{"id":"r2","name":"r2"}`)
	_, a := postJSON(t, srv, "/requests", `{"resource_ids":["r1","r2"],"start":"2026-09-28T11:00:00Z","duration_minutes":60}`)
	_, b := postJSON(t, srv, "/requests", `{"resource_ids":["r1","r2"],"start":"2026-09-28T11:00:00Z","duration_minutes":60}`)
	clk.advance(time.Hour)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, id := range []string{a["id"].(string), b["id"].(string)} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			resp, err := http.Post(srv.URL+"/requests/"+id+"/start", "application/json", nil)
			if err != nil {
				t.Error(err)
				return
			}
			codes[i] = resp.StatusCode
			resp.Body.Close()
		}(i, id)
	}
	wg.Wait()
	if codes[0]+codes[1] != http.StatusOK+http.StatusBadRequest {
		t.Fatalf("start codes = %v, want exactly one 200 and one 400", codes)
	}

	// 日历中 r1/r2 上各只有一个 active 窗口
	resp, _ := http.Get(srv.URL + "/calendar?from=2026-09-28T00:00:00Z&to=2026-09-29T00:00:00Z")
	var wins []Window
	json.NewDecoder(resp.Body).Decode(&wins)
	resp.Body.Close()
	count := map[string]int{}
	for _, w := range wins {
		if w.Kind == "active" {
			count[w.ResourceID]++
		}
	}
	if count["r1"] != 1 || count["r2"] != 1 {
		t.Fatalf("active window counts = %v", count)
	}
}

func TestHTTPAPIExpireAndCalendar(t *testing.T) {
	srv, clk := newAPITestServer(t)
	defer srv.Close()
	postJSON(t, srv, "/resources", `{"id":"db-1","name":"primary"}`)
	_, out := postJSON(t, srv, "/requests", `{"resource_ids":["db-1"],"start":"2026-09-28T11:00:00Z","duration_minutes":60}`)
	id := out["id"].(string)

	resp, _ := http.Get(srv.URL + "/calendar?from=2026-09-28T11:00:00Z&to=2026-09-28T12:00:00Z")
	var scheduled []Window
	json.NewDecoder(resp.Body).Decode(&scheduled)
	resp.Body.Close()
	if len(scheduled) != 1 || scheduled[0].Kind != "scheduled" {
		t.Fatalf("scheduled window = %v", scheduled)
	}

	clk.advance(2 * time.Hour) // 错过窗口
	resp, _ = http.Post(srv.URL+"/expire", "application/json", nil)
	var expired struct {
		Expired []string `json:"expired"`
	}
	json.NewDecoder(resp.Body).Decode(&expired)
	resp.Body.Close()
	if len(expired.Expired) != 1 || expired.Expired[0] != id {
		t.Fatalf("expired = %v", expired.Expired)
	}

	resp, _ = http.Get(srv.URL + "/requests/" + id)
	var got Request
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Status != StatusExpired {
		t.Fatalf("status = %s, want expired", got.Status)
	}
	// 参数错误返回 400
	resp, _ = http.Get(srv.URL + "/calendar?from=bad&to=2026-09-29T00:00:00Z")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad calendar params status = %d", resp.StatusCode)
	}
	resp.Body.Close()
}
