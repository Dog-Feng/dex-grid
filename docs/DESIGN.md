# dex-grid 开发设计文档

版本：v0.4（Lighter + RH Lighter + 普通/马丁网格。Web 控制台已 embed，默认不自动开网格）

本文描述分层职责、核心抽象、网格算法、事件与命令流、HTTP API 契约与工程约定。Lighter 协议、签名、nonce、改单与主网踩坑见 [LIGHTER.md](LIGHTER.md)。RH Lighter（独立实例）见 [RH_LIGHTER.md](RH_LIGHTER.md)。配置字段规范见 [GRID_CONFIG.md](GRID_CONFIG.md)，部署见 [DEPLOYMENT.md](DEPLOYMENT.md)。

---

## 目录

1. [设计原则与核心约束](#1-设计原则与核心约束)
2. [分层与依赖](#2-分层与依赖)
3. [核心抽象](#3-核心抽象)
4. [普通网格算法](#4-普通网格算法)
5. [建仓触发器](#5-建仓触发器)
6. [事件流与实例运行时](#6-事件流与实例运行时)
7. [命令通道与运行时操作](#7-命令通道与运行时操作)
8. [HTTP API 契约](#8-http-api-契约)
9. [订单标识与幂等](#9-订单标识与幂等)
10. [风控、止盈止损与区间外策略](#10-风控止盈止损与区间外策略)
11. [Trailing 网格](#11-trailing-网格)
12. [行情分析与参数推荐](#12-行情分析与参数推荐)
13. [Lighter 适配器](#13-lighter-适配器)（正文在 [LIGHTER.md](LIGHTER.md)）
14. [持久化与对账](#14-持久化与对账)
15. [错误处理与限流](#15-错误处理与限流)
16. [可观测性与日志面板](#16-可观测性与日志面板)
17. [跨平台工程约定](#17-跨平台工程约定)
18. [马丁网格](#18-马丁网格)
19. [测试策略](#19-测试策略)
20. [依赖清单](#20-依赖清单)
21. [扩展指南](#21-扩展指南)

---

## 1. 设计原则与核心约束

### 1.0 标的范围：只做永续合约

系统里的策略全部是**合约网格**。普通网格的做空与中性方向需要开空能力，三种方向的平仓腿都依赖 reduce-only，马丁网格更是直接按保证金与杠杆计算加仓规模——这些语义现货市场一个都不具备。

因此**现货市场在适配器层就被过滤掉**，不进缓存、不进下拉、不能下单。这不是待实现的功能，是划定的边界：往上层加"如果是现货就走另一套逻辑"的分支，等于在每个策略里都开一条几乎没人走的路径。

三条具体规则：

| 情况 | 处理 |
| --- | --- |
| 现货市场 | 适配器刷新缓存时直接丢弃，按"未找到永续合约交易对"报错 |
| 已下架的合约 | 仍然缓存元数据（运行中的实例可能持有仓位，对账要用），但不进下拉、不允许开新仓 |
| 已下架合约上的存量仓位 | `reduce_only` 的平仓单放行，否则仓位会被锁死在里面 |

Lighter 主网的实际分布：235 个市场 = 227 个永续 + 8 个现货（`ETH/USDC` 这类带斜杠的符号，`market_id` 从 2048 起），永续里 17 个已下架，最终可选 210 个。

### 1.1 核心约束：一个 DEX 一个实例

**每个交易所同时只运行一个网格实例，一个实例只跑一个交易对、一套策略。**

这条约束不是偷懒，它是本项目复杂度控制的支点。直接消除的复杂度：

| 若允许多实例 | 单实例下 |
| --- | --- |
| Lighter nonce 需要跨实例的分配器 + 回收 + 空洞修复 | 单条串行发送队列，nonce 自然递增 |
| `ClientOrderID` 需要实例槽位分片 | 槽位退化为交易所在注册表中的固定索引 |
| 保证金/挂单额度需要跨实例配额管理 | 整个交易所账户归一个实例支配 |
| 对账需要区分「别的实例的单」和「孤儿单」 | 所有非当前 epoch 的本账户挂单都是孤儿单 |
| 存储、配置、API 路径都要带实例维度 | 一切以交易所名为键 |

代价是同一个交易所不能同时跑多个币种。这符合原型图的形态（一个 Tab 一套配置），也符合实际使用场景（一个账户的保证金本来就该由一个策略统一支配）。

**打破这条约束需要重新评估上面整张表，不要顺手加个 map 就以为支持多实例了。**

### 1.2 设计原则

| 原则 | 落地 |
| --- | --- |
| **纯领域** | `domain` 零 IO、零第三方 SDK、零 `time.Now()`（时间由事件携带），可确定性测试 |
| **意图与执行分离** | 策略返回 `[]Action`，`Executor` 负责翻译、批量、限流、重试 |
| **单线程状态** | 行情、回报、定时器、页面命令全走同一个 goroutine，领域状态无锁 |
| **能力协商** | 交易所差异用 `Capabilities` 描述，上层按能力降级 |
| **确定性幂等** | `ClientOrderID` 由 (交易所, 轮次, 层级, 用途, 序号) 确定性计算 |
| **配置双源** | 凭证/运维在 `config.yaml`（重启生效）；策略参数走控制台 / REST（SQLite）。`strategy_file` 仅在尚无已保存配置时作为模板；`autostart` 默认关闭 | |
| **不过度设计** | 只抽象 `Exchange` 与 `Strategy` 两个端口（两者都确定有多实现）；日志直接用 `*slog.Logger`，不包接口；不引入 DI 容器、ORM、事件总线 |

---

## 2. 分层与依赖

```
   Web 控制台（go:embed web/）/ 策略 YAML / REST
        │  同源 HTTP（静态页 + REST）
   ┌────▼──────────────────────────────────────────┐
   │ api    路由 · DTO 校验 · 命令下发 · 实时推送      │
   └────┬──────────────────────────────────────────┘
        │ 命令 channel（阻塞等回执）
   ┌────▼──────────────────────────────────────────┐
   │ app    engine.Runner · executor · entry        │
   │        reconcile · risk · analysis             │
   └────┬───────────────────────┬──────────────────┘
        │ 调用接口                │ 调用接口
   ┌────▼───────────────┐  ┌────▼──────────────────┐
   │ domain（纯逻辑）     │  │ exchange 端口 + 适配器  │
   │ strategy.Strategy   │  │ exchange.Exchange      │
   │ grid / martingale   │  │ lighter / <后续 DEX>    │
   │ market/order/position│ └───────────────────────┘
   └─────────────────────┘
   ┌───────────────────────────────────────────────┐
   │ infra  config · store · logx · proxy · metrics │
   └───────────────────────────────────────────────┘
```

依赖方向永远向内。CI 守卫：

```bash
# domain 不允许 import 任何 internal 子包
go list -deps ./internal/domain/... | grep -E "dex-grid/internal/(app|api|exchange|infra)" && exit 1
```

### 职责边界

| 包 | 职责 | 明确不做 |
| --- | --- | --- |
| `api` | HTTP 路由、DTO 校验、把请求翻译成命令、WS 广播 | 不持有策略状态，不直接调 Exchange |
| `app/engine` | 事件循环、命令处理、生命周期 | 不含策略逻辑 |
| `app/executor` | Action → Exchange，批量拆分、限流、重试 | 不决定"该下什么单" |
| `app/entry` | 三种建仓模式的状态机 | 不管网格排布 |
| `app/reconcile` | 启动/周期对账 | 不改策略内部结构 |
| `app/risk` | 止盈止损、区间外策略、熔断 | 不直接下单，返回 Action |
| `app/analysis` | K 线 → 指标 → 趋势判定 → 参数推荐 | 不下单、不改配置，只返回建议 |
| `domain/grid` | 价位表、层级配对、成交后的下一步意图、派生量计算 | 不知道 Lighter 存在 |
| `exchange/lighter` | 协议翻译、签名、nonce、WS 重连、精度换算 | 不含策略语义 |
| `infra/logx` | slog 输出 + 内存环形缓冲（页面日志面板数据源） | — |
| `infra/proxy` | 代理拨号器与连通性探测（对应页面「IP 配置」与顶部「代理正常」） | — |

---

## 3. 核心抽象

### 3.1 Exchange 端口

```go
// internal/exchange/exchange.go
package exchange

type Exchange interface {
    Name() string
    Capabilities() Capabilities

    Markets(ctx context.Context) ([]MarketInfo, error)      // 页面交易对下拉（仅永续）
    Market(ctx context.Context, symbol string) (market.Market, error)
    Ticker(ctx context.Context, symbol string) (Ticker, error)
    Klines(ctx context.Context, symbol, interval string, limit int) ([]market.Kline, error)

    SetLeverage(ctx context.Context, symbol string, leverage int, mode market.MarginMode) error

    // 入参出参都是切片，单笔传长度为 1。适配器按 Capabilities 决定批量还是串行。
    PlaceOrders(ctx context.Context, reqs []PlaceRequest) ([]PlaceResult, error)
    ModifyOrders(ctx context.Context, reqs []ModifyRequest) ([]ModifyResult, error)
    CancelOrders(ctx context.Context, reqs []CancelRequest) ([]CancelResult, error)
    CancelAll(ctx context.Context, symbol string) error

    // 对账用的快照查询
    OpenOrders(ctx context.Context, symbol string) ([]order.Order, error)
    Position(ctx context.Context, symbol string) (position.Position, error)
    Account(ctx context.Context) (account.Snapshot, error)  // 余额、权益、保证金率

    Close() error
}

// 事件流与交易端口分开：REST 可先交付，WS 后补。
type Streamer interface {
    Subscribe(ctx context.Context, symbol string) (<-chan StreamEvent, error)
}

type Capabilities struct {
    BatchPlace    int  // 单批最大下单数，0 = 不支持批量
    BatchCancel   int
    ModifyOrder   bool // 支持改价改量，可省一次撤单
    PostOnly      bool // false 时本系统无法运行，启动即报错
    ReduceOnly    bool
    NativeTPSL    bool
    MaxOpenOrders int  // 单市场最大挂单数，0 = 不限
}
```

**设计说明**

- `PlaceOrders` / `ModifyOrders` / `CancelOrders` 天然收批量，避免上层出现单笔/批量两套路径。铺 80 格网格时批量是刚需；Lighter 目前 `BatchPlace=0`，Executor 拆成串行。
- 事件合流成单 channel 而非多个，让 Runner 的 select 只有三个分支（事件、命令、定时器），也避免多 channel 之间乱序导致「先收到成交、后收到挂单确认」这类难处理的时序问题。
- `PostOnly` 为 false 时直接拒绝启动，不做静默降级。策略挂单默认全是 maker；仅止损平仓与建仓超时允许市价 IOC。
- `ModifyOrder` 为 true 时改价改量（马丁止盈主路径）；为 false 时 Executor 降级成撤旧单再挂同一 `ClientOrderID`。改单失败不得把旧单当成 `Rejected` 清掉，否则会双挂。
- `Markets` 与 `Klines` 是为页面加的只读查询。事件流在 `Streamer` 上，不塞进交易端口。

### 3.2 Strategy 端口

```go
// internal/domain/strategy/strategy.go
type Strategy interface {
    Init(st State) ([]Action, error)              // 启动时一次，返回设杠杆/建仓/铺网格意图
    OnEvent(ev Event) ([]Action, error)           // 所有运行时事件的唯一入口
    OnCommand(cmd Command) ([]Action, error)      // 页面命令（调区间、补格等）
    OnStop(reason StopReason) ([]Action, error)   // 收尾意图
    Snapshot() ([]byte, error)                    // 落盘
    Restore(data []byte) error
    View() View                                   // 供页面展示的只读视图（层级表、完成格数等）
}

type State struct {
    Market   market.Market
    Position position.Position
    Account  account.Snapshot
    Book     BookTicker
    Orders   []order.Order // 对账后确认存活的订单
    Now      time.Time
}
```

事件与意图：

```go
type Event interface{ eventMarker() }

type BookEvent     struct{ Bid, Ask decimal.Decimal; Now time.Time }
type OrderEvent    struct{ Order order.Order;        Now time.Time }
type PositionEvent struct{ Position position.Position; Account account.Snapshot; Now time.Time }
type TickEvent     struct{ Now time.Time }   // 秒级，驱动跟价、超时、止盈止损检查

type Action interface{ actionMarker() }

type PlaceOrder struct {
    ClientOrderID order.ClientOrderID
    Side          order.Side
    Type          order.Type
    Price         decimal.Decimal
    Quantity      decimal.Decimal
    TIF           order.TIF
    ReduceOnly    bool
}
type ModifyOrder struct { // 改已存活挂单的价和量，ClientOrderID 不变
    ClientOrderID order.ClientOrderID
    Price         decimal.Decimal
    Quantity      decimal.Decimal
    // Side/Type/TIF/ReduceOnly 供无改单能力时降级撤+挂
}
type CancelOrder   struct{ ClientOrderID order.ClientOrderID }
type CancelAll     struct{}
type SetLeverage   struct{ Leverage int; Mode market.MarginMode }
type ClosePosition struct{ Urgency Urgency }   // Market / Maker
type Stop          struct{ Reason StopReason }
```

**为什么 `OnEvent` 是单方法而 `OnCommand` 单列**：事件是"外部世界发生了什么"，命令是"用户要求做什么"，两者的错误处理完全不同 —— 事件处理失败要熔断，命令处理失败只需给页面返回错误。分开后 API 层能拿到明确的成功/失败回执。

### 3.3 注册表

`main.go` 显式注册，不用 `init()` 副作用，保持依赖可见：

```go
exchange.Register("lighter", lighter.New)          // slot 0（lighter 包 init）
exchange.Register("rh_lighter", rhlighter.New)    // slot 1，只能追加
strategy.Register("grid", grid.New)
strategy.Register("martingale", martingale.New)
```

交易所在注册表中的顺序即 `exchange_slot`，写死后**不可调整**（会影响历史 `ClientOrderID` 的解码）。新增交易所只能追加到末尾。

---

## 4. 普通网格算法

### 4.1 价位表生成

给定区间 `[L, U]` 与网格数量 `n`，产生 `n+1` 条价格线（`n` 个格子）。

**等差（arithmetic）**

```
step = (U - L) / n
P_i  = L + i × step          i = 0 … n
```

**等比（geometric，第二阶段）**

```
r    = (U / L)^(1/n)
P_i  = L × r^i               i = 0 … n
```

生成后按 `tick_size` 规整到最近可交易价并去重；去重后价位数 < 2 则校验失败（区间太窄或格数太多）。

### 4.2 每格数量：两种输入方式

原型图上用户直接填「每格数量（币）」，保证金是**派生量**。同时保留按保证金反推的方式：

| `sizing_mode` | 用户输入 | 派生 |
| --- | --- | --- |
| `per_grid_qty`（页面默认） | 每格数量 `q`（币） | `名义敞口 = q × n × 现价`，`所需保证金 = 名义敞口 / leverage` |
| `margin` | 总保证金 `M` | `名义总额 = M × leverage`，`每格名义 = 名义总额 / n`，`q_i = 每格名义 / P_i` |

原型图数据校验：80 格 × 0.002 BTC = 0.16 BTC，均价约 63000 → 名义 ≈ 10080 USDC，30x 杠杆 → 约需保证金 336 USDC。与图上「名义敞口 10080 · 约需保证金 336 USDC (30x)」一致。

`per_grid_qty` 下所有格数量相同；`margin` 下按 `qty_mode` 选 `equal_notional`（每格金额相同）或 `equal_qty`（每格数量相同）。

数量按 `lot_size` 向下规整；规整后 `q × P_i < min_notional` 则校验失败并提示最小可行值。

### 4.3 格子模型与配对

网格用**格子（cell）**建模而不是「价格线 + 角色」。`n+1` 条价格线切出 `n` 个格子，每个格子覆盖区间 `[P_i, P_{i+1}]`：

```go
type Cell struct {
    Index int
    Low   decimal.Decimal
    High  decimal.Decimal
    Qty   decimal.Decimal

    Side  order.Side  // 当前挂哪一侧：Buy 挂在 Low，Sell 挂在 High
    State CellState   // Empty | Pending | Resting
    COID  order.ClientOrderID
    Seq   uint8
    Armed bool        // 已完成循环的前半程，下一笔成交即闭合一个网格
}
```

**为什么不用价格线模型**：价格线模型在配对时会撞车。做多网格里买单在 `P_i` 成交后要在 `P_{i+1}` 挂卖单，但 `P_{i+1}` 上可能已经有一笔初始卖单，两者数量还未必相同，需要额外的合并规则。格子模型天然没有这个问题——每个格子独立完成一轮「Low 买入、High 卖出」，任意时刻只持有一笔挂单，而相邻两格不可能同时想在同一条价格线上挂单（一个要 Low 挂买说明价格在它上方，另一个要 High 挂卖说明价格在它下方，矛盾）。

**挂单方向的分配规则对三种方向完全统一**，设当前价为 `P_cur`：

```
格子整体在现价之上（Low >= P_cur） → 挂卖单在 High
否则（含现价所在的那一格）        → 挂买单在 Low
```

**配对规则也完全统一**：买单成交 → 该格翻转为卖，挂在 High；卖单成交 → 翻转为买，挂在 Low。每次翻转 `Seq` 递增，`ClientOrderID` 随之变化。

方向只影响两件事：

| | 初始目标仓位 | Armed 初值 | reduce-only 的腿 |
| --- | --- | --- | --- |
| 做多 | `+Σ Qty`（挂卖的格子）| 挂卖的格子为真 | 卖出腿 |
| 做空 | `−Σ Qty`（挂买的格子）| 挂买的格子为真 | 买入腿 |
| 中性 | `0`（默认，无需建仓）| 全为假 | 无 |

`Armed` 表示格子持有一条已成交的开腿。做多网格中挂卖的格子由初始底仓背书，因此初值为真——它们的第一笔卖出就闭合一个循环。中性网格没有初始仓位，两侧都可能开仓，因此**挂单不能带 reduce-only**，否则开仓单会被直接拒绝。

**中性网格默认不需要建仓**（初始目标仓位为 0），`entry` 配置在 `neutral_base_ratio = 0` 时不生效，页面上该区域置灰并提示。配置 `neutral_base_ratio ∈ (0,1]` 时按挂卖格子总量的该比例建立多头底仓，行为介于中性与做多之间。

每闭合一个循环，毛利按**真实成交均价**配对（不是格子 High−Low）。页面「已实现」：有交易所成交历史时用其净盈亏（已扣费，不再二次扣）；否则用毛利 − 自估手续费（maker 成交按 maker 费率，止损/超时吃单按 taker 费率）。

**挂单数 = 格子数**。80 格网格铺满是 80 笔单，现价所在的那条价格线上没有单——那条线是相邻两格的交界，两边都不需要在它上面挂单。（控制台原型上写的 81 是按价格线数量算的，实际以格子数为准。）

### 4.4 挂单窗口

`max_active_orders`（默认 0 = 全挂）控制同时挂出的订单数，只挂距现价最近的若干格，价格移动时滚动撤远端补近端。远端格子保持 Empty 但仍参与配对计算。这是纯执行层优化，在生成 Action 前做一次窗口过滤即可，不影响算法正确性。

Lighter 的挂单额度足以支撑 80 格全挂，但换到挂单上限低的交易所时这个开关是必需的。

### 4.5 post-only 前置检查

生成挂单意图前先判断该价格会不会立即成交：买单要求 `price < best_ask`，卖单要求 `price > best_bid`（盘口不可用时退化为与标记价比较）。会穿价的格子直接跳过，等下一个 `TickEvent` 用新价格再试。

这一步不能省。价格快速穿过若干格时，被穿过的格子会翻转成「在现价错误一侧挂单」的状态，不做检查就会连续发出必被拒绝的 post-only 单，白白消耗请求配额并把连续失败计数推向熔断阈值。

### 4.6 数值精度

全链路 `shopspring/decimal`，**禁止在价格/数量计算中使用 float64**。网格价位涉及大量除法与累加，float 误差会导致规整后价位重复或数量对不齐。适配器在最后一步转成交易所要求的定点整数。

---

## 5. 建仓触发器

`app/entry` 输入"目标仓位增量"，输出订单意图，完成后通知策略进入正常网格循环。

```
                ┌───────────────────────────────────────────┐
                ▼                                           │
   Idle ──▶ Placing ──▶ Resting ──(book 变化超阈值)──▶ Repricing
                │           │                               │
                │           ├──(成交)──▶ Done               │
                │           └──(超时)──▶ Fallback ───────────┘
                └──(拒绝)──▶ Retry / Failed
```

| 模式 | 行为 | 关键参数 |
| --- | --- | --- |
| `maker_follow`（默认；旧名 `market` 同样走这条） | 做多挂买一价、做空挂卖一价，全程 post-only。最优价偏离超过 `reprice_ticks` 个 tick 时撤单重挂 | `depth_level`、`reprice_ticks`、`reprice_interval`、`max_reprice`、`timeout`、`on_timeout` |
| `limit_price` | 在指定价挂 post-only 单等待，不跟价 | `price`、`timeout`、`on_timeout` |

**共同约束**

- 部分成交是常态：持续跟踪 `filled_qty`，只对剩余量重挂，直到 `filled_qty >= target × (1 − fill_tolerance)`。
- `on_timeout` 可选 `market`（**市价 IOC 吃掉剩余量**，滑点保护价）、`keep`（继续等）、`abort`（放弃并停止）。这是建仓路径里唯一的 taker。
- Executor 默认把限价单强制成 `Limit + PostOnly`；仅 `Type=Market` / `TIF=IOC`（止损、建仓超时）放行吃单。
- `max_reprice` 防止剧烈行情中无限跟价烧手续费与请求配额。
- **建仓期间不铺网格**。建仓完成后策略才生成铺网格意图，避免仓位未到位就挂 reduce-only 平仓单被拒。页面此阶段显示「建仓中」。
- `EnsurePosition` 的目标是带符号的绝对仓位。Runner 在启动建仓前必须 `syncEpoch` 并重新查询交易所仓位，禁止用过期快照算差额。
- 差额是**减仓**（当前仓与目标同向且更大）时，建仓单必须 `ReduceOnly`，防止快照偏大时误开反向仓。

---

## 6. 事件流与实例运行时

### 6.1 Runner 主循环

```go
// internal/app/engine/runner.go
func (r *Runner) Run(ctx context.Context) error {
    ticker := time.NewTicker(r.tickInterval)   // 默认 1s
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return r.shutdown(context.WithoutCancel(ctx), strategy.StopShutdown)

        case cmd := <-r.commands:              // 页面命令，优先处理
            cmd.Reply <- r.handleCommand(ctx, cmd)

        case se, ok := <-r.stream:
            if !ok {
                r.setStatus(StatusReconnecting)
                continue                        // 重连由后台 goroutine 驱动，成功后重新对账
            }
            r.dispatch(ctx, r.translate(se))

        case t := <-ticker.C:
            r.dispatch(ctx, strategy.TickEvent{Now: t})
        }
    }
}

func (r *Runner) dispatch(ctx context.Context, ev strategy.Event) {
    if r.status != StatusRunning {
        r.updateViewOnly(ev)                    // 未运行时只更新页面展示数据，不产生交易意图
        return
    }
    // 风控先于策略，保证止损不被策略逻辑阻塞
    if acts, stop := r.guard.Check(ev); stop {
        r.executor.Apply(ctx, acts)
        r.stopInstance(ctx, r.guard.Reason())
        return
    }
    acts, err := r.strategy.OnEvent(ev)
    if err != nil {
        r.fail(ctx, err)
        return
    }
    for _, e := range r.executor.Apply(ctx, acts).Events() {
        r.enqueue(e)                            // 下单结果作为 OrderEvent 回灌
    }
    r.publish()                                 // 向 WebSocket 广播最新视图
    r.persistIfDirty()
}
```

**关键点**

- **顺序执行，不开子 goroutine**：Executor 内部可以并发发请求，但对策略而言 `Apply` 是同步的，返回后状态一致。消除绝大部分并发 bug。
- **命令与事件同一循环**：页面并发点按钮不会破坏状态一致性。
- **风控前置**：止损永远优先于策略逻辑。
- **结果回灌**：下单成功/失败作为 `OrderEvent` 塞回事件队列，策略只有一个状态更新入口。
- **epoch 必须跟策略走**：策略在启动、调区间、trailing、马丁止盈重开时会 `epoch++`。`Apply` 与看门狗执行前要 `syncEpoch`，否则看门狗会把新轮次挂单当孤儿单撤掉，策略再挂，形成挂撤循环。
- **未运行也在跑循环**：策略停止时 Runner 不退出，继续接收行情用于页面展示（最新价、账户余额），只是不产生交易意图。这样页面在未启动状态下也有实时数据。

### 6.2 Executor

```go
func (e *Executor) Apply(ctx context.Context, acts []strategy.Action) Results
```

1. **归类合并**：`PlaceOrder` / `ModifyOrder` / `CancelOrder` 各自成批。
2. **顺序保证**：设杠杆 → 撤单 → **改单** → 平仓 → 下单。改单在撤单之后、新挂之前，避免改一张马上要撤的单。
3. **批量拆分**：按 `Capabilities.BatchPlace` 切片；改单目前按 1 笔发。
4. **限流**：per-exchange 令牌桶。
5. **重试**：只对可重试错误退避重试，带上限。改单失败**不**发 `Rejected` 回灌，旧单留在盘上。
6. **进度计数**：维护「挂单目标 / 已确认 / 待重试」三个计数供页面显示（80 格网格铺满即 80 / 80 / 0）。
7. **dry-run**：`-dry-run` 时换成 `LogExecutor`，只打印不发送。

### 6.3 Supervisor

每个 enabled 的交易所启动一个 Runner goroutine，互相隔离：一个 panic 或熔断不影响其他。因为一个交易所只有一个实例，Runner 与 Exchange 适配器一一对应，不存在共享争用。

```
Supervisor
 ├── Runner(lighter)  ──▶ lighter.Adapter
 └── Runner(<后续 DEX>) ──▶ <后续 DEX>.Adapter
```

---

## 7. 命令通道与运行时操作

### 7.1 命令模型

```go
type Command struct {
    Kind    CommandKind
    Payload any
    Reply   chan CommandResult      // 容量 1，避免发送方阻塞
}

type CommandKind int
const (
    CmdStart CommandKind = iota  // 启动网格
    CmdStop                      // 停止 + 撤本交易对挂单，保留仓位
    CmdAdjustRange               // 调整区间（不停止网格）
    CmdCancelOrders              // 撤销所有挂单（保留持仓）
    CmdRefill                    // 补齐网格挂单（一键补格）
    CmdResetStats                // 重置统计
    CmdReconnect                 // 重连交易所
    CmdSaveConfig                // 保存策略配置（未运行时）
)

type CommandResult struct {
    OK      bool
    Message string
    View    strategy.View        // 执行后的最新视图，直接回给页面
}
```

API handler 投递命令后阻塞等待 `Reply`，默认超时 10s（`CmdStop` 因涉及平仓放宽到 30s），超时返回 504 并在日志标记该命令仍在执行中。

### 7.2 各命令语义

| 命令 | 前置条件 | 行为 | 失败处理 |
| --- | --- | --- | --- |
| `Start` | 状态为 Stopped，配置校验通过，余额充足 | 设杠杆 → 对账 → 建仓 → 铺网格 → 转 Running | 任一步失败则回滚到 Stopped 并撤销已挂出的单 |
| `Stop` | Running / Paused | 撤销全部挂单 → **保留仓位** → 落盘终态 → 转 `Stopped(Manual)` | — |
| `AdjustRange` | Running | 见 7.3 | 失败则保持旧区间不变，返回错误 |
| `CancelOrders` | Running / Paused(OutOfRange) | 撤销本实例全部挂单，**仓位不动**，层级全部置 Empty，转 `Paused(Manual)` | 部分失败则返回未撤成功的层级列表 |
| `Refill` | Running / Paused(Manual) | 对账 → 计算缺失层级 → 补挂；从 Paused(Manual) 调用时同时转回 Running | 部分失败计入待重试计数，下个 tick 继续补 |
| `ResetStats` | 任意 | 清零已实现盈亏、成交计数、完成格数；**不动挂单与持仓** | — |
| `Reconnect` | 任意 | 关闭并重建 WS/REST 连接 → 重新订阅 → 全量对账；**不动挂单与持仓** | 重连失败保持原状态并告警 |
| `SaveConfig` | 状态为 Stopped | 校验 → 写 SQLite → 返回派生量 | 校验失败返回逐字段错误 |

**`Paused` 状态**由「撤销所有挂单（保留持仓）」与「区间外暂停」两个场景引入：策略层级表仍在、仓位保留、不产生新的交易意图。它让用户可以在行情剧烈时先撤单避险，冷静后一键补格恢复，而不必走「停止 + 平仓 + 重新配置 + 重新建仓」这一整套。完整状态机见 10.3。

### 7.3 调整区间（不停止网格）

目标是**不平仓**地迁移到新区间。执行序列：

```
1. 用新参数跑一遍 Preview 做完整校验，失败则整体拒绝、保持旧区间不变
2. 撤销本实例全部挂单
3. epoch + 1，按新参数重建格子
4. 计算新的目标仓位与当前实际仓位的差额
     |diff| <= tolerance  → 不动
     |diff| >  tolerance  → 交给建仓触发器补齐或减仓
5. 按新网格铺单
```

**为什么是全撤重铺而不是只动边缘几格**：`ClientOrderID` 里编码了轮次与格子索引。区间变化后格子索引会整体平移，被保留的旧单再解码就会指向错误的格子，对账时会做出错误判断。要支持增量迁移就得额外持久化一张 `COID → 格子` 的映射表，而这张表一旦与 `ClientOrderID` 的自描述语义并存，重启恢复的逻辑就多了一条容易出错的分支。

代价是可控的：80 格网格全撤重铺，用批量接口是 4 次撤单请求 + 4 次下单请求，在 10 rps 的限流下不到一秒。用一点请求量换掉一整类对账歧义，这笔交易划算。

**Trailing 网格复用同一条路径**，因此价格每突破一次边界也是一次全量重铺，`trailing_cooldown`（默认 30s）就是用来限制这个频率的。

页面上这个操作要求二次确认，并预览新区间的派生量与需要调仓的数量。

---

## 8. HTTP API 契约

单端口同时服务静态资源、REST 与 WebSocket。所有 REST 响应统一信封：

```json
{ "ok": true, "data": {...} }
{ "ok": false, "error": { "code": "INVALID_RANGE", "message": "上边界必须大于下边界", "field": "upper_price" } }
```

### 8.1 端点

| 方法 | 路径 | 说明 | 对应 UI |
| --- | --- | --- | --- |
| GET | `/api/system/status` | 各交易所连接状态、代理状态、版本、运行时长 | 顶部状态徽章 |
| GET | `/api/exchanges` | 已启用交易所列表与能力 | Tab 列表 |
| GET | `/api/exchanges/{ex}/symbols` | 可交易对列表 | 交易对下拉 |
| GET | `/api/exchanges/{ex}/klines?symbol=&interval=&limit=` | K 线（默认 1h） | 价格/网格曲线 |
| GET | `/api/exchanges/{ex}/analysis?symbol=&interval=` | 趋势分析结果 | 趋势分析卡片 |
| POST | `/api/exchanges/{ex}/suggest` | 按风格预设生成推荐参数与自动区间 | 「智能填充参数」「采用推荐策略 + 自动区间」 |
| GET | `/api/exchanges/{ex}/config` | 当前策略配置 | 策略配置表单回填 |
| POST | `/api/exchanges/{ex}/preview` | 只校验并计算派生量，不保存 | 表单下方实时派生量 |
| PUT | `/api/exchanges/{ex}/config` | 保存策略配置（`CmdSaveConfig`） | 表单保存 |
| GET | `/api/exchanges/{ex}/status` | 运行状态、持仓、盈亏、挂单进度 | 账户状态卡片 |
| GET | `/api/exchanges/{ex}/levels` | 网格层级表（价格/数量/角色/状态） | 图表网格线 |
| GET | `/api/exchanges/{ex}/trades?limit=` | 成交记录 | 成交记录表 |
| GET | `/api/exchanges/{ex}/logs?limit=&level=` | 运行日志 | 日志面板 |
| POST | `/api/exchanges/{ex}/start` | 启动网格 | 「启动网格」 |
| POST | `/api/exchanges/{ex}/stop` | 停止 + 撤本交易对挂单，保留仓位 | 「停止策略」 |
| POST | `/api/exchanges/{ex}/adjust-range` | 调整区间 | 「调整区间（不停止网格）」 |
| POST | `/api/exchanges/{ex}/cancel-orders` | 撤单保留持仓 | 「撤销所有挂单（保留持仓）」 |
| POST | `/api/exchanges/{ex}/refill` | 补齐挂单 | 「补齐网格挂单（一键补格）」 |
| POST | `/api/exchanges/{ex}/reset-stats` | 重置统计 | 「重置统计」 |
| POST | `/api/exchanges/{ex}/reconnect` | 重连交易所 | 「重连交易所」 |
| GET | `/api/proxy` / PUT | 代理配置与连通性探测 | 「IP 配置」Tab |
| GET | `/healthz` | 健康检查 | — |
| GET | `/metrics` | Prometheus | — |
| WS | `/api/stream` | 实时推送 | 全页面 |

### 8.2 WebSocket 推送

客户端连接后按交易所订阅，服务端推送增量：

```json
{ "type": "status",  "exchange": "lighter", "data": { ... } }
{ "type": "ticker",  "exchange": "lighter", "data": { "price": "62942.7", "ts": 1234567890 } }
{ "type": "trade",   "exchange": "lighter", "data": { "ts": ..., "side": "buy", "price": "62925", "qty": "0.002" } }
{ "type": "levels",  "exchange": "lighter", "data": [ ... ] }
{ "type": "log",     "exchange": "lighter", "data": { "level": "warn", "ts": ..., "msg": "..." } }
```

推送做**合流限频**：`status` 与 `ticker` 最多 2 次/秒（按最新值覆盖），`trade` 与 `log` 逐条推但带背压丢弃（客户端消费不过来时丢最旧的，并发一条「日志已截断」提示）。避免行情剧烈时 WS 缓冲爆掉。

### 8.3 鉴权

默认监听 `0.0.0.0:8080`，无鉴权，可直接公网访问 REST API。`config.yaml` 中开启 `server.auth.enabled` 后启用 Bearer Token（token 从环境变量读），所有 `/api/*` 校验。鉴权与反向代理均为可选项，不是启动前提。

---

## 9. 订单标识与幂等

Lighter 的 `ClientOrderIndex` 是 48 位正整数（`[1, 2^48−1]`），按位划分：

| 位段 | 宽度 | 含义 |
| --- | --- | --- |
| 47..40 | 8 | `exchange_slot`：交易所在注册表中的固定索引 |
| 39..24 | 16 | `epoch`：网格轮次，每次启动/调整区间/trailing 移动/马丁止盈重开递增 |
| 23..12 | 12 | `level`：网格层级索引 0-4095 |
| 11..4 | 8 | `purpose`：1=开仓腿 2=平仓腿 3=建仓 4=止盈 5=止损 6=平仓 |
| 3..0 | 4 | `seq`：同层级重挂序号，模 16 循环 |

```go
// internal/domain/order/coid.go
type ClientOrderID uint64

func Encode(slot uint8, epoch uint16, level uint16, purpose Purpose, seq uint8) ClientOrderID
func (c ClientOrderID) Decode() (slot uint8, epoch, level uint16, purpose Purpose, seq uint8)
```

相比多实例方案，省下的 8 位实例槽位分给了 `epoch`（12 → 16 位）。`epoch` 需要更大空间是因为「调整区间」会频繁递增它，16 位（65535 次）足够长期运行。

带来的能力：

- **归属判定**：对账时看到任意挂单，解码即知轮次与层级。非当前 epoch 的直接撤销。
- **重放安全**：同一层级同一 seq 重复下单会被交易所以「重复 client order index」拒绝，这正是期望行为。
- **无需映射表**：不用维护 `exchangeOrderID → level` 的内存 map，重启不会丢。

`seq` 只有 4 位（0-15 循环）是可接受的：同一层级在同一 epoch 内重挂超过 16 次的概率极低，且循环回绕时旧单早已终结。

对于 `ClientOrderID` 位宽不足的其他交易所，适配器降级为字符串前缀 `dg-<slot>-<epoch>-<level>-<purpose>-<seq>`，语义一致。

---

## 10. 风控、止盈止损与区间外策略

### 10.1 Guard 检查顺序

| 优先级 | 检查项 | 触发动作 |
| --- | --- | --- |
| 1 | 止损价 | `CancelAll` + `ClosePosition{Market}` + `Stop{StopLoss}`（市价 IOC，避免对手盘不够） |
| 2 | 止盈价 | `CancelAll` + `EnsurePosition{0}`（maker 跟价收到 0 后再停止） |
| 3 | 区间外策略（`pause` / `stop_and_cancel`） | 见 10.2 |
| 4 | 最大持仓名义 | 暂停开仓腿，只留平仓腿，告警 |
| 5 | 保证金率低于阈值 | 同上 + 告警 |
| 6 | 连续下单失败达阈值 | `CancelAll` + `Stop{Circuit}`，**不平仓**，等人工判断 |
| 7 | 事件流静默超时 | 触发重连；重连失败达阈值则熔断 |

**触发价格源**默认用标记价（mark price）而非最新成交价，避免被瞬时插针触发。交易所不提供标记价时退化为 `(bid+ask)/2` 并在启动日志明确提示。

**为什么默认本地触发而非交易所原生条件单**：跨交易所行为一致、不占挂单额度、能保证「先撤全部网格单再平仓」的顺序（原生条件单做不到）。可切换到原生条件单，代价是需要额外维护条件单与网格单的一致性。

**止损后状态**：进入 `Stopped(StopLoss)` 终态并落盘，**不自动重启**。防止单边行情中反复止损。页面显示醒目的停止原因。

### 10.2 区间外策略

价格跌破下边界（做多）或突破上边界（做空）后的行为，两选一：

| 值 | 行为 | 结束状态 |
| --- | --- | --- |
| `pause`（默认） | 不撤单、不平仓、不下新单，挂起等待价格回归 | `Paused(OutOfRange)` → 回归后自动转 `Running` |
| `stop_and_cancel` | 撤销本实例全部挂单，**仓位原样保留交给用户处理**，不再自动恢复 | `Stopped(OutOfRange)`，持仓保留 |

**出界与回归判定（带滞后，防边界抖动）**

```
出界：price < lower − exit_buffer_ticks × tick   或   price > upper + exit_buffer_ticks × tick
回归：price ∈ [lower, upper] 且持续 resume_confirm 时长（默认 5s）
```

只做出界的缓冲而不做回归的价格缓冲，是因为回归用「持续时长」确认已经足够抑制抖动，再加价格缓冲会让恢复变得迟钝。

**`pause` 的实现要点**

- 已有挂单**全部保留**。做多网格跌破下沿时买单本就已成交完毕，剩下的都是上方的平仓卖单，保留它们意味着反弹时能正常止盈 —— 这正是「不做任何处理」比主动干预更优的场景。
- 挂起期间 Runner 仍在跑，但 `dispatch` 走 `updateViewOnly` 分支：只更新页面展示数据，不调用 `strategy.OnEvent` 产生交易意图。trailing 与补格也一并挂起。
- 恢复前**先做一次对账**再转 Running。暂停可能持续很久，期间交易所侧可能发生本地未感知的变化（挂单过期、被交易所清理）。
- 页面显示「暂停（价格超出区间）」并标出当前价距最近边界的距离。

**`stop_and_cancel` 的实现要点**

- 撤单后**不平仓**。这是有意为之：脱离区间往往是最差的平仓时机，把决定权交给用户比程序自作主张更好。
- 转入 `Stopped(OutOfRange)` 终态，页面醒目展示持仓、均价、浮亏、强平价，并给出「市价平仓」与「重新配置区间后启动」两个入口。
- 不自动恢复，即使价格随后回到区间内。

**与止损价的关系**：两者独立，可同时配置。止损价优先级更高（检查顺序 1、2 在区间外策略之前），命中止损直接平仓停止，不再走区间外逻辑。

### 10.3 实例状态机

```
                    ┌─────────────────────────────────────────┐
                    │                                         │
   Stopped ──Start──▶ Running ──价格出区间(pause)──▶ Paused(OutOfRange)
      ▲                 │  ▲                              │
      │                 │  └──────价格回归 + 对账───────────┘
      │                 │
      │                 ├──手动「撤销所有挂单」──▶ Paused(Manual)
      │                 │                              │
      │                 │      ◀───「补齐网格挂单」──────┘
      │                 │
      │                 ├──价格出区间(stop_and_cancel)──▶ Stopped(OutOfRange)  [撤单，保留仓位]
      │                 ├──止盈/止损命中──▶ Stopped(TakeProfit/StopLoss)  [已平仓]
      │                 ├──连续失败熔断──▶ Stopped(Circuit)  [撤单，保留仓位]
      │                 └──手动停止 / 关进程──▶ Stopped(Manual/Shutdown)  [撤本交易对挂单，保留仓位]
      │                                                    │
      └────────────────────────────────────────────────────┘
```

两种 `Paused` 的共同点是保留仓位、不产生新的交易意图；区别在挂单与恢复方式：

| | 挂单 | 恢复方式 |
| --- | --- | --- |
| `Paused(OutOfRange)` | 保留 | 价格回归 + 对账后自动转 Running |
| `Paused(Manual)` | 已撤销 | 用户点「补齐网格挂单」 |

因此 `Paused` 必须携带 reason，页面上分别展示不同的提示与操作入口。

`Stopped` 同样携带 reason，且**默认不保证已平仓**：`StopLoss` 市价吃单平仓；`TakeProfit` maker 跟价平仓。`Manual`、`Shutdown`、`OutOfRange`、`Circuit`、`Error`、`EntryFailed` 都只撤销**当前策略交易对**的挂单并保留仓位。页面在有残留仓位时必须醒目提示，不能让用户以为停止了就等于空仓。

---

## 11. Trailing 网格

`trailing_up` / `trailing_down`（默认 false）。

**上移触发**：最新价 > `upper + trailing_trigger_ticks × tick_size`

**上移动作**（等差）：

```
shift  = trailing_step_grids × step
lower' = lower + shift
upper' = upper + shift
epoch' = epoch + 1
```

执行序列复用「调整区间」的路径（见 7.3）：全撤 → epoch+1 → 重建格子 → 调仓 → 重铺。把 trailing 和手动调区间统一到一套代码，是避免两处逻辑各自漂移的关键。

等比网格用乘法：`lower' = lower × r^k`。

**约束**

- `trailing_max_shifts`（默认 0 = 不限）限制总移动次数。
- `trailing_cooldown`（默认 30s）避免震荡中反复触发。
- 做多网格开 `trailing_up` 会持续追高，见顶回落时亏损放大；做空开 `trailing_down` 同理。开启 trailing 但未设止损时，保存配置返回一条 warn 级提示（不阻止保存，页面上黄色高亮）。

---

## 12. 行情分析与参数推荐

`app/analysis` 是纯计算模块，输入 K 线，输出建议，**不产生任何交易行为**。

### 12.1 指标

| 指标 | 计算 | 用途 |
| --- | --- | --- |
| EMA 差 | `(EMA_fast − EMA_slow) / EMA_slow`，默认 fast=12 slow=26 | 趋势方向与强度 |
| 斜率 | 对最近 `N` 根收盘价做最小二乘线性回归，归一化为「每根 K 线百分比变化」 | 趋势陡峭度 |
| ATR% | `ATR(14) / 最新价` | 波动率，决定网格间距下限 |
| 区间高低 | 最近 `N` 根的最高价与最低价 | 自动区间的基准 |

原型图上「EMA 差 −0.28%，斜率 −0.031%/根，波动率 ATR 0.33%」就是这三个值。

### 12.2 趋势判定

```
|EMA差| < ema_threshold  且  |斜率| < slope_threshold   →  震荡  → 推荐中性网格
 EMA差 > ema_threshold   且   斜率 > slope_threshold    →  上涨  → 推荐做多网格
 EMA差 < −ema_threshold  且   斜率 < −slope_threshold   →  下跌  → 推荐做空网格
其余                                                    →  转换中 → 推荐中性网格 + 降低强度
```

强度 = 三个指标归一化后的加权得分，映射到 0-100%。原型图显示「震荡 ↔ 推荐：中性网格 · 强度 7%」，低强度意味着信号弱，页面应提示谨慎。

### 12.3 参数推荐与自动区间

| 风格 | 网格数 | 区间宽度 | 杠杆建议 |
| --- | --- | --- | --- |
| `stable`（稳健） | 少（格宽，成交少但每格利润厚） | ±2 × ATR% × √N | 低 |
| `aggressive`（激进） | 多（格密，成交频繁） | ±1 × ATR% × √N | 中 |
| `safe`（成交少更安全） | 最少 | ±3 × ATR% × √N | 最低 |

**手续费硬约束**（原型图上「建议单格间距不小于该值的一半以覆盖手续费」的严谨版本）：

```
单格毛利率 = step / P ≈ (upper − lower) / (grid_count × P)
必要条件：单格毛利率 > (maker_fee_rate × 2) × safety_mult      safety_mult 默认 2.0
```

不满足时 `suggest` 会自动收敛格数，`preview` 会返回明确错误：「当前格数下单格毛利率 0.08% 低于双边手续费 0.04% 的 2 倍，建议格数不超过 XX」。这是网格策略最常见的亏损原因，必须在保存配置时就拦住。

自动区间同时受账户余额约束：推荐的每格数量必须满足 `所需保证金 ≤ 可用余额 × max_margin_usage`（默认 0.5）。

---

## 13. Lighter 适配器

协议、签名、nonce、改单、事件流与主网踩坑已独立成篇：[LIGHTER.md](LIGHTER.md)。

这里只保留与系统抽象相关的要点：

- 适配器只做端口翻译，不含网格 / 马丁语义。
- `Capabilities.ModifyOrder = true`。马丁加仓后改止盈走 `ModifyOrders`，数量与仓位一致，不撤旧单重挂。
- `CancelAll(symbol)` 按市场逐笔撤，禁止调用账户级 `CancelAllOrders`。
- nonce 靠单实例 + 发送锁保证递增；文件锁防止双开进程。

---

## 14. 持久化与对账

### 14.1 存储

SQLite（`modernc.org/sqlite`，纯 Go 无 CGO，Windows 编译友好），单文件 `data/gridbot.db`。

```sql
-- 策略配置：页面下发，每个交易所一行
CREATE TABLE strategy_configs (
    exchange    TEXT PRIMARY KEY,
    symbol      TEXT NOT NULL,
    strategy    TEXT NOT NULL,        -- grid | martingale
    direction   TEXT NOT NULL,        -- long | short | neutral
    params      TEXT NOT NULL,        -- JSON，策略专有参数
    updated_at  INTEGER NOT NULL
);

-- 运行状态：每个交易所一行
CREATE TABLE runtime_state (
    exchange    TEXT PRIMARY KEY,
    status      TEXT NOT NULL,        -- stopped | running | paused | error
    stop_reason TEXT,
    epoch       INTEGER NOT NULL,
    snapshot    BLOB,                 -- strategy.Snapshot() 的 JSON
    updated_at  INTEGER NOT NULL
);

CREATE TABLE orders (
    exchange     TEXT NOT NULL,
    coid         INTEGER NOT NULL,
    exchange_oid TEXT,
    epoch        INTEGER NOT NULL,
    level        INTEGER NOT NULL,
    side         TEXT NOT NULL,
    price        TEXT NOT NULL,       -- decimal 存字符串，避免精度损失
    qty          TEXT NOT NULL,
    filled_qty   TEXT NOT NULL,
    state        TEXT NOT NULL,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (exchange, coid)
);

CREATE TABLE fills (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    exchange   TEXT NOT NULL,
    coid       INTEGER NOT NULL,
    side       TEXT NOT NULL,
    price      TEXT NOT NULL,
    qty        TEXT NOT NULL,
    fee        TEXT NOT NULL,
    is_maker   INTEGER NOT NULL,
    ts         INTEGER NOT NULL
);
CREATE INDEX idx_fills_ex_ts ON fills(exchange, ts DESC);

-- 统计：支持「重置统计」而不丢历史成交
CREATE TABLE stats (
    exchange       TEXT PRIMARY KEY,
    reset_at       INTEGER NOT NULL,  -- 重置时间点，统计只算此后的 fills
    realized_pnl   TEXT NOT NULL,
    completed_grids INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);
```

「重置统计」实现为把 `stats.reset_at` 更新为当前时间并清零累计值，`fills` 表原样保留 —— 用户清零的是显示，不是审计记录。

写入时机：每次 `Apply` 后状态有变更则批量写一个事务。不追求每笔实时落盘，崩溃丢失部分由对账补回。

### 14.2 启动恢复

```
1. 加载 config.yaml，建立各交易所连接
2. 对每个交易所读 strategy_configs + runtime_state
3. status == running：
     执行对账（见 14.3）→ 恢复 Running
     若当前价已在区间外 → 按 out_of_range 策略处理，而不是直接进 Running
     对账失败 → 转 error 状态，页面显示原因，等人工处理
4. status == paused(out_of_range)：
     对账 → 挂单应仍存在；价格若已回归则转 Running，否则维持 Paused
5. status == paused(manual)：
     对账 → 维持 Paused，等用户点「补齐网格挂单」
6. status == stopped（含 out_of_range / circuit 等带仓位的终态）：
     Runner 空转，只推送行情与账户数据；若有残留仓位，页面醒目提示
```

**进程重启后自动恢复运行**是必需的（服务化部署会因为升级、崩溃、机器重启而重启进程），但有两条底线：**对账失败时绝不盲目恢复**，以及**恢复前必须重新判定价格是否在区间内** —— 进程停了几小时后价格早已跑出区间的情况很常见，直接进 Running 会立刻触发一堆异常挂单。

### 14.3 对账算法

启动时以及每次事件流重连后执行：

```
1. exchangeOrders ← Exchange.OpenOrders(symbol)
2. exchangePos    ← Exchange.Position(symbol)
3. localState     ← store 读取 + strategy.Restore

4. 遍历 exchangeOrders，解码 ClientOrderID：
     ├ slot 不是本交易所 / 解码失败 → 忽略（手动单，不动）
     ├ epoch 旧于当前              → 加入待撤列表
     └ epoch 为当前                → 标记该层级 Resting，写回本地

5. 遍历 localState 中标记 Resting 但交易所没有的：
     查该 coid 的历史（fills / 订单终态）
     ├ 已成交 → 补记成交，触发策略生成对手单
     ├ 已撤销 → 层级置 Empty，等待重挂
     └ 查不到 → 层级置 Empty，告警（保守处理）

6. 比较实际仓位与策略期望仓位：
     drift = |actual − expected| / max(|expected|, minDenominator)
     ├ drift <= position_tolerance（默认 1%）→ 接受实际值
     └ drift >  position_tolerance
         ├ auto_fix = true  → 生成市价补齐/减仓意图
         └ auto_fix = false → 转 error 状态，页面展示对比明细，等人工确认（默认）

7. 执行待撤列表 → 执行补单意图 → 进入正常循环
```

**默认保守**：仓位漂移超阈值时默认停在 error 状态而非自动市价纠偏，因为误判下的自动纠偏会造成真实亏损。页面提供「确认并自动纠偏」按钮把决定权交给用户。

周期性对账现已接到运行循环（`app.reconcile_interval`，默认 15s）：对照交易所真实挂单，**本实例多余的撤掉、缺失的格子补挂**。仓位漂移超阈值仍默认不自动市价纠偏。解不出本系统 COID 的手工单不动。

---

## 15. 错误处理与限流

```go
type ErrClass int
const (
    ErrRetryable ErrClass = iota  // 网络超时、5xx、限流、nonce 冲突
    ErrPostOnlyRejected            // post-only 会立即成交被拒
    ErrInsufficientMargin
    ErrInvalidParam                // 价格/数量非法、市场不存在
    ErrFatal                       // 签名失败、认证失败
)
```

| 分类 | 处理 |
| --- | --- |
| `ErrRetryable` | 指数退避重试，最多 `max_retries`（默认 3）；nonce 冲突先校准再重试 |
| `ErrPostOnlyRejected` | **不算失败**。说明价格已穿过该层级，等下个 `TickEvent` 用新价重挂，`seq` 递增。计入「待重试」计数供页面显示 |
| `ErrInsufficientMargin` | 暂停开仓腿，保留平仓腿，告警；保证金恢复后自动解除 |
| `ErrInvalidParam` | 记录并跳过该笔，计入连续失败计数 |
| `ErrFatal` | 立即熔断实例 |

连续失败达 `max_consecutive_errors`（默认 10）触发熔断：撤销本交易对挂单，实例停止，**不平仓**，页面红色告警等待人工介入。

原型图日志里的「批量下单失败（10 笔）：RHC 接口限流；程序将按实际吸退流退避，并在重新校准后重试」正是 `ErrRetryable` 的典型表现，说明限流退避必须做扎实。

**限流**：每个交易所一个 `golang.org/x/time/rate` 令牌桶（`rate_limit.rps` / `burst`），所有出站请求过桶，WS 发送交易同样过桶。批量接口按「一次请求」计费，因此批量在限流下天然更优 —— 铺 80 格用 4 次批量请求而非 80 次单笔请求，是能否快速铺满网格的关键。

---

## 16. 可观测性与日志面板

**日志**：`log/slog`，JSON 输出到文件/stdout。同时写入一个**内存环形缓冲**（默认 2000 条），供页面日志面板读取与 WS 推送。每条日志强制带 `exchange` 字段（Runner 构造时通过 `logger.With` 注入）。

环形缓冲的存在是为了让页面不依赖读文件，跨平台行为一致（Windows 上读正在写入的日志文件容易踩锁）。

**指标**（Prometheus，`/metrics`）：

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `grid_open_orders` | Gauge | 当前挂单数（按 exchange/side） |
| `grid_order_progress` | Gauge | 挂单目标 / 已确认 / 待重试（按 exchange/kind） |
| `grid_fills_total` | Counter | 成交笔数（按 exchange/side/maker） |
| `grid_completed_grids` | Counter | 完成的网格套利循环数 |
| `grid_realized_pnl` / `grid_unrealized_pnl` | Gauge | 盈亏 |
| `grid_position_size` | Gauge | 当前仓位（带符号） |
| `grid_reconcile_drift` | Gauge | 对账仓位偏差比例 |
| `exchange_requests_total` | Counter | 请求数（按 exchange/method/result） |
| `exchange_request_duration` | Histogram | 请求耗时 |
| `instance_status` | Gauge | 1=running 0=stopped/paused −1=error |

**健康检查** `/healthz`：任一实例处于 error 返回 503。

---

## 17. 跨平台工程约定

| 项 | 约定 |
| --- | --- |
| CGO | **必须 `CGO_ENABLED=0`**。SQLite 用 `modernc.org/sqlite`，不用 `mattn/go-sqlite3` |
| 路径 | 一律 `filepath.Join`；数据目录同时支持相对与绝对路径，相对路径基于可执行文件所在目录而非工作目录（Windows 服务的工作目录常常不是安装目录） |
| 前端资源 | `web/` 静态页由 `go:embed` 打进二进制；`GET /` 与 `/css/` `/js/` 同源托管，`/api/*` 仍走 REST |
| 换行 | `.gitattributes` 统一 LF；`*.ps1`、`*.bat` 标记为 CRLF |
| 信号 | `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`；`syscall.SIGTERM` 在 Windows 上也有定义，同一份代码即可 |
| 文件锁 | 用 `data/gridbot.lock`（`O_CREATE\|O_EXCL`）防止同一数据目录被两个进程打开 —— 这会导致 nonce 冲突和重复下单 |
| 时区 | 内部一律 UTC |
| 构建 | `scripts/build.ps1` 与 `scripts/build.sh` 输出一致的产物命名 |

**文件锁是必须的**。用户很容易在 Windows 上双击两次 exe，两个进程用同一份配置连同一个 Lighter 账户，nonce 会立刻错乱、订单会重复。

---

## 18. 马丁网格

已实现。复用 `Strategy` 接口、`entry` 建仓触发器、`Executor`、`Guard`、对账；`ClientOrderID` 的 `Cell` 位段表示「第几次加仓」，止盈用 `PurposeTakeProfit`。

方向仅 `long` / `short`（无中性）。保证金默认全仓。止盈后重开默认无限循环（`max_cycles = 0`）。

核心状态机（做多；做空把跌/涨对调）：

```
建仓（首单，保证金 = initial_margin）
  ├─ 价格达到 均价 × (1 + take_profit_pct) → reduce-only 止盈全平 → 周期结束 → 重新建仓
  └─ 价格跌到 基准价 × (1 − add_drop_pct) → 加仓
        本次保证金 = add_margin × add_multiplier^(已加仓次数)
        仓位增加、均价下移
        Modify 止盈单：新价按新均价计算，数量 = 当前仓位绝对值
        已加仓次数 == max_add_times → 停止加仓，只留止盈单
```

要点：

- 加仓单按计划价 post-only 预挂，成交即加仓，避免轮询滞后。
- **加仓后改止盈，不撤旧单重挂。** 有 `ModifyOrder` 时 `ModifyOrders`；没有则 Executor 降级撤+挂。改单失败保留旧止盈，禁止当成拒单清掉（否则双挂）。
- 启动时若已有同向仓且数量 ≥ 首单，不反向平，直接挂加仓与止盈；反向仓拒绝启动，要求先平。
- **止盈后重开**：`epoch++` → 全撤 → maker 限价平掉残留 → 按新均价重建计划 → `EnsurePosition(首单量)`。Runner 必须立刻同步 epoch，并按交易所最新仓位建仓；不得用止盈前的仓位快照去「减仓」，否则做多下一轮会先挂一笔空单，随后加仓单被看门狗当旧轮次循环撤销。
- 看门狗只在 `Idle` / `Entering` 才 `EnsurePosition`。Running 后仓位随加仓变化，不得把仓位拉回初始首单量。
- Maker 建仓跟价：已有在途单则改价，不发第二笔；盘口未到则等 tick，不连打。
- `add_multiplier > 1` 时资金指数增长；保存时预计算总保证金与加满仓强平价，超过可用余额拒绝。预览按**首单**保证金拦截即可启动，加满仓需求在计划表里展示。
- 马丁没有天然止损，`stop_loss_price` 强烈建议配置。

配置字段见 [GRID_CONFIG.md](GRID_CONFIG.md)。Lighter 改单协议见 [LIGHTER.md](LIGHTER.md)。

---

## 19. 测试策略

| 层级 | 方式 | 覆盖目标 |
| --- | --- | --- |
| `domain` | 表驱动单测，纯函数 | 价位表生成（等差/等比/规整/去重）、三方向配对、两种 sizing 模式、派生量公式、`ClientOrderID` 编解码往返、精度规整边界 |
| `app` | 内存撮合引擎 `fakeexchange` | 完整场景：启动 → 建仓 → 铺网格 → 波动成交 → 配对补单 → 止盈退出；断线重连对账；post-only 被拒时跳过与重挂；调整区间与 trailing 的全量重铺；区间外暂停与回归恢复 |
| `api` | `httptest` + fake Runner | 端点契约、命令超时、鉴权、WS 推送限频与背压丢弃 |
| `exchange/lighter` | 录制回放 + 测试网冒烟 | 签名正确性、精度换算、nonce 序列（含超时不回退场景）、WS 重连、错误码分类 |
| 端到端 | 测试网小资金跑 24h，Windows 与 Linux 各一轮 | 内存/句柄泄漏、nonce 长期一致性、对账稳定性、跨平台差异 |

`fakeexchange` 是本项目测试的核心资产：约 200 行的内存撮合器，支持限价挂单、价格驱动撮合、post-only 拒绝模拟、部分成交模拟、限流与超时注入。有了它绝大部分逻辑不连网就能测。

**确定性要求**：`domain` 不允许调用 `time.Now()` 与 `rand`，时间来自事件字段，随机来自注入，保证测试可复现。

---

## 20. 依赖清单

| 依赖 | 用途 |
| --- | --- |
| `github.com/shopspring/decimal` | 精确十进制运算 |
| `gopkg.in/yaml.v3` | 配置解析 |
| `github.com/coder/websocket` | WebSocket（客户端连交易所 + 服务端推页面） |
| `github.com/go-chi/chi/v5` | HTTP 路由（标准库 mux 也可，chi 的中间件更省事） |
| `golang.org/x/time/rate` | 限流 |
| `golang.org/x/net/proxy` | SOCKS5 代理 |
| `modernc.org/sqlite` | 纯 Go SQLite，无 CGO |
| `github.com/prometheus/client_golang` | 指标 |
| `github.com/elliottech/lighter-go` | Lighter 交易签名与类型定义 |
| `log/slog`（标准库） | 结构化日志 |

不引入：DI 框架、ORM、大型 web 框架、通用事件总线。

前端：`web/` 纯静态文件，`internal/api` 通过 `dex-grid/web` 的 `embed.FS` 同源托管。账户状态每秒刷新；价格曲线默认 1h K 线。成交路径是交易所 WS 推送后立刻翻转格子，看门狗只做挂单缺补。

---

## 21. 扩展指南

### 新增一个交易所

1. `internal/exchange/<name>/` 实现 `exchange.Exchange`
2. 正确填写 `Capabilities()`（尤其 `PostOnly` 与 `BatchPlace`）
3. 若原生 client order id 格式不兼容 48 位整数，实现降级的字符串映射
4. `main.go` **追加**一行 `exchange.Register("<name>", <name>.New)`（不可插入到中间，会改变 slot）
5. `config.yaml` 增加该交易所的凭证段
6. 在 `config.yaml` 增加该交易所的凭证段与可选 `strategy_file`

**不允许**改动 `app` 与 `domain`。若必须改，说明 `Exchange` 或 `Capabilities` 抽象不足，先修抽象。

### 新增一个策略

1. `internal/domain/strategy/<name>/` 实现 `strategy.Strategy`
2. `internal/config` 增加参数结构体与校验（策略参数存 SQLite 的 `params` JSON 字段）
3. `main.go` 加一行 `strategy.Register("<name>", <name>.New)`
4. 前端按策略类型渲染不同表单

**不允许**在 `app` 层出现 `if strategy == "grid"` 这类分支。

### 新增一种建仓模式

在 `app/entry` 实现新的 `Trigger` 并在工厂注册：

```go
type Trigger interface {
    Start(target Target) []strategy.Action
    OnEvent(ev strategy.Event) (acts []strategy.Action, done bool, err error)
}
```
