# go-maintenance-window

跨资源的维护窗口编排服务：一份维护申请可以同时占用多项资源，经过准备、
原子占用、执行到定案（完成/中止/超时）的完整生命周期，并持久化占用与历史。

## 核心概念

- **资源（Resource）**：可被占用的对象，运行状态为 `running` / `maintenance`。
  只有在维护**正式开始**后资源才进入 `maintenance`；创建/修改申请不会提前改变资源状态。
- **维护申请（Request）**：声明资源集合、开始时间、持续时间和准备步骤，
  生命周期为 `pending → ready → executing → completed/aborted/expired`；
  被紧急插入打断的窗口转为 `reschedule`（待重排），重排后回到
  `ready/pending`。
- **版本（Version）**：修改时间、资源集合或准备步骤会递增版本并重置准备进度；
  旧版本的准备回执不能推进新版本。
- **窗口（Window）**：某资源上的一段占用区间，同一资源的窗口不可重叠。
- **紧急插入（Emergency）**：临时插入的紧急变更。登记时保存资源集合、
  紧急原因、预计时长和受影响窗口快照；确认后打断原排期，被影响的普通
  窗口转为 `reschedule`（待重排）并保留原计划时间，不会悄悄消失。

## 时间规则

所有窗口（普通维护与紧急插入）都是**左闭右开**的半开区间 `[start, end)`：

- `end = start + duration`，`end` 时刻本身不属于窗口。
- 两个窗口只有在区间真正相交时才算冲突；一个窗口的 `end` 等于另一个的
  `start` 时**不冲突**，可以首尾相接排期。
- 开始维护/确认插入要求当前时刻落在 `[start, end)` 内：早于 `start`
  报 `ErrWindowNotReached`，到达或超过 `end` 视为错过窗口。
- 日历查询 `Calendar(from, to)` 同样按 `[from, to)` 与窗口区间是否相交
  来筛选。

## 主要流程

1. **登记资源** `RegisterResource` —— 初始状态 `running`。
2. **创建申请** `CreateRequest` —— 校验资源存在、无重复、时间范围合法
   （开始时间在未来、时长为正）。无准备步骤的申请直接为 `ready`。
3. **准备回执** `ReportPrep(requestID, version, step)` —— 回执允许重复、乱序到达，
   幂等去重；版本不匹配的回执被拒绝。当前版本全部步骤完成后转为 `ready`。
4. **修改申请** `ModifyRequest` —— 仅限 `pending/ready`；内容变化即递增版本。
5. **开始维护** `Start` —— 在单把锁下原子地取得**全部**资源的使用权：
   任一资源窗口冲突即整体回滚，已占部分立即释放，不会留下部分锁定。
   多个申请并发开始时，只有能一次取齐全部资源的申请进入 `executing`。
6. **定案** `Complete` / `Abort` / `Expire` —— 三者共用同一个定案入口，
   释放全部资源、记录原因，并保证**只生成一份最终记录和一条通知**。
   `Expire` 处理两类超时：错过窗口未开始的申请、执行超过窗口结束的维护。
7. **日历查询** `Calendar(from, to)` —— 返回区间内的实际占用（`active`）
   与已排期计划（`scheduled`）。

## 紧急插入流程

1. **登记** `CreateEmergency` —— 校验资源集合后保存紧急原因、预计时长，
   并为受影响窗口（涉及资源上与紧急窗口重叠的待开始申请）拍摄快照，
   同时记录每项资源的版本。申请处于 `draft`，不改变任何排期。
2. **幂等** —— 相同申请号（`idempotency_key`）+ 相同内容的重复登记返回
   原结果；资源集合或时长等内容变化返回 `ErrEmergencyConflict`。
3. **确认** `ConfirmEmergency` —— 先核对快照：任一资源版本或受影响窗口
   的版本/时间/状态发生变化（包括被完成），原申请即告失效（`stale`），
   已完成的窗口不会被回写。快照有效时在单把锁下**一次性取得全部资源**；
   任一资源被执行中的窗口占用则整体失败，原排期完全保留，不会只挪走
   其中一项。确认成功即紧急窗口开始，受影响窗口转为 `reschedule` 并记录
   重排原因与原计划时间。
