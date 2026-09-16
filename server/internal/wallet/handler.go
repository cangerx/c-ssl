package wallet

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/server/httpx"
)

// Handler 处理钱包相关的 HTTP 请求。
type Handler struct {
	svc *Service
	// auth 由 router 注入，避免本包反向依赖 middleware 的具体实现。
	auth func(http.Handler) http.Handler
}

// NewHandler 构造处理器。
func NewHandler(svc *Service, auth func(http.Handler) http.Handler) *Handler {
	return &Handler{svc: svc, auth: auth}
}

// Routes 注册钱包路由。挂载点已带 /api/v1 前缀。
//
// 钱包属于用户私有数据，两个接口都要求登录，因此整组套上鉴权中间件。
func (h *Handler) Routes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(h.auth)
		r.Get("/wallet", h.get)
		r.Get("/wallet/ledger", h.ledger)
	})
}

// ── DTO ──────────────────────────────────────────
//
// 字段与 openapi/components/schemas/wallet.yaml 严格对应。
// 金额一律用「分」的整数，不用浮点也不用字符串——前端做展示时再除以 100。
//
// EntryNo 刻意不下发：它是服务端的幂等键，属于内部实现，
// 客户端拿到它既没有用途，又多一个可以被外部构造的标识。

type accountDTO struct {
	AvailableBalance int64 `json:"availableBalance"`
	FrozenBalance    int64 `json:"frozenBalance"`
	TotalBalance     int64 `json:"totalBalance"`
}

type entryDTO struct {
	ID             int64  `json:"id"`
	Op             string `json:"op"`
	BizType        string `json:"bizType"`
	BizNo          string `json:"bizNo"`
	AvailableDelta int64  `json:"availableDelta"`
	FrozenDelta    int64  `json:"frozenDelta"`
	AvailableAfter int64  `json:"availableAfter"`
	FrozenAfter    int64  `json:"frozenAfter"`
	Remark         string `json:"remark"`
	CreatedAt      string `json:"createdAt"`
}

type ledgerDTO struct {
	Items      []entryDTO `json:"items"`
	NextCursor *int64     `json:"nextCursor"`
}

func toAccountDTO(a *Account) accountDTO {
	return accountDTO{
		AvailableBalance: a.AvailableBalance.Cents(),
		FrozenBalance:    a.FrozenBalance.Cents(),
		TotalBalance:     a.TotalBalance().Cents(),
	}
}

func toEntryDTO(e Entry) entryDTO {
	return entryDTO{
		ID:             e.ID,
		Op:             string(e.Op),
		BizType:        e.BizType,
		BizNo:          e.BizNo,
		AvailableDelta: e.AvailableDelta.Cents(),
		FrozenDelta:    e.FrozenDelta.Cents(),
		AvailableAfter: e.AvailableAfter.Cents(),
		FrozenAfter:    e.FrozenAfter.Cents(),
		Remark:         e.Remark,
		CreatedAt:      e.CreatedAt.Format(time.RFC3339),
	}
}

// ── 处理函数 ──────────────────────────────────────

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	account, err := h.svc.Get(r.Context(), httpx.MustUserID(r.Context()))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, r, toAccountDTO(account))
}

func (h *Handler) ledger(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	filter := EntryFilter{Limit: DefaultEntryLimit}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 {
			httpx.Fail(w, r, errs.New(errs.CodeInvalidParam).
				WithField("limit", "limit 必须是正整数"))
			return
		}
		// 超出上限直接截断而不是报错：客户端传大值只是想要更多数据，
		// 没必要让它失败，但服务端必须守住资源上限。
		if limit > MaxEntryLimit {
			limit = MaxEntryLimit
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

	page, err := h.svc.ListEntries(r.Context(), httpx.MustUserID(r.Context()), filter)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	dto := ledgerDTO{Items: make([]entryDTO, 0, len(page.Items))}
	for _, entry := range page.Items {
		dto.Items = append(dto.Items, toEntryDTO(entry))
	}
	if page.NextCursor > 0 {
		dto.NextCursor = &page.NextCursor
	}
	httpx.OK(w, r, dto)
}
