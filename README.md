# go-maintenance-window

跨资源的维护窗口编排服务：一份维护申请可以同时占用多项资源，经过准备、
原子占用、执行到定案（完成/中止/超时）的完整生命周期，并持久化占用与历史。

## 核心概念

- **资源（Resource）**：可被占用的对象，运行状态为 `running` / `maintenance`。
  只有在维护**正式开始**后资源才进入 `maintenance`；创建/修改申请不会提前改变资源状态。
- **维护申请（Request）**：声明资源集合、开始时间、持续时间和准备步骤，
  生命周期为 `pending → ready → executing → completed/aborted/expired`。
- **版本（Version）**：修改时间、资源集合或准备步骤会递增版本并重置准备进度；
  旧版本的准备回执不能推进新版本。
- **窗口（Window）**：某资源上的一段占用区间，同一资源的窗口不可重叠。

## 主要流程

1. **登记资源** `RegisterResource` —— 初始状态 `running`。
2. **创建申请** `CreateRequest` —— 校验资源存在、无重复、时间范围合法
   （开始时间在未来、时长为正）。无准备步骤的申请直接为 `ready`。
3. **准备回执** `ReportPrep(requestID, version, step)` —— 回执允许重复、乱序到达，
   幂等去重；版本不匹配的回执返回 `ErrStalePrepVersion`，不改变任何状态。
   未知步骤返回 `ErrUnknownPrepStep`；创建/修改时重名或空步骤直接被拒，
   避免出现永远无法集齐回执的申请。当前版本全部步骤完成后转为 `ready`。
4. **修改申请** `ModifyRequest` —— 仅限 `pending/ready`；时间、时长、资源集合或
   准备步骤任一变化即递增版本并清空回执，旧版本回执随之失效；**内容完全相同的
   修改不改变版本、回执与状态**（不会把已经 `ready` 的申请打回 `pending`）。
5. **开始维护** `Start` —— 在单把锁下原子地取得**全部**资源的使用权：
   先校验全部资源在窗口内均空闲，再一次性占用；任一冲突时什么都不取
   （`ErrResourceBusy`），不会留下部分锁定。多个申请并发开始时，
   只有能一次取齐全部资源的申请进入 `executing`。
6. **定案** `Complete` / `Abort` / `Expire` —— 三者共用同一个定案入口，
   释放全部资源、记录原因，并保证**只生成一份最终记录和一条通知**。
   `Expire` 处理两类超时：错过窗口未开始的申请、执行超过窗口结束的维护。
7. **日历查询** `Calendar(from, to)` —— 返回区间内的实际占用（`active`）
   与已排期计划（`scheduled`）。

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
```

## 持久化

所有状态（资源、申请、执行中占用、定案历史、通知记录）在每次变更后
以「临时文件 + rename」的方式原子写入 JSON 数据文件，重启后自动恢复。
测试中可使用 `OpenStore("")` 获得纯内存存储。

## 排期与冲突的仲裁规则

- 创建申请只校验**资源关系与时间范围**（资源存在且不重复、开始时间在未来、
  时长为正、准备步骤非空且不重名），**不**校验排期重叠，资源保持 `running`。
  多个申请可以对同一资源提交重叠的排期，日历中均以 `scheduled` 展示。
- “同一资源的维护窗口不可重叠”在**开始时**仲裁：`Start` 只检查已执行占用
  （`active`），失败的申请保持 `ready`、不锁任何资源，待占用方完成/中止/超时
  释放后可直接重试 `Start`。
- 错过窗口（到 `start+duration` 仍未开始）的申请由 `Expire` 定案为 `expired`，
  不会占用任何资源；执行超过窗口结束的维护同样会被 `Expire` 回收。

## 并发与单次定案

- 全部变更操作在同一把互斥锁内完成“校验—修改—持久化”，
  `go test ./... -race` 下的并发开始压测验证了“每资源至多一个 active 窗口”。
- `Complete`、`Abort`、`Expire` 共用唯一定案入口 `finalizeLocked`：
  已终态的申请再次定案返回 `ErrAlreadyFinal`，历史记录、持久化通知以及
  `Notifier` 外部回调对每个申请都**至多触发一次**。

## 测试

`maintenance` 包包含以下自动化测试（`go test ./... -race`）：

- 服务层：创建校验、资源状态不提前变更、回执重复/乱序、版本递增与旧回执失效、
  空修改不回退状态、开始前置条件、并发开始的原子占用、失败回滚、
  完成/中止/超时竞争只生成一份定案、错过窗口与执行超时、日历、文件持久化恢复、
  8 申请并发压测。
- HTTP 层（`httpapi_test.go`）：完整生命周期、版本递增、并发开始、
  超时与日历的端到端接口验证（含 400 错误路径）。
