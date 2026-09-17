package order

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
	"github.com/cangerx/c-ssl/server/internal/domain/orderstate"
	"github.com/cangerx/c-ssl/server/internal/platform/mysql"
	"github.com/cangerx/c-ssl/server/internal/platform/tx"
	"github.com/cangerx/c-ssl/server/internal/product/rules"
)

// Repository 负责订单、域名、证书与上游事件的数据访问。
//
// 所有读写都经过 tx.Of(ctx, r.db) 取执行器，而不是直接用 r.db：
// 下单要在同一个事务里建单、冻结余额、推进状态，任何一步绕过事务
// 都会产生半提交——例如「钱冻了但没有订单」。
type Repository struct {
	db *sql.DB
}

// NewRepository 构造仓储。
func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

const orderColumns = `id, order_no, user_id, product_id, product_name, brand,
	validation_type, upstream_product_id, years, key_algorithm, amount,
	cost_price, status, upstream_order_no, cert_id, upstream_order_status,
	upstream_cert_status, upstream_prepare_status, upstream_reissue_status,
	csr, failure_reason, cancel_reason, submitted_at, issued_at, expires_at,
	created_at, updated_at`

const domainColumns = `id, order_no, domain, is_wildcard, is_primary, status,
	dcv_method, dns_record_type, dns_record_name, dns_record_value,
	file_path, file_content, email_addresses, verified_at, dcv_expires_at,
	created_at, updated_at`

const certificateColumns = `id, order_no, cert_id, status, common_name, domains,
	key_algorithm, serial_number, certificate, ca_bundle, issued_at, expires_at,
	created_at, updated_at`

// ── 订单 ──────────────────────────────────────────

// Create 插入订单及其联系人、企业信息与域名。
//
// 全部写在调用方的事务里：建单与冻结余额必须原子，
// 否则会出现「钱冻了但订单不存在」或者反过来。
func (r *Repository) Create(ctx context.Context, o *Order) error {
	exec := tx.Of(ctx, r.db)

	result, err := exec.ExecContext(ctx,
		`INSERT INTO certificate_orders
			(order_no, user_id, product_id, product_name, brand, validation_type,
			 upstream_product_id, years, key_algorithm, amount, cost_price,
			 status, upstream_order_no, cert_id, csr)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.OrderNo, o.UserID, o.ProductID, o.ProductName, o.Brand,
		string(o.ValidationType), o.UpstreamProductID, o.Years,
		string(o.KeyAlgorithm), o.Amount, o.CostPrice, string(o.Status),
		nullIfEmpty(o.UpstreamOrderNo), o.CertID, nullIfEmpty(o.CSR))
	if err != nil {
		return fmt.Errorf("创建订单失败: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return fmt.Errorf("获取订单 ID 失败: %w", err)
	}
	o.ID = id

	if o.Contact != nil {
		if _, err := exec.ExecContext(ctx,
			`INSERT INTO order_contacts (order_no, name, email, phone, title)
			 VALUES (?, ?, ?, ?, ?)`,
			o.OrderNo, o.Contact.Name, o.Contact.Email,
			o.Contact.Phone, o.Contact.Title); err != nil {
			return fmt.Errorf("写入订单联系人失败: %w", err)
		}
	}
	if o.Organization != nil {
		org := o.Organization
		if _, err := exec.ExecContext(ctx,
			`INSERT INTO order_orgs
				(order_no, name, registration_no, country, province, city,
				 address, postal_code, phone)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			o.OrderNo, org.Name, org.RegistrationNo, org.Country, org.Province,
			org.City, org.Address, org.PostalCode, org.Phone); err != nil {
			return fmt.Errorf("写入企业信息失败: %w", err)
		}
	}
	if len(o.Domains) > 0 {
		if err := r.insertDomains(ctx, o.OrderNo, o.Domains); err != nil {
			return err
		}
	}
	return nil
}

