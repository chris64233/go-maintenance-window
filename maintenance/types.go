// Package maintenance implements cross-resource maintenance-window
// orchestration.
//
// A maintenance request occupies one or more resources for a declared time
// window and may require a set of preparation steps to be acknowledged before
// the window is allowed to start. The engine guarantees:
//
//   - resource relationships and time ranges are validated when a request is
//     created or modified, and resources are not touched before the window is
//     actually started;
//   - windows on the same resource never overlap and resource usage rights are
//     acquired for every resource at once (a failed acquisition rolls back in
//     full, never leaving resources locked behind);
//   - preparation receipts are version-aware: changes to a request's time or
//     resource set bump the version, reset the receipts and stale receipts can
//     never advance the new version;
//   - completion, abort and timeout race into a single finalizer, so each
//     request produces exactly one final history record and one notification,
//     and all occupied resources are released.
package maintenance

import "time"

// Status is the lifecycle state of a maintenance request.
type Status string

const (
	// StatusPending: created, waiting for the current version's preparation
	// steps to be acknowledged.
	StatusPending Status = "PENDING"
	// StatusReady: every preparation step of the current version has been
	// acknowledged; the window may be started.
	StatusReady Status = "READY"
	// StatusRunning: the window has started and every declared resource is held.
	StatusRunning Status = "RUNNING"
	// StatusCompleted: a running window was completed explicitly.
	StatusCompleted Status = "COMPLETED"
	// StatusAborted: a request was aborted explicitly or swept by the timeout
	// handler while running.
	StatusAborted Status = "ABORTED"
	// StatusExpired: a non-running request reached its end time without being
	// started.
	StatusExpired Status = "EXPIRED"
)

// Final reports whether the status is a terminal state.
func (s Status) Final() bool {
	return s == StatusCompleted || s == StatusAborted || s == StatusExpired
}

// Resource is a bookable entity (a database host, a switch, a service, ...).
// OccupiedBy is the only piece of "runtime state" a resource carries; it stays
// empty until a request actually starts, even if requests for the resource
// were created long before.
type Resource struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	OccupiedBy string `json:"occupied_by,omitempty"`
}

// Request is a maintenance request. A request is versioned: modifying its time
// window or resource set bumps Version and resets DoneSteps, so receipts
// reported against an older version can never satisfy the new one.
type Request struct {
	ID        string          `json:"id"`
	Version   int             `json:"version"`
	Resources []string        `json:"resources"`
	Start     time.Time       `json:"start"`
	Duration  time.Duration   `json:"duration"`
	PrepSteps []string        `json:"prep_steps"`
	DoneSteps map[string]bool `json:"done_steps"`
	Status    Status          `json:"status"`
	Reason    string          `json:"reason,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// End returns the scheduled end time of the request's window.
func (r *Request) End() time.Time {
	return r.Start.Add(r.Duration)
}

func (r *Request) clone() *Request {
	cp := *r
	cp.Resources = append([]string(nil), r.Resources...)
	cp.PrepSteps = append([]string(nil), r.PrepSteps...)
	cp.DoneSteps = make(map[string]bool, len(r.DoneSteps))
	for k, v := range r.DoneSteps {
		cp.DoneSteps[k] = v
	}
	return &cp
}

// RequestSpec describes a new maintenance request.
type RequestSpec struct {
	ID        string
	Resources []string
	Start     time.Time
	Duration  time.Duration
	PrepSteps []string
}

// ModifySpec describes changes to an existing request's schedule or resource
// set. Both changes bump the request version.
type ModifySpec struct {
	Resources []string
	Start     time.Time
	Duration  time.Duration
}

// Record is the immutable final record appended once per request when it
// reaches a terminal state.
type Record struct {
	RequestID  string    `json:"request_id"`
	Version    int       `json:"version"`
	Outcome    Status    `json:"outcome"`
	Reason     string    `json:"reason"`
	Resources  []string  `json:"resources"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	FinishedAt time.Time `json:"finished_at"`
}

// Window is a calendar entry returned by Engine.Calendar.
type Window struct {
	RequestID string
	Version   int
	Resources []string
	Start     time.Time
	End       time.Time
	Status    Status
}
