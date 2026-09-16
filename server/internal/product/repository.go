package product

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
	"github.com/cangerx/c-ssl/server/internal/product/rules"
)

// Filter 是产品列表的查询条件。
type Filter struct {
	ValidationType string
	Brand          string
	// IncludeOffShelf 为 true 时包含已下架产品，仅供后台使用。
	IncludeOffShelf bool
}

// Repository 负责产品的数据访问。
type Repository struct {
	db *sql.DB
}

// NewRepository 构造仓储。
func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const productColumns = `
	p.id, p.name, p.brand, p.validation_type,
	p.wildcard_supported, p.ip_supported, p.multi_domain_supported,
	p.min_sans, p.max_sans, p.key_algorithms, p.dcv_methods,
	p.reissue_supported, p.cancel_supported, p.require_organization_info,
	p.recommend_tag, p.upstream_product_id, p.status, p.sort_order,
	p.created_at, p.updated_at`

// List 按条件查询产品，并批量载入价格。
func (r *Repository) List(ctx context.Context, filter Filter) ([]Product, error) {
	query := strings.Builder{}
	query.WriteString("SELECT ")
	query.WriteString(productColumns)
	query.WriteString(" FROM products p WHERE 1 = 1")

	args := make([]any, 0, 2)

	if !filter.IncludeOffShelf {
		query.WriteString(" AND p.status = ?")
		args = append(args, string(StatusActive))
	}
	if filter.ValidationType != "" {
		query.WriteString(" AND p.validation_type = ?")
		args = append(args, filter.ValidationType)
	}
	if filter.Brand != "" {
		query.WriteString(" AND p.brand = ?")
		args = append(args, filter.Brand)
	}
	query.WriteString(" ORDER BY p.sort_order ASC, p.id ASC")

	rows, err := r.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("查询产品列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	products, err := scanProducts(rows)
	if err != nil {
		return nil, err
	}
	if err := r.attachPrices(ctx, products); err != nil {
		return nil, err
	}
	return products, nil
}

// GetByID 按 ID 查询单个产品。
func (r *Repository) GetByID(ctx context.Context, id int64) (*Product, error) {
	query := "SELECT " + productColumns + " FROM products p WHERE p.id = ?"

	rows, err := r.db.QueryContext(ctx, query, id)
	if err != nil {
		return nil, fmt.Errorf("查询产品失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	products, err := scanProducts(rows)
	if err != nil {
		return nil, err
	}
	if len(products) == 0 {
		return nil, nil
	}
	if err := r.attachPrices(ctx, products); err != nil {
		return nil, err
	}
	return &products[0], nil
}

// attachPrices 一次性把价格装载到全部产品上，避免逐个产品查价导致 N+1。
func (r *Repository) attachPrices(ctx context.Context, products []Product) error {
	if len(products) == 0 {
		return nil
	}

	placeholders := make([]string, len(products))
	args := make([]any, len(products))
	index := make(map[int64]int, len(products))

	for i := range products {
		placeholders[i] = "?"
		args[i] = products[i].ID
		index[products[i].ID] = i
	}

	query := fmt.Sprintf(`
		SELECT product_id, years, cost_price, retail_price, original_price
		FROM product_prices
		WHERE product_id IN (%s)
		ORDER BY product_id ASC, years ASC`, strings.Join(placeholders, ", "))

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("查询产品价格失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			productID     int64
			years         int
			costPrice     int64
			retailPrice   int64
			originalPrice sql.NullInt64
		)
		if err := rows.Scan(&productID, &years, &costPrice, &retailPrice, &originalPrice); err != nil {
			return fmt.Errorf("解析产品价格失败: %w", err)
		}

		price := Price{
			Years:       years,
			CostPrice:   money.Amount(costPrice),
			RetailPrice: money.Amount(retailPrice),
		}
		if originalPrice.Valid {
			value := money.Amount(originalPrice.Int64)
			price.OriginalPrice = &value
		}

		if idx, ok := index[productID]; ok {
			products[idx].Prices = append(products[idx].Prices, price)
		}
	}
	return rows.Err()
}

func scanProducts(rows *sql.Rows) ([]Product, error) {
	var products []Product

	for rows.Next() {
		var (
			p              Product
			validationType string
			keyAlgorithms  string
			dcvMethods     string
			minSans        sql.NullInt64
			maxSans        sql.NullInt64
			recommendTag   sql.NullString
			upstreamID     sql.NullInt64
			status         string
		)

		err := rows.Scan(
			&p.ID, &p.Name, &p.Brand, &validationType,
			&p.WildcardSupported, &p.IPSupported, &p.MultiDomainSupported,
			&minSans, &maxSans, &keyAlgorithms, &dcvMethods,
			&p.ReissueSupported, &p.CancelSupported, &p.RequireOrgInfo,
			&recommendTag, &upstreamID, &status, &p.SortOrder,
			&p.CreatedAt, &p.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("解析产品记录失败: %w", err)
		}

		p.ValidationType = rules.ValidationType(validationType)
		p.KeyAlgorithms = parseAlgorithms(keyAlgorithms)
		p.DcvMethods = parseDcvMethods(dcvMethods)
		p.Status = Status(status)

		if minSans.Valid {
			v := int(minSans.Int64)
			p.MinSans = &v
		}
		if maxSans.Valid {
			v := int(maxSans.Int64)
			p.MaxSans = &v
		}
		if recommendTag.Valid {
			p.RecommendTag = recommendTag.String
		}
		if upstreamID.Valid {
			v := int(upstreamID.Int64)
			p.UpstreamProductID = &v
		}

		products = append(products, p)
	}

	return products, rows.Err()
}

// 枚举串用逗号分隔存储，解析时跳过空项，容忍手工改库留下的多余逗号。
func parseAlgorithms(raw string) []rules.KeyAlgorithm {
	parts := splitEnum(raw)
	out := make([]rules.KeyAlgorithm, 0, len(parts))
	for _, part := range parts {
		out = append(out, rules.KeyAlgorithm(part))
	}
	return out
}

func parseDcvMethods(raw string) []rules.DcvMethod {
	parts := splitEnum(raw)
	out := make([]rules.DcvMethod, 0, len(parts))
	for _, part := range parts {
		out = append(out, rules.DcvMethod(part))
	}
	return out
}

func splitEnum(raw string) []string {
	fields := strings.Split(raw, ",")
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
