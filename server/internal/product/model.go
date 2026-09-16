// Package product 是产品目录域。
//
// 目录结构遵循垂直切片约定：model / repository / service / handler 同包，
// 规则判断委托给 rules 子包。
package product

import (
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
	"github.com/cangerx/c-ssl/server/internal/product/rules"
)

// Status 是产品上下架状态。
type Status string

const (
	// StatusActive 已上架，用户端可见。
	StatusActive Status = "active"
	// StatusOffShelf 已下架，仅后台可见。
	StatusOffShelf Status = "off_shelf"
)

// Price 是某一年限的价格。
//
// CostPrice 是上游成本价，属于内部数据，任何面向用户的接口都不得返回。
// 转换 DTO 时必须显式挑选字段，不要直接序列化本结构。
type Price struct {
	Years         int
	CostPrice     money.Amount
	RetailPrice   money.Amount
	OriginalPrice *money.Amount
}

// Product 是产品聚合。
type Product struct {
	ID                   int64
	Name                 string
	Brand                string
	ValidationType       rules.ValidationType
	WildcardSupported    bool
	IPSupported          bool
	MultiDomainSupported bool
	MinSans              *int
	MaxSans              *int
	KeyAlgorithms        []rules.KeyAlgorithm
	DcvMethods           []rules.DcvMethod
	ReissueSupported     bool
	CancelSupported      bool
	RequireOrgInfo       bool
	RecommendTag         string
	UpstreamProductID    *int
	Status               Status
	SortOrder            int
	Prices               []Price
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Capability 提取规则相关的字段，供 rules 包使用。
func (p Product) Capability() rules.Capability {
	return rules.Capability{
		Brand:                p.Brand,
		ValidationType:       p.ValidationType,
		WildcardSupported:    p.WildcardSupported,
		IPSupported:          p.IPSupported,
		MultiDomainSupported: p.MultiDomainSupported,
		MinSans:              p.MinSans,
		MaxSans:              p.MaxSans,
		KeyAlgorithms:        p.KeyAlgorithms,
		DcvMethods:           p.DcvMethods,
		ReissueSupported:     p.ReissueSupported,
		CancelSupported:      p.CancelSupported,
		RequireOrgInfo:       p.RequireOrgInfo,
		IsFree:               p.IsFree(),
	}
}

// ApplyCapability 把（可能被 rules 修正过的）能力字段写回产品。
func (p *Product) ApplyCapability(cap rules.Capability) {
	p.WildcardSupported = cap.WildcardSupported
	p.IPSupported = cap.IPSupported
	p.MultiDomainSupported = cap.MultiDomainSupported
	p.MinSans = cap.MinSans
	p.MaxSans = cap.MaxSans
	p.KeyAlgorithms = cap.KeyAlgorithms
	p.DcvMethods = cap.DcvMethods
	p.ReissueSupported = cap.ReissueSupported
	p.CancelSupported = cap.CancelSupported
	p.RequireOrgInfo = cap.RequireOrgInfo
}

// Years 返回可购买的年限，取自价格表并按升序排列。
func (p Product) Years() []int {
	out := make([]int, 0, len(p.Prices))
	for _, price := range p.Prices {
		out = append(out, price.Years)
	}
	return out
}

// IsFree 判断是否为免费产品（零售价为 0）。
func (p Product) IsFree() bool {
	if len(p.Prices) == 0 {
		return false
	}
	for _, price := range p.Prices {
		if !price.RetailPrice.IsZero() {
			return false
		}
	}
	return true
}
