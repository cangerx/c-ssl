package recharge

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/money"
	"github.com/cangerx/c-ssl/server/internal/server/httpx"
	"github.com/cangerx/c-ssl/server/internal/upstream/payment"
)

// signatureHeader 是渠道回调的签名请求头。
//
// 与 docs/06 第 6 节里 FoxSSL Webhook 的请求头保持一致，不另立一套。
const signatureHeader = "X-Webhook-Signature"

// maxWebhookBody 是回调报文的读取上限。
//
// 回调接口不需要登录，是任何人都能打的公开端点。没有上限时，
// 一个声明了超大 Content-Length 的请求就能把内存吃光——
// 这是最廉价的一类 DoS。
const maxWebhookBody = 256 * 1024

// Handler 处理充值订单与支付回调的 HTTP 请求。
type Handler struct {
	svc *Service
	// auth 由 router 注入，避免本包反向依赖 middleware 的具体实现。
	auth func(http.Handler) http.Handler
	// mockChannel 非空时注册开发环境的模拟回调接口。
	// 由装配层决定——本包不判断环境，环境判断散落在业务包里迟早会漏掉一处。
	mockChannel *payment.MockChannel
	trustProxy  bool
}

// NewHandler 构造处理器。mockChannel 为 nil 时不注册模拟回调接口。
func NewHandler(
	svc *Service,
	auth func(http.Handler) http.Handler,
	mockChannel *payment.MockChannel,
	trustProxy bool,
) *Handler {
	return &Handler{svc: svc, auth: auth, mockChannel: mockChannel, trustProxy: trustProxy}
}

// Routes 注册充值域路由。挂载点已带 /api/v1 前缀。
func (h *Handler) Routes(r chi.Router) {
	// 充值订单属于用户私有数据，整组套上鉴权中间件。
	r.Group(func(r chi.Router) {
		r.Use(h.auth)
		r.Post("/recharge/orders", h.createOrder)
		r.Get("/recharge/orders", h.listOrders)
	})

	// 渠道回调不套鉴权：调用方是渠道服务器，不是用户。
	// 唯一的身份凭证是签名，验签在 Service 里、读取任何数据之前完成。
	r.Post("/payments/webhook/{channel}", h.webhook)

	// 开发环境的模拟回调。它要求登录，且只能给自己的订单付款——
	// 否则这个开发辅助接口就成了「给别人充值」的入口。
	if h.mockChannel != nil {
		r.Group(func(r chi.Router) {
			r.Use(h.auth)
			r.Post("/payments/mock/notify", h.mockNotify)
		})
	}
}

// ── DTO ──────────────────────────────────────────
//
// 字段与 openapi/components/schemas/recharge.yaml 严格对应。
// 金额一律用「分」的整数。
//
// ChannelOrderNo / ChannelTradeNo 刻意不下发：它们是渠道侧的内部标识，
// 属于实现细节。用户要查一笔支付，用平台单号就够了；
// 下发渠道交易号只会多一个可以被外部引用、却无法被平台校验的标识。

type orderDTO struct {
	OrderNo   string  `json:"orderNo"`
	Amount    int64   `json:"amount"`
	Status    string  `json:"status"`
	Channel   string  `json:"channel"`
	PayURL    string  `json:"payUrl"`
	PaidAt    *string `json:"paidAt"`
	ExpiresAt string  `json:"expiresAt"`
	CreatedAt string  `json:"createdAt"`
}

type orderListDTO struct {
	Items      []orderDTO `json:"items"`
	NextCursor *int64     `json:"nextCursor"`
}

type createOrderRequest struct {
	Amount  int64  `json:"amount"`
	Channel string `json:"channel"`
}

type mockNotifyRequest struct {
	OrderNo        string `json:"orderNo"`
	Status         string `json:"status"`
	ChannelTradeNo string `json:"channelTradeNo"`
}

func toOrderDTO(o Order) orderDTO {
	dto := orderDTO{
		OrderNo:   o.OrderNo,
		Amount:    o.Amount.Cents(),
		Status:    string(o.Status),
		Channel:   o.Channel,
		PayURL:    o.PayURL,
		ExpiresAt: o.ExpiresAt.Format(time.RFC3339),
		CreatedAt: o.CreatedAt.Format(time.RFC3339),
	}
	if o.PaidAt != nil {
		paidAt := o.PaidAt.Format(time.RFC3339)
		dto.PaidAt = &paidAt
	}
	return dto
}

// ── 用户侧处理函数 ────────────────────────────────

