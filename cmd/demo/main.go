// Command demo walks through a complete maintenance-window lifecycle against
// a JSON file store, and then reloads the persisted state in a second engine.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chris64233/go-maintenance-window/maintenance"
)

func main() {
	dir, err := os.MkdirTemp("", "mw-demo-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	statePath := filepath.Join(dir, "state.json")

	notify := maintenance.NotifierFunc(func(rec maintenance.Record) {
		fmt.Printf("[notify] %-9s %s %s -> %s\n", rec.Outcome, rec.RequestID, rec.Resources, rec.Reason)
	})

	eng, err := maintenance.NewEngine(maintenance.NewFileStore(statePath), maintenance.WithNotifier(notify))
	must(err)

	// 1. Register resources.
	must(eng.RegisterResource("db-primary", "primary database"))
	must(eng.RegisterResource("db-replica", "read replica"))
	must(eng.RegisterResource("cache", "redis cache"))
	fmt.Println("registered: db-primary, db-replica, cache")

	// 2. Create a window starting "now" occupying two resources, with two
	//    preparation steps that must be acknowledged before it can start.
	start := time.Now()
	_, err = eng.CreateRequest(maintenance.RequestSpec{
		ID:        "mw-001",
		Resources: []string{"db-primary", "cache"},
		Start:     start,
		Duration:  30 * time.Minute,
		PrepSteps: []string{"backup", "notify-users"},
	})
	must(err)
	fmt.Println("created mw-001 (PENDING): db-primary + cache, starts now, 30m, steps backup/notify-users")

	// 3. Creating another request for an overlapping window on a shared
	//    resource is rejected up front, before anything is occupied.
	_, err = eng.CreateRequest(maintenance.RequestSpec{
		ID:        "mw-002",
		Resources: []string{"db-replica", "db-primary"},
		Start:     start,
		Duration:  10 * time.Minute,
	})
	if errors.Is(err, maintenance.ErrWindowOverlap) {
		fmt.Printf("overlap rejected at creation: %v\n", err)
	} else {
		must(err)
	}

	// 4. Receipts may arrive out of order or repeated. Modifying the window
	//    bumps the version and resets receipts: the late v1 receipt below is
	//    ignored, so both steps must be acknowledged again for v2.
	must(eng.ReportPrep("mw-001", 1, "notify-users"))
	r := mustReq(eng.ModifyRequest("mw-001", maintenance.ModifySpec{
		Resources: []string{"db-primary", "cache"},
		Start:     start,
		Duration:  45 * time.Minute,
	}))
	fmt.Printf("modified mw-001 -> version %d, status back to %s\n", r.Version, r.Status)
	must(eng.ReportPrep("mw-001", 1, "backup")) // stale v1 receipt: ignored
	must(eng.ReportPrep("mw-001", 2, "backup"))
	must(eng.ReportPrep("mw-001", 2, "backup")) // duplicate: harmless
	must(eng.ReportPrep("mw-001", 2, "notify-users"))
	r = mustReq(eng.Request("mw-001"))
	fmt.Printf("all v2 steps acknowledged: mw-001 is %s\n", r.Status)

	// 5. Atomic start: usage rights for every resource are taken at once.
	must(eng.Start("mw-001"))
	r = mustReq(eng.Request("mw-001"))
	fmt.Printf("started: mw-001 is %s and holds %v\n", r.Status, r.Resources)

	// 6. Complete: all resources released, exactly one record + notification.
	must(eng.Complete("mw-001", "upgrade finished, checks green"))
	fmt.Println("completed: mw-001, resources released")

	// 7. Calendar over the next hour shows the finalized window too.
	for _, w := range eng.Calendar(start.Add(-time.Minute), start.Add(time.Hour)) {
		fmt.Printf("calendar: %s %s %s..%s\n", w.RequestID, w.Status, w.Start.Format("15:04:05"), w.End.Format("15:04:05"))
	}

	// 8. Reload from disk: occupancy and history survive a restart.
	eng2, err := maintenance.NewEngine(maintenance.NewFileStore(statePath))
	must(err)
	for _, rec := range eng2.History() {
		fmt.Printf("history (reloaded): %s -> %s (%s)\n", rec.RequestID, rec.Outcome, rec.Reason)
	}
	fmt.Printf("state file: %s\n", statePath)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustReq(r *maintenance.Request, err error) *maintenance.Request {
	must(err)
	return r
}
