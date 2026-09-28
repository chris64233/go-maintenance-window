package maintenance

import (
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"
)

// Engine is the maintenance-window orchestrator. All state transitions are
// serialized under one mutex: acquiring the resources of a request is checked
// and committed as a single critical section, so concurrent starts either take
// every resource they need at once or take none at all.
type Engine struct {
	mu       sync.Mutex
	now      func() time.Time
	store    Store
	notifier Notifier

	resources map[string]*Resource
	requests  map[string]*Request
	history   []Record
}

// Option configures an Engine.
type Option func(*Engine)

// WithClock injects a clock function (tests use it to move time deterministically).
func WithClock(now func() time.Time) Option {
	return func(e *Engine) { e.now = now }
}

// WithNotifier attaches the notifier invoked once per finalized request.
func WithNotifier(n Notifier) Option {
	return func(e *Engine) { e.notifier = n }
}

// NewEngine creates an engine and loads previously persisted state from store
// (nil store means an ephemeral in-memory store).
func NewEngine(store Store, opts ...Option) (*Engine, error) {
	if store == nil {
		store = NewMemoryStore()
	}
	snap, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("maintenance: load persisted state: %w", err)
	}
	e := &Engine{
		now:       time.Now,
		store:     store,
		resources: snap.Resources,
		requests:  snap.Requests,
		history:   snap.History,
	}
	if e.resources == nil {
		e.resources = map[string]*Resource{}
	}
	if e.requests == nil {
		e.requests = map[string]*Request{}
	}
	for _, o := range opts {
		o(e)
	}
	return e, nil
}