func (h *Handler) createOrder(w http.ResponseWriter, r *http.Request) {
	var body createOrderRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	order, err := h.svc.Create(r.Context(), CreateInput{
		UserID:  httpx.MustUserID(r.Context()),
		Amount:  money.Amount(body.Amount),
		Channel: body.Channel,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, toOrderDTO(*order))
}

func (h *Handler) listOrders(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	filter := Filter{Limit: DefaultLimit}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			httpx.Fail(w, r, errs.New(errs.CodeInvalidParam).
				WithField("limit", "limit 必须是正整数"))
			return
		}
		// 超出上限直接截断而不是报错，与钱包账本接口保持一致
		if limit > MaxLimit {
			limit = MaxLimit
		}
		filter.Limit = limit
	}
	if raw := query.Get("cursor"); raw != "" {
		cursor, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || cursor < 0 {
			httpx.Fail(w, r, errs.New(errs.CodeInvalidParam).
				WithField("cursor", "cursor 必须是非负整数"))
			return
		}
		filter.Cursor = cursor
	}

	page, err := h.svc.List(r.Context(), httpx.MustUserID(r.Context()), filter)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	dto := orderListDTO{Items: make([]orderDTO, 0, len(page.Items))}
	for _, order := range page.Items {
		dto.Items = append(dto.Items, toOrderDTO(order))
	}
	if page.NextCursor > 0 {
		dto.NextCursor = &page.NextCursor
	}
	httpx.OK(w, r, dto)
}

// ── 渠道回调 ──────────────────────────────────────

func (h *Handler) webhook(w http.ResponseWriter, r *http.Request) {
	channelName := chi.URLParam(r, "channel")

	// 必须先读原始字节：签名是对原始字节做的，
	// 先反序列化再重新序列化会改变字节，验签必然失败。
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		httpx.Fail(w, r, errs.Newf(errs.CodeInvalidParam, "读取回调报文失败"))
		return
	}

	if err := h.svc.HandleNotification(
		r.Context(), channelName, raw, r.Header.Get(signatureHeader),
	); err != nil {
		h.logRejection(r, channelName, err)
		httpx.Fail(w, r, err)
		return
	}

	h.writeAck(w, r, channelName)
}

// logRejection 记录被拒绝的回调。
//
// 验签失败刻意不落库：攻击者可以用伪造签名的报文把任意交易号写进
// 渠道流水表，让后续真实回调撞上唯一索引而被判成重放，白白丢掉一笔真实入账。
// 代价是这类事件只留在日志里，所以来源信息必须记全。
func (h *Handler) logRejection(r *http.Request, channel string, err error) {
	if !errors.Is(err, payment.ErrInvalidSignature) {
		return
	}
	slog.WarnContext(r.Context(), "支付回调验签失败",
		"channel", channel,
		"remote_addr", httpx.ClientIP(r, h.trustProxy),
		"signature", r.Header.Get(signatureHeader),
		"error", err,
	)
}

// writeAck 按渠道约定的格式应答。
//
// 不用本服务的统一信封：真实渠道各有各的成功标识，格式不对会被判为失败
// 并触发无限重试。应答格式属于渠道的领域知识，因此由适配器给出。
func (h *Handler) writeAck(w http.ResponseWriter, r *http.Request, channelName string) {
	ack, ok := h.svc.Ack(channelName)
	if !ok {
		// 走不到：能返回成功说明渠道已注册。
		httpx.OK(w, r, nil)
		return
	}
	w.Header().Set("Content-Type", ack.ContentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(ack.Body)
}

// ── 开发环境的模拟回调 ────────────────────────────

// mockNotify 模拟渠道投递一次回调。
//
// 它按 Mock 渠道的格式构造报文、用同一个密钥签名，然后交给与真实回调
// 完全相同的处理路径——包括验签、幂等与事务。
// 重复调用同一张订单不会重复入账，这正是验收标准第 2 条要验证的行为。
func (h *Handler) mockNotify(w http.ResponseWriter, r *http.Request) {
	var body mockNotifyRequest
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// 只允许给自己的订单付款
	order, err := h.svc.Get(r.Context(), httpx.MustUserID(r.Context()), body.OrderNo)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	status := payment.Status(body.Status)
	if body.Status == "" {
		status = payment.StatusSuccess
	}
	if !status.Valid() {
		httpx.Fail(w, r, errs.New(errs.CodeInvalidParam).
			WithField("status", "只支持 success 或 failed"))
		return
	}

	now := time.Now().UTC()
	tradeNo := body.ChannelTradeNo
	if tradeNo == "" {
		tradeNo = payment.NewTradeNo(now)
	}

	raw, signature, err := h.mockChannel.EncodeNotification(&payment.Notification{
		Channel:        payment.NameMock,
		ChannelTradeNo: tradeNo,
		ChannelOrderNo: order.ChannelOrderNo,
		OrderNo:        order.OrderNo,
		Amount:         order.Amount,
		Status:         status,
		PaidAt:         now,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.svc.HandleNotification(
		r.Context(), payment.NameMock, raw, signature,
	); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	h.writeAck(w, r, payment.NameMock)
}
