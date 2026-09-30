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
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Status ResourceStatus `json:"status"`
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
}

// End 返回维护窗口的结束时刻。
func (r *Request) End() time.Time { return r.Start.Add(r.Duration) }

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
	ErrEmptyPrepStep     = errors.New("maintenance: preparation step must not be empty")
	ErrDuplicatePrepStep = errors.New("maintenance: duplicate preparation step in request")
	ErrStalePrepVersion  = errors.New("maintenance: preparation receipt belongs to an old version")
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