// RegisterResource registers a bookable resource.
func (e *Engine) RegisterResource(id, name string) error {
	if id == "" {
		return fmt.Errorf("maintenance: resource id must not be empty")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.resources[id]; ok {
		return fmt.Errorf("%w: %s", ErrResourceExists, id)
	}
	e.resources[id] = &Resource{ID: id, Name: name}
	return e.persistLocked()
}

// Resources returns a copy of the resource registry ordered by id.
func (e *Engine) Resources() []Resource {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Resource, 0, len(e.resources))
	for _, r := range e.resources {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// CreateRequest validates resource relationships and the time range, then
// registers the request in PENDING state (READY when no preparation steps are
// required). No resource runtime state changes: the windows merely reserve
// the schedule until the request is started.
func (e *Engine) CreateRequest(spec RequestSpec) (*Request, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	if spec.ID == "" {
		return nil, fmt.Errorf("maintenance: request id must not be empty")
	}
	if _, ok := e.requests[spec.ID]; ok {
		return nil, fmt.Errorf("%w: %s", ErrRequestExists, spec.ID)
	}
	resources, err := e.normalizeResourcesLocked(spec.Resources)
	if err != nil {
		return nil, err
	}
	steps, err := normalizeSteps(spec.PrepSteps)
	if err != nil {
		return nil, err
	}
	if spec.Duration <= 0 {
		return nil, fmt.Errorf("%w: duration must be positive", ErrInvalidWindow)
	}
	end := spec.Start.Add(spec.Duration)
	if !end.After(now) {
		return nil, fmt.Errorf("%w: window %s..%s has already elapsed", ErrInvalidWindow, spec.Start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	if err := e.checkOverlapLocked(resources, spec.Start, end, ""); err != nil {
		return nil, err
	}

	r := &Request{
		ID:        spec.ID,
		Version:   1,
		Resources: resources,
		Start:     spec.Start,
		Duration:  spec.Duration,
		PrepSteps: steps,
		DoneSteps: map[string]bool{},
		Status:    StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if len(steps) == 0 {
		r.Status = StatusReady
	}
	e.requests[r.ID] = r
	if err := e.persistLocked(); err != nil {
		delete(e.requests, r.ID)
		return nil, err
	}
	return r.clone(), nil
}

// ModifyRequest changes the window and/or resource set of a request that has
// not reached a terminal state. Every modification bumps the version and
// discards preparation receipts, forcing every step to be re-acknowledged
// against the new version; old receipts can no longer advance the request.
func (e *Engine) ModifyRequest(id string, spec ModifySpec) (*Request, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	r, ok := e.requests[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrRequestNotFound, id)
	}
	if r.Status.Final() || r.Status == StatusRunning {
		return nil, fmt.Errorf("%w: request %s is %s", ErrInvalidState, id, r.Status)
	}

	now := e.now()
	resources, err := e.normalizeResourcesLocked(spec.Resources)
	if err != nil {
		return nil, err
	}
	if spec.Duration <= 0 {
		return nil, fmt.Errorf("%w: duration must be positive", ErrInvalidWindow)
	}
	end := spec.Start.Add(spec.Duration)
	if !end.After(now) {
		return nil, fmt.Errorf("%w: window %s..%s has already elapsed", ErrInvalidWindow, spec.Start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	if err := e.checkOverlapLocked(resources, spec.Start, end, id); err != nil {
		return nil, err
	}

	r.Resources = resources
	r.Start = spec.Start
	r.Duration = spec.Duration
	r.Version++
	r.DoneSteps = map[string]bool{}
	r.Reason = ""
	r.Status = StatusPending
	if len(r.PrepSteps) == 0 {
		r.Status = StatusReady
	}
	r.UpdatedAt = now
	if err := e.persistLocked(); err != nil {
		return nil, err
	}
	return r.clone(), nil
}

// ReportPrep records a preparation-step receipt for the given version.
// Receipts may arrive repeatedly or out of order: duplicates and late
// reorderings of the current version are harmless. Receipts reporting a
// different version (typically a late reply against an old version after a
// modify) are silently ignored and never satisfy the current version.
func (e *Engine) ReportPrep(id string, version int, step string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	r, ok := e.requests[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrRequestNotFound, id)
	}
	if r.Status == StatusRunning || r.Status.Final() {
		return fmt.Errorf("%w: request %s is %s", ErrInvalidState, id, r.Status)
	}
	if version != r.Version {
		// Stale receipt for an older (or impossible future) version: ignore.
		return nil
	}
	if !slices.Contains(r.PrepSteps, step) {
		return fmt.Errorf("%w: %s (request %s v%d)", ErrUnknownStep, step, id, version)
	}
	if !r.DoneSteps[step] {
		r.DoneSteps[step] = true
		r.UpdatedAt = e.now()
		if len(r.DoneSteps) == len(r.PrepSteps) {
			r.Status = StatusReady
		}
		return e.persistLocked()
	}
	return nil
}

// Start moves a READY request into RUNNING once its scheduled start time has
// arrived. It acquires usage rights for every declared resource at once: if
// any resource is already held, the resources acquired earlier in the loop
// are released and the request stays READY.
func (e *Engine) Start(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	r, ok := e.requests[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrRequestNotFound, id)
	}
	switch r.Status {
	case StatusReady:
		// proceed
	case StatusPending:
		return fmt.Errorf("%w: request %s is waiting for preparation steps", ErrInvalidState, id)
	default:
		return fmt.Errorf("%w: request %s is %s", ErrInvalidState, id, r.Status)
	}

	now := e.now()
	if now.Before(r.Start) {
		// Do not touch resource state ahead of the scheduled start.
		return fmt.Errorf("%w: request %s starts at %s", ErrTooEarly, id, r.Start.Format(time.RFC3339))
	}
	if !now.Before(r.End()) {
		return fmt.Errorf("%w: window of request %s has elapsed", ErrInvalidWindow, id)
	}

	acquired := make([]string, 0, len(r.Resources))
	for _, rid := range r.Resources {
		res, ok := e.resources[rid]
		if !ok {
			e.releaseLocked(acquired, id)
			return fmt.Errorf("%w: %s", ErrResourceNotFound, rid)
		}
		if res.OccupiedBy != "" && res.OccupiedBy != id {
			// Partial acquisition: roll back everything taken so far.
			e.releaseLocked(acquired, id)
			return fmt.Errorf("%w: %s is held by request %s", ErrResourceBusy, rid, res.OccupiedBy)
		}
		res.OccupiedBy = id
		acquired = append(acquired, rid)
	}

	r.Status = StatusRunning
	r.UpdatedAt = now
	return e.persistLocked()
}

// Complete finishes a running maintenance request, releasing all resources.
func (e *Engine) Complete(id, reason string) error {
	e.mu.Lock()
	r, ok := e.requests[id]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrRequestNotFound, id)
	}
	if r.Status != StatusRunning {
		e.mu.Unlock()
		return fmt.Errorf("%w: request %s is %s, expected RUNNING", ErrInvalidState, id, r.Status)
	}
	recs := e.finalizeLocked(r, StatusCompleted, reason)
	err := e.persistLocked()
	e.mu.Unlock()
	e.notifyAll(recs)
	return err
}

// Abort aborts any non-final request, releasing every resource it holds.
func (e *Engine) Abort(id, reason string) error {
	e.mu.Lock()
	r, ok := e.requests[id]
	if !ok {
		e.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrRequestNotFound, id)
	}
	if r.Status.Final() {
		e.mu.Unlock()
		return fmt.Errorf("%w: request %s is %s", ErrInvalidState, id, r.Status)
	}
	recs := e.finalizeLocked(r, StatusAborted, reason)
	err := e.persistLocked()
	e.mu.Unlock()
	e.notifyAll(recs)
	return err
}

// Tick sweeps timed-out windows: a RUNNING request past its end is aborted as
// a timeout, and a PENDING/READY request past its end expires. Callers may run
// it periodically; it competes with Complete and Abort through the same
// finalizer, so at most one final result is ever produced.
func (e *Engine) Tick() error {
	e.mu.Lock()
	now := e.now()
	var recs []Record
	// Deterministic traversal order (only matters for readability).
	ids := make([]string, 0, len(e.requests))
	for id := range e.requests {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := e.requests[id]
		if r.Status.Final() || now.Before(r.End()) {
			continue
		}
		if r.Status == StatusRunning {
			recs = append(recs, e.finalizeLocked(r, StatusAborted, "maintenance window elapsed: timed out")...)
		} else {
			recs = append(recs, e.finalizeLocked(r, StatusExpired, "maintenance window elapsed before start")...)
		}
	}
	var err error
	if len(recs) > 0 {
		err = e.persistLocked()
	}
	e.mu.Unlock()
	e.notifyAll(recs)
	return err
}

// Calendar returns every window overlapping the half-open range [from, to),
// ordered by start time and request id, independently of request status.
func (e *Engine) Calendar(from, to time.Time) []Window {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !from.Before(to) {
		return []Window{}
	}
	out := make([]Window, 0)
	for _, r := range e.requests {
		if !rangesOverlap(from, to, r.Start, r.End()) {
			continue
		}
		out = append(out, Window{
			RequestID: r.ID,
			Version:   r.Version,
			Resources: append([]string(nil), r.Resources...),
			Start:     r.Start,
			End:       r.End(),
			Status:    r.Status,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].RequestID < out[j].RequestID
	})
	return out
}

// Request returns a copy of a request.
func (e *Engine) Request(id string) (*Request, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.requests[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrRequestNotFound, id)
	}
	return r.clone(), nil
}

