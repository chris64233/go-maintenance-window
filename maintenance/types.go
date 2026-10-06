// Package maintenance 实现跨资源的维护窗口编排：
// 资源登记、维护申请、版本化准备回执、原子资源占用、
// 完成/中止/超时的单次定案以及日历查询。
package maintenance

import (
	"errors"
	"sort"
	"time"
)

// 资源运行状态。只有在维护正式开始后资源才进入 maintenance，
// 申请创建/修改阶段不得提前改变资源运行状态。
type ResourceStatus string

const (
	ResourceRunning     ResourceStatus = "running"
	ResourceMaintenance ResourceStatus = "maintenance"
)

// Resource 是可被维护申请占用的资源。
type Resource struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Status  ResourceStatus `json:"status"`
	Version int            `json:"version"` // 运行状态每次切换时递增
}

// Status 是维护申请的生命周期状态。
type Status string

const (
	StatusPending   Status = "pending"   // 已创建，准备步骤未全部完成
	StatusReady     Status = "ready"     // 当前版本全部准备步骤已完成，可开始
	StatusExecuting Status = "executing" // 已占用全部资源，维护进行中
	StatusCompleted Status = "completed" // 正常完成（终态）
	StatusAborted   Status = "aborted"   // 人工中止（终态）
	StatusExpired   Status = "expired"   // 超时（终态：错过窗口或执行超时）
	// StatusReschedule 表示窗口被紧急插入打断，等待重排。
	// 原计划时间保留在 OriginalStart/OriginalDuration 中，不会被悄悄移除。
	StatusReschedule Status = "reschedule"
)

// Final 报告状态是否为终态。
func (s Status) Final() bool {
	return s == StatusCompleted || s == StatusAborted || s == StatusExpired
}

// Request 是一次维护申请。Version 在时间、资源集合或准备步骤
// 被修改后递增；旧版本的准备回执不能推进新版本。
type Request struct {
	ID          string          `json:"id"`
	ResourceIDs []string        `json:"resource_ids"`
	Start       time.Time       `json:"start"`
	Duration    time.Duration   `json:"duration"`
	PrepSteps   []string        `json:"prep_steps"`
	Version     int             `json:"version"`
	Status      Status          `json:"status"`
	PrepDone    map[string]bool `json:"prep_done"` // 仅当前版本有效
	Reason      string          `json:"reason,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	FinalizedAt *time.Time      `json:"finalized_at,omitempty"`

	// 以下字段仅在窗口被紧急插入打断（StatusReschedule）后有意义：
	// 原计划时间被完整保留，重排只改 Start，不回写这些字段。
	OriginalStart    *time.Time    `json:"original_start,omitempty"`
	OriginalDuration time.Duration `json:"original_duration,omitempty"`
	RescheduleReason string        `json:"reschedule_reason,omitempty"`
}

// End 返回维护窗口的结束时刻。
func (r *Request) End() time.Time { return r.Start.Add(r.Duration) }

// EmergencyStatus 是紧急插入申请的生命周期状态。
type EmergencyStatus string

const (
	EmergencyDraft     EmergencyStatus = "draft"     // 已登记快照，等待确认插入
	EmergencyExecuting EmergencyStatus = "executing" // 已确认插入，紧急窗口进行中
	EmergencyCompleted EmergencyStatus = "completed" // 紧急窗口正常完成（终态）
	EmergencyCancelled EmergencyStatus = "cancelled" // 已取消（终态）
	EmergencyStale     EmergencyStatus = "stale"     // 资源或窗口版本变化，申请失效（终态）
	EmergencyExpired   EmergencyStatus = "expired"   // 执行超过预计时长（终态）
)

// Final 报告紧急申请状态是否为终态。
func (s EmergencyStatus) Final() bool {
	switch s {
	case EmergencyCompleted, EmergencyCancelled, EmergencyStale, EmergencyExpired:
		return true
	}
	return false
}

// WindowSnapshot 是紧急申请登记时对一个受影响窗口的快照。
// 确认插入前会逐项核对：任何版本或时间变化都会使原申请失效。
type WindowSnapshot struct {
	RequestID   string    `json:"request_id"`
	ResourceIDs []string  `json:"resource_ids"`
	Version     int       `json:"version"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
}