4. **取消** `CancelEmergency` —— `draft` 直接取消；`executing` 取消会释放
   其自身占用的资源。已终态的申请返回 `ErrEmergencyFinal` 且不触碰任何
   资源：迟到的取消不会释放别人（如已接手的普通窗口）的资源。
   插入、取消与普通窗口启动并发时，只形成一个资源占用结果。
5. **重排** `Reschedule(requestID, newStart)` —— 把待重排窗口改期到新
   时间并恢复为 `ready/pending`；版本号与准备进度不变，原计划时间仍保留
   在 `original_start` / `original_duration` 中供对比。
6. **查询** `EmergencyReport(id)` —— 返回排期前后对比（`before`/`after`）、
   受影响窗口及其当前状态与重排原因（`affected`）、涉及资源上的实际
   占用（`occupancy`）。
7. **超时** —— 执行超过预计时长的紧急窗口由 `Expire` 定案为 `expired`
   并释放资源。

## 目录结构

```
maintenance/        核心包：类型、编排服务、JSON 持久化、HTTP 接口
cmd/mwserver/       HTTP 服务入口（含周期性超时处理）
```

## 运行方式

```bash
# 运行测试（含竞态检测）
go test ./... -race

# 启动 HTTP 服务（数据持久化到 maintenance.json）
go run ./cmd/mwserver -addr :8080 -data maintenance.json
```

### HTTP 接口示例

```bash
# 登记资源
curl -X POST localhost:8080/resources -d '{"id":"db-1","name":"primary"}'

# 创建申请（开始时间为 RFC3339，时长单位为分钟）
curl -X POST localhost:8080/requests -d '{
  "resource_ids":["db-1"],
  "start":"2026-09-28T23:00:00Z",
  "duration_minutes":30,
  "prep_steps":["backup","notify"]
}'

# 上报准备回执（version 需与当前版本一致）
curl -X POST localhost:8080/requests/<id>/prep -d '{"version":1,"step":"backup"}'

# 修改申请（递增版本）/ 开始 / 完成 / 中止
curl -X POST localhost:8080/requests/<id>/modify -d '{...同创建...}'
curl -X POST localhost:8080/requests/<id>/start
curl -X POST localhost:8080/requests/<id>/complete -d '{"reason":"patched"}'
curl -X POST localhost:8080/requests/<id>/abort   -d '{"reason":"operator abort"}'

# 手动触发超时处理 / 日历查询 / 定案历史
curl -X POST localhost:8080/expire
curl 'localhost:8080/calendar?from=2026-09-28T00:00:00Z&to=2026-09-29T00:00:00Z'
curl localhost:8080/history

# 紧急插入：登记（带申请号幂等）→ 确认 → 查询 → 完成/取消
curl -X POST localhost:8080/emergencies -d '{
  "idempotency_key":"INC-1234",
  "resource_ids":["db-1"],
  "start":"2026-09-28T23:30:00Z",
  "duration_minutes":15,
  "reason":"安全补丁紧急修复"
}'
curl -X POST localhost:8080/emergencies/<id>/confirm
curl localhost:8080/emergencies/<id>          # 排期前后对比/受影响窗口/占用/重排原因
curl -X POST localhost:8080/emergencies/<id>/complete
curl -X POST localhost:8080/emergencies/<id>/cancel

# 被打断的窗口重排到新时间
curl -X POST localhost:8080/requests/<id>/reschedule -d '{"start":"2026-09-29T01:00:00Z"}'
```

## 持久化

所有状态（资源、申请、执行中占用、定案历史、通知记录）在每次变更后
以「临时文件 + rename」的方式原子写入 JSON 数据文件，重启后自动恢复。
测试中可使用 `OpenStore("")` 获得纯内存存储。