// History returns a copy of every final record, ordered by finished time.
func (e *Engine) History() []Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Record, len(e.history))
	copy(out, e.history)
	return out
}

// finalizeLocked is the single path into a terminal state. It is idempotent:
// a request already finalized produces no second record and no second
// notification, whichever of Complete/Abort/Tick wins the race.
func (e *Engine) finalizeLocked(r *Request, outcome Status, reason string) []Record {
	if r.Status.Final() {
		return nil
	}
	now := e.now()
	r.Status = outcome
	r.Reason = reason
	r.UpdatedAt = now
	for _, rid := range r.Resources {
		if res, ok := e.resources[rid]; ok && res.OccupiedBy == r.ID {
			res.OccupiedBy = ""
		}
	}
	rec := Record{
		RequestID:  r.ID,
		Version:    r.Version,
		Outcome:    outcome,
		Reason:     reason,
		Resources:  append([]string(nil), r.Resources...),
		Start:      r.Start,
		End:        r.End(),
		FinishedAt: now,
	}
	e.history = append(e.history, rec)
	return []Record{rec}
}

func (e *Engine) releaseLocked(ids []string, holder string) {
	for _, rid := range ids {
		if res, ok := e.resources[rid]; ok && res.OccupiedBy == holder {
			res.OccupiedBy = ""
		}
	}
}

func (e *Engine) notifyAll(recs []Record) {
	if e.notifier == nil {
		return
	}
	for _, rec := range recs {
		e.notifier.Notify(rec)
	}
}

func (e *Engine) persistLocked() error {
	return e.store.Save(Snapshot{
		Resources: e.resources,
		Requests:  e.requests,
		History:   e.history,
	})
}

func (e *Engine) normalizeResourcesLocked(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("maintenance: at least one resource is required")
	}
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("maintenance: resource id must not be empty")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := e.resources[id]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrResourceNotFound, id)
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func normalizeSteps(steps []string) ([]string, error) {
	seen := make(map[string]bool, len(steps))
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		if s == "" {
			return nil, fmt.Errorf("maintenance: preparation step name must not be empty")
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func (e *Engine) checkOverlapLocked(resources []string, start, end time.Time, exclude string) error {
	for _, other := range e.requests {
		if other.ID == exclude || other.Status.Final() {
			continue
		}
		if !rangesOverlap(start, end, other.Start, other.End()) {
			continue
		}
		for _, rid := range resources {
			if slices.Contains(other.Resources, rid) {
				return fmt.Errorf("%w: resource %s is booked by request %s", ErrWindowOverlap, rid, other.ID)
			}
		}
	}
	return nil
}

// Half-open intervals: touching at an endpoint is allowed.
func rangesOverlap(aStart, aEnd, bStart, bEnd time.Time) bool {
	return aStart.Before(bEnd) && bStart.Before(aEnd)
}
