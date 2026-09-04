package sodex

// WebSocket 频道名与推送结构，字段与官方 SDK ws 包对齐。
// 不直接 import 官方 ws 包，避免把 gorilla/websocket 拉进本模块。
const (
	channelBookTicker      = "bookTicker"
	channelMarkPrice       = "markPrice"
	channelAccountOrderUpd = "accountOrderUpdate"
	channelAccountTrade    = "accountTrade"
	channelAccountUpdate   = "accountUpdate"
)

type wsBookTicker struct {
	EventTime int64  `json:"E"`
	Symbol    string `json:"s"`
	UpdateID  int64  `json:"u"`
	AskPrice  string `json:"a"`
	AskQty    string `json:"A"`
	BidPrice  string `json:"b"`
	BidQty    string `json:"B"`
}

type wsMarkPrice struct {
	EventTime       int64  `json:"E"`
	Symbol          string `json:"s"`
	OpenInterest    string `json:"oi"`
	MarkPx          string `json:"p"`
	IndexPx         string `json:"i"`
	FundingRate     string `json:"r"`
	NextFundingTime int64  `json:"T"`
}

type wsAccountOrderUpdate struct {
	EventTime   flexInt64  `json:"E"`
	TradeTime   flexInt64  `json:"T"`
	Symbol      string     `json:"s"`
	ClOrdID     flexString `json:"c"`
	ClOrdIDAlt  flexString `json:"clOrdID"`
	OrderID     flexInt64  `json:"i"`
	OrderIDAlt  flexInt64  `json:"orderID"`
	Side        flexString `json:"S"`
	OrderType   flexString `json:"o"`
	Price       flexString `json:"p"`
	OrigQty     flexString `json:"q"`
	Status      flexString `json:"X"`
	FilledQty   flexString `json:"z"`
	FilledValue flexString `json:"v"`
	TradeID     flexInt64  `json:"t"`
	LastQty     flexString `json:"l"`
	LastPrice   flexString `json:"L"`
	Fee         flexString `json:"n"`
	IsMaker     flexBool   `json:"m"`
	ExecType    flexString `json:"x"`
	Reason      flexString `json:"r"`
}

type wsAccountTrade struct {
	EventTime flexInt64  `json:"E"`
	TradeTime flexInt64  `json:"T"`
	TradeID   flexInt64  `json:"t"`
	Symbol    string     `json:"s"`
	OrderID   flexInt64  `json:"i"`
	ClOrdID   flexString `json:"c"`
	ClOrdIDAlt flexString `json:"clOrdID"`
	Side      flexString `json:"S"`
	Price     flexString `json:"p"`
	Quantity  flexString `json:"q"`
	Fee       flexString `json:"f"`
	IsMaker   flexBool   `json:"m"`
	Direction flexString `json:"d,omitempty"`
}
