package product

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/product/rules"
)

// Service 是产品域的业务入口。
type Service struct {
	repo *Repository
}

// NewService 构造服务。
func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// List 返回上架产品列表。
func (s *Service) List(ctx context.Context, filter Filter) ([]Product, error) {
	products, err := s.repo.List(ctx, filter)
	if err != nil {
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}

	out := make([]Product, 0, len(products))
	for i := range products {
		normalized, err := normalize(&products[i])
		if err != nil {
			// 单个产品配置有问题不应拖垮整个列表，跳过并记录
			slog.WarnContext(ctx, "跳过配置异常的产品",
				"product_id", products[i].ID,
				"error", err,
			)
			continue
		}
		out = append(out, *normalized)
	}
	return out, nil
}

// Get 返回单个产品。产品不存在或已下架时返回 1003。
func (s *Service) Get(ctx context.Context, id int64) (*Product, error) {
	p, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}
	if p == nil {
		return nil, errs.New(errs.CodeNotFound).WithCause(fmt.Errorf("产品 %d 不存在", id))
	}
	if p.Status != StatusActive {
		// 对用户端而言，下架等同于不存在，不暴露"存在但下架"这一信息
		return nil, errs.New(errs.CodeNotFound).WithCause(fmt.Errorf("产品 %d 已下架", id))
	}

	normalized, err := normalize(p)
	if err != nil {
		return nil, errs.New(errs.CodeInternal).WithCause(err)
	}
	return normalized, nil
}

// normalize 对从库里读出的产品施加硬约束。
//
// 这是最后一道防线：即使有人绕过后台直接改库，把 EV 产品的
// wildcard_supported 写成 1，这里也会改回 0 并留下日志。
func normalize(p *Product) (*Product, error) {
	capability := p.Capability()

	corrections := rules.Normalize(&capability)
	if len(corrections) > 0 {
		attrs := make([]any, 0, len(corrections)*2+2)
		attrs = append(attrs, "product_id", p.ID)
		for _, c := range corrections {
			attrs = append(attrs, c.Field, c.Reason)
		}
		slog.Warn("产品配置违反硬约束，已强制修正", attrs...)
	}

	p.ApplyCapability(capability)

	if err := rules.Validate(capability); err != nil {
		return nil, fmt.Errorf("产品 %d 配置非法: %w", p.ID, err)
	}
	return p, nil
}
