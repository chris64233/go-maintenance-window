# go-maintenance-window

跨资源维护窗口编排库：维护申请可以一次占用多项资源，声明开始时间与持续时间，并要求先完成若干准备步骤。库负责校验资源关系与时间范围、防止窗口重叠、保证资源使用权“要么全拿到、要么都不拿”，并持久化占用与历史。

## 核心语义

1. **创建即校验，开始前不改变资源状态**
   `CreateRequest` 校验资源是否已登记、时长是否为正、窗口是否尚未结束，以及与同一资源上其他非终态申请是否重叠（半开区间 `[start, end)`，首尾相接允许）。创建成功后申请处于 `PENDING`（无准备步骤时为 `READY`），资源的 `occupied_by` 始终为空，直到显式 `Start`。

2. **窗口不重叠 + 资源全有或全无获取**
   所有状态转换在同一把互斥锁内完成。`Start` 逐个占用资源，若任一资源正被其他执行中的申请持有，立即回滚已取得的全部资源，申请保持 `READY`，不会留下部分锁定。并发开始时，只有一次取得全部资源的申请能进入 `RUNNING`。

3. **准备回执版本化**
   回执可重复、可乱序到达，重复回执幂等。只有当前版本的**全部**步骤都有回执时申请才变为 `READY`。修改时间或资源集合（`ModifyRequest`）会使 `Version` 加一、清空回执并回到 `PENDING`；携带旧版本号的回执被直接忽略，无法推进新版申请。

4. **唯一终态记录与通知**
   `Complete` / `Abort` / 超时扫描 `Tick` 竞争时走同一个终态化入口，终态化是幂等的：每个申请只追加一份 `Record` 到历史、只调用一次 `Notifier`，并释放全部资源、记录原因。
   - 执行中的申请超过结束时间：标记 `ABORTED`（原因：超时）。
   - 未开始（`PENDING`/`READY`）的申请超过结束时间：标记 `EXPIRED`。

5. **日历查询与持久化**
   `Calendar(from, to)` 返回与时间区间相交的全部窗口（含已完成的历史窗口）。`FileStore` 以单个 JSON 文件保存资源登记表（含当前占用）、申请和历史，写入采用临时文件 + rename 原子替换；另有 `MemoryStore` 供测试和临时运行使用。

## 状态机

```
                全部步骤回执齐备
 PENDING ─────────────────────────▶ READY
    │  ▲                              │
    │  │ 修改时间/资源(版本+1,清空回执) │ Start（到点且全部资源可用）
    ▼  │                              ▼
  (PENDING 重置)                    RUNNING
                                     │  │  │
              Complete               │  │  └ Abort
             ┌───────────────────────┘  └────────────┐
             ▼                                       ▼
        COMPLETED                                ABORTED
   PENDING/READY 超过结束时间 ─▶ EXPIRED ；RUNNING 超时 ─▶ ABORTED
```

## API 概览（`maintenance` 包）

| 方法 | 说明 |
| --- | --- |
| `NewEngine(store, opts...)` | 创建引擎并从 Store 载入状态；支持 `WithClock`、`WithNotifier` |
| `RegisterResource(id, name)` | 资源登记 |
| `CreateRequest(RequestSpec)` | 创建维护申请（校验资源、时间、重叠） |
| `ModifyRequest(id, ModifySpec)` | 修改时间/资源集合，版本递增、回执清空 |
| `ReportPrep(id, version, step)` | 准备步骤回执（幂等、乱序、版本过滤） |
| `Start(id)` | 到点后原子取得全部资源并进入 RUNNING |
| `Complete(id, reason)` / `Abort(id, reason)` | 完成 / 中止，释放资源并留下原因 |
| `Tick()` | 超时扫描：运行中超时中止、未开始超时过期 |
| `Calendar(from, to)` | 日历查询 |
| `Request(id)` / `Resources()` / `History()` | 状态查询 |

主要错误哨兵：`ErrResourceNotFound`、`ErrResourceExists`、`ErrRequestExists`、`ErrInvalidWindow`、`ErrWindowOverlap`、`ErrResourceBusy`、`ErrInvalidState`、`ErrUnknownStep`、`ErrTooEarly`（配合 `errors.Is` 使用）。

## 最小示例

```go
eng, _ := maintenance.NewEngine(
    maintenance.NewFileStore("data/state.json"),
    maintenance.WithNotifier(maintenance.NotifierFunc(func(r maintenance.Record) {
        log.Printf("final: %s -> %s (%s)", r.RequestID, r.Outcome, r.Reason)
    })),
)
_ = eng.RegisterResource("db-1", "primary database")

_, _ = eng.CreateRequest(maintenance.RequestSpec{
    ID:        "mw-001",
    Resources: []string{"db-1"},
    Start:     time.Now(),
    Duration:  30 * time.Minute,
    PrepSteps: []string{"backup", "notify-users"},
})
_ = eng.ReportPrep("mw-001", 1, "backup")
_ = eng.ReportPrep("mw-001", 1, "notify-users")
_ = eng.Start("mw-001")                       // READY -> RUNNING
_ = eng.Complete("mw-001", "upgrade done")    // 释放资源、写历史、发一次通知
```

## 运行方式

```bash
# 运行全部自动化测试（含竞态检测）
go test -race ./...

# 跑一遍完整生命周期演示：登记 -> 创建/重叠拒绝 -> 修改升版本 ->
# 回执（旧版本被忽略）-> 原子开始 -> 完成 -> 日历 -> 重启后读回历史
go run ./cmd/demo
```

## 项目结构

```
maintenance/        核心库
  types.go          领域模型：Resource / Request / Record / Window / Status
  errors.go         哨兵错误
  engine.go         编排引擎：校验、版本、全有或全无获取、唯一终态化、日历
  store.go          Store 接口、FileStore（原子 JSON 持久化）、MemoryStore
  notify.go         Notifier 接口
  engine_test.go    自动化测试
cmd/demo/           端到端演示
```

## 测试覆盖

- 创建校验：未知/空资源、重复 ID、非正时长、已过期窗口、同资源重叠、首尾相接与多资源不重叠放行
- 创建后、提前 `Start` 被拒后资源运行状态保持不变
- 回执乱序、重复、未知步骤；修改后版本递增、回执重置，旧版本/未来版本回执不能推进
- `Start` 部分取得资源后失败会整体回滚，申请保持 READY；超时清扫后可再次启动
- 完成/中止/超时高并发竞争：恰好一份历史记录、恰好一次通知、资源全部释放
- 超时扫描：运行中→ABORTED、未开始→EXPIRED、未来窗口不受影响
- 日历半开区间查询，含终态窗口
- `FileStore` 重启后占用、申请版本/状态与历史均可读回