// EmergencyRequest 是一次紧急插入申请。登记时保存资源集合、紧急原因、
// 预计时长以及受影响窗口快照；确认时一次性取得全部资源，否则原排期完全保留。
type EmergencyRequest struct {
	ID               string           `json:"id"`
	IdempotencyKey   string           `json:"idempotency_key"` // 申请号：相同内容重放返回原结果
	ResourceIDs      []string         `json:"resource_ids"`
	Reason           string           `json:"reason"` // 紧急原因
	Start            time.Time        `json:"start"`
	Duration         time.Duration    `json:"duration"` // 预计时长
	Status           EmergencyStatus  `json:"status"`
	ResourceVersions map[string]int   `json:"resource_versions"` // 登记时的资源版本
	Affected         []WindowSnapshot `json:"affected"`          // 受影响窗口快照
	CreatedAt        time.Time        `json:"created_at"`
	UpdatedAt        time.Time        `json:"updated_at"`
	FinalizedAt      *time.Time       `json:"finalized_at,omitempty"`
}

// End 返回紧急窗口的结束时刻。
func (e *EmergencyRequest) End() time.Time { return e.Start.Add(e.Duration) }

// AffectedWindow 是查询结果中对一个受影响窗口的描述：
// 快照内容 + 当前状态 + 保留的原计划时间与重排原因。
type AffectedWindow struct {
	Snapshot         WindowSnapshot `json:"snapshot"`
	Status           Status         `json:"status"`
	OriginalStart    *time.Time     `json:"original_start,omitempty"`
	OriginalEnd      *time.Time     `json:"original_end,omitempty"`
	RescheduleReason string         `json:"reschedule_reason,omitempty"`
}

// EmergencyReport 是紧急插入的查询结果：排期前后对比、受影响窗口、
// 资源占用与重排原因。
type EmergencyReport struct {
	Emergency *EmergencyRequest `json:"emergency"`
	Before    []Window          `json:"before"`    // 插入前受影响窗口的计划占用
	After     []Window          `json:"after"`     // 当前这些资源上的计划与实际占用
	Affected  []AffectedWindow  `json:"affected"`  // 受影响窗口及其重排原因
	Occupancy []Window          `json:"occupancy"` // 涉及资源上的 active 占用
}

// Window 是某资源上一段被占用/计划占用的时间区间。
type Window struct {
	ResourceID string    `json:"resource_id"`
	RequestID  string    `json:"request_id"`
	Kind       string    `json:"kind"` // "active"（执行中占用）或 "scheduled"（已排期未开始）
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
}

// FinalRecord 是申请的最终定案记录，每个申请至多一条。
type FinalRecord struct {
	RequestID string    `json:"request_id"`
	Outcome   Status    `json:"outcome"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

// Notification 是定案时发出的通知，与 FinalRecord 一一对应。
type Notification struct {
	RequestID string    `json:"request_id"`
	Outcome   Status    `json:"outcome"`
	Reason    string    `json:"reason"`
	At        time.Time `json:"at"`
}

// 业务错误。
var (
	ErrResourceExists    = errors.New("maintenance: resource already registered")
	ErrResourceNotFound  = errors.New("maintenance: resource not found")
	ErrRequestNotFound   = errors.New("maintenance: request not found")
	ErrInvalidTimeRange  = errors.New("maintenance: invalid time range")
	ErrEmptyResourceSet  = errors.New("maintenance: request must occupy at least one resource")
	ErrDuplicateResource = errors.New("maintenance: duplicate resource in request")
	ErrNotModifiable     = errors.New("maintenance: request can no longer be modified")
	ErrNotReady          = errors.New("maintenance: preparation steps not complete for current version")
	ErrNotExecuting      = errors.New("maintenance: request is not executing")
	ErrAlreadyFinal      = errors.New("maintenance: request already finalized")
	ErrResourceBusy      = errors.New("maintenance: resource window overlaps an active maintenance")
	ErrWindowNotReached  = errors.New("maintenance: maintenance window has not started yet")
	ErrUnknownPrepStep   = errors.New("maintenance: unknown preparation step")
	ErrStalePrepVersion  = errors.New("maintenance: preparation receipt belongs to an old version")
	ErrEmergencyNotFound = errors.New("maintenance: emergency request not found")
	ErrEmergencyConflict = errors.New("maintenance: idempotency key reused with different content")
	ErrEmergencyFinal    = errors.New("maintenance: emergency request already finalized")
	ErrStaleEmergency    = errors.New("maintenance: emergency snapshot outdated by resource or window changes")
	ErrNotReschedulable  = errors.New("maintenance: request is not waiting for reschedule")
)

// Overlaps 报告两个半开区间 [s1,e1) 与 [s2,e2) 是否重叠。
func Overlaps(s1, e1, s2, e2 time.Time) bool {
	return s1.Before(e2) && s2.Before(e1)
}

// sortedUnique 返回排序去重后的副本，用于稳定的资源顺序（避免死锁并便于比较）。
func sortedUnique(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}
