package maintenance

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Notifier 在申请定案（完成/中止/超时）时被调用，每个申请至多一次。
type Notifier interface {
	Notify(Notification)
}

// NotifierFunc 让普通函数可用作 Notifier。
type NotifierFunc func(Notification)

// Notify 实现 Notifier。
func (f NotifierFunc) Notify(n Notification) { f(n) }

// Service 是维护窗口编排的核心服务。所有变更操作都在同一把互斥锁下
// 完成“校验—修改—持久化”，因此并发开始时的资源获取是原子的。
type Service struct {
	mu        sync.Mutex
	store     *Store
	now       func() time.Time
	notifiers []Notifier
}

// Option 定制 Service。
type Option func(*Service)

// WithClock 注入时钟（测试用）。
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithNotifier 注册额外的定案通知钩子。
func WithNotifier(n Notifier) Option {
	return func(s *Service) { s.notifiers = append(s.notifiers, n) }
}

// NewService 基于 store 创建服务。
func NewService(store *Store, opts ...Option) *Service {
	s := &Service{store: store, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// RegisterResource 登记资源，初始状态为 running。
func (s *Service) RegisterResource(id, name string) (*Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.store.st.Resources[id]; ok {
		return nil, ErrResourceExists
	}
	r := &Resource{ID: id, Name: name, Status: ResourceRunning}
	s.store.st.Resources[id] = r
	return r, s.store.save()
}

// Resources 返回全部资源（按 ID 排序）。
func (s *Service) Resources() []*Resource {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Resource, 0, len(s.store.st.Resources))
	for _, r := range s.store.st.Resources {
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RequestInput 是创建/修改维护申请的入参。
type RequestInput struct {
	ResourceIDs []string
	Start       time.Time
	Duration    time.Duration
	PrepSteps   []string
}

// validate 校验资源关系与时间范围。
func (s *Service) validate(in RequestInput) error {
	if len(in.ResourceIDs) == 0 {
		return ErrEmptyResourceSet
	}
	seen := make(map[string]bool, len(in.ResourceIDs))
	for _, id := range in.ResourceIDs {
		if seen[id] {
			return fmt.Errorf("%w: %s", ErrDuplicateResource, id)
		}
		seen[id] = true
		if _, ok := s.store.st.Resources[id]; !ok {
			return fmt.Errorf("%w: %s", ErrResourceNotFound, id)
		}
	}
	if in.Duration <= 0 || in.Start.Before(s.now()) {
		return ErrInvalidTimeRange
	}
	seenStep := make(map[string]bool, len(in.PrepSteps))
	for _, step := range in.PrepSteps {
		if step == "" {
			return ErrEmptyPrepStep
		}
		if seenStep[step] {
			return fmt.Errorf("%w: %s", ErrDuplicatePrepStep, step)
		}
		seenStep[step] = true
	}
	return nil
}

// CreateRequest 创建维护申请。仅做校验与登记，不改变任何资源运行状态。
// 无准备步骤的申请创建后即为 ready。
func (s *Service) CreateRequest(in RequestInput) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validate(in); err != nil {
		return nil, err
	}
	now := s.now()
	steps := append([]string(nil), in.PrepSteps...)
	req := &Request{
		ID:          newID(),
		ResourceIDs: sortedUnique(in.ResourceIDs),
		Start:       in.Start,
		Duration:    in.Duration,
		PrepSteps:   steps,
		Version:     1,
		Status:      StatusPending,
		PrepDone:    make(map[string]bool),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if len(steps) == 0 {
		req.Status = StatusReady
	}
	s.store.st.Requests[req.ID] = req
	if err := s.store.save(); err != nil {
		return nil, err
	}
	cp := *req
	return &cp, nil
}

// GetRequest 返回申请快照。
func (s *Service) GetRequest(id string) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.store.st.Requests[id]
	if !ok {
		return nil, ErrRequestNotFound
	}
	cp := *req
	return &cp, nil
}

// ModifyRequest 修改尚未开始的申请。时间、资源集合或准备步骤发生变化时
// 版本号递增，旧版本的准备回执随之失效。
func (s *Service) ModifyRequest(id string, in RequestInput) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.store.st.Requests[id]
	if !ok {
		return nil, ErrRequestNotFound
	}
	if req.Status != StatusPending && req.Status != StatusReady {
		return nil, ErrNotModifiable
	}
	if err := s.validate(in); err != nil {
		return nil, err
	}
	changed := !req.Start.Equal(in.Start) ||
		req.Duration != in.Duration ||
		!equalStrings(req.ResourceIDs, sortedUnique(in.ResourceIDs)) ||
		!equalStrings(req.PrepSteps, in.PrepSteps)
	if changed {
		req.Version++
		req.PrepDone = make(map[string]bool)
	}
	req.ResourceIDs = sortedUnique(in.ResourceIDs)
	req.Start = in.Start
	req.Duration = in.Duration
	req.PrepSteps = append([]string(nil), in.PrepSteps...)
	// 内容未变化时保留版本、已收回执与生命周期状态（空修改不得把
	// ready 打回 pending）；发生变化时旧回执失效，按新步骤集重新判定。
	if changed {
		req.Status = StatusPending
		if len(req.PrepSteps) == 0 || len(req.PrepDone) == len(req.PrepSteps) {
			req.Status = StatusReady
		}
	}
	req.UpdatedAt = s.now()
	if err := s.store.save(); err != nil {
		return nil, err
	}
	cp := *req
	return &cp, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ReportPrep 上报准备步骤回执。回执允许重复或乱序到达；
// 版本不匹配的回执不会推进当前版本。全部步骤完成后申请转为 ready。
func (s *Service) ReportPrep(requestID string, version int, step string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.store.st.Requests[requestID]
	if !ok {
		return ErrRequestNotFound
	}
	if req.Status.Final() || req.Status == StatusExecuting {
		return ErrNotModifiable
	}
	if version != req.Version {
		return ErrStalePrepVersion
	}
	known := false
	for _, st := range req.PrepSteps {
		if st == step {
			known = true
			break
		}
	}
	if !known {
		return ErrUnknownPrepStep
	}
	req.PrepDone[step] = true // 重复回报幂等
	if len(req.PrepDone) == len(req.PrepSteps) {
		req.Status = StatusReady
	}
	req.UpdatedAt = s.now()
	return s.store.save()
}

// Start 开始执行维护：原子地取得全部资源的使用权。
// 任一资源在窗口内已被占用时整体失败并回滚，不会留下部分占用。
func (s *Service) Start(requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.store.st.Requests[requestID]
	if !ok {
		return ErrRequestNotFound
	}
	if req.Status.Final() {
		return ErrAlreadyFinal
	}
	if req.Status == StatusExecuting {
		return nil // 幂等
	}
	if req.Status != StatusReady {
		return ErrNotReady
	}
	now := s.now()
	if now.Before(req.Start) {
		return ErrWindowNotReached
	}
	if !now.Before(req.End()) {
		return s.finalizeLocked(req, StatusExpired, "maintenance window missed", now)
	}
	// 先校验全部资源均可用，再一次性占用，保证“取得全部或什么都不取”，
	// 不会出现部分资源被改状态后再回滚的中间态。
	for _, rid := range req.ResourceIDs {
		if s.resourceBusyLocked(rid, req.Start, req.End()) {
			return fmt.Errorf("%w: %s", ErrResourceBusy, rid)
		}
	}
	for _, rid := range req.ResourceIDs {
		s.acquireLocked(rid, req)
	}
	req.Status = StatusExecuting
	req.UpdatedAt = now
	return s.store.save()
}

func (s *Service) resourceBusyLocked(resourceID string, start, end time.Time) bool {
	for _, w := range s.store.st.Occupancy {
		if w.ResourceID == resourceID && Overlaps(start, end, w.Start, w.End) {
			return true
		}
	}
	return false
}

func (s *Service) acquireLocked(resourceID string, req *Request) {
	s.store.st.Occupancy = append(s.store.st.Occupancy, Window{
		ResourceID: resourceID,
		RequestID:  req.ID,
		Kind:       "active",
		Start:      req.Start,
		End:        req.End(),
	})
	if r, ok := s.store.st.Resources[resourceID]; ok {
		r.Status = ResourceMaintenance
	}
}

// releaseLocked 释放指定申请在 resources 上的占用并恢复资源运行状态。
func (s *Service) releaseLocked(resources []string, requestID string) {
	if len(resources) == 0 {
		return
	}
	inSet := make(map[string]bool, len(resources))
	for _, r := range resources {
		inSet[r] = true
	}
	kept := s.store.st.Occupancy[:0]
	for _, w := range s.store.st.Occupancy {
		if w.RequestID == requestID && inSet[w.ResourceID] {
			continue
		}
		kept = append(kept, w)
	}
	s.store.st.Occupancy = kept
	for _, rid := range resources {
		if r, ok := s.store.st.Resources[rid]; ok {
			r.Status = ResourceRunning
		}
	}
}

// Complete 正常完成执行中的维护，释放全部资源并记录原因。
func (s *Service) Complete(requestID, reason string) error {
	return s.finalize(requestID, StatusCompleted, reason)
}

// Abort 中止执行中的维护，释放全部资源并记录原因。
func (s *Service) Abort(requestID, reason string) error {
	return s.finalize(requestID, StatusAborted, reason)
}

func (s *Service) finalize(requestID string, outcome Status, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.store.st.Requests[requestID]
	if !ok {
		return ErrRequestNotFound
	}
	if req.Status != StatusExecuting {
		if req.Status.Final() {
			return ErrAlreadyFinal
		}
		return ErrNotExecuting
	}
	return s.finalizeLocked(req, outcome, reason, s.now())
}

// finalizeLocked 是完成/中止/超时共用的唯一定案入口：
// 若申请已处于终态则直接返回，保证只生成一份最终记录和一条通知。
func (s *Service) finalizeLocked(req *Request, outcome Status, reason string, now time.Time) error {
	if req.Status.Final() {
		return ErrAlreadyFinal
	}
	req.Status = outcome
	req.Reason = reason
	req.UpdatedAt = now
	req.FinalizedAt = &now
	s.releaseLocked(req.ResourceIDs, req.ID)
	rec := FinalRecord{RequestID: req.ID, Outcome: outcome, Reason: reason, At: now}
	s.store.st.History = append(s.store.st.History, rec)
	n := Notification{RequestID: req.ID, Outcome: outcome, Reason: reason, At: now}
	s.store.st.Notifications = append(s.store.st.Notifications, n)
	for _, nt := range s.notifiers {
		nt.Notify(n)
	}
	return s.store.save()
}

// Expire 处理超时：错过窗口的待开始申请、以及执行超过窗口结束的维护，
// 都会被定案为 expired 并释放资源。与 Complete/Abort 竞争时只有一方生效。
// 返回本次被定案的申请 ID。
func (s *Service) Expire() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var expired []string
	ids := make([]string, 0, len(s.store.st.Requests))
	for id := range s.store.st.Requests {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		req := s.store.st.Requests[id]
		if req.Status.Final() {
			continue
		}
		switch req.Status {
		case StatusPending, StatusReady:
			if !now.Before(req.End()) {
				_ = s.finalizeLocked(req, StatusExpired, "maintenance window missed", now)
				expired = append(expired, id)
			}
		case StatusExecuting:
			if !now.Before(req.End()) {
				_ = s.finalizeLocked(req, StatusExpired, "execution exceeded maintenance window", now)
				expired = append(expired, id)
			}
		}
	}
	return expired
}

// Calendar 返回与 [from, to) 重叠的全部窗口：执行中的实际占用（active）
// 以及已排期但尚未开始的计划窗口（scheduled），按开始时间排序。
func (s *Service) Calendar(from, to time.Time) []Window {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Window
	for _, w := range s.store.st.Occupancy {
		if Overlaps(from, to, w.Start, w.End) {
			out = append(out, w)
		}
	}
	for _, req := range s.store.st.Requests {
		if req.Status != StatusPending && req.Status != StatusReady {
			continue
		}
		if !Overlaps(from, to, req.Start, req.End()) {
			continue
		}
		for _, rid := range req.ResourceIDs {
			out = append(out, Window{
				ResourceID: rid,
				RequestID:  req.ID,
				Kind:       "scheduled",
				Start:      req.Start,
				End:        req.End(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ResourceID < out[j].ResourceID
	})
	return out
}

// History 返回全部最终定案记录。
func (s *Service) History() []FinalRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]FinalRecord(nil), s.store.st.History...)
}

// Notifications 返回已发出的定案通知。
func (s *Service) Notifications() []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Notification(nil), s.store.st.Notifications...)
}
