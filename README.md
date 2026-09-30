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

## 边界与并发语义

- **冲突只在开始时仲裁**：创建/修改申请不检查窗口重叠，排期阶段允许时间
  重叠（日历中都显示为 `scheduled`）；只有 `Start` 才做原子仲裁，因此
  申请失败后调整时间或资源集合即可重试，不会互相提前阻塞。
- **半开区间** `[start, end)`：同资源上首尾相接（前一段结束时刻恰为后一段
  开始时刻）的两段维护不算冲突，可以同时执行。
- **开始失败全回滚**：多资源申请逐资源获取，任一冲突即释放已获取的全部资源，
  申请保持 `ready`，不会留下部分锁定。
- **资源状态由剩余占用推导**：一个申请定案或回滚后，资源仅在不存在其它执行中
  窗口时才恢复 `running`；例如两段首尾相接的维护并存时，先完成的一段不会
  提前解除另一段的 `maintenance`。
- **重复 `Start` 幂等**：执行中的申请再次开始直接成功且不产生重复占用。
- **准备步骤不可重复声明**：创建/修改时出现重复的步骤名会被拒绝
  （`ErrDuplicatePrepStep`）；回执本身仍然允许重复、乱序到达。
- **无实际变化的修改不递增版本**：只有时间、时长、资源集合或准备步骤确实变化
  时才递增版本并重置回执；原样提交不影响当前 `ready` 状态。
- **终态唯一**：完成、中止、超时共用同一定案入口并由单锁串行化，
  每个申请至多一条 `FinalRecord`、一条 `Notification`，后续调用返回
  `ErrAlreadyFinal`。
- **HTTP 错误约定**：所有业务错误以 `400 Bad Request` 返回
  （`{"error": "..."}`），包括资源/申请不存在等情况。

## 目录结构

```
maintenance/        核心包：类型、编排服务、JSON 持久化、HTTP 接口
cmd/mwserver/       HTTP 服务入口（含周期性超时处理）
```

核心包测试覆盖：创建校验与资源状态保护、回执乱序/重复、版本失效、
并发开始的原子获取与回滚（含相邻窗口）、定案竞争单次记录、两类超时、
日历、文件持久化恢复，以及 `httptest` 驱动的完整 HTTP 生命周期。

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