// GetByNo 按单号读取订单，不存在时返回 ErrOrderNotFound。
//
// 返回的订单**不含**域名、联系人与企业信息：调用方需要时单独取。
// 一次查询就 join 四张表会让每次读取都付出不必要的代价，
// 而多数场景（改状态、算金额）根本用不到那些数据。
func (r *Repository) GetByNo(ctx context.Context, orderNo string) (*Order, error) {
	row := tx.Of(ctx, r.db).QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM certificate_orders WHERE order_no = ?`, orderNo)
	return scanOrder(row)
}

// LockByNo 按单号锁定订单，不存在时返回 ErrOrderNotFound。
//
// 必须是 SELECT ... FOR UPDATE：普通 SELECT 在 REPEATABLE READ 下读的是
// 事务快照，读不到并发事务刚提交的状态，状态机校验会基于过期数据通过。
func (r *Repository) LockByNo(ctx context.Context, orderNo string) (*Order, error) {
	row := tx.Of(ctx, r.db).QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM certificate_orders WHERE order_no = ? FOR UPDATE`, orderNo)
	return scanOrder(row)
}

// GetByUpstreamOrderNo 按**上游**订单号读取订单。
//
// 上游回调里带的是它自己的订单号，不是平台订单号。两者搞混会让
// 每一次事件处理都报「订单不存在」，而事件看起来又收到并落库了。
func (r *Repository) GetByUpstreamOrderNo(ctx context.Context, upstreamOrderNo string) (*Order, error) {
	row := tx.Of(ctx, r.db).QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM certificate_orders WHERE upstream_order_no = ?`,
		upstreamOrderNo)
	return scanOrder(row)
}

// GetContact 读取订单联系人，没有时返回 nil。
func (r *Repository) GetContact(ctx context.Context, orderNo string) (*Contact, error) {
	var c Contact
	err := tx.Of(ctx, r.db).QueryRowContext(ctx,
		`SELECT name, email, phone, title FROM order_contacts WHERE order_no = ?`,
		orderNo).Scan(&c.Name, &c.Email, &c.Phone, &c.Title)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取订单联系人失败: %w", err)
	}
	return &c, nil
}

// GetOrganization 读取订单企业信息，没有时返回 nil（DV 订单没有这一行）。
func (r *Repository) GetOrganization(ctx context.Context, orderNo string) (*Organization, error) {
	var o Organization
	err := tx.Of(ctx, r.db).QueryRowContext(ctx,
		`SELECT name, registration_no, country, province, city, address,
		        postal_code, phone
		 FROM order_orgs WHERE order_no = ?`, orderNo).
		Scan(&o.Name, &o.RegistrationNo, &o.Country, &o.Province, &o.City,
			&o.Address, &o.PostalCode, &o.Phone)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取企业信息失败: %w", err)
	}
	return &o, nil
}

func scanOrder(row *sql.Row) (*Order, error) {
	var (
		o              Order
		validationType string
		keyAlgorithm   string
		status         string
		upstreamNo     sql.NullString
		csr            sql.NullString
		submittedAt    sql.NullTime
		issuedAt       sql.NullTime
		expiresAt      sql.NullTime
	)
	err := row.Scan(
		&o.ID, &o.OrderNo, &o.UserID, &o.ProductID, &o.ProductName, &o.Brand,
		&validationType, &o.UpstreamProductID, &o.Years, &keyAlgorithm,
		&o.Amount, &o.CostPrice,
		&status, &upstreamNo, &o.CertID, &o.UpstreamOrderStatus,
		&o.UpstreamCertStatus, &o.UpstreamPrepareStatus, &o.UpstreamReissueStatus,
		&csr, &o.FailureReason, &o.CancelReason,
		&submittedAt, &issuedAt, &expiresAt, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOrderNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取订单失败: %w", err)
	}

	o.ValidationType = rules.ValidationType(validationType)
	o.KeyAlgorithm = rules.KeyAlgorithm(keyAlgorithm)
	o.Status = orderstate.State(status)
	o.UpstreamOrderNo = upstreamNo.String
	o.CSR = csr.String
	o.SubmittedAt = nullTimePtr(submittedAt)
	o.IssuedAt = nullTimePtr(issuedAt)
	o.ExpiresAt = nullTimePtr(expiresAt)
	return &o, nil
}

// ListByUser 按用户分页读取订单。
func (r *Repository) ListByUser(ctx context.Context, userID int64, f Filter) (*Page, error) {
	query := `SELECT ` + orderColumns + ` FROM certificate_orders WHERE user_id = ?`
	args := []any{userID}

	if f.Cursor > 0 {
		query += ` AND id < ?`
		args = append(args, f.Cursor)
	}
	if f.Status != "" {
		query += ` AND status = ?`
		args = append(args, string(f.Status))
	}
	// 多取一条用来判断还有没有下一页，避免额外的 COUNT 查询
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limit+1)

	rows, err := tx.Of(ctx, r.db).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询订单列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := &Page{Items: []Order{}}
	for rows.Next() {
		o, err := scanOrderRows(rows)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, *o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历订单列表失败: %w", err)
	}

	if len(page.Items) > f.Limit {
		page.Items = page.Items[:f.Limit]
		page.NextCursor = page.Items[len(page.Items)-1].ID
	}
	return page, nil
}

func scanOrderRows(rows *sql.Rows) (*Order, error) {
	var (
		o              Order
		validationType string
		keyAlgorithm   string
		status         string
		upstreamNo     sql.NullString
		csr            sql.NullString
		submittedAt    sql.NullTime
		issuedAt       sql.NullTime
		expiresAt      sql.NullTime
	)
	err := rows.Scan(
		&o.ID, &o.OrderNo, &o.UserID, &o.ProductID, &o.ProductName, &o.Brand,
		&validationType, &o.UpstreamProductID, &o.Years, &keyAlgorithm,
		&o.Amount, &o.CostPrice,
		&status, &upstreamNo, &o.CertID, &o.UpstreamOrderStatus,
		&o.UpstreamCertStatus, &o.UpstreamPrepareStatus, &o.UpstreamReissueStatus,
		&csr, &o.FailureReason, &o.CancelReason,
		&submittedAt, &issuedAt, &expiresAt, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("读取订单行失败: %w", err)
	}

	o.ValidationType = rules.ValidationType(validationType)
	o.KeyAlgorithm = rules.KeyAlgorithm(keyAlgorithm)
	o.Status = orderstate.State(status)
	o.UpstreamOrderNo = upstreamNo.String
	o.CSR = csr.String
	o.SubmittedAt = nullTimePtr(submittedAt)
	o.IssuedAt = nullTimePtr(issuedAt)
	o.ExpiresAt = nullTimePtr(expiresAt)
	return &o, nil
}

// AdvanceStatus 把订单从 from 推进到 to。
//
// 带 from 条件而不是无条件更新：状态机的合法性由服务层用 Path 判定，
// 这里的 from 条件挡住的是「读到的状态在写入前被别人改了」——
// 那意味着两个流程在同时操作同一张单，应当失败而不是覆盖。
func (r *Repository) AdvanceStatus(
	ctx context.Context, orderNo string, from, to orderstate.State,
) error {
	result, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`UPDATE certificate_orders SET status = ? WHERE order_no = ? AND status = ?`,
		string(to), orderNo, string(from))
	if err != nil {
		return fmt.Errorf("更新订单状态失败: %w", err)
	}
	return expectOneRow(result, fmt.Sprintf("订单 %s 状态 %s → %s", orderNo, from, to))
}

// SetUpstream 写入上游订单号、成本价与上游状态。
//
// cost 是平台付给上游的钱，只用于运营对账，不参与任何用户余额计算。
func (r *Repository) SetUpstream(
	ctx context.Context, orderNo, upstreamOrderNo string, cost money.Amount, status string,
) error {
	now := time.Now().UTC()
	result, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`UPDATE certificate_orders
		 SET upstream_order_no = ?, cost_price = ?, upstream_order_status = ?,
		     submitted_at = ?
		 WHERE order_no = ?`,
		upstreamOrderNo, cost, status, now, orderNo)
	if err != nil {
		return fmt.Errorf("写入上游订单信息失败: %w", err)
	}
	return expectOneRow(result, "写入上游订单信息 "+orderNo)
}

// SyncUpstream 同步上游状态字段与证书编号。
//
// 四个原始状态一起写：它们各自独立变化，分开写会让中间态
// 出现「订单状态是新的、证书状态是旧的」这种对不上的组合。
func (r *Repository) SyncUpstream(
	ctx context.Context, orderNo string, st UpstreamSnapshot,
) error {
	result, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`UPDATE certificate_orders
		 SET upstream_order_status = ?, upstream_cert_status = ?,
		     upstream_prepare_status = ?, upstream_reissue_status = ?,
		     cert_id = ?, issued_at = COALESCE(?, issued_at),
		     expires_at = COALESCE(?, expires_at)
		 WHERE order_no = ?`,
		st.OrderStatus, st.CertStatus, st.PrepareStatus, st.ReissueStatus,
		st.CertID, st.IssuedAt, st.ExpiresAt, orderNo)
	if err != nil {
		return fmt.Errorf("同步上游状态失败: %w", err)
	}
	return expectOneRow(result, "同步上游状态 "+orderNo)
}

// UpstreamSnapshot 是一次上游状态的快照。
type UpstreamSnapshot struct {
	OrderStatus   string
	CertStatus    string
	PrepareStatus string
	ReissueStatus string
	CertID        string
	IssuedAt      *time.Time
	ExpiresAt     *time.Time
}

// MarkFailed 把订单置为失败并记录原因。
//
// reason 为空时不写库，见 MarkCancelled 的说明。
func (r *Repository) MarkFailed(ctx context.Context, orderNo, reason string) error {
	if reason = truncateRunes(reason, ReasonMaxLen); reason == "" {
		return nil
	}
	result, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`UPDATE certificate_orders SET failure_reason = ? WHERE order_no = ?`,
		reason, orderNo)
	if err != nil {
		return fmt.Errorf("记录失败原因失败: %w", err)
	}
	return expectOneRow(result, "记录失败原因 "+orderNo)
}

// MarkCancelled 记录取消原因。
//
// **reason 为空时直接返回，不写库。** 契约里取消的请求体是 required: false，
// 「用户没填原因」是最常见的情况。往一个已经是空串的列里再写一次空串
// 不会改变任何值，MySQL 会报告「影响 0 行」——把它当成错误会让
// 所有不带原因的取消都返回 500，而这是一个完全合法的请求。
//
// 影响行数在这里本来也不适合用来判断订单是否存在：调用方在同一个事务里
// 刚刚用带 from 条件的 AdvanceStatus 推进过状态，那一步已经保证了
// 行存在且状态符合预期。
func (r *Repository) MarkCancelled(ctx context.Context, orderNo, reason string) error {
	if reason = truncateRunes(reason, ReasonMaxLen); reason == "" {
		return nil
	}
	result, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`UPDATE certificate_orders SET cancel_reason = ? WHERE order_no = ?`,
		reason, orderNo)
	if err != nil {
		return fmt.Errorf("记录取消原因失败: %w", err)
	}
	return expectOneRow(result, "记录取消原因 "+orderNo)
}

// ── 域名 ──────────────────────────────────────────

func (r *Repository) insertDomains(ctx context.Context, orderNo string, domains []Domain) error {
	if len(domains) == 0 {
		return nil
	}
	exec := tx.Of(ctx, r.db)

	var sb strings.Builder
	sb.WriteString(`INSERT INTO order_domains
		(order_no, domain, is_wildcard, is_primary, status, dcv_method,
		 dns_record_type, dns_record_name, dns_record_value,
		 file_path, file_content, email_addresses, verified_at, dcv_expires_at)
		VALUES `)
	args := make([]any, 0, len(domains)*14)
	for i, d := range domains {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		emails, err := marshalEmails(d.Emails)
		if err != nil {
			return err
		}
		args = append(args,
			orderNo, d.Domain, d.Wildcard, d.Primary, string(d.Status),
			string(d.Method), d.Record.Type, d.Record.Name, d.Record.Value,
			d.File.Path, d.File.Content, emails, d.VerifiedAt, d.ExpiresAt)
	}

	if _, err := exec.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("写入订单域名失败: %w", err)
	}
	return nil
}

// ReplaceDomains 用新的域名材料整体替换订单的域名行。
//
// 用于重新生成 token：新 token 会同时改变 DNS 记录值与文件路径，
// 逐字段更新容易漏掉某个字段，留下「记录名是新的、值是旧的」这种组合。
func (r *Repository) ReplaceDomains(ctx context.Context, orderNo string, domains []Domain) error {
	exec := tx.Of(ctx, r.db)
	if _, err := exec.ExecContext(ctx,
		`DELETE FROM order_domains WHERE order_no = ?`, orderNo); err != nil {
		return fmt.Errorf("清除旧域名材料失败: %w", err)
	}
	return r.insertDomains(ctx, orderNo, domains)
}

// ListDomains 读取订单的域名材料，按主域名优先、ID 升序返回。
func (r *Repository) ListDomains(ctx context.Context, orderNo string) ([]Domain, error) {
	rows, err := tx.Of(ctx, r.db).QueryContext(ctx,
		`SELECT `+domainColumns+` FROM order_domains
		 WHERE order_no = ? ORDER BY is_primary DESC, id ASC`, orderNo)
	if err != nil {
		return nil, fmt.Errorf("查询订单域名失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Domain{}
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历订单域名失败: %w", err)
	}
	return out, nil
}

// ListDomainNames 批量读取多个订单的域名名列表。
//
// 订单列表接口要展示域名，但不需要验证材料。这里刻意只 SELECT 域名：
// `file_content` 是 TEXT，逐个拉回来会让一次翻页传输大量用不到的正文。
// 一条查询解决整页也比「每个订单查一次域名表」的 N+1 便宜得多。
//
// 返回值以订单号为键；没有任何域名的订单不会出现在 map 里，
// 调用方按零值处理即可。
func (r *Repository) ListDomainNames(
	ctx context.Context, orderNos []string,
) (map[string][]string, error) {
	out := make(map[string][]string, len(orderNos))
	if len(orderNos) == 0 {
		return out, nil
	}

	placeholders := make([]string, len(orderNos))
	args := make([]any, len(orderNos))
	for i, no := range orderNos {
		placeholders[i] = "?"
		args[i] = no
	}

	// 排序与 ListDomains 一致：主域名在前。列表页展示的是同一个域名集合，
	// 两处顺序不同会让用户在列表和详情里看到不一样的排列。
	rows, err := tx.Of(ctx, r.db).QueryContext(ctx, fmt.Sprintf(
		`SELECT order_no, domain FROM order_domains
		 WHERE order_no IN (%s) ORDER BY order_no ASC, is_primary DESC, id ASC`,
		strings.Join(placeholders, ", ")), args...)
	if err != nil {
		return nil, fmt.Errorf("批量查询订单域名失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var orderNo, domain string
		if err := rows.Scan(&orderNo, &domain); err != nil {
			return nil, fmt.Errorf("读取订单域名行失败: %w", err)
		}
		out[orderNo] = append(out[orderNo], domain)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历订单域名失败: %w", err)
	}
	return out, nil
}

func scanDomain(rows *sql.Rows) (*Domain, error) {
	var (
		d          Domain
		method     string
		status     string
		emails     sql.NullString
		verifiedAt sql.NullTime
		expiresAt  sql.NullTime
	)
	err := rows.Scan(
		&d.ID, &d.OrderNo, &d.Domain, &d.Wildcard, &d.Primary, &status,
		&method, &d.Record.Type, &d.Record.Name, &d.Record.Value,
		&d.File.Path, &d.File.Content, &emails, &verifiedAt, &expiresAt,
		&d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("读取域名行失败: %w", err)
	}

	d.Status = DomainStatus(status)
	d.Method = rules.DcvMethod(method)
	d.VerifiedAt = nullTimePtr(verifiedAt)
	d.ExpiresAt = nullTimePtr(expiresAt)

	if emails.Valid && emails.String != "" {
		if err := json.Unmarshal([]byte(emails.String), &d.Emails); err != nil {
			return nil, fmt.Errorf("解析域名 %s 的邮件地址失败: %w", d.Domain, err)
		}
	}
	if d.Emails == nil {
		d.Emails = []string{}
	}
	return &d, nil
}

// UpdateDomainMethod 记录域名选定的验证方式并推进状态。
func (r *Repository) UpdateDomainMethod(
	ctx context.Context, orderNo, domain string, method rules.DcvMethod, status DomainStatus,
) error {
	result, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`UPDATE order_domains SET dcv_method = ?, status = ?
		 WHERE order_no = ? AND domain = ?`,
		string(method), string(status), orderNo, domain)
	if err != nil {
		return fmt.Errorf("更新域名验证方式失败: %w", err)
	}
	return expectOneRow(result, fmt.Sprintf("更新域名 %s 的验证方式", domain))
}

// SyncDomainStatus 按上游返回的状态更新单个域名。
func (r *Repository) SyncDomainStatus(
	ctx context.Context, orderNo, domain string, status DomainStatus, verifiedAt *time.Time,
) error {
	result, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`UPDATE order_domains
		 SET status = ?, verified_at = COALESCE(?, verified_at)
		 WHERE order_no = ? AND domain = ?`,
		string(status), verifiedAt, orderNo, domain)
	if err != nil {
		return fmt.Errorf("同步域名状态失败: %w", err)
	}
	return expectOneRow(result, fmt.Sprintf("同步域名 %s 的状态", domain))
}

// ── 证书 ──────────────────────────────────────────

// UpsertCertificate 写入或更新证书记录。
//
// 用 ON DUPLICATE KEY UPDATE 而不是先查再插：重签会换 cert_id，
// 同一订单下有多张证书，而「同一个 cert_id 重复落库」是上游重推导致的，
// 应当幂等而不是报错。唯一键是 cert_id。
func (r *Repository) UpsertCertificate(ctx context.Context, c *Certificate) error {
	domains, err := json.Marshal(c.Domains)
	if err != nil {
		return fmt.Errorf("序列化证书域名失败: %w", err)
	}

	result, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`INSERT INTO certificates
			(order_no, cert_id, status, common_name, domains, key_algorithm,
			 serial_number, certificate, ca_bundle, issued_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
			status = VALUES(status), common_name = VALUES(common_name),
			domains = VALUES(domains), key_algorithm = VALUES(key_algorithm),
			serial_number = VALUES(serial_number), certificate = VALUES(certificate),
			ca_bundle = VALUES(ca_bundle), issued_at = VALUES(issued_at),
			expires_at = VALUES(expires_at)`,
		c.OrderNo, c.CertID, c.Status, c.CommonName, domains,
		string(c.KeyAlgorithm), c.SerialNumber, c.Certificate, c.CABundle,
		c.IssuedAt, c.ExpiresAt)
	if err != nil {
		return fmt.Errorf("写入证书失败: %w", err)
	}
	if id, err := result.LastInsertId(); err == nil && id > 0 {
		c.ID = id
	}
	return nil
}

// GetCertificate 读取订单最新的一张证书。
//
// 取最新而不是唯一：重签会在同一订单下产生第二张证书，
// 用户要下载的是当前有效的那张。按 ID 倒序取第一条。
func (r *Repository) GetCertificate(ctx context.Context, orderNo string) (*Certificate, error) {
	row := tx.Of(ctx, r.db).QueryRowContext(ctx,
		`SELECT `+certificateColumns+` FROM certificates
		 WHERE order_no = ? ORDER BY id DESC LIMIT 1`, orderNo)

	var (
		c            Certificate
		domains      []byte
		keyAlgorithm string
		issuedAt     sql.NullTime
		expiresAt    sql.NullTime
	)
	err := row.Scan(&c.ID, &c.OrderNo, &c.CertID, &c.Status, &c.CommonName,
		&domains, &keyAlgorithm, &c.SerialNumber, &c.Certificate, &c.CABundle,
		&issuedAt, &expiresAt, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取证书失败: %w", err)
	}

	c.KeyAlgorithm = rules.KeyAlgorithm(keyAlgorithm)
	c.IssuedAt = nullTimePtr(issuedAt)
	c.ExpiresAt = nullTimePtr(expiresAt)
	if err := json.Unmarshal(domains, &c.Domains); err != nil {
		return nil, fmt.Errorf("解析证书域名失败: %w", err)
	}
	return &c, nil
}

// ── 上游事件 ──────────────────────────────────────

// InsertEvent 写入一条上游事件，返回它是否是一次重复投递。
//
// 幂等靠 event_key 的唯一索引拦下，而不是「先查有没有、没有就插」——
// 后者中间有竞态窗口，并发的两次重复投递会双双通过检查。
func (r *Repository) InsertEvent(ctx context.Context, e *WebhookEvent) (bool, error) {
	result, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`INSERT INTO webhook_events
			(provider, event_key, event_type, upstream_order_no, upstream_status,
			 payload_hash, payload, occurred_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Provider, e.EventKey, e.EventType, e.UpstreamOrderNo, e.UpstreamStatus,
		e.PayloadHash, e.Payload, e.OccurredAt)
	if err == nil {
		if id, idErr := result.LastInsertId(); idErr == nil {
			e.ID = id
		}
		return false, nil
	}

	if !isDuplicateKey(err) {
		return false, fmt.Errorf("写入上游事件失败: %w", err)
	}
	// 撞唯一索引 = 这条事件已经收到过。上游重推是常态，不算错误。
	return true, nil
}

// MarkEventProcessed 记录事件的处理结果。
func (r *Repository) MarkEventProcessed(
	ctx context.Context, id int64, status, processErr string,
) error {
	now := time.Now().UTC()
	var processedAt any
	if status != EventPending {
		processedAt = now
	}
	result, err := tx.Of(ctx, r.db).ExecContext(ctx,
		`UPDATE webhook_events
		 SET process_status = ?, process_error = ?, attempts = attempts + 1,
		     processed_at = ?
		 WHERE id = ?`,
		status, truncateRunes(processErr, 255), processedAt, id)
	if err != nil {
		return fmt.Errorf("更新事件处理状态失败: %w", err)
	}
	return expectOneRow(result, fmt.Sprintf("更新事件 %d 的处理状态", id))
}

// GetEventByKey 按幂等键读取事件，不存在时返回 nil。
//
// 用于判断一次重推要不要重新处理：已经 done 的跳过，
// 上次 failed 的正好借这次重推再试一遍。
func (r *Repository) GetEventByKey(ctx context.Context, eventKey string) (*WebhookEvent, error) {
	var (
		e           WebhookEvent
		occurredAt  sql.NullTime
		processedAt sql.NullTime
	)
	err := tx.Of(ctx, r.db).QueryRowContext(ctx,
		`SELECT id, provider, event_key, event_type, upstream_order_no,
		        upstream_status, payload_hash, payload, occurred_at,
		        received_at, process_status, process_error, attempts, processed_at
		 FROM webhook_events WHERE event_key = ?`, eventKey).
		Scan(&e.ID, &e.Provider, &e.EventKey, &e.EventType, &e.UpstreamOrderNo,
			&e.UpstreamStatus, &e.PayloadHash, &e.Payload, &occurredAt,
			&e.ReceivedAt, &e.ProcessStatus, &e.ProcessError, &e.Attempts, &processedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取上游事件失败: %w", err)
	}
	e.OccurredAt = nullTimePtr(occurredAt)
	e.ProcessedAt = nullTimePtr(processedAt)
	return &e, nil
}

// ── 辅助 ──────────────────────────────────────────

// expectOneRow 断言一次写入恰好影响一行。
//
// 影响 0 行说明目标行不存在或状态已被别人改走，两种情况都不该静默通过：
// 静默通过会让调用方以为操作成功了，而数据库里什么都没发生。
func expectOneRow(result sql.Result, what string) error {
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取 %s 的影响行数失败: %w", what, err)
	}
	if n != 1 {
		return fmt.Errorf("%s 影响了 %d 行，期望 1 行", what, n)
	}
	return nil
}

// isDuplicateKey 判断错误是否为唯一索引冲突。
//
// 包一层是为了让「为什么用错误码而不是先查再插」有个注释落点：
// 先查再插中间有竞态窗口，并发的两次重复投递会双双通过检查。
func isDuplicateKey(err error) bool {
	return mysql.IsDuplicateKey(err)
}

func marshalEmails(emails []string) (any, error) {
	if len(emails) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(emails)
	if err != nil {
		return nil, fmt.Errorf("序列化邮件地址失败: %w", err)
	}
	return string(b), nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	utc := t.Time.UTC()
	return &utc
}

// truncateRunes 按字符截断，避免截出半个汉字导致 MySQL 报
// Incorrect string value。
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
