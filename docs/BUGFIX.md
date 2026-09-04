# dex-grid 代码审计与 Bug 修复文档

审计范围：`cmd/`、`internal/`（domain / app / exchange / api / infra）全量 Go 代码，约 2.4 万行。
审计方式：逐文件静态阅读 + `go vet` + 全量单测 + 针对性竞态/场景复现测试。

基线状态：`go vet ./...` 无输出，`go test ./...` 全部通过（21 个包）。
**下面所有问题都发生在现有测试未覆盖的路径上**，因此"测试全绿"不能作为这些问题不存在的依据。

## 优先级总览

| 编号 | 问题 | 位置 | 影响 | 级别 |
| --- | --- | --- | --- | --- |
| [P0-1](#p0-1) | HTTP 读路径与 Runner 事件循环无锁并发访问领域状态 | `engine/runner.go`、`supervisor/supervisor.go` | 数据错乱 / 进程崩溃 | 严重 |
| [P0-2](#p0-2) | 中性网格的止盈止损被瞬间误触发 | `risk/guard.go:154` | 开仓即市价平仓停机 | 严重 |
| [P0-3](#p0-3) | nonce 类 4xx 不重新校准，重试必然全败 | `lighter/tx.go:115`、`rhlighter/tx.go:115` | 全部下单失败直至熔断 | 严重 |
| [P0-4](#p0-4) | WS 订阅被拒只发一条 Err，不重连 | `lighter/stream.go:294`、`rhlighter`、`sodex/stream.go:219` | 成交回报永久静默 | 严重 |
| [P0-5](#p0-5) | 重建 Runner 不等旧 goroutine 退出 | `supervisor/supervisor.go:419` | 旧实例撤掉新网格 / 快照被覆盖 | 严重 |
| [P1-1](#p1-1) | 窗口外撤单立即清空 COID，撤单与成交竞争时丢成交 | `grid/strategy.go:700` | 格子状态错乱、少记已实现 | 中等 |
| [P1-2](#p1-2) | 建仓跨零换向时误加 reduce-only | `entry/entry.go:429` | 建仓永远无法完成 | 中等 |
| [P1-3](#p1-3) | 马丁止盈单部分成交后被撤，误判周期完成 | `martingale/runtime.go:176` | 仓位残留 + 反向建仓 | 中等 |
| [P1-4](#p1-4) | 马丁 `PhaseEntering` 恢复路径卡死 | `martingale/runtime.go:375` | 实例永久静止 | 中等 |
| [P1-5](#p1-5) | 成交解析失败被静默跳过，导致成交重复入库 | `store/store.go:304` | 盈亏统计翻倍 | 中等 |
| [P1-6](#p1-6) | 优雅退出未等落盘完成就关闭 SQLite | `cmd/gridbot/main.go:81` | 终态快照丢失 | 中等 |
| [P1-7](#p1-7) | `price_source: last` 配置项完全失效 | `risk/guard.go:190` | 风控按错误价源触发 | 中等 |
| [P1-8](#p1-8) | 行情静默触发的重连没有退避 | `engine/runner.go:410` | 每秒重连风暴 | 中等 |
| [P1-9](#p1-9) | `StatusReconnecting` 无法自行恢复 | `engine/runner.go:114`、`:785` | 实例卡在"重连中"停止交易 | 中等 |
| [P1-10](#p1-10) | 默认 `0.0.0.0:8080` + 鉴权默认关闭 | `config/config.go:185` | 未授权远程控制资金 | 中等 |
| [P1-11](#p1-11) | 开启鉴权后跨域预检被 401 | `api/server.go:105` | 跨域控制台不可用 | 中等 |
| [P1-12](#p1-12) | 请求体无大小上限 | `api/server.go:166`、`:189`、`:250` | 内存 DoS | 中等 |
| [P1-13](#p1-13) | 反向代理后 IP 白名单失效 | `api/server.go:350` | 白名单形同虚设 | 中等 |
| [P2-1](#p2-1) | `lastFillQty` map 无界增长 | `engine/runner.go:832` | 长跑内存泄漏 | 轻微 |
| [P2-2](#p2-2) | 批量下单结果与请求按下标配对，长度不等即错位 | `executor/executor.go:247` | 静默错配 / 越界 panic | 轻微 |
| [P2-3](#p2-3) | 撤单结果被丢弃，撤单失败无人处理 | `executor/executor.go:432` | 幽灵挂单 | 轻微 |
| [P2-4](#p2-4) | Pending 超时重挂复用同一 COID，重复拒绝会喂熔断 | `grid/strategy.go:395` | 误熔断停机 | 轻微 |
| [P2-5](#p2-5) | `ActiveWindow` 的 pivot 与 `pivotCell` 判定不一致 | `grid/grid.go:322` | 挂单窗口整体偏低一格 | 轻微 |
| [P2-6](#p2-6) | 建仓改价序号 4 bit，16 次后 COID 回绕 | `entry/entry.go:411` | 重挂被判重号 | 轻微 |
| [P2-7](#p2-7) | HTTP 客户端超时复用 `shutdown_timeout` | `cmd/gridbot/main.go:83` | 语义错配 | 轻微 |
| [P2-8](#p2-8) | 代理凭证经 `/api/proxy` 明文回显 | `supervisor/supervisor.go:293` | 凭证泄漏 | 轻微 |
| [P2-9](#p2-9) | `ListFills` 的 `symbol=''` 兜底混入跨交易对成交 | `store/store.go:331` | 成交表脏数据 | 轻微 |
| [P2-10](#p2-10) | 平仓腿数量向下取整留下 dust | `market/market.go:100` | 微量仓位残留累积 | 轻微 |

---

## P0 严重问题

<a id="p0-1"></a>
### P0-1 HTTP 读路径与 Runner 事件循环无锁并发访问领域状态

**位置**

- `internal/app/engine/runner.go:177-195`（`Runner.View`）
- `internal/app/supervisor/supervisor.go:244-252`（`Supervisor.View`）、`:256-290`（`Supervisor.Status`）
- `internal/api/server.go:202-218`（`handleStatus` / `handleLevels`）

**现象与根因**

README 第 7 节声称"HTTP handler 不直接改状态，投递命令并等回执"，写路径确实如此，但**读路径完全绕过了命令 channel**：

```177:186:internal/app/engine/runner.go
func (r *Runner) View() InstanceView {
	v := InstanceView{
		Exchange: r.cfg.Name,
		Status:   r.status.String(),
		Residual: r.residual,
		Entering: r.entering,
		Symbol:   r.symbol,
		Mark:     r.state.Mark,
		Position: r.state.Position,
		Account:  r.state.Account,
```

`View()` 在 HTTP goroutine 里裸读 `r.status`、`r.state`、`r.stopReason`，并进一步调用 `r.strat.View()`，后者遍历 `s.grid.Cells`、读 `s.retrying` map 和 `s.stats`。同一时刻 Runner goroutine 正在 `onStream` / `dispatch` / `syncStatus` 里写这些字段。控制台每秒轮询 `GET /api/exchanges/{ex}/status`，因此这是**持续发生**的竞争，不是理论风险。

**复现**（临时测试，已验证后删除）

```
go test -race -run TestViewRaceFromHTTPGoroutine ./internal/app/engine/
```

```
WARNING: DATA RACE
Read at 0x00c0001f4298 by goroutine 12:
  dex-grid/internal/app/engine.(*Runner).View()   runner.go:184
Previous write at 0x00c0001f4298 by goroutine 11:
  dex-grid/internal/app/engine.(*Runner).onStream()  runner.go:374
...
--- FAIL: TestViewRaceFromHTTPGoroutine (0.61s)
    race detected during execution of test
```

竞态检测器在 `runner.go:180`（`r.status`）、`:184`（`r.state.Mark`）、`:188`（`r.stopReason`）三处都报了，达到默认上报上限后才停止，尚未深入到 `strat.View()`。

**为什么不能只当成"读到旧值"**

`decimal.Decimal` 内部是 `{value *big.Int, exp int32}`。撕裂读会拿到一个指针与另一个值的指数配对，`json.Marshal` 时可能产出错误数字甚至 panic。`strategy.View` 里 `len(s.retrying)`、`s.grid.Cells` 的并发读写属于 Go 内存模型的未定义行为；若后续有人把 `View` 改成 range map（例如加"每格重试次数"字段），会直接触发 `fatal error: concurrent map read and map write`，整个进程带着实盘仓位崩掉。

**修复方案（推荐：走既有命令通道，与设计文档保持一致）**

1. `engine` 增加一个只读命令：

```go
// command.go
const CmdView CommandKind = /* 追加到末尾 */

// runner.go handleCommand
case CmdView:
    return CommandResult{OK: true, View: r.View()}
```

2. `Runner` 暴露一个安全入口，HTTP 侧只能用它：

```go
// ViewSafe 从事件循环内部取视图快照。Run 未运行时退化为直接读（此时无并发）。
func (r *Runner) ViewSafe(ctx context.Context) InstanceView {
    if !r.running.Load() {
        return r.View()
    }
    res := r.Call(ctx, CmdView, nil)
    return res.View
}
```

`running` 用 `atomic.Bool`，在 `Run` 入口 `Store(true)`、`defer Store(false)`。

3. `Supervisor.View` / `Status` 改调 `ViewSafe(ctx)`；把 `View(name string)` 的签名改成 `View(ctx context.Context, name string)`，`api/server.go` 传 `r.Context()`。

4. 顺带修掉 `inst.runner` 字段本身的竞争：`get()` 返回 `*instance` 后 RLock 已释放，`Start`/`command`/`View` 都在锁外读 `inst.runner`（`supervisor.go:353`、`:409`、`:249`）。给 `instance` 加一个 `sync.RWMutex`，或提供 `inst.currentRunner()` 访问器：

```go
func (i *instance) currentRunner() *engine.Runner {
    i.mu.RLock()
    defer i.mu.RUnlock()
    return i.runner
}
```

**备选方案（改动小但不彻底）**：给 Runner 加 `viewMu sync.RWMutex`，在 `dispatch`/`onStream`/`syncStatus` 写入前加写锁、`View()` 加读锁。缺点是必须保证**所有**写点都被覆盖，且 `strat.View()` 仍在读策略内部状态，容易漏。若采用此方案，应同时让策略提供一份深拷贝快照。

**验证**：把上面的复现测试固化进 `runner_test.go`（去掉 `t.Fatalf`，改为断言不 panic），并在 CI 里加 `go test -race ./...`。当前 CI 只跑 `go test`，这正是问题至今未暴露的原因。

---

<a id="p0-2"></a>
### P0-2 中性网格的止盈止损被瞬间误触发

**位置** `internal/app/risk/guard.go:154-181`

**现象与根因**

`checkTPSL` 只用**当前仓位的正负号**判断该往哪个方向比价：

```154:178:internal/app/risk/guard.go
func (g *Guard) checkTPSL(price decimal.Decimal) Verdict {
	long := g.pos.Size.IsPositive()
	short := g.pos.Size.IsNegative()

	if g.params.HasStopLoss() {
		sl := g.params.StopLossPrice
		if (long && price.LessThanOrEqual(sl)) || (short && price.GreaterThanOrEqual(sl)) {
			g.reason = strategy.StopStopLoss
			return Verdict{
				Stop:    true,
				Reason:  strategy.StopStopLoss,
				Actions: closeActions(strategy.StopStopLoss),
			}
		}
	}
	if g.params.HasTakeProfit() {
		tp := g.params.TakeProfitPrice
		if (long && price.GreaterThanOrEqual(tp)) || (short && price.LessThanOrEqual(tp)) {
```

这个判定隐含假设"止损价一定在持仓的亏损侧"。做多网格（止损在下沿之下、仓位恒为正）与做空网格（止损在上沿之上、仓位恒为负）都成立，**但中性网格不成立**：中性网格的净仓位围绕 0 来回翻转，而 `grid/derive.go:192-209` 的 `validateTPSL` 明确要求中性网格的止盈止损"分处区间两侧"。两条约束合在一起必然产生矛盾——只要仓位翻到某一侧，位于另一侧的触发价就会被立刻判定为"已穿越"。

**触发场景**

区间 `[100, 110]`，止损 95，止盈 115（这是 `validateTPSL` 唯一允许的中性配置形态）。价格 105 时上方卖开腿成交，净仓位变成 -1：

- `short && price(105) >= sl(95)` → **成立** → 立刻 `CancelAll` + 市价 IOC 平仓 + 停止实例

镜像配置（止盈 95、止损 115）下，只要仓位为正，`long && price(105) <= sl(115)` 同样立刻成立。也就是说**中性网格配了止损就一定在第一笔成交后自杀**。README 第 9 节还在建议"必须配置止损价"。

同一个 bug 也会波及做多/做空网格：若仓位因超卖短暂反向（例如平仓腿多成交了一点），反向那一支比较立刻成立，同样误触发。

**复现**（临时测试，已验证后删除）

```
=== RUN   TestNeutralGridShortLegTriggersStopLossImmediately
    价格 105 在区间 [100,110] 内，却触发了 stop_loss；
    actions=[]strategy.Action{strategy.CancelAll{}, strategy.ClosePosition{Urgency:0x0}, strategy.Stop{Reason:0x2}}
--- FAIL
=== RUN   TestNeutralGridLongLegTriggersTakeProfitImmediately
    价格 105 在区间内，却触发了 stop_loss
--- FAIL
```

**修复方案**

止盈止损是**价格区间的边界事件**，不是持仓方向事件。判定必须基于"触发价相对区间/参考价在哪一侧"，而不是仓位符号。给 `RiskParams` 增加显式的触发边，由策略在启动时告知 Guard：

```go
// domain/strategy/params.go
type RiskParams struct {
    // ...
    // TPSide / SLSide 指明触发价位于参考价的哪一侧，由策略按区间推导后下发。
    // 做多：SL 在下、TP 在上；做空反之；中性按各自实际落点填。
}

// risk/guard.go
type Guard struct {
    // ...
    slBelow bool // 止损价在区间下方 → 价格向下穿越才触发
    tpAbove bool // 止盈价在区间上方 → 价格向上穿越才触发
}

func (g *Guard) SetTriggerSides(slBelow, tpAbove bool) {
    g.slBelow, g.tpAbove = slBelow, tpAbove
}

func (g *Guard) checkTPSL(price decimal.Decimal) Verdict {
    if g.params.HasStopLoss() {
        sl := g.params.StopLossPrice
        hit := (g.slBelow && price.LessThanOrEqual(sl)) ||
               (!g.slBelow && price.GreaterThanOrEqual(sl))
        if hit {
            g.reason = strategy.StopStopLoss
            return Verdict{Stop: true, Reason: strategy.StopStopLoss,
                Actions: closeActions(strategy.StopStopLoss)}
        }
    }
    if g.params.HasTakeProfit() {
        tp := g.params.TakeProfitPrice
        hit := (g.tpAbove && price.GreaterThanOrEqual(tp)) ||
               (!g.tpAbove && price.LessThanOrEqual(tp))
        if hit { /* 同上，StopTakeProfit */ }
    }
    return Verdict{}
}
```

`Runner.handleStart`（`runner.go:281-284` 附近，已有 `SetPosition`/`SetAccount`/`SetMark` 三连）追加一行：

```go
sl, tp := r.riskP.StopLossPrice, r.riskP.TakeProfitPrice
v := r.strat.View()
r.guard.SetTriggerSides(sl.LessThan(v.LowerPrice), tp.GreaterThan(v.UpperPrice))
```

`adjustRange` / trailing 改变区间后需要重新下发，可在 `syncEpoch()` 里一并刷新。

**最小止血版**（若不想改接口）：`checkTPSL` 保留仓位方向判定，但额外要求价格确实在区间之外——即触发价与当前价必须落在区间同一侧。这能挡住中性网格的误杀，但语义不如上面清晰。

**回归测试**：三个方向 × （TP 在上/在下）× 仓位正/负/零，共 12 个用例，断言只有"价格真正穿越触发价"时才 `Stop`。现有 `guard_test.go` 只覆盖了做多做空各一条，正是漏检原因。

---

<a id="p0-3"></a>
### P0-3 nonce 类 4xx 不重新校准，重试必然全部失败

**位置** `internal/exchange/lighter/tx.go:115-121`，`internal/exchange/rhlighter/tx.go:115-121`（逻辑相同）

**现象与根因**

```110:121:internal/exchange/lighter/tx.go
// onSendFailure 决定失败后 nonce 怎么处理。
//
// 服务端明确拒绝（4xx）说明交易没有被接受，nonce 可以原样复用。
// 网络层错误无法确认交易是否已被接受，此时绝不能猜——标记为需要重新校准，
// 下一笔交易会重新问服务端要 nonce。猜错的代价是后续所有交易连续失败。
func (s *txSender) onSendFailure(err error) {
	var ae *apiError
	if errorsAs(err, &ae) && ae.Status >= 400 && ae.Status < 500 {
		return
	}
	s.hasNonce = false
}
```

注释的推理对"参数非法"类 4xx 成立，但对"nonce 不对"类 4xx 恰好相反：本地 nonce 已经和服务端脱节，复用只会一直被拒。而 `classify.go` 把带 `nonce` 字样的错误归为**可重试**：

```75:78:internal/exchange/lighter/classify.go
	{exchange.ClassRetryable, []string{
		"nonce", "rate limit", "too many requests", "timeout", "timed out",
		"temporarily", "try again", "sequencer is", "busy",
	}},
```

`messagePatterns` 的匹配早于 `4xx → ClassInvalidParam` 兜底（`classify.go:49-56`），所以 `400 invalid nonce` 一定是 `ClassRetryable`。于是形成闭环：

`executor.place` 按可重试退避重试 → `currentNonce()` 因 `hasNonce == true` 返回同一个陈旧 nonce → 再次 400 → 耗尽 `MaxRetries`（默认 3）→ `recordClass` 计入 `res.Failures` → `guard.RecordFailures` 累计到 `MaxConsecutiveErrors`（默认 10）→ **熔断停机**。整个过程中一次都没有重新取号。

**触发场景**

- 同一 `api_key_index` 被另一个进程/官方前端用过（`config.example.yaml:55` 专门警告过这点）
- 进程重启后本地缓存与链上 nonce 不一致
- 某笔交易实际入块但响应丢失，之后所有 nonce 都落后 1

**修复方案**

让 nonce 类错误明确地触发重新校准。在 `onSendFailure` 里区分 4xx 的两种语义：

```go
func (s *txSender) onSendFailure(err error) {
	var ae *apiError
	if errorsAs(err, &ae) && ae.Status >= 400 && ae.Status < 500 {
		// nonce 类 4xx 说明本地号已与服务端脱节，复用只会一直被拒。
		if strings.Contains(strings.ToLower(ae.Message), "nonce") {
			s.hasNonce = false
		}
		return
	}
	s.hasNonce = false
}
```

同时建议给 `classify.go` 拆出一个独立分类，避免"可重试"这个语义同时承载"直接重试"和"重置后重试"两种含义：

```go
// exchange/errors.go
// ClassNonceStale 本地 nonce 与服务端脱节：必须重新取号后再重试。
ClassNonceStale
```

`ErrorClass.Retryable()` 让 `ClassNonceStale` 也返回 `true`，`classOf` 把 `nonce` 关键字单独归到它，`txSender.send` 在返回前对该分类调用一次 `ResetNonce()`。这样 `rhlighter` / 未来新链复用同一套逻辑时不会再漏。

**同步修改** `rhlighter/tx.go` 与 `rhlighter/classify.go`（两份文件是 `lighter` 的复制体，必须同改，否则 RH 实例仍然有问题）。

**回归测试**：伪造一个返回 `400 {"message":"invalid nonce"}` 的 `rest.sendTx`，断言第二次 `send` 会重新调用 `nextNonce`。

---

<a id="p0-4"></a>
### P0-4 WebSocket 订阅被拒只发一条 Err，不触发重连

**位置**

- `internal/exchange/lighter/stream.go:294-300`
- `internal/exchange/rhlighter/stream.go:294-300`
- `internal/exchange/sodex/stream.go:219-223`

**现象与根因**

```293:300:internal/exchange/lighter/stream.go
	case env.Type == "error":
		// 订阅被拒（比如 auth 过期）不该被当成普通日志吞掉，
		// 上抛让重连逻辑处理——重连会重新生成 auth token。
		s.emit(ctx, exchange.StreamEvent{
			Err: fmt.Errorf("lighter 事件流返回错误: %s (code=%d)", env.Message, env.Code),
		})
		return nil
```

注释说的是"上抛让重连逻辑处理"，代码做的是"发一条事件然后 `return nil`"——`handle` 返回 nil，`session()` 的读循环继续，连接保持存活。而 `run()` 只在 `session()` **返回 error** 时才重连（`stream.go:103-111`）。

后果：三个频道里只有 `account_market` 需要 auth token。它被拒后，`ticker` 与 `market_stats` 仍在正常推送，于是：

- `readTimeout`（90s）永远不会触发，因为行情帧一直在来
- **订单、成交、仓位回报永久丢失**
- Runner 侧 `onStream` 只是 `r.log.Warn("stream error", ...)` 就 `return`（`runner.go:361-364`），状态仍是 `running`，页面看起来一切正常
- 网格只能靠 15s 一次的看门狗 `Resync` 苟活，成交翻转格子完全失效

**触发场景** auth token 过期（有效期 8 小时，缓存到过期前 30 分钟）、`account_index` 配错、服务端临时拒绝订阅。长时间运行必然遇到。

**修复方案**

改成真正的上抛：

```go
	case env.Type == "error":
		// 返回 error 让 session 退出，由 run() 的重连循环重新取 auth token 并重新订阅。
		return fmt.Errorf("lighter 事件流返回错误: %s (code=%d)", env.Message, env.Code)
```

`run()` 已经会把这个 error 通过 `emit` 上报，所以不会丢日志，且退避重连时 `subscribe()` 会重新调用 `s.authToken()`（`stream.go:171-174`），token 过期能自愈。

SODEx 同理，把 `success:false` 的分支改成 `return fmt.Errorf(...)`。

**额外加固（建议一并做）**：`env.Type == "error"` 目前不区分"订阅被拒"与"某条业务消息报错"。如果服务端也用同一 type 报非致命错误，无条件重连会造成连接抖动。可按 `env.Channel` 判断——只有 auth 频道相关的错误才断开重连，其余仅告警。

**同时修 Runner 侧的可观测性**：`onStream` 收到 `se.Err` 时应把状态标记为异常（见 [P1-9](#p1-9) 一并处理），而不是只打一条 warn 就当没事发生。

---

<a id="p0-5"></a>
### P0-5 重建 Runner 时不等旧 goroutine 退出

**位置** `internal/app/supervisor/supervisor.go:419-459`

**现象与根因**

```428:458:internal/app/supervisor/supervisor.go
	if inst.cancel != nil {
		inst.cancel()
		inst.cancel = nil
	}
	...
	inst.runner = engine.New(inst.ex, strat, engine.Config{...})
	ctx, cancel := context.WithCancel(context.Background())
	inst.cancel = cancel
	go func() { _ = inst.runner.Run(ctx) }()
```

`inst.cancel()` 之后立刻创建并启动新 Runner，**完全不等旧 goroutine 退出**。旧 Runner 收到 `ctx.Done()` 会执行：

```98:101:internal/app/engine/runner.go
		case <-ctx.Done():
			if r.status != StatusStopped && r.status != StatusError {
				r.finishStop(context.WithoutCancel(ctx), strategy.StopShutdown)
			}
```

注意 `context.WithoutCancel` —— 旧 goroutine 的收尾**不受取消影响**，会完整跑完 `finishStop`：`refreshPosition`（REST）→ `strat.OnStop()` → `exec.Apply([CancelAll])` → 再 `refreshPosition` → `persist()`。而新旧 Runner **共用同一个 `inst.ex` 交易所连接和同一个 symbol**。

两个具体后果：

1. **旧实例撤掉新网格**：`ex.CancelAll(ctx, symbol)` 撤的是该交易对上的全部挂单。如果新 Runner 已经铺完网格（`CmdStart` 里的 REST 预热 + 批量下单，通常几百毫秒到数秒），这些新单会被旧 goroutine 一把撤光，且没有任何错误——新 Runner 认为自己挂单成功，格子状态是 `CellResting`，交易所上却是空的。只能等 15s 后看门狗 `Resync` 才发现。
2. **快照被旧状态覆盖**：`finishStop` 最后调 `r.persist()`，把 `status="stopped"` + **旧策略的快照**写进 `runtime_state`。若此时进程崩溃，`RestoreRunning` 读到的是旧轮次的格子配对。

**触发场景** 策略处于 `Paused`（区间外挂起）或 `Reconnecting`，用户在页面上再点一次「启动」；或 `RestoreRunning` 之后紧接着 `Autostart`（`main.go:120-121` 就是连着调的）。

**修复方案**

给 `instance` 增加退出信号，`ensureRunner` 必须等旧循环彻底结束：

```go
type instance struct {
	name    string
	slot    uint8
	ex      exchange.StreamingExchange
	exCfg   config.Exchange
	runner  *engine.Runner
	cancel  context.CancelFunc
	runDone chan struct{}
}

func (s *Supervisor) ensureRunner(inst *instance, name string, params []byte, restore bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst.runner != nil {
		st := inst.runner.Status()
		if st == engine.StatusRunning || st == engine.StatusStarting {
			return nil
		}
	}
	if inst.cancel != nil {
		inst.cancel()
		inst.cancel = nil
	}
	// 必须等旧事件循环收尾结束：它会 CancelAll 并覆盖 runtime 快照。
	if inst.runDone != nil {
		select {
		case <-inst.runDone:
		case <-time.After(30 * time.Second):
			s.log.Error("旧事件循环退出超时，放弃重建", "exchange", inst.name)
			return fmt.Errorf("实例 %s 上一轮尚未退出", inst.name)
		}
		inst.runDone = nil
	}
	// ... strategy.New / Restore ...
	inst.runner = engine.New(...)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	inst.cancel, inst.runDone = cancel, done
	go func() { defer close(done); _ = inst.runner.Run(ctx) }()
	return nil
}
```

`Supervisor.Close`（`supervisor.go:68-80`）同样要在 `inst.cancel()` 之后 `<-inst.runDone`，这正好一并修掉 [P1-6](#p1-6)。

另外 `Close` 目前持着 `s.mu.Lock()` 做网络 I/O（`runner.Call` + `ex.Close`），会把所有 API 请求堵到超时。建议先在锁内拷出实例列表，再在锁外收尾。

---

## P1 中等问题

<a id="p1-1"></a>
### P1-1 窗口外撤单立即清空 COID，撤单与成交竞争时丢掉成交

**位置** `internal/domain/strategy/grid/strategy.go:700-708`

**现象与根因**

`realignGrid` 里有一段专门防这个坑的代码，注释写得很清楚：

```359:364:internal/domain/strategy/grid/strategy.go
		// 存活挂单已被现价穿过时等成交回报，不要先撤再反向挂。
		// 否则 ticker 早于 fill 到达会清掉 COID，成交被当成陈旧回报丢掉，
		// 开腿均价也记不上。
		if c.State == CellResting && crossedByMark(c.Side, c.OrderPrice(), s.mark) {
			continue
		}
```

但 `placeActions` 里撤"挂单窗口之外"的格子时**没有同样的保护**：

```700:708:internal/domain/strategy/grid/strategy.go
		if !window[i] {
			if c.State != CellEmpty && c.COID != 0 {
				acts = append(acts, strategy.CancelOrder{ClientOrderID: c.COID})
				c.State = CellEmpty
				c.COID = 0
				c.PendingSince = time.Time{}
			}
			continue
		}
```

`c.COID = 0` 是**乐观的**——撤单请求还没发出去就已经把本地记录抹掉了。之后成交回报到达时：

```571:573:internal/domain/strategy/grid/strategy.go
	if c.COID != o.ClientOrderID {
		return nil, nil // 陈旧回报，当前格子上挂的是另一笔
	}
```

成交被当成陈旧回报直接丢弃。结果是：格子不翻转、`OpenQty`/`OpenPrice` 不记账、`CompletedGrids` 与 `GridProfit` 少算，而仓位已经真实变化。策略认知的仓位与实际仓位从此偏离，`recoverFromPosition` 只在 `Init`/`Resync` 跑，中间这段时间格子配对是错的。

**触发场景** 配置了 `max_active_orders`（挂单额度紧张的交易所必配）+ 价格快速移动导致窗口平移 + 边缘格子恰好正在成交。撤单和成交本来就是天然竞态，撤单失败（订单已成交）是常态而非异常。

**修复方案**

复用同一个保护条件，并且不要提前清 COID：

```go
		if !window[i] {
			if c.State == CellEmpty || c.COID == 0 {
				continue
			}
			// 已被现价穿过的挂单可能正在成交，撤单会和成交赛跑。
			// 保留 COID，等成交/撤单回报把格子推到下一状态。
			if c.State == CellResting && crossedByMark(c.Side, c.OrderPrice(), s.mark) {
				continue
			}
			acts = append(acts, strategy.CancelOrder{ClientOrderID: c.COID})
			// 只降状态，保留 COID：撤单成功会收到 Canceled 回报，
			// 撤单失败（已成交）会收到 Filled 回报，两者都能被 onOrder 正确认领。
			c.State = CellEmpty
			c.PendingSince = time.Time{}
			continue
		}
```

保留 COID 是安全的：`onOrder` 对 `StateCanceled` 且 `FilledQty` 为零的处理就是把格子置空（`strategy.go:588-594`），而 `placeActions` 只对 `CellEmpty` 的格子挂单，此时窗口外的格子不会被重新挂出。

**更彻底的做法**：给 `Cell` 增加 `CellCanceling` 状态，明确区分"空闲可挂"与"已发撤单等回报"，并给它配一个超时兜底（同 `expirePending`）。这样也能顺带修掉 [P2-3](#p2-3) 里撤单失败无人处理的问题。

---

<a id="p1-2"></a>
### P1-2 建仓跨零换向时误加 reduce-only

**位置** `internal/app/entry/entry.go:429`

**现象与根因**

```429:438:internal/app/entry/entry.go
	reduceOnly := t.currentSize().Mul(rem).IsNegative()
	return []strategy.Action{strategy.PlaceOrder{
		ClientOrderID: coid,
		Side:          side,
		Type:          typ,
		Price:         price,
		Quantity:      qty,
		TIF:           tif,
		ReduceOnly:    reduceOnly,
	}}
```

判定逻辑是"当前仓位与剩余需求异号 ⇒ 这是平仓"。这对不跨零的情况都对，但当**目标与当前仓位方向相反**时会误判：

| 当前仓位 | 目标 | rem | 乘积 | reduceOnly | 正确吗 |
| --- | --- | --- | --- | --- | --- |
| +5 | 0 | −5 | −25 | true | ✅ 纯平仓 |
| +5 | +10 | +5 | +25 | false | ✅ 纯加仓 |
| **−5** | **+10** | **+15** | **−75** | **true** | ❌ 需要买 15，其中只有 5 是平仓 |

第三行是问题所在：只有 5 是减仓、10 是开新的多头，却给整单 15 打上 reduce-only。交易所会把数量截断到 5 或直接拒单，`t.done()` 永远不满足，触发器一直重挂，直到 `Timeout`（默认 5m）→ `onTimeout` 转市价 IOC，而市价单走同一段代码，**仍然带着 reduce-only**，于是同样失败。最终建仓永远无法完成。

**触发场景** 用户手上已有反向持仓时启动网格（做多网格但账户里是空头），或上一轮策略残留了反向仓位。普通网格没有马丁那样的 `wrongDirectionPosition` 前置校验，所以会一路走到这里。

**修复方案**

只有"不跨零的纯减仓"才允许 reduce-only；跨零时拆成两笔（先平后开），或直接放弃 reduce-only 标记：

```go
	// reduceOnly 只在整笔都用于减仓时才成立。跨零换向的单子有一部分是开新仓，
	// 打上 reduce-only 会被交易所截断或拒绝，导致建仓永远完不成。
	cur := t.currentSize()
	reduceOnly := cur.Mul(rem).IsNegative() && rem.Abs().LessThanOrEqual(cur.Abs())
```

推荐再进一步：在 `Start()` 里检测跨零并拆成两阶段，第一阶段 reduce-only 平到 0，第二阶段正常开仓。这样既保留了 reduce-only 的安全性，也不会把反向仓位一次性翻过去。

同时建议给普通网格补上马丁已有的方向校验：`grid.Strategy.Init` 里若 `s.position` 与 `s.target` 反向且量不可忽略，返回明确的 `Issue` 让用户先手动处理，而不是默默尝试翻仓。

---

<a id="p1-3"></a>
### P1-3 马丁止盈单部分成交后被撤，误判周期完成

**位置** `internal/domain/strategy/martingale/runtime.go:176-178`（分发）、`:200-236`（`handleTPFill`）

**现象与根因** 止盈单以 `StateCanceled`/`StateExpired` 且 `FilledQty > 0` 回报时，直接走 `handleTPFill`，而后者无条件把仓位当成已清空：`s.position = decimal.Zero`、`CompletedGrids++`、`cycles++`，随后 `CancelAll` + 递增 epoch + `EnsurePosition` 开启下一轮。

**触发场景** reduce-only 止盈单部分成交后被交易所撤销/过期（自成交保护、改单失败、风控介入）。本地认为已全平并开始新一轮，实际仓位还在，而止盈单已被撤 —— 新一轮的 `EnsurePosition` 可能在残留仓位之上反向建仓。

**修复方案** 区分"部分成交后被撤"与"成交完毕"：

```go
case order.StateCanceled, order.StateExpired:
    if o.FilledQty.IsPositive() && s.remainingAfterTP(o).Abs().GreaterThan(s.mkt.LotSize) {
        // 只成交了一部分：记账、清掉本地止盈单，按剩余仓位重挂，不结束周期。
        s.noteFill(o)
        s.tp.State, s.tp.COID = liveEmpty, 0
        return s.rehangTakeProfit(now), nil
    }
    if o.FilledQty.IsPositive() {
        return s.handleTPFill(o, now), nil
    }
    s.tp.State, s.tp.COID = liveEmpty, 0
    return nil, nil
```

并在 `handleTPFill` 入口加断言式防御：只有当累计成交已覆盖 `|position|`（容差一个 `LotSize`）时才允许结束周期，否则退化为重挂。

---

<a id="p1-4"></a>
### P1-4 马丁 `PhaseEntering` 恢复路径卡死

**位置** `internal/domain/strategy/martingale/runtime.go:375-387`（`resumeActions`）

**现象与根因** 三条分支没有覆盖全部组合：

```go
if s.needsEntry() && !s.hasWorkingInventory() { return EnsurePosition }
if !s.needsEntry() { s.phase = PhaseRunning; return placeActions }
return nil   // needsEntry() && hasWorkingInventory() → 什么都不做
```

当 `needsEntry()` 与 `hasWorkingInventory()**同时**为真（例如快照 `target=10`、实际仓位 15，偏差超过 1% 容差）时，既不发 `EnsurePosition` 也不转 `Running`，`phase` 永远停在 `PhaseEntering`，实例永久静止。对比全新 `Init`（`strategy.go:160-164`）在 `hasWorkingInventory()` 为真时会直接进 `Running`，所以只有恢复路径中招。

**修复方案**

```go
case strategy.PhaseEntering:
    if !s.needsEntry() {
        s.phase = strategy.PhaseRunning
        return s.placeActions(now)
    }
    if s.hasWorkingInventory() {
        // 已有同向仓但偏离目标：交给 EnsurePosition 收敛，不要静止。
        return []strategy.Action{strategy.EnsurePosition{Target: s.target}}
    }
    return []strategy.Action{strategy.EnsurePosition{Target: s.target}}
```

两条分支动作相同，可合并；关键是**不存在"什么都不做"的出口**。建议同时给 `PhaseEntering` 加一个墙钟超时兜底：进入该阶段超过 `Entry.Timeout` 的两倍仍未推进，就转 `PhaseStopped` 并写明原因，避免任何未来的分支遗漏又变成静默卡死。

---

<a id="p1-5"></a>
### P1-5 成交解析失败被静默跳过，导致成交重复入库

**位置** `internal/infra/store/store.go:304-312`

**现象与根因**

```304:312:internal/infra/store/store.go
		q, err := decimal.NewFromString(qStr)
		if err != nil {
			continue
		}
		p, _ := decimal.NewFromString(pStr)
		f, _ := decimal.NewFromString(fStr)
		qty = qty.Add(q)
		notional = notional.Add(q.Mul(p))
		fee = fee.Add(f)
```

`fillProgress` 的职责是"这张单已经入库多少量"，`RecordOrderFill` 用它算增量：`deltaQty = o.FilledQty - prevQty`。一旦某行数量字符串损坏被 `continue` 跳过，`prevQty` 被低估，`deltaQty` 相应偏大，**同一笔成交被再次 INSERT**。页面成交记录与已实现盈亏随之翻倍。第 309 行的 `p, _ :=` / `f, _ :=` 同样把错误丢弃，损坏的价格会静默变成 0，把均价拉低。

**修复方案** 这是记账路径，宁可报错也不能算错：

```go
		q, err := decimal.NewFromString(qStr)
		if err != nil {
			return decimal.Zero, decimal.Zero, decimal.Zero,
				fmt.Errorf("store: fills 数据损坏 exchange=%s coid=%d qty=%q: %w",
					exchange, coid, qStr, err)
		}
		p, err := decimal.NewFromString(pStr)
		if err != nil {
			return decimal.Zero, decimal.Zero, decimal.Zero,
				fmt.Errorf("store: fills 数据损坏 exchange=%s coid=%d price=%q: %w",
					exchange, coid, pStr, err)
		}
		f, err := decimal.NewFromString(fStr)
		if err != nil {
			return decimal.Zero, decimal.Zero, decimal.Zero,
				fmt.Errorf("store: fills 数据损坏 exchange=%s coid=%d fee=%q: %w",
					exchange, coid, fStr, err)
		}
```

`RecordOrderFill` 的调用方 `persistAdapter.RecordFill` → `runner.recordFill` 已经会打 warn 日志（`persist.go:61-63`），报错不会中断交易，只会明确暴露数据问题。

**建库层面加固**：`fills` 表的 `price`/`qty`/`fee` 存的是文本。建议加 `CHECK (CAST(qty AS REAL) > 0)` 之类的约束，或改为在写入前统一 `StringFixed` 规范化，从源头避免空串。

---

<a id="p1-6"></a>
### P1-6 优雅退出未等落盘完成就关闭 SQLite

**位置** `cmd/gridbot/main.go:81`（`defer st.Close()`）、`:142`（`sup.Close`）、`supervisor.go:68-80`

**现象与根因** `sup.Close(shutCtx)` 只做了 `Call(CmdStop)` + `cancel()` + `ex.Close()` 就返回，**不等 Runner goroutine 退出**。`run()` 返回后 `defer st.Close()` 立即关掉数据库，而 Runner 可能还在 `finishStop` → `persist()` → `SaveRuntime` 的路上（`finishStop` 里有两次 REST `refreshPosition`，可能耗时数秒）。

后果：退出时的终态快照写入 `sql: database is closed` 失败，只留一条 warn。下次启动 `RestoreRunning` 读到的是更早的快照，格子配对、`epoch`、已实现统计都会回退。

**修复方案** 用 [P0-5](#p0-5) 引入的 `runDone` 收口：

```go
// supervisor.go
func (s *Supervisor) Close(ctx context.Context) {
	s.mu.Lock()
	list := make([]*instance, 0, len(s.inst))
	for _, inst := range s.inst {
		list = append(list, inst)
	}
	s.mu.Unlock()   // 不要持锁做网络 I/O

	for _, inst := range list {
		if inst.runner != nil && inst.runner.Status() != engine.StatusStopped {
			_ = inst.runner.Call(ctx, engine.CmdStop, nil)
		}
		if inst.cancel != nil {
			inst.cancel()
		}
	}
	// 等所有事件循环真正退出，之后 main 才能关闭 SQLite。
	for _, inst := range list {
		if inst.runDone == nil {
			continue
		}
		select {
		case <-inst.runDone:
		case <-ctx.Done():
		}
	}
	for _, inst := range list {
		_ = inst.ex.Close()
	}
}
```

`main.go` 里把 `defer st.Close()` 换成显式顺序，确保它排在 `sup.Close` 之后（`defer` 本来就是后进先出，`st.Close` 的 defer 注册在 `sup.Close` 调用之前，所以顺序是对的；关键是 `sup.Close` 必须真的等到位）。

---

<a id="p1-7"></a>
### P1-7 `price_source: last` 配置项完全失效

**位置** `internal/app/risk/guard.go:183-198`

**现象与根因**

```183:198:internal/app/risk/guard.go
func (g *Guard) price() decimal.Decimal {
	switch g.params.PriceSource {
	case market.PriceMid:
		if g.book.Valid() {
			return g.book.Mid()
		}
	case market.PriceLast:
		if g.last.IsPositive() {
			return g.last
		}
	}
```

`g.last` 在整个仓库里**只有这两行读取，没有任何赋值点**：

```
$ rg '\.last\b' internal/app/risk/
guard.go:190: if g.last.IsPositive() {
guard.go:191:     return g.last
```

而 `price_source: last` 是一个合法可解析的配置（`market/quotes.go:60-69` 的 `ParsePriceSource` 接受 `"last"`），文档也把它列为可选项。用户配了它，Guard 会静默回落到标记价。

数据其实是齐备的：`exchange.Ticker` 有 `Last` 字段，`lighter/stream.go:359` 就在填 `s.latest.Last = st.LastTradePrice`。断点只在 `Guard.Observe`——`BookEvent` 里没有 `Last`。

**修复方案**

1. `strategy.BookEvent` 增加 `Last decimal.Decimal` 字段；
2. `runner.onStream` 转发时带上：`strategy.BookEvent{Book: ..., Mark: ..., Last: se.Ticker.Last, Now: now}`；
3. `Guard.Observe` 的 `BookEvent` 分支补 `if e.Last.IsPositive() { g.last = e.Last }`；
4. `Guard.SetMark` 增加一个 last 参数，或另加 `SetLast`，让 `handleStart`/`reconcile` 的初始快照也能填上。

**若短期不想实现**：至少让 `config` 校验阶段明确拒绝 `price_source: last`，返回"暂不支持"，而不是接受配置后静默换成另一个价源——风控触发价用错价源是会直接造成资金损失的。

---

<a id="p1-8"></a>
### P1-8 行情静默触发的重连没有退避

**位置** `internal/app/risk/guard.go:147-150`、`internal/app/engine/runner.go:409-414`

**现象与根因** Guard 在 `now - lastMarket >= StaleTimeout`（默认 60s）时返回 `Verdict{Reconnect: true}`，Runner 立刻 `resubscribe`：

```409:414:internal/app/engine/runner.go
		if v.Reconnect {
			r.log.Warn("market data stale, reconnecting")
			_ = r.resubscribe(ctx)
			return
		}
```

`g.lastMarket` 只在 `BookEvent` / `PositionEvent` 里更新（`guard.go:57`、`:59-64`），`TickEvent` 明确注释"快照不变"。所以只要行情真的断了：每一次 `TickEvent`（默认 1s 一次）都满足 stale 条件 → **每秒重建一次 WebSocket 连接**，每次 `resubscribe` 还会跟一次 `reconcile`（4 个 REST 调用：Position / Account / OpenOrders / Ticker）。

这会迅速打爆交易所的限流（配置里 `rps: 10`），并让适配器内部的指数退避完全失效——因为每次都是新建的 stream，退避计数从头开始。

**触发场景** 交易所维护、代理中断、深夜冷门合约长时间无报价（后者不算故障，却会被当成故障）。

**修复方案** 给 Runner 加重连节流，并在触发后重置计时器：

```go
type Runner struct {
	// ...
	lastResubscribe time.Time
	resubBackoff    time.Duration
}

func (r *Runner) staleReconnect(ctx context.Context, now time.Time) {
	if r.resubBackoff <= 0 {
		r.resubBackoff = 5 * time.Second
	}
	if !r.lastResubscribe.IsZero() && now.Sub(r.lastResubscribe) < r.resubBackoff {
		return
	}
	r.lastResubscribe = now
	r.log.Warn("market data stale, reconnecting", "backoff", r.resubBackoff)
	if err := r.resubscribe(ctx); err != nil {
		r.resubBackoff = min(r.resubBackoff*2, 2*time.Minute)
		return
	}
	r.resubBackoff = 5 * time.Second
	// 重连成功后刷新 stale 基准，避免下一 tick 立刻又判定为静默。
	r.guard.SetMark(r.state.Mark, r.state.Book, now)
}
```

`Guard` 侧也应在返回 `Reconnect` 后把 `lastMarket` 前推（或增加 `NoteReconnect(now)`），否则调用方无论怎么节流，判定条件都永远成立。

---

<a id="p1-9"></a>
### P1-9 `StatusReconnecting` 无法自行恢复

**位置** `internal/app/engine/runner.go:110-118`、`:784-798`

**现象与根因** 事件流 channel 关闭时状态被置为 `StatusReconnecting`：

```110:118:internal/app/engine/runner.go
		case se, ok := <-r.stream:
			if r.stream == nil {
				continue
			}
			if !ok {
				r.status = StatusReconnecting
				_ = r.resubscribe(ctx)
				continue
			}
```

而 `syncStatus` 对这个状态直接早退，不会再把它推回 `Running`：

```784:787:internal/app/engine/runner.go
func (r *Runner) syncStatus() {
	if r.status == StatusError || r.status == StatusReconnecting || r.status == StatusStopped {
		return
	}
```

于是即便 `resubscribe` 完全成功、事件流恢复正常，状态仍是 `Reconnecting`。而 `dispatch` 只在 `Running`/`Starting`/`Paused` 才把事件交给策略（`runner.go:404-407`），其余走 `updateViewOnly`。结果：**行情在流、页面在刷，但策略再也收不到任何事件，网格彻底冻结**。手动点「重连」也没用——`CmdReconnect` 只调 `resubscribe`，不重置状态（`runner.go:204-208`），而 `reconcile` 里的 `ResyncEvent` 派发同样被 `status == Running || Starting` 的条件挡住（`runner.go:759`）。唯一出路是重新 `Start`。

顺带一提，`r.stream == nil` 时那个 `continue` 是死代码：nil channel 的接收永远阻塞，`select` 不会选中该分支。

**修复方案** 让重连成功后状态明确回到工作态，并把 `StatusReconnecting` 从 `syncStatus` 的早退名单里去掉：

```go
		case se, ok := <-r.stream:
			if !ok {
				r.status = StatusReconnecting
				if err := r.resubscribe(ctx); err != nil {
					r.log.Error("resubscribe failed", "err", err)
					continue   // 由 P1-8 的节流逻辑在 tick 里重试
				}
				// 事件流已恢复，交回策略阶段决定真实状态。
				r.syncStatus()
				continue
			}
			r.onStream(ctx, se)
```

```go
func (r *Runner) syncStatus() {
	if r.status == StatusError || r.status == StatusStopped {
		return
	}
	// Reconnecting 是临时态，策略阶段是唯一权威。
	...
}
```

同时在 tick 分支里为 `StatusReconnecting` 安排重试（复用 P1-8 的 `staleReconnect`），确保首次 `resubscribe` 失败后仍会持续尝试而不是永久躺平。

`CmdReconnect` 也应在成功后调一次 `syncStatus()`。

---

<a id="p1-10"></a>
### P1-10 默认公网监听且鉴权默认关闭

**位置** `internal/config/config.go:184-186`、`Validate()`（`:217` 起，不拦截此组合）、`config/config.example.yaml:24-28`

**现象** `applyDefaults` 把 `server.addr` 缺省成 `0.0.0.0:8080`，`auth.enabled` 默认 `false`，`Validate` 允许这个组合通过（`config_test.go` 里的 `TestValidateAllowsPublicAddrWithoutAuth` 还把它固化成了预期行为）。所有 `/api/*` 写操作——启动/停止网格、改策略、撤单、调整区间——都不需要任何凭证。

按示例配置部署到云主机、端口对公网可达，就等于把资金控制权公开。README 第 9 节提到了这个风险，但默认值本身仍是不安全的一侧。

**修复方案** 把安全默认前移，让不安全配置需要显式声明：

```go
// applyDefaults
if c.Server.Addr == "" {
	c.Server.Addr = "127.0.0.1:8080"   // 默认只本机
}

// Validate
if !c.Server.Auth.Enabled && !c.Server.IPWhitelist.Enabled && isPublicAddr(c.Server.Addr) {
	return fmt.Errorf("server.addr 为 %s（公网可达）时必须启用 server.auth 或 server.ip_whitelist；"+
		"只本机访问请改成 127.0.0.1:8080", c.Server.Addr)
}

func isPublicAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && !ip.IsLoopback()
}
```

`config.example.yaml` 与 `docs/DEPLOYMENT.md` 同步改成"默认 127.0.0.1，需要外网访问时同时打开 auth"。相应地把 `TestValidateAllowsPublicAddrWithoutAuth` 改成 `TestValidateRejectsPublicAddrWithoutAuth`。

---

<a id="p1-11"></a>
### P1-11 开启鉴权后跨域预检被 401

**位置** `internal/api/server.go:105-131`

**现象与根因** `withAuth` 的顺序是"先校验 Authorization，再设置 CORS 头并处理 OPTIONS"：

```105:128:internal/api/server.go
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Auth.Enabled && strings.HasPrefix(r.URL.Path, "/api/") {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" || got != s.cfg.Auth.Token {
				writeError(w, http.StatusUnauthorized, ...)
				return
			}
		}
		if len(s.cfg.CORSOrigins) > 0 {
			...
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
```

浏览器的 CORS 预检 `OPTIONS` **按规范不携带 `Authorization` 头**，所以在 `auth.enabled=true` 时必然拿到 401（且响应里没有 CORS 头），浏览器随即阻止真实请求。同源控制台不受影响，跨域前端完全不可用。

**修复方案** 预检必须在鉴权之前放行：

```go
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.writeCORS(w, r)
		// 预检不带 Authorization，必须在鉴权之前短路返回。
		if r.Method == http.MethodOptions && len(s.cfg.CORSOrigins) > 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if s.cfg.Auth.Enabled && strings.HasPrefix(r.URL.Path, "/api/") {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Auth.Token)) != 1 {
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "无效的访问令牌", "")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
```

顺手把 token 比较换成 `crypto/subtle.ConstantTimeCompare`，消除时序侧信道（当前是 `got != s.cfg.Auth.Token` 的短路比较）。

---

<a id="p1-12"></a>
### P1-12 请求体无大小上限

**位置** `internal/api/server.go:166`、`:189`、`:250`

**现象** `handlePreview` 与 `handlePutConfig` 用 `io.ReadAll(r.Body)` 无上限读取，`handleAdjust` 用 `json.NewDecoder(r.Body).Decode` 同样无限制。单个超大请求即可把进程内存吃满——对一个持有实盘仓位的进程来说，OOM 意味着失控。

**修复方案** 统一在 `withAccess` 里加一层：

```go
const maxRequestBody = 1 << 20 // 1MB，策略 JSON 远小于此

func (s *Server) withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withAccess(next http.Handler) http.Handler {
	return s.withIPWhitelist(s.withAuth(s.withBodyLimit(next)))
}
```

同时给 `http.Server` 补上超时（当前 `server.go:36` 只设了 `Addr` 和 `Handler`）：

```go
s.http = &http.Server{
	Addr:              cfg.Addr,
	Handler:           s.withAccess(s.mux),
	ReadHeaderTimeout: 10 * time.Second,
	ReadTimeout:       30 * time.Second,
	WriteTimeout:      60 * time.Second,   // K 线响应可能较大
	IdleTimeout:       120 * time.Second,
}
```

缺少 `ReadHeaderTimeout` 会让 Slowloris 类连接长期占用 goroutine。

---

<a id="p1-13"></a>
### P1-13 反向代理后 IP 白名单失效

**位置** `internal/api/server.go:350-356`

**现象与根因**

```350:356:internal/api/server.go
func clientIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}
```

只看 `RemoteAddr`。在 nginx / Caddy 反代之后，`RemoteAddr` 恒为 `127.0.0.1` 或内网地址，而 `ipAllowed` 的第一条规则就是 `if ip.IsLoopback() { return true }`（`server.go:362-364`）——**所有外部流量都会被无条件放行**，白名单形同虚设。用户以为自己加了一层防护，实际没有。

注意：直连场景下当前实现是**安全的**（不解析 `X-Forwarded-For`，无法伪造）。所以不能简单改成"信任 XFF"，那会引入伪造漏洞。

**修复方案** 引入显式的可信代理配置，只对可信 hop 解析转发头：

```go
// config
type Server struct {
	// ...
	// TrustedProxies 是可信反向代理的 IP/CIDR 列表。
	// 只有 RemoteAddr 命中该列表时，才采信 X-Forwarded-For 的最右一跳。
	TrustedProxies []string `yaml:"trusted_proxies"`
}
```

```go
func (s *Server) clientIP(r *http.Request) net.IP {
	direct := parseHostIP(r.RemoteAddr)
	if len(s.cfg.TrustedProxies) == 0 || direct == nil {
		return direct
	}
	if !ipAllowed(direct, s.cfg.TrustedProxies) {
		return direct // 不是可信代理，忽略转发头
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(parts[i]))
		if ip == nil {
			continue
		}
		if !ipAllowed(ip, s.cfg.TrustedProxies) {
			return ip // 最右侧第一个非可信 IP 即真实客户端
		}
	}
	return direct
}
```

同时把 `ipAllowed` 里的 `ip.IsLoopback()` 无条件放行改成"仅当未配置 `trusted_proxies` 时放行"——反代场景下 loopback 就是代理本身，不该自动过关。

若不想引入新配置，最简做法是在文档里明确写死"启用 `ip_whitelist` 时 gridbot 必须直接监听、不得置于反代之后；反代场景请在反代层做 IP 限制"，并在检测到 `X-Forwarded-For` 存在且白名单开启时打一条显眼的告警日志。

---

## P2 轻微问题

<a id="p2-1"></a>
### P2-1 `lastFillQty` map 无界增长

**位置** `internal/app/engine/runner.go:832-861`

`logOrderFill` 为每个见过的 `ClientOrderID` 记一条累计成交量，用于算增量打日志，**但从不清理**。COID 编码了 `epoch`（16 bit）与 `cell`（12 bit），长期运行（尤其开启 trailing，每次移格 epoch +1）会持续新增键。

修复：订单终态时删除条目。

```go
func (r *Runner) recordFill(o order.Order) {
	r.logOrderFill(o)
	// ...
}

// logOrderFill 末尾
if o.State.IsTerminal() {
	delete(r.lastFillQty, o.ClientOrderID)
}
```

另外 `syncEpoch` 里 epoch 前进时可以整表清空（旧轮次的订单不会再有回报）。

<a id="p2-2"></a>
### P2-2 批量下单结果与请求按下标配对

**位置** `internal/app/executor/executor.go:247-249`（`place`）、`:363-365`（`modify`）

```go
for i, r := range results {
	req := chunk[i]
```

隐含契约"`PlaceOrders` 返回的结果数量与顺序必须与请求完全一致"，但 `exchange.Exchange` 接口没有声明这一点，适配器也没有断言。若某个适配器（或未来新增的交易所）少返回一条，剩余请求就静默丢失——对应格子停在 `CellPending` 直到 30s 超时；若多返回一条则 `chunk[i]` 直接越界 panic。

修复：加显式校验，把契约变成硬约束。

```go
	results, err := e.ex.PlaceOrders(ctx, chunk)
	if err == nil && len(results) != len(chunk) {
		err = exchange.Classify(exchange.ClassUnknown, "place_order",
			fmt.Errorf("适配器返回 %d 条结果，请求 %d 条", len(results), len(chunk)))
	}
```

并在 `exchange.go` 的接口注释里写明"返回值必须与入参一一对应、顺序一致"。

<a id="p2-3"></a>
### P2-3 撤单结果被丢弃

**位置** `internal/app/executor/executor.go:432-439`

```go
	for i, r := range results {
		if r.Err == nil {
			continue
		}
		e.recordClass(exchange.ClassOf(r.Err), res)
		_ = now
		_ = chunk[i]
	}
```

`_ = now` / `_ = chunk[i]` 是留下的空壳：撤单失败只被计入失败数，既不产生 `OrderEvent`，也不告知策略"这一格的撤单没成功"。而策略早已乐观地把格子置空（见 [P1-1](#p1-1)），于是交易所上留下一笔幽灵挂单，只能等看门狗发现。

修复：撤单失败时回灌一条事件，让策略知道订单仍然存活。

```go
	for i, r := range results {
		if r.Err == nil {
			continue
		}
		class := exchange.ClassOf(r.Err)
		e.recordClass(class, res)
		// 撤单失败（订单已成交或已不存在）时告知策略，避免本地认为已撤而留下幽灵单。
		res.Events = append(res.Events, strategy.OrderEvent{
			Order: order.Order{
				ClientOrderID: chunk[i].ClientOrderID,
				Symbol:        e.opts.Symbol,
				State:         order.StateOpen,   // 仍在场上，交由 Resync 裁决
				UpdatedAt:     now,
			},
			Now: now,
		})
	}
```

<a id="p2-4"></a>
### P2-4 Pending 超时重挂复用同一 COID，重复拒绝会喂熔断

**位置** `internal/domain/strategy/grid/strategy.go:395-411`（`expirePending`）

`expirePending` 把格子置回 `CellEmpty` 但保留 `COID` 与 `Seq`，`placeActions` 会用完全相同的 `(slot, epoch, cell, purpose, seq)` 重新编码，即同一个 COID。这是**有意设计**（`strategy.go:43-49` 的注释说明：若原单其实还活着，交易所会以"重复订单号"拒绝，比下出两单更安全），思路是对的。

问题在下游：重复订单号的错误文案（`duplicate`、`already exists`）在 `lighter/classify.go:62-86` 的 `messagePatterns` 里**没有任何匹配项**，因此归为 `ClassUnknown`。而 `ClassUnknown.CountsAsFailure()` 返回 `true`（`exchange/errors.go:54-56`），会累加到 `guard.RecordFailures`。价格剧烈波动、多个格子同时超时重挂时，很容易在 10 次连续失败内触发熔断，把一个健康的实例停掉。

修复：给"重复订单号"一个明确分类，语义是"意图已经生效，不算失败"。

```go
// exchange/errors.go
// ClassDuplicate 客户端订单号重复：说明原单仍然存活，意图已经生效，不算失败。
ClassDuplicate

func (c ErrorClass) CountsAsFailure() bool {
	return c != ClassPostOnlyRejected && c != ClassInsufficientMargin && c != ClassDuplicate
}
```

```go
// lighter/classify.go 与 rhlighter/classify.go，放在 InvalidParam 之前
{exchange.ClassDuplicate, []string{
	"duplicate", "already exists", "client order index", "order id exists",
}},
```

策略侧收到该分类时应把格子恢复成 `CellResting`（原单还在），而不是 `CellEmpty`。

<a id="p2-5"></a>
### P2-5 `ActiveWindow` 的 pivot 与 `pivotCell` 判定不一致

**位置** `internal/domain/strategy/grid/grid.go:322-327` vs `internal/domain/strategy/grid/derive.go:334-343`

`ActiveWindow` 找 pivot 用的是"最后一个 `High <= mark` 的格子"：

```322:327:internal/domain/strategy/grid/grid.go
	pivot := 0
	for i := range g.Cells {
		if g.Cells[i].High.LessThanOrEqual(mark) {
			pivot = i
		}
	}
```

而 `derive.go` 的 `pivotCell` 用的是真正的包含判定 `Low <= mark < High`。两者对同一个 mark 会给出不同答案：格子 `[100,101] [101,102] [102,103]`、`mark=101.5` 时，`pivotCell` 返回 1（正确包含），`ActiveWindow` 返回 0。

后果：挂单窗口整体向下偏一格。配了 `max_active_orders` 时，现价上方少挂一格、下方多挂一格，网格对上行行情的响应变差。同时 `Derived.OrderCount` 与实际挂单集合的中心也不一致。

修复：统一用包含判定，并把它提成 `Grid` 的方法供两处复用。

```go
// grid.go
// PivotCell 返回现价所在格子的索引；现价在区间外时返回最近的边界格。
func (g *Grid) PivotCell(mark decimal.Decimal) int {
	for i := range g.Cells {
		if mark.GreaterThanOrEqual(g.Cells[i].Low) && mark.LessThan(g.Cells[i].High) {
			return i
		}
	}
	if mark.LessThan(g.Lower()) {
		return 0
	}
	return len(g.Cells) - 1
}
```

`ActiveWindow` 改用 `pivot := g.PivotCell(mark)`，`derive.go` 里的 `pivotCell` 函数删除并改调它。

顺带：`ActiveWindow` 的扩散循环总是先向下扩一格再向上（`grid.go:330-348`），`maxActive` 为奇数时下方恒比上方多一格。若希望严格对称，应交替方向或按到 mark 的距离排序后取前 N 个。

<a id="p2-6"></a>
### P2-6 建仓改价序号 4 bit，16 次后 COID 回绕

**位置** `internal/app/entry/entry.go:411`、`internal/domain/order/coid.go:152-159`

`Seq` 只有 4 bit（`MaxSeq = 15`），`NextSeq` 到顶回绕。建仓触发器的 `MaxReprice` 默认 **100**，`RepriceInterval` 默认 **500ms**，即约 8 秒就会跑完 16 个序号开始复用。建仓单的 COID 里 `cell` 恒为 0、`purpose` 恒为 `PurposeEntry`、`epoch` 不变，所以第 17 次改价会撞上第 1 次的号。

`coid.go:155-157` 的注释论证过安全性："同一格子在同一 epoch 内重挂超过 16 次的情况下，最早那批订单早已终结"。对网格格子（重挂间隔以秒计）成立，但**建仓跟价 8 秒转一圈**，前一轮的撤单回报未必已经落地，尤其在 Lighter 这种入块延迟 3-7 秒的链上。撞号会被交易所以重复订单号拒绝，进而叠加 [P2-4](#p2-4) 的熔断风险。

修复（三选一）：

1. 把 `MaxReprice` 的默认值降到 12 以下，并在 `Validate` 里拒绝 `MaxReprice > 15`；
2. 建仓单改用 `cell` 字段承载改价轮次（建仓不需要格子索引，12 bit 可用 → 4096 次），`Seq` 仍留给同轮次内的重试；
3. 每次改价轮次用满后递增 `epoch`（代价是要同步 executor 与看门狗的 epoch 认知，较重）。

推荐方案 2，改动小且彻底：

```go
	t.repriceSlot = (t.repriceSlot + 1) & 0x0FFF
	coid, err := order.Encode(order.Ref{
		Slot:    t.slot,
		Epoch:   t.epoch,
		Cell:    t.repriceSlot,   // 建仓不使用格子索引，借用它做改价轮次
		Purpose: order.PurposeEntry,
		Seq:     t.seq,
	})
```

注意 `grid.syncFromOrders`（`strategy.go:788-790`）已经会跳过 `PurposeEntry` 的订单，`runner.watchdog`（`runner.go:691`）会撤掉它们，因此借用 `cell` 字段不会干扰网格对账。

<a id="p2-7"></a>
### P2-7 HTTP 客户端超时复用 `shutdown_timeout`

**位置** `cmd/gridbot/main.go:83`

```go
httpClient, err := httpx.New(cfg.Proxy, cfg.App.ShutdownTimeout.Std())
```

把"进程退出等待时长"当成"交易所 REST 请求超时"。两者没有任何语义关联：调大 `shutdown_timeout` 会让单次挂死的 REST 调用拖住整个事件循环（`handleStart` 里串了 5 个 REST 调用），调小则可能让正常下单超时。

注意 `exchanges[].timeout`（默认 10s）已经存在于配置里，但只传给了适配器，没用于这个共享 client。

修复：新增独立配置项，并给出合理默认。

```go
// config: app.http_timeout，默认 15s
httpClient, err := httpx.New(cfg.Proxy, cfg.App.HTTPTimeout.Std())
```

`main.go:87-89` 已有 `if httpClient.Timeout <= 0 { httpClient.Timeout = 15 * time.Second }` 兜底，可以直接把默认值提到 config 层。

<a id="p2-8"></a>
### P2-8 代理凭证经 `/api/proxy` 明文回显

**位置** `internal/app/supervisor/supervisor.go:293-300`

```go
func (s *Supervisor) Proxy() map[string]any {
	p := s.cfg.Proxy
	return map[string]any{
		"enabled":  p.Enabled,
		"url":      p.URL,
		"no_proxy": p.NoProxy,
	}
}
```

注释写的是"不含密码"，但 `p.URL` 是原文返回。代理 URL 支持 `http://user:pass@host:port` 形式，凭证会通过 `GET /api/proxy` 暴露给任何能访问 API 的客户端（在 [P1-10](#p1-10) 的默认配置下就是任何人）。

修复：

```go
func (s *Supervisor) Proxy() map[string]any {
	p := s.cfg.Proxy
	safe := p.URL
	if u, err := url.Parse(p.URL); err == nil && u.User != nil {
		u.User = nil
		safe = u.String()
	}
	return map[string]any{"enabled": p.Enabled, "url": safe, "no_proxy": p.NoProxy}
}
```

同时检查 `httpx/client.go` 与日志路径，确保带凭证的代理 URL 不会被打进日志（`log.Info("proxy enabled", "url", ...)` 之类）。

<a id="p2-9"></a>
### P2-9 `ListFills` 的 `symbol=''` 兜底混入跨交易对成交

**位置** `internal/infra/store/store.go:330-333`

```go
	if symbol != "" {
		q += ` AND (symbol=? OR symbol='')`
		args = append(args, symbol)
	}
```

`OR symbol=''` 是为兼容 `migrateFillsSymbol` 之前的旧数据（那时 `fills` 没有 symbol 列，迁移时填了默认空串）。副作用是：切换交易对后，所有历史遗留成交都会混进当前交易对的成交表和盈亏视图。

修复：一次性回填而不是长期兜底。在 `migrateFillsSymbol` 里补一步——用 `strategy_configs.symbol` 把该交易所的 legacy 空串行回填，之后查询条件收紧为 `AND symbol=?`。

```go
func (s *Store) migrateFillsSymbol() error {
	// ... 现有 ALTER TABLE / CREATE INDEX ...
	// 用各交易所当前配置的交易对回填历史空 symbol，之后查询不再需要 OR symbol=''
	_, err = s.db.Exec(`
UPDATE fills SET symbol = (
    SELECT symbol FROM strategy_configs WHERE strategy_configs.exchange = fills.exchange
)
WHERE symbol = '' AND EXISTS (
    SELECT 1 FROM strategy_configs WHERE strategy_configs.exchange = fills.exchange
)`)
	return err
}
```

回填不精确（旧成交可能属于更早的交易对），但比无条件混入所有交易对更接近事实；若要求严格，可把无法归属的行标为 `symbol='<unknown>'` 并在页面上单独归类。

<a id="p2-10"></a>
### P2-10 平仓腿数量向下取整留下 dust

**位置** `internal/domain/market/market.go:97-102`、`grid/strategy.go:542-550`、`martingale/runtime.go:600`

`RoundQty` 一律向下取整，注释解释为"宁可少下一点，也不要因为向上取整导致保证金不足"。对**开仓腿**完全正确，对**平仓腿**则会持续留下残渣：

- 网格：`orderQty` 对 armed 格子返回 `RoundQty(c.OpenQty)`。`OpenQty=0.35`、`LotSize=0.1` → 挂 0.3；平仓成交后 `handleFill` 把 `OpenQty` 直接清零（`strategy.go:672-674`），0.05 从此无人认领。
- 马丁：止盈单数量 `RoundQty(position.Abs())`，同样可能小于实际仓位。

单次量极小，但网格是高频循环策略，长期累积会形成一笔方向不定的残留仓位，并在停止时体现为 `residual = true`。

修复：区分开仓与平仓的取整方向。平仓腿受 reduce-only 保护，向上取整不会意外反向开仓（交易所会自动截断到实际仓位）。

```go
// market.go
// RoundQtyUp 向上规整数量，仅用于平仓腿：reduce-only 会兜住超出部分，
// 而向下取整会持续留下无法闭合的 dust。
func (m Market) RoundQtyUp(q decimal.Decimal) decimal.Decimal {
	return roundTo(q, m.LotSize, RoundUp)
}
```

```go
// grid/strategy.go orderQty
func (s *Strategy) orderQty(c *Cell) decimal.Decimal {
	if c.Armed && c.OpenQty.IsPositive() {
		q := s.mkt.RoundQtyUp(c.OpenQty)   // 平腿：宁可多挂一个 lot，由 reduce-only 兜住
		if q.IsPositive() {
			return q
		}
	}
	return c.Qty
}
```

前提是平仓腿确实带了 reduce-only。`params.go:305-308` 的 `reduceOnlyFor` 对**中性网格恒返回 false**（因为两侧都可能开仓），所以中性网格不能用向上取整。修复时需按 `reduceOnlyFor(c.Side)` 的结果选择取整方向：

```go
	if c.Armed && c.OpenQty.IsPositive() {
		q := s.mkt.RoundQty(c.OpenQty)
		if s.params.reduceOnlyFor(c.Side) {
			q = s.mkt.RoundQtyUp(c.OpenQty)
		}
		...
	}
```

---

## 建议的修复顺序

**第一批（上实盘前必须修完）**

1. [P0-2](#p0-2) 中性网格止损误触发 —— 直接造成资金损失，且当前配置校验会主动引导用户进入这个陷阱
2. [P0-3](#p0-3) nonce 重校准 —— 决定实例能否稳定下单
3. [P0-4](#p0-4) WS 订阅被拒重连 —— 决定成交回报是否可靠
4. [P0-1](#p0-1) 数据竞争 —— 崩溃时机不可控，带仓位崩溃后果最重

**第二批（一周内）**

5. [P0-5](#p0-5) + [P1-6](#p1-6) 生命周期收口（同一处改动一并完成）
6. [P1-1](#p1-1) 窗口外撤单丢成交
7. [P1-2](#p1-2) 建仓 reduce-only
8. [P1-3](#p1-3) / [P1-4](#p1-4) 马丁两处
9. [P1-9](#p1-9) + [P1-8](#p1-8) 重连状态机（同一处改动一并完成）

**第三批（部署前）**

10. [P1-10](#p1-10) ~ [P1-13](#p1-13) 四个 HTTP / 部署安全问题
11. [P1-5](#p1-5) 成交记账
12. [P1-7](#p1-7) `price_source: last`（或明确拒绝该配置）

**第四批** P2 全部。

## 建议补充的工程手段

三个问题（[P0-1](#p0-1)、[P0-2](#p0-2)、[P1-2](#p1-2)）都属于"现有测试全绿但功能是错的"，说明测试的形状需要调整：

1. **CI 加 `go test -race ./...`**。当前 21 个包全部通过 `go test`，但 [P0-1](#p0-1) 的竞态在加上 `-race` 后一个简单的并发读测试就能抓到。这是投入产出比最高的一项。
2. **风控测试按"方向 × 触发价位置 × 仓位符号"做矩阵覆盖**。`guard_test.go` 目前只有做多做空各一条正例，中性网格这个完整象限没有任何用例。
3. **给建仓触发器补跨零场景**。`entry_test.go` 覆盖了同向加仓与纯平仓，缺"从空头翻到多头"。
4. **策略层补"撤单与成交竞争"的场景测试**：先发 `CancelOrder`、再投递该 COID 的 `StateFilled` 回报，断言成交仍被正确记账。这类测试能同时守住 [P1-1](#p1-1) 与 [P2-3](#p2-3)。
5. **`lighter` 与 `rhlighter` 是两份复制体**（`tx.go`/`classify.go`/`stream.go`/`rest.go` 几乎逐行相同）。本次审计发现的 [P0-3](#p0-3)、[P0-4](#p0-4) 在两份文件里都存在，任何修复都必须同改。建议把共用逻辑抽到一个内部包，用 chain_id / 端点 / 费率兜底作为参数注入，避免下一次修复只改一半。本次未发现两者之间有"复制漏改"导致的差异（端点、`chain_id` 304 / 466324、slot 0 / 1 都是有意区分的），但错误文案里 `rhlighter/rest.go:71,178,185` 与 `stream.go:298` 仍写着 `lighter`，会让监控把 RH 的故障归到 Core。
