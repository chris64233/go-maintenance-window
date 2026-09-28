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
