# Lighter 适配设计

本文只写 **Lighter（zkLighter）** 这一侧：协议怎么映射、订单怎么签、事件怎么收、主网踩过哪些坑。系统分层、网格算法、HTTP API 见 [DESIGN.md](DESIGN.md)；凭证字段见 [GRID_CONFIG.md](GRID_CONFIG.md)；部署见 [DEPLOYMENT.md](DEPLOYMENT.md)。

代码在 `internal/exchange/lighter/`。适配器实现 `exchange.Exchange` + `exchange.Streamer`，**不含策略语义**：不知道网格还是马丁，只翻译端口上的下单 / 改单 / 撤单 / 行情。

---

## 目录

1. [在系统里的位置](#1-在系统里的位置)
2. [端点与协议](#2-端点与协议)
3. [能力声明](#3-能力声明)
4. [常量映射](#4-常量映射)
5. [精度换算](#5-精度换算)
6. [市场过滤](#6-市场过滤)
7. [Nonce 与发送队列](#7-nonce-与发送队列)
8. [下单、改单、撤单](#8-下单改单撤单)
9. [事件流](#9-事件流)
10. [源码对照](#10-源码对照)
11. [主网实测记录](#11-主网实测记录)
12. [参考实现](#12-参考实现)

---

## 1. 在系统里的位置

```
策略 Action（Place / Modify / Cancel）
        │
   app/executor     按 Capabilities 决定改单还是撤+挂，限流、重试
        │
   exchange.Exchange
        │
   lighter.Adapter  精度规整 → 签名 L2 交易 → sendTx
                    REST 查市场/仓位/挂单
                    WS 合流成 StreamEvent
```

一条硬约束贯穿适配器：**一个 DEX 一个实例**。Lighter 的 nonce 必须对 `(account_index, api_key_index)` 严格递增无空洞。单实例意味着所有出站交易可以锁在一个发送队列里，不必做跨实例 nonce 分配器。

`ClientOrderID` 直接当作 Lighter 的 `ClientOrderIndex`（48 位正整数）。撤单、改单的 `Index` 都用同一个值，不要换成交易所内部 `order_index`，否则对账对不上。

---

## 2. 端点与协议

| 项 | 值 |
| --- | --- |
| REST 主网 | `https://mainnet.zklighter.elliot.ai` |
| REST 测试网 | `https://testnet.zklighter.elliot.ai` |
| WS | `wss://mainnet.zklighter.elliot.ai/stream` |
| 下单 | 本地签名 L2 交易 → `POST /api/v1/sendTx`（或 WS `jsonapi/sendtx`） |
| 签名库 | `github.com/elliottech/lighter-go`（官方 Go SDK，**必须依赖**） |
| 主网 chain_id | `304` |

配置里 `options.tx_send_channel` 可选 `ws` / `rest`。设计上优先 WS 发送（延迟更低、与订阅共用连接）；当前实现走 REST `sendTx`。批量 `sendTxBatch` **尚未实现**，`Capabilities.BatchPlace = 0`，上层串行发。

查询类走普通 REST：市场、盘口、K 线、账户、挂单、仓位。交易类一律本地签名后再提交，私钥不出进程。

---

## 3. 能力声明

```go
Capabilities{
    BatchPlace:  0,     // sendTxBatch 未接，Executor 按 1 笔一批
    BatchCancel: 0,
    ModifyOrder: true,  // L2ModifyOrder=17，马丁止盈走改单
    PostOnly:    true,  // 网格前提；为 false 时进程拒绝启动
    ReduceOnly:  true,
    NativeTPSL:  true,  // 交易所有条件单，本系统网格止盈仍用普通限价
}
```

`PostOnly` 为 false 时本系统无法运行——除建仓市价外，全部挂单都是 post-only。适配器必须如实申报，不能静默降级。

`ModifyOrder` 为 true 时，Executor 把策略的 `ModifyOrder` 译成 `ModifyOrders`；为 false 时才撤旧单再按同一 `ClientOrderID` 重挂。马丁加仓后改止盈依赖这条能力。

---

## 4. 常量映射

交易类型：`L2CreateOrder=14`、`L2CancelOrder=15`、`L2CancelAllOrders=16`、`L2ModifyOrder=17`、`L2UpdateLeverage=20`、`L2CreateGroupedOrders=28`

订单类型：`Limit=0`、`Market=1`、`StopLoss=2`、`StopLossLimit=3`、`TakeProfit=4`、`TakeProfitLimit=5`、`TWAP=6`

Time-In-Force：`ImmediateOrCancel=0`、`GoodTillTime=1`、`PostOnly=2`

保证金模式：`CrossMargin=0`、`IsolatedMargin=1`

分组类型：`OTO=1`、`OCO=2`、`OTOCO=3`（单组最多 3 笔；本系统未使用分组单）

| domain | Lighter |
| --- | --- |
| `TIFPostOnly` | `TimeInForce=2`，`OrderType=0` |
| `TIFGTC` | `TimeInForce=1`（GoodTillTime）+ `ExpiredAt`。Lighter **无永久单**，用 `order_expiry`（默认 28 天，上限 30 天） |
| `TypeMarket` | `OrderType=1`，`TimeInForce=0`（IOC），`OrderExpiry` 必须留空（`NilOrderExpiry`），`Price` 是可接受的最差价（滑点保护） |
| `ReduceOnly` | 下单请求 `0/1`；查询回报可能是 `true/false`，解析要同时接受 |
| 普通限价改单 | `TriggerPrice = NilOrderTriggerPrice`（`0`） |

设杠杆走 `L2UpdateLeverage`。初始保证金率按市场 `min_initial_margin_fraction` 与目标杠杆换算；设杠杆失败不得中止整次启动（保证金模式可能已经是目标值）。

---

## 5. 精度换算

价格是 `uint32`、数量是 `int64`，按市场元数据小数位缩放：

```go
priceInt = uint32(price.Shift(int32(m.PriceDecimals)).IntPart())
sizeInt  = int64(qty.Shift(int32(m.SizeDecimals)).IntPart())
```

必须是最小变动单位的整数倍，否则本地直接报非法参数，不要把带毛刺的小数丢给交易所。

边界（`txtypes/constants.go`）：`MinOrderPrice=1`、`MaxOrderPrice=2^32−1`、`MinOrderBaseAmount=1`、`MaxOrderBaseAmount=2^48−1`。换算后校验，越界返回明确错误。

市场元数据来自 `GET /api/v1/orderBookDetails`（含 `market_id`、`price_decimals`、`size_decimals`、`min_base_amount`、`min_quote_amount`、保证金率）。启动时拉取，缓存约 10 分钟，映射成 `domain/market.Market`。

费率字段是百分数字符串（`"0.0200"` 表示 0.02%），要除以 100 才是比率。主网多数市场 maker/taker 目前是 `"0.0000"`，校验时仍按文档费率处理，不能假设永远为零。

---

## 6. 市场过滤

本系统只做永续合约网格。适配器在刷新缓存时就丢掉现货：

| 情况 | 处理 |
| --- | --- |
| `market_type != "perp"` | 不进缓存、不下拉、不能下单 |
| `status != "active"` | 仍缓存元数据（运行中实例可能持有仓位），但不进下拉、不允许开新仓 |
| 已下架合约上的减仓单 | `ReduceOnly` 的 Place / Modify 走 `lookup` 而不是 `tradable`，否则仓位锁死 |

主网分布（接入时核对）：235 个市场 = 227 永续 + 8 现货。现货 `market_id` 从 2048 起，符号带斜杠（`ETH/USDC`）。`ETH` 永续（id 0）与 `ETH/USDC` 现货（id 2048）并存，按符号查找不会混，按 index 指定时要当心。永续里约 17 个已下架，下拉大约 210 个。

`/orderBooks` 与 `/orderBookDetails` 返回顺序**不是**按 `market_id` 排的，页面下拉必须自己按成交额或符号排序。

---

## 7. Nonce 与发送队列

同一 `(account_index, api_key_index)` 下 nonce 必须严格递增、不能有空洞。SDK 构造 `TxClient` 时传入 `httpClient=nil`，禁止它在签名路径上偷偷打 `nextNonce`。

`txSender.send` 用一把互斥锁串行：取号 → 签名 → `Validate` → `sendTx` → 成功则 `nonce++`。

| 情况 | 处理 |
| --- | --- |
| 启动 / 本地无缓存 | `GET /api/v1/nextNonce` 拉初始值 |
| 本地校验失败（没发出去） | 不消耗 nonce |
| 发送失败且确认未被接受 | 回退计数器，复用该 nonce |
| 网络超时、无法确认是否入块 | **不回退**；下次发送前再拉 `nextNonce` 校准。服务端已前进说明交易实际被接受 |
| `sendTxBatch`（未实现） | 一次应分配连续一段 nonce |

两个进程用同一 API Key 会立刻把 nonce 打乱，所以进程启动必须抢 `data/gridbot.lock`。

---

## 8. 下单、改单、撤单

### 8.1 下单 `PlaceOrders`

逐笔 `GetCreateOrderTransaction`。单笔失败不中断后续，结果按入参顺序带 `Err`，上层决定重试哪几笔。

- 限价 + post-only：网格腿、加仓、止盈
- 市价：仅建仓 / 紧急平仓。`Price` 是滑点保护上限，**不是成交价**
- 只减仓单允许打到已下架合约上

Post-only 若会立即成交，Lighter 表现为提交成功、随后订单 `rejected`（常见原因 `canceled-post-only`）。这**不算**连续失败：策略等下一 tick 换价重挂，`seq` 递增。

### 8.2 改单 `ModifyOrders`

`GetModifyOrderTransaction`，对应 `L2ModifyOrder=17`：

```
MarketIndex, Index(=ClientOrderID), BaseAmount, Price, TriggerPrice=0
```

改的是**已存活挂单**的价格和数量，客户端订单号不变。这是马丁止盈的主路径：

1. 加仓成交 → 仓位变大、持仓均价下移
2. 新止盈价 = 均价 × (1 ± 止盈%)
3. 新数量 = 当前仓位绝对值（必须与仓位一致）
4. 对同一笔止盈单 `ModifyOrder`，**不要先撤再挂**

改单失败时旧单仍在盘上。Executor **不得**把失败翻译成 `Rejected` 再回灌策略，否则策略会清掉本地止盈状态、下一 tick 再挂一笔，出现双止盈。超时后策略把状态从「改单中」恢复为「挂着」，再改一次。

Post-only 改单若新价格会穿盘口，交易所可能拒改；旧止盈保留，等盘口允许后再试。

### 8.3 撤单 `CancelOrders` / `CancelAll`

撤单同样用 `ClientOrderID` 当 `Index`。

**禁止**调用 Lighter 原生 `CancelAllOrders`：那是账户级全撤，会把同一账户上其他交易对（包括手动挂的单）一起撤掉。适配器的 `CancelAll(symbol)` 必须先 `OpenOrders(symbol)` 再逐笔撤。`symbol` 为空直接拒绝。

---

## 9. 事件流

`Subscribe` 订三个频道，归并到单个 `chan StreamEvent`：

| 频道 | 映射 | 说明 |
| --- | --- | --- |
| `ticker/{market_id}` | → `Ticker.Book` | 最优买卖价（BBO） |
| `market_stats/{market_id}` | → `Ticker.Mark/Index/Last` | 标记价与指数价 |
| `account_market/{market_id}/{account_id}` | → `Order` / `Position` | 订单回报与仓位，需要 auth token |

**不订 `order_book`**：那是增量档位，本地维护完整订单簿还要对 `begin_nonce`。策略只需要买一卖一，`ticker` 直接给。

**要订 `market_stats`**：`ticker` 不带标记价，风控默认按标记价触发（抗插针）。两个频道各自更新字段，再发合并快照。`market_stats` 还没到时标记价先用中间价顶上。

**账户频道一个就够**：`account_market` 同时带 `orders`、`position`、`trades`，不必再订 `account_orders`。文档写 `position` 是数组，主网实际推**单个对象**，解析要兼容两种。

**重连**：指数退避（`reconnect.initial` → `reconnect.max`，默认 1s → 30s）。每次连上先发 `Resync`，Runner 全量对账——断线期间可能有成交。auth token 每次重连重新生成。

**保活**：服务端要求至少每 2 分钟一帧，否则断开。适配器每 45 秒发 `{"type":"ping"}`。读超时 90 秒：`ticker` 很活跃，这么久没消息视为连接僵死，主动断开重连。

时延量级（主网）：`sendTx` 成功到 WS 订单事件约 3–6 秒（排序器入块，不是网速）。单笔下单到可查询约 2–4 秒，市价成交后仓位约 4 秒内可见。

市价单回报里的 `price` 是滑点保护上限，成交价看 `filled_quote_amount / filled_base_amount`。

---

## 10. 源码对照

| 文件 | 职责 |
| --- | --- |
| `adapter.go` | `Exchange` 实现：市场、杠杆、Place / Modify / Cancel、仓位账户 |
| `tx.go` | 签名客户端、nonce、串行 `sendTx` |
| `rest.go` | HTTP 查询封装 |
| `api.go` | REST 路径与 JSON DTO |
| `stream.go` | WS 订阅、重连、合流 |
| `convert.go` | 价格数量整数换算、TIF / 方向 / 保证金模式 |
| `classify.go` | 交易所错误 → `ErrRetryable` / `InvalidParam` / `Fatal` / post-only 拒单 |

命令行核对工具：`cmd/lighterctl`（查市场、盘口、账户、演练下单；改变账户状态必须显式 `-yes`）。

---

## 11. 主网实测记录

以下都是接入时在主网上核对过的，与官方文档不一致时以这里为准。

| 项 | 实测结果 |
| --- | --- |
| 市场数量 | `/orderBooks` 返回 235 个（227 永续 + 8 现货），`/orderBookDetails` 只返回 227 个永续。返回顺序不是按 market_id 排的 |
| 现货识别 | `market_type = "spot"`，`market_id` 从 2048 起，符号带斜杠 |
| 下架合约 | `status = "inactive"`，约 17 个；`mark_price` 和成交额为 0，盘口空 |
| `market_index = 2` | SOL 永续，`size_decimals = 3`、`price_decimals = 3`、最大杠杆 25x |
| 手续费 | 全市场 `maker_fee` / `taker_fee` 曾为 `"0.0000"`（百分数字符串，需 ÷100） |
| 最小下单额 | 所有市场 `min_quote_amount` 统一 10 USDC，另有各自 `min_base_amount`，**两者取严** |
| 保证金率单位 | `/orderBookDetails` 是万分之一整数（`240` = 2.4%）；`/account` 持仓里是百分数字符串（`"5.00"` = 5%）。同一语义两种表示 |
| `is_ask` / `reduce_only` | 下单 `0/1`，查询 `true/false` |
| `/orderBookOrders` | 返回逐笔挂单，不是按价格聚合的档位。第一条是最优价，数量只是那一笔的量 |
| `/api/v1/candles` | 公开 K 线。旧路径 `/candlesticks` 主网 403，已弃用 |
| `CancelAllOrders` | 账户级。适配器不得调用，改为按市场逐笔撤 |
| WS `account_market.position` | 文档数组，实际常为单个对象 |
| WS 订单回报延迟 | sendTx 成功后约 3–6 秒 |
| WS 市价单 `price` | 滑点保护上限，不是成交价 |
| 改单 | `ModifyOrder` 用 ClientOrderIndex；普通限价 `TriggerPrice=0` |

---

## 12. 参考实现

- `github.com/elliottech/lighter-go` —— 官方签名与交易类型（**必须依赖**）
- `github.com/elliottech/lighter-python` —— 官方 Python SDK，行为对照
- Lighter 官方文档 System Setup —— API Key 生成；**不要**和官方前端复用同一个 `api_key_index`
