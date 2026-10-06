package maintenance

import (
	"fmt"
	"sort"
	"time"
)

// EmergencyInput 是登记紧急插入申请的入参。
type EmergencyInput struct {
	IdempotencyKey string        // 申请号：相同内容重放返回原结果，内容变化返回冲突
	ResourceIDs    []string      // 资源集合
	Reason         string        // 紧急原因
	Start          time.Time     // 计划开始时间
	Duration       time.Duration // 预计时长
}

// CreateEmergency 登记紧急插入申请：校验资源集合，保存紧急原因、预计时长，
// 并为受影响窗口（涉及资源上与紧急窗口重叠的待开始申请）拍摄快照。
// 相同申请号 + 相同内容的重复调用返回原结果；资源或时长等内容变化返回
// ErrEmergencyConflict。
func (s *Service) CreateEmergency(in EmergencyInput) (*EmergencyRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.IdempotencyKey != "" {
		for _, e := range s.store.st.Emergencies {
			if e.IdempotencyKey != in.IdempotencyKey {
				continue
			}
			if emergencySameContent(e, in) {
				cp := *e
				return &cp, nil // 重放：返回原结果
			}
			return nil, ErrEmergencyConflict
		}
	}
	if err := s.validate(RequestInput{
		ResourceIDs: in.ResourceIDs,
		Start:       in.Start,
		Duration:    in.Duration,
	}); err != nil {
		return nil, err
	}
	now := s.now()
	resources := sortedUnique(in.ResourceIDs)
	versions := make(map[string]int, len(resources))
	for _, rid := range resources {
		versions[rid] = s.store.st.Resources[rid].Version
	}
	e := &EmergencyRequest{
		ID:               newID(),
		IdempotencyKey:   in.IdempotencyKey,
		ResourceIDs:      resources,
		Reason:           in.Reason,
		Start:            in.Start,
		Duration:         in.Duration,
		Status:           EmergencyDraft,
		ResourceVersions: versions,
		Affected:         s.affectedLocked(resources, in.Start, in.Start.Add(in.Duration)),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	s.store.st.Emergencies[e.ID] = e
	if err := s.store.save(); err != nil {
		return nil, err
	}
	cp := *e
	return &cp, nil
}

// emergencySameContent 报告重放的申请内容是否与已登记的一致。
func emergencySameContent(e *EmergencyRequest, in EmergencyInput) bool {
	return e.Reason == in.Reason &&
		e.Start.Equal(in.Start) &&
		e.Duration == in.Duration &&
		equalStrings(e.ResourceIDs, sortedUnique(in.ResourceIDs))
}

// affectedLocked 计算 resources 上与 [start, end) 重叠的受影响窗口快照。
// 只包含待开始（pending/ready）与待重排的普通申请；执行中的占用不属于
// “受影响窗口”（它会阻止确认插入），已完成窗口更不会被回写。
func (s *Service) affectedLocked(resources []string, start, end time.Time) []WindowSnapshot {
	inSet := make(map[string]bool, len(resources))
	for _, rid := range resources {
		inSet[rid] = true
	}
	var out []WindowSnapshot
	for _, req := range s.store.st.Requests {
		switch req.Status {
		case StatusPending, StatusReady, StatusReschedule:
		default:
			continue
		}
		if !Overlaps(start, end, req.Start, req.End()) {
			continue
		}
		shared := false
		for _, rid := range req.ResourceIDs {
			if inSet[rid] {
				shared = true
				break
			}
		}
		if !shared {
			continue
		}
		out = append(out, WindowSnapshot{
			RequestID:   req.ID,
			ResourceIDs: append([]string(nil), req.ResourceIDs...),
			Version:     req.Version,
			Start:       req.Start,
			End:         req.End(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestID < out[j].RequestID })
	return out
}

// snapshotStaleLocked 核对登记时的快照：任一资源版本变化，或任一受影响
// 窗口的版本/时间/状态变化（包括被完成），都使原申请失效。
func (s *Service) snapshotStaleLocked(e *EmergencyRequest) bool {
	for _, rid := range e.ResourceIDs {
		r, ok := s.store.st.Resources[rid]
		if !ok || r.Version != e.ResourceVersions[rid] {
			return true
		}
	}
	current := s.affectedLocked(e.ResourceIDs, e.Start, e.End())
	if len(current) != len(e.Affected) {
		return true
	}
	for i, snap := range e.Affected {
		cur := current[i]
		if cur.RequestID != snap.RequestID || cur.Version != snap.Version ||
			!cur.Start.Equal(snap.Start) || !cur.End.Equal(snap.End) {
			return true
		}
		req := s.store.st.Requests[snap.RequestID]
		if req == nil || req.Status.Final() {
			return true // 已完成的窗口不能被回写
		}
	}
	return false
}

// ConfirmEmergency 确认插入：先核对快照仍然有效，再一次性取得全部资源。
// 任一资源被执行中的窗口占用时整体失败，原排期完全保留，不会只挪走其中
// 一项。成功后紧急窗口开始，受影响的普通窗口转为待重排并保留原计划时间。
func (s *Service) ConfirmEmergency(id string) (*EmergencyRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.store.st.Emergencies[id]
	if !ok {
		return nil, ErrEmergencyNotFound
	}
	if e.Status == EmergencyExecuting {
		cp := *e
		return &cp, nil // 幂等
	}
	if e.Status.Final() {
		return nil, ErrEmergencyFinal
	}
	now := s.now()
	if now.Before(e.Start) {
		return nil, ErrWindowNotReached
	}
	if !now.Before(e.End()) {
		return nil, ErrInvalidTimeRange
	}
	if s.snapshotStaleLocked(e) {
		e.Status = EmergencyStale
		e.UpdatedAt = now
		e.FinalizedAt = &now
		_ = s.store.save()
		return nil, ErrStaleEmergency
	}
	// 先整体检查全部资源，无法取得时不做任何改动。
	for _, rid := range e.ResourceIDs {
		if s.resourceBusyLocked(rid, e.Start, e.End()) {
			return nil, fmt.Errorf("%w: %s", ErrResourceBusy, rid)
		}
	}
	// 受影响的普通窗口转为待重排，保留原计划时间与重排原因。
	for _, snap := range e.Affected {
		req := s.store.st.Requests[snap.RequestID]
		if req == nil || req.Status == StatusReschedule {
			continue
		}
		origStart := req.Start
		req.OriginalStart = &origStart
		req.OriginalDuration = req.Duration
		req.RescheduleReason = fmt.Sprintf("紧急插入 %s: %s", e.ID, e.Reason)
		req.Status = StatusReschedule
		req.UpdatedAt = now
	}
	for _, rid := range e.ResourceIDs {
		s.acquireLocked(rid, e.ID, e.Start, e.End())
	}
	e.Status = EmergencyExecuting
	e.UpdatedAt = now
	if err := s.store.save(); err != nil {
		return nil, err
	}
	cp := *e
	return &cp, nil
}

// CancelEmergency 取消紧急申请。草稿直接取消；执行中取消会释放其自身
// 占用的资源；已终态的申请返回 ErrEmergencyFinal，且不会触碰任何资源——
// 迟到的取消不得释放别人（如已接手的普通窗口）的资源。
func (s *Service) CancelEmergency(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.store.st.Emergencies[id]
	if !ok {
		return ErrEmergencyNotFound
	}
	switch e.Status {
	case EmergencyDraft:
		e.Status = EmergencyCancelled
		e.UpdatedAt = s.now()
		now := s.now()
		e.FinalizedAt = &now
		return s.store.save()
	case EmergencyExecuting:
		return s.finalizeEmergencyLocked(e, EmergencyCancelled, s.now())
	case EmergencyCancelled:
		return nil // 幂等
	default:
		return ErrEmergencyFinal
	}
}

// CompleteEmergency 正常完成执行中的紧急窗口，释放其全部资源。
func (s *Service) CompleteEmergency(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.store.st.Emergencies[id]
	if !ok {
		return ErrEmergencyNotFound
	}
	if e.Status != EmergencyExecuting {
		if e.Status.Final() {
			return ErrEmergencyFinal
		}
		return ErrNotExecuting
	}
	return s.finalizeEmergencyLocked(e, EmergencyCompleted, s.now())
}

// finalizeEmergencyLocked 是紧急申请终态的唯一入口：只释放其自身占用，
// 与确认/取消/普通窗口启动竞争时只形成一个资源占用结果。
func (s *Service) finalizeEmergencyLocked(e *EmergencyRequest, outcome EmergencyStatus, now time.Time) error {
	if e.Status.Final() {
		return ErrEmergencyFinal
	}
	e.Status = outcome
	e.UpdatedAt = now
	e.FinalizedAt = &now
	s.releaseLocked(e.ResourceIDs, e.ID)
	return s.store.save()
}

// GetEmergency 返回紧急申请快照。
func (s *Service) GetEmergency(id string) (*EmergencyRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.store.st.Emergencies[id]
	if !ok {
		return nil, ErrEmergencyNotFound
	}
	cp := *e
	return &cp, nil
}

// EmergencyReport 返回紧急插入的完整查询结果：排期前后对比、受影响窗口、
// 资源占用与重排原因。
func (s *Service) EmergencyReport(id string) (*EmergencyReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.store.st.Emergencies[id]
	if !ok {
		return nil, ErrEmergencyNotFound
	}
	inSet := make(map[string]bool, len(e.ResourceIDs))
	for _, rid := range e.ResourceIDs {
		inSet[rid] = true
	}
	report := &EmergencyReport{Emergency: func() *EmergencyRequest { cp := *e; return &cp }()}
	// 排期前：快照中受影响窗口的计划占用。
	for _, snap := range e.Affected {
		aw := AffectedWindow{Snapshot: snap}
		if req := s.store.st.Requests[snap.RequestID]; req != nil {
			aw.Status = req.Status
			aw.OriginalStart = req.OriginalStart
			if req.OriginalStart != nil {
				origEnd := req.OriginalStart.Add(req.OriginalDuration)
				aw.OriginalEnd = &origEnd
			}
			aw.RescheduleReason = req.RescheduleReason
		}
		report.Affected = append(report.Affected, aw)
		for _, rid := range snap.ResourceIDs {
			if inSet[rid] {
				report.Before = append(report.Before, Window{
					ResourceID: rid,
					RequestID:  snap.RequestID,
					Kind:       "scheduled",
					Start:      snap.Start,
					End:        snap.End,
				})
			}
		}
	}
	// 排期后：涉及资源上当前的计划与实际占用。
	for _, w := range s.store.st.Occupancy {
		if inSet[w.ResourceID] && Overlaps(e.Start, e.End(), w.Start, w.End) {
			report.Occupancy = append(report.Occupancy, w)
		}
	}
	for _, req := range s.store.st.Requests {
		switch req.Status {
		case StatusPending, StatusReady, StatusReschedule:
		default:
			continue
		}
		if !Overlaps(e.Start, e.End(), req.Start, req.End()) {
			continue
		}
		for _, rid := range req.ResourceIDs {
			if inSet[rid] {
				report.After = append(report.After, Window{
					ResourceID: rid,
					RequestID:  req.ID,
					Kind:       "scheduled",
					Start:      req.Start,
					End:        req.End(),
				})
			}
		}
	}
	sortWindows := func(ws []Window) {
		sort.Slice(ws, func(i, j int) bool {
			if !ws[i].Start.Equal(ws[j].Start) {
				return ws[i].Start.Before(ws[j].Start)
			}
			return ws[i].ResourceID < ws[j].ResourceID
		})
	}
	sortWindows(report.Before)
	sortWindows(report.After)
	sortWindows(report.Occupancy)
	return report, nil
}

// Reschedule 把待重排的普通窗口改期到 newStart。只调整时间并恢复为
// ready/pending，版本号与准备进度保持不变；原计划时间仍保留在
// OriginalStart/OriginalDuration 中供查询对比。
func (s *Service) Reschedule(id string, newStart time.Time) (*Request, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.store.st.Requests[id]
	if !ok {
		return nil, ErrRequestNotFound
	}
	if req.Status != StatusReschedule {
		return nil, ErrNotReschedulable
	}
	if newStart.Before(s.now()) {
		return nil, ErrInvalidTimeRange
	}
	req.Start = newStart
	req.Status = StatusPending
	if len(req.PrepDone) == len(req.PrepSteps) {
		req.Status = StatusReady
	}
	req.UpdatedAt = s.now()
	if err := s.store.save(); err != nil {
		return nil, err
	}
	cp := *req
	return &cp, nil
}
