# RH Lighter 适配设计

本文只写 **RH Lighter**（Robinhood 链上的 Lighter 实例）这一侧。它是**独立 DEX**：另一条链、另一套账户 / API Key / nonce、独立的 `ClientOrderID` slot，**不得**与 `internal/exchange/lighter`（zkLighter Core）共用配置或凭证。

系统分层、网格算法、HTTP API 见 [DESIGN.md](DESIGN.md)；凭证字段见 [GRID_CONFIG.md](GRID_CONFIG.md)；Core Lighter 见 [LIGHTER.md](LIGHTER.md)。官方 API：[SDK](https://apidocs.rh.lighter.xyz/docs/sdk)、[Get Started](https://apidocs.rh.lighter.xyz/docs/get-started.md)。

代码在 `internal/exchange/rhlighter/`。适配器实现 `exchange.Exchange` + `exchange.Streamer`，不含策略语义。

注册名：`rh_lighter`（config.yaml / REST 路径 `/api/exchanges/rh_lighter/...`）。控制台 Tab 显示为「RH Lighter」。

---

## 1. 与现有 Lighter 的隔离

| | Core `lighter` | RH `rh_lighter` |
| --- | --- | --- |
| 代码包 | `internal/exchange/lighter` | `internal/exchange/rhlighter` |
| 注册名 | `lighter` | `rh_lighter` |
| ClientOrderID slot | **0**（已冻结） | **1**（追加，不可前插） |
| 链 | zkLighter 主网 | Robinhood Lighter 主网 |
| 账户 / nonce | 独立 | 独立，禁止复用 Core 的 API Key |
| SQLite 行 | `exchange = lighter` | `exchange = rh_lighter` |
| 核对 CLI | `cmd/lighterctl` | `cmd/rhlighterctl` |

两个实例可以同时 `enabled: true`，各跑一个交易对、各支配自己账户的保证金。Supervisor 为每个启用的交易所起一个 Runner，互不影响。

`rhlighterctl` 会空白导入 `lighter` 包**只为占用 slot 0**，保证本工具发出的测试单与 gridbot 一样落在 slot 1。它不会去连 Core Lighter。

---

## 2. 端点与协议

官方说明：Robinhood 实例没有单独 SDK，Core 实例换 base path 即可用。Python SDK 将其建模为 `EndpointProfile`（`robinhood` / `robinhood_testnet`）。

| 项 | 值 |
| --- | --- |
| REST 主网 | `https://api.rh.lighter.xyz` |
| REST 测试网 | `https://api.rh-testnet.lighter.xyz` |
| WS 主网 | `wss://api.rh.lighter.xyz/stream` |
| WS 测试网 | `wss://api.rh-testnet.lighter.xyz/stream` |
| 下单 | 本地签名 L2 交易 → `POST /api/v1/sendTx` |
| 签名库 | `github.com/elliottech/lighter-go`（与 Core 同一依赖） |
| 主网 chain_id | **466324** |
| 测试网 chain_id | 300 |

`options.chain_id` 可覆盖；未填时主网固定 466324。签错 chain_id 会导致全部交易被拒。

当前实现走 REST `sendTx`。`sendTxBatch` 未接，`Capabilities.BatchPlace = 0`，上层串行发。

---

## 3. 能力声明

与 Core Lighter 相同：

```go
Capabilities{
    BatchPlace:  0,
    BatchCancel: 0,
    ModifyOrder: true,
    PostOnly:    true,
    ReduceOnly:  true,
    NativeTPSL:  true,
}
```

`PostOnly` 为 false 时进程拒绝启动。马丁加仓后改止盈走 `ModifyOrders`。

---

## 4. 常量映射

交易类型、订单类型、TIF、保证金模式与 Core 相同（`L2CreateOrder=14`、`L2ModifyOrder=17`、`PostOnly=2` 等）。领域枚举到交易所整数的换算在 `convert.go`。

| domain | RH Lighter |
| --- | --- |
| `TIFPostOnly` | `TimeInForce=2`，`OrderType=0` |
| `TIFGTC` | `TimeInForce=1`（GoodTillTime）+ `ExpiredAt`。无永久单，默认 `order_expiry` 28 天、上限 30 天 |
| `TypeMarket` | `OrderType=1`，IOC，`OrderExpiry` 留空，`Price` 是可接受最差价 |
| `ReduceOnly` | 下单 `0/1`；查询可能是 `true/false` |
| 普通限价改单 | `TriggerPrice = 0` |

---

## 5. 精度、费率、最小名义

与 Core 相同：

- 价格 `uint32`、数量 `int64`，按市场 `price_decimals` / `size_decimals` 缩放
- **费率是百分数字符串**（`"0.0120"` 表示 0.012%），适配器 **÷100** 才是比率
- API 若返回 0，自算已实现盈亏用 RH 公布费率兜底：**maker 0.012%**、**taker 0.035%**
- 页面已实现：成交里的 `ask/bid_account_pnl` 非 0 时用该净盈亏（已扣费，不再二次扣）；为 0（RH 常见）或无成交历史时用格子毛利 − 估算手续费
- `min_base_amount` 与 `min_quote_amount` **两者取严**
- 保证金率：`/orderBookDetails` 为万分之一整数；`/account` 持仓里是百分数字符串

---

## 6. 市场过滤

过滤规则与现有 Lighter **完全相同**，在刷新缓存时执行：

| 情况 | 处理 |
| --- | --- |
| `market_type != "perp"` | 不进缓存、不下拉、不能下单 |
| `status != "active"` | 仍缓存元数据（存量仓位对账要用），但不进下拉、不允许开新仓 |
| 已下架合约上的减仓单 | `ReduceOnly` 的 Place / Modify 走 `lookup` 而不是 `tradable` |

---

## 7. Nonce 与发送队列

同一 `(account_index, api_key_index)` 下 nonce 必须严格递增、不能有空洞。本实例有自己的发送锁，与 Core Lighter 的锁无关。

RH 文档：API Key 索引 **0 和 1 留给官方 Web/移动端**，本程序使用 **≥ 2** 的独立槽位。不要和官方前端、也不要和 Core Lighter 复用同一把 key。

进程锁仍是整进程一份 `data/gridbot.lock`：一个 gridbot 进程可以同时挂两个交易所适配器，但不能开两个进程打同一数据目录。

---

## 8. 下单、改单、撤单

行为与 Core 适配器一致：

- `PlaceOrders` 逐笔签名 `sendTx`，单笔失败不中断后续
- `ModifyOrders` 用 `ClientOrderID` 当 `Index`，改价改量、号不变（马丁止盈主路径）
- 改单失败不得把旧单当成 `Rejected` 清掉
- **禁止**调用账户级 `CancelAllOrders`；`CancelAll(symbol)` 先拉该市场挂单再逐笔撤
- 限价 + post-only：网格 / 加仓 / 止盈 / 建仓
- 市价 IOC：仅止损平仓、建仓超时。`Price` 是可接受最差价
- Post-only 穿价：提交可能成功，随后订单 `canceled-post-only`，不算连续失败

---

## 9. 事件流

`Subscribe` 订三个频道，归并到单个 `chan StreamEvent`：

| 频道 | 映射 |
| --- | --- |
| `ticker/{market_id}` | 最优买卖价 |
| `market_stats/{market_id}` | 标记价 / 指数 / 最新价 |
| `account_market/{market_id}/{account_id}` | 订单与仓位（需 auth token） |

重连指数退避；连上先 `Resync` 全量对账。每 45 秒 ping。读超时 90 秒视为僵死。

---

## 10. 源码对照

| 文件 | 职责 |
| --- | --- |
| `adapter.go` | `Exchange` 实现；`Name = "rh_lighter"`；**不在 init 注册** |
| `tx.go` | 签名客户端、nonce、串行 `sendTx`（chain_id=466324） |
| `rest.go` | HTTP 查询封装 |
| `api.go` | REST 路径、JSON DTO、RH 端点常量 |
| `stream.go` | WS 订阅、重连、合流 |
| `convert.go` | 价格数量整数换算；费率 ÷100；API 为 0 时兜底 maker 0.012% / taker 0.035% |
| `classify.go` | 交易所错误分类 |

注册：`cmd/gridbot` 在 lighter（slot 0）之后 `exchange.Register("rh_lighter", rhlighter.New)`。

Lighter / RH 共用 `internal/exchange/lighttrade`：WS `account_market.trades` 解析成交净盈亏，策略按本轮 slot/epoch 累加、`trade_id` 去重。

核对工具：`cmd/rhlighterctl`（改账户必须 `-yes`）。

---

## 11. 配置

```yaml
exchanges:
  - name: rh_lighter
    enabled: true
    network: mainnet
    credentials:
      account_index: ${RH_LIGHTER_ACCOUNT_INDEX}
      api_key_index: ${RH_LIGHTER_API_KEY_INDEX}       # 建议 ≥ 2
      api_key_private_key: ${RH_LIGHTER_API_KEY_PRIVATE_KEY}
    options:
      # chain_id 默认 466324，一般不用写
      tx_send_channel: ws
      batch_enabled: true
      batch_size: 20
      order_expiry: 28d
      price_protection: true
    autostart: false
```

密钥环境变量必须与 Core 的 `LIGHTER_*` 分开。

---

## 12. 参考

- [RH Lighter SDK](https://apidocs.rh.lighter.xyz/docs/sdk) —— base path `https://api.rh.lighter.xyz`
- [RH Get Started](https://apidocs.rh.lighter.xyz/docs/get-started.md)
- [RH WebSocket](https://apidocs.rh.lighter.xyz/docs/websocket) —— `wss://api.rh.lighter.xyz/stream`
- `github.com/elliottech/lighter-go` —— 签名（TxClient 传入 chain_id **466324**）
- `github.com/elliottech/lighter-python` `endpoint_profiles.py` —— `ROBINHOOD.chain_id = 466324`
