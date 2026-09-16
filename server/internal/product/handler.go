package product

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/product/rules"
	"github.com/cangerx/c-ssl/server/internal/server/httpx"
)

// Handler 处理产品相关的 HTTP 请求。
type Handler struct {
	svc *Service
}

// NewHandler 构造处理器。
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// Routes 注册产品路由。挂载点已带 /api/v1 前缀。
func (h *Handler) Routes(r chi.Router) {
	r.Get("/products", h.list)
	r.Get("/products/{id}", h.detail)
}

// ── DTO ──────────────────────────────────────────
//
// DTO 字段与 openapi/components/schemas/product.yaml 严格对应。
//
// 注意这里没有 CostPrice：上游成本价属于内部数据。
// 转换时逐个字段显式挑选，而不是直接序列化领域模型，
// 这样新增内部字段时不会意外泄露给用户端。

type productDTO struct {
	ID                      int64      `json:"id"`
	Name                    string     `json:"name"`
	Brand                   string     `json:"brand"`
	ValidationType          string     `json:"validationType"`
	WildcardSupported       bool       `json:"wildcardSupported"`
	IPSupported             bool       `json:"ipSupported"`
	MultiDomainSupported    bool       `json:"multiDomainSupported"`
	MinSans                 *int       `json:"minSans"`
	MaxSans                 *int       `json:"maxSans"`
	Years                   []int      `json:"years"`
	KeyAlgorithms           []string   `json:"keyAlgorithms"`
	DcvMethods              []string   `json:"dcvMethods"`
	ReissueSupported        bool       `json:"reissueSupported"`
	CancelSupported         bool       `json:"cancelSupported"`
	RequireOrganizationInfo bool       `json:"requireOrganizationInfo"`
	RecommendTag            *string    `json:"recommendTag"`
	Prices                  []priceDTO `json:"prices"`
}

type priceDTO struct {
	Years         int    `json:"years"`
	RetailPrice   int64  `json:"retailPrice"`
	OriginalPrice *int64 `json:"originalPrice"`
}

type productListData struct {
	Items []productDTO `json:"items"`
}

func toDTO(p Product) productDTO {
	algorithms := make([]string, 0, len(p.KeyAlgorithms))
	for _, a := range p.KeyAlgorithms {
		algorithms = append(algorithms, string(a))
	}

	methods := make([]string, 0, len(p.DcvMethods))
	for _, m := range p.DcvMethods {
		methods = append(methods, string(m))
	}

	prices := make([]priceDTO, 0, len(p.Prices))
	for _, price := range p.Prices {
		dto := priceDTO{
			Years:       price.Years,
			RetailPrice: price.RetailPrice.Cents(),
		}
		if price.OriginalPrice != nil {
			value := price.OriginalPrice.Cents()
			dto.OriginalPrice = &value
		}
		prices = append(prices, dto)
	}

	var tag *string
	if p.RecommendTag != "" {
		value := p.RecommendTag
		tag = &value
	}

	return productDTO{
		ID:                      p.ID,
		Name:                    p.Name,
		Brand:                   p.Brand,
		ValidationType:          string(p.ValidationType),
		WildcardSupported:       p.WildcardSupported,
		IPSupported:             p.IPSupported,
		MultiDomainSupported:    p.MultiDomainSupported,
		MinSans:                 p.MinSans,
		MaxSans:                 p.MaxSans,
		Years:                   p.Years(),
		KeyAlgorithms:           algorithms,
		DcvMethods:              methods,
		ReissueSupported:        p.ReissueSupported,
		CancelSupported:         p.CancelSupported,
		RequireOrganizationInfo: p.RequireOrgInfo,
		RecommendTag:            tag,
		Prices:                  prices,
	}
}

// ── 处理函数 ──────────────────────────────────────

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := Filter{
		ValidationType: query.Get("validationType"),
		Brand:          query.Get("brand"),
	}

	if filter.ValidationType != "" && !rules.ValidationType(filter.ValidationType).Valid() {
		httpx.Fail(w, r, errs.New(errs.CodeInvalidParam).
			WithField("validationType", "只能是 dv、ov 或 ev"))
		return
	}

	products, err := h.svc.List(r.Context(), filter)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	items := make([]productDTO, 0, len(products))
	for _, p := range products {
		items = append(items, toDTO(p))
	}

	httpx.OK(w, r, productListData{Items: items})
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		httpx.Fail(w, r, errs.New(errs.CodeInvalidParam).
			WithField("id", "必须是正整数"))
		return
	}

	p, err := h.svc.Get(r.Context(), id)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	httpx.OK(w, r, toDTO(*p))
}
