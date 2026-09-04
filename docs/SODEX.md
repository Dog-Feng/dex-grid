# SODEx 适配设计

本文只写 **SODEx**（Bolt 永续引擎）这一侧。它是独立 DEX：独立账户、独立 API Key / nonce、独立的 `ClientOrderID` slot，**不得**与 Lighter / RH Lighter 共用配置或凭证。

只做永续。现货 Spark 引擎不接入，适配器入口只拉 `/api/v1/perps`。

系统分层、网格算法、HTTP API 见 [DESIGN.md](DESIGN.md)；凭证字段见 [GRID_CONFIG.md](GRID_CONFIG.md)。官方文档：[Trading API](https://sodex.com/documentation/trading-api/trading-api)、[Go SDK](https://github.com/sodex-tech/sodex-go-sdk-public)。

代码在 `internal/exchange/sodex/`。适配器实现 `exchange.Exchange` + `exchange.Streamer`，不含策略语义。

注册名：`sodex`（config.yaml / REST 路径 `/api/exchanges/sodex/...`）。控制台 Tab 显示为「SODEx」。

---

## 1. 与已有 DEX 的隔离

| | Core `lighter` | RH `rh_lighter` | `sodex` |
| --- | --- | --- | --- |
| 代码包 | `internal/exchange/lighter` | `internal/exchange/rhlighter` | `internal/exchange/sodex` |
| 注册名 | `lighter` | `rh_lighter` | `sodex` |
| ClientOrderID slot | **0**（已冻结） | **1**（已冻结） | **2**（追加，不可前插） |
| 核对 CLI | `cmd/lighterctl` | `cmd/rhlighterctl` | `cmd/sodexctl` |

`sodexctl` 会空白导入 `lighter` 并注册 `rh_lighter` **只为占用 slot 0、1**，保证本工具发出的测试单与 gridbot 一样落在 slot 2。

---

## 2. 端点与协议

| 项 | 值 |
| --- | --- |
| REST 主网 | `https://mainnet-gw.sodex.dev` |
| REST 测试网 | `https://testnet-gw.sodex.dev` |
| WS 主网 | `wss://mainnet-gw.sodex.dev/ws/perps` |
| WS 测试网 | `wss://testnet-gw.sodex.dev/ws/perps` |
| 主网 chain ID | **286623** |
| 测试网 chain ID | **138565** |
| 签名库 | `github.com/sodex-tech/sodex-go-sdk-public` |
| EIP-712 domain name | `"futures"`（不是 `"perps"`） |

交易：官方 SDK 签 EIP-712 → `POST /api/v1/perps/trade/orders`（body 只有 params，没有 `type` 包装）。

查询账户不需要签名，但必须带**主钱包地址**（不是 API key 地址）。

---

## 3. 能力声明

```go
Capabilities{
    BatchPlace:  20,  // newOrder.orders 数组
    BatchCancel: 20,  // cancel.cancels 数组
    ModifyOrder: true, // POST /trade/orders/replace（GTC/GTX 限价）；/modify 只改止盈止损
    PostOnly:    true, // TIF = GTX
    ReduceOnly:  true,
    NativeTPSL:  false, // 止盈止损仍由本系统本地触发
}
```

网格挂单一律 post-only（GTX）。市价 IOC 仅止损与建仓超时。

SODEx 永续是**单向净额**模式：每个账户每个交易对只有一个净仓位。下单 `positionSide` 只允许 `BOTH`（1）；`LONG`/`SHORT` 会出现在成交推送里，但下单路径会拒。

---

## 4. 凭证

三个身份不能混用：

| 配置字段 | 含义 |
| --- | --- |
| `credentials.account_index` 或 `account_id` | SODEx 数字 `accountID`（首次入金后分配） |
| `credentials.api_key_private_key` | **API key** 的 ECDSA 私钥（不是主钱包私钥） |
| `credentials.api_key_name` 或 `options.api_key_name` | API key 的 name，写入 `X-API-Key`（如 `api-key-01`） |
| `credentials.account_address` 或 `options.user_address` | **主钱包**地址，账户查询与 WS 用户频道用它 |

在 https://sodex.com/apikeys 创建 API key。查 accountID：

```
sodexctl check
# 或
curl "https://mainnet-gw.sodex.dev/api/v1/spot/accounts/0xYourAddress/state"
```

价格/数量字符串不能带末尾 0（`"0.4060"` 会被拒）。适配器经 shopspring/decimal `String()` 输出，已去掉尾零。

`clOrdID` 用本系统 48 位整数的十进制字符串，长度远小于 36 字符上限。

---

## 5. 事件流

订阅：

- `bookTicker` — 最优买卖价
- `markPrice` — 标记价 / 指数价
- `accountOrderUpdate` — 订单状态（`user` = 主钱包地址，无需签名）
- `accountTrade` — 成交
- `accountUpdate` — 仓位 / 余额（尽力解析）

心跳 `{"op":"ping"}` 每 20s；服务端 60s 无数据断开。重连后发 `Resync`，上层全量对账。

订单/成交字段类型不固定：`m`/`c`/`i` 可能是布尔、`"true"`/`"1"`，也可能是小数（像手续费）；`r` 可能是字符串或布尔。适配器用宽松解析，未知值当 false，避免整帧丢弃。订阅 `bookTicker` / `markPrice` 同时带 `symbol` 与 `symbols`；账户频道带 `accountID`。

GTX 可能先部分成交再拒绝剩余量。剩余被拒且已有成交时映射为 `Canceled`，避免格子把已成交部分当成整单拒绝而重挂双份。

没有客户端订单号（`clOrdID=0`）的挂单，看门狗解不出 COID，**不会撤**。用 `sodexctl cancel -m <市场> -oid <交易所订单号> -yes`。

---

## 6. 文件

| 文件 | 职责 |
| --- | --- |
| `adapter.go` | `Exchange` 实现；`Name = "sodex"`；**不在 init 注册** |
| `rest.go` | 官方 client + 限流/重试 |
| `convert.go` | 领域枚举 ↔ SODEx 枚举、订单/仓位映射 |
| `classify.go` | 错误分类 |
| `stream.go` | WS 重连、订阅、合流 |
| `ws_types.go` | 推送帧结构 |

注册：`cmd/gridbot` 在 rh_lighter（slot 1）之后 `exchange.Register("sodex", sodex.New)`。

核对工具：`cmd/sodexctl`（改账户必须 `-yes`）。

```yaml
  - name: sodex
    enabled: false
    network: mainnet   # 先 testnet 跑通再改 mainnet
    credentials:
      account_index: ${SODEX_ACCOUNT_ID}
      api_key_private_key: ${SODEX_PRIVATE_KEY}
    options:
      api_key_name: ${SODEX_API_KEY}
      user_address: ${SODEX_ADDRESS}
```

---

## 7. 小额实盘核对

配置好测网或主网凭证后：

```
go build -o sodexctl.exe ./cmd/sodexctl
./sodexctl markets -q btc
./sodexctl check
./sodexctl maker -m <id> -side buy -qty <最小量> -offset 0.02        # 演练
./sodexctl maker -m <id> -side buy -qty <最小量> -offset 0.02 -yes   # 挂 maker
./sodexctl modify -m <id> -coid <号> -price <价> -qty <量> -yes
./sodexctl cancel -m <id> -coid <号> -yes
./sodexctl cancel -m <id> -oid <交易所订单号> -yes  # 没有客户端订单号时
./sodexctl taker -m <id> -side buy -qty <最小量> -yes               # 市价开
./sodexctl close -m <id> -yes                                      # 市价平
```

最小名义约 10 USD。先测网，确认无误再开主网。
