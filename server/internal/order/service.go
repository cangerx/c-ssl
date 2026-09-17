package order

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/ids"
	"github.com/cangerx/c-ssl/server/internal/domain/orderstate"
	"github.com/cangerx/c-ssl/server/internal/platform/tx"
	"github.com/cangerx/c-ssl/server/internal/product"
	"github.com/cangerx/c-ssl/server/internal/product/rules"
	"github.com/cangerx/c-ssl/server/internal/upstream/foxssl"
	"github.com/cangerx/c-ssl/server/internal/wallet"
)

// Wallet 是本域需要的钱包能力。
//
// 刻意定义成本域自己的窄接口，而不是直接依赖 *wallet.Service：
// 订单只需要冻结、结算、解冻三个动作，接口收窄后钱包新增的方法
// 不会自动成为订单可以调用的能力。
type Wallet interface {
	Freeze(ctx context.Context, in wallet.ChangeInput) (*wallet.Result, error)
	Settle(ctx context.Context, in wallet.ChangeInput) (*wallet.Result, error)
	Unfreeze(ctx context.Context, in wallet.ChangeInput) (*wallet.Result, error)
	Refund(ctx context.Context, in wallet.ChangeInput) (*wallet.Result, error)
}

// Products 是本域需要的产品能力。
//
// 只在**下单时**用到：价格与上游产品编号都取自产品，
// 之后一律使用订单上的快照。域名验证阶段刻意不读产品——
// 产品可能已被下架或改配置，而在途订单还要继续走完。
type Products interface {
	Get(ctx context.Context, id int64) (*product.Product, error)
}

// Service 是订单域的业务入口。
type Service struct {
	repo     *Repository
	txm      *tx.Manager
	wallet   Wallet
	products Products
	upstream foxssl.Client
	// clock 可被测试替换，让与时间相关的分支能被确定性地覆盖。
	clock func() time.Time
}

// NewService 构造服务。
func NewService(
	repo *Repository, walletSvc Wallet, products Products, upstream foxssl.Client,
) *Service {
	return &Service{
		repo:     repo,
		txm:      tx.NewManager(repo.db),
		wallet:   walletSvc,
		products: products,
		upstream: upstream,
		clock:    time.Now,
	}
}

// ── 下单 ──────────────────────────────────────────

// Create 创建订单并向上游提交签发请求。
//
// 全流程被切成三段，中间夹着一次网络调用：
//
//	事务①：建单 + 冻结金额 + pending_payment → paid
//	事务②：paid → submitting
//	网络  ：调用上游（绝不能放进事务，一次慢响应会长时间占住连接）
//	事务③：写上游订单号 + 冻结转实扣 + submitting → waiting_dcv
//	      或者：解冻 + submitting → failed
//
// 事务②单独存在是为了让「已经打算提交」这件事先落盘。崩溃发生在
// 事务②之前，订单停在 paid，可以确定上游没被调用过；发生在之后则停在
// submitting，此时上游可能已经建了单。两种情况的补偿动作不同，
// 合成一个事务就区分不出来了。
func (s *Service) Create(ctx context.Context, in CreateInput) (*Order, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	in.Normalize()

	p, err := s.products.Get(ctx, in.ProductID)
	if err != nil {
		return nil, err
	}
	cap := p.Capability()

	// 产品规则校验。用户选择必须落在产品允许的范围内，
	// 前端过滤只是体验，把关必须在后端——绕过前端直接调接口是最容易的事。
	if err := rules.ValidateSelection(cap, rules.Selection{
		Years:        in.Years,
		KeyAlgorithm: in.KeyAlgorithm,
		Domains:      in.Domains,
	}, p.Years()); err != nil {
		return nil, errs.Newf(errs.CodeInvalidParam, "%s", err).
			WithField("domains", err.Error())
	}

	// 企业信息：需要时必须给，不需要时也不能给。
	//
	// 多给了同样拒绝，而不是静默忽略：DV 产品的企业信息不会被提交给 CA，
	// 用户填了会以为「更正式、更容易过审」，而实际上什么都没发生。
	// 让请求失败比让用户抱着错误预期走完流程便宜。
	if cap.RequireOrgInfo && in.Organization == nil {
		return nil, errs.New(errs.CodeInvalidParam).
			WithField("organization", "该产品必须提交企业信息")
	}
	if !cap.RequireOrgInfo && in.Organization != nil {
		return nil, errs.New(errs.CodeInvalidParam).
			WithField("organization", "该产品不需要企业信息")
	}

	price, ok := findPrice(p, in.Years)
	if !ok {
		// ValidateSelection 已经保证年限在价格表里，走到这里说明产品数据不一致
		return nil, errs.New(errs.CodeInternal).
			WithCause(fmt.Errorf("产品 %d 缺少 %d 年的价格", p.ID, in.Years))
	}
	if p.UpstreamProductID == nil {
		return nil, errs.New(errs.CodeInternal).
			WithCause(fmt.Errorf("产品 %d 未配置上游产品编号", p.ID))
	}

	now := s.clock().UTC()
	o := &Order{
		OrderNo:           ids.NewOrderNo(now),
		UserID:            in.UserID,
		ProductID:         p.ID,
		ProductName:       p.Name,
		Brand:             p.Brand,
		ValidationType:    p.ValidationType,
		UpstreamProductID: *p.UpstreamProductID,
		Years:             in.Years,
		KeyAlgorithm:      in.KeyAlgorithm,
		Amount:            price.RetailPrice,
		Status:            orderstate.PendingPayment,
		CSR:               in.CSR,
		Contact:           &in.Contact,
		Organization:      in.Organization,
	}
	for i, d := range in.Domains {
		o.Domains = append(o.Domains, NewPendingDomain(o.OrderNo, d, i == 0))
	}

	// 事务①：建单 + 冻结 + 标记已支付。
	//
	// 冻结与建单必须在同一个事务里：只成其一就会出现「钱冻了但没有订单」
	// （用户凭空少一笔可用余额）或者「有订单但没冻结」（用户可以反复下单
	// 而不受限）。余额不足时 Freeze 返回 ErrInsufficientAvailable，
	// 整个事务回滚，订单不会留下。
	//
	// 冻结完成后立即推进到 paid：在钱包模型里「冻结」就是用户的付款动作，
	// 钱已经不可用了。真正的扣款（settle）要等上游受理，
	// 因为上游拒绝时要原路退回可用余额。
	err = s.txm.Run(ctx, func(ctx context.Context) error {
		if err := s.repo.Create(ctx, o); err != nil {
			return err
		}
		if err := s.freeze(ctx, o); err != nil {
			return err
		}
		return s.repo.AdvanceStatus(ctx, o.OrderNo,
			orderstate.PendingPayment, orderstate.Paid)
	})
	if err != nil {
		return nil, mapError(err)
	}
	o.Status = orderstate.Paid

	// 事务②：标记即将提交上游。
	if err := s.advance(ctx, o.OrderNo, orderstate.Paid, orderstate.Submitting); err != nil {
		return nil, mapError(err)
	}
	o.Status = orderstate.Submitting

	// 网络调用与事务③。
	if err := s.submit(ctx, o, p.Capability()); err != nil {
		return nil, mapError(err)
	}

	return s.reload(ctx, o.UserID, o.OrderNo)
}

// submit 调用上游并落结果。
//
// Create 与补偿重试（RetrySubmit）共用这一条路径，两者的差异只在于
// 入口处的状态校验——这样「上游调用成功后要做什么」只有一处实现，
// 不会出现补偿路径漏掉某个字段的情况。
//
// cap 是产品能力，只用来挑下单时要报给上游的初始验证方式，见 initialDcvMethod。
func (s *Service) submit(ctx context.Context, o *Order, cap rules.Capability) error {
	req := foxssl.CreateOrderRequest{
		// MerchantOrderNo 只用于日志与本地关联。
		// **它不是上游的幂等键**：上游下单接口不接受任何商户侧标识，
		// 重复提交就是真的再买一张证书，见 foxssl 包注释的约定 1。
		MerchantOrderNo:   o.OrderNo,
		UpstreamProductID: o.UpstreamProductID,
		Years:             o.Years,
		KeyAlgorithm:      string(o.KeyAlgorithm),
		DcvMethod:         string(initialDcvMethod(cap)),
		Domains:           o.DomainNames(),
		CSR:               o.CSR,
		NotifyURL:         "",
	}
	if o.Contact != nil {
		req.Contact = foxssl.Contact{
			Name:  o.Contact.Name,
			Email: o.Contact.Email,
			Phone: o.Contact.Phone,
			Title: o.Contact.Title,
		}
	}
	if o.Organization != nil {
		req.Organization = &foxssl.Organization{
			Name:           o.Organization.Name,
			RegistrationNo: o.Organization.RegistrationNo,
			Country:        o.Organization.Country,
			Province:       o.Organization.Province,
			City:           o.Organization.City,
			Address:        o.Organization.Address,
			PostalCode:     o.Organization.PostalCode,
			Phone:          o.Organization.Phone,
		}
	}

	resp, err := s.upstream.CreateOrder(ctx, req)
	if err != nil {
		// 结果未知时**不解冻**，订单停在 submitting。
		//
		// 上游可能已经建了单，只是响应没回来。此时解冻并置失败，
		// 就等于平台白付一张证书的钱——而上游那张证书还在，
		// 后续对账时会发现一笔对不上的支出。
		//
		// 停在这里是安全的：钱还在冻结中，不会重复扣款，也不会白付。
		//
		// **但真实上游不能靠「按同一个订单号重试」来收敛**：它的下单接口
		// 不接受商户订单号，重试就是真的再买一张证书。补偿必须先反查
		// （foxssl.Client.FindOrder）确认上游没建单，这条流程还没实现，
		// 所以真实上游下卡住的订单只能人工处理——见 foxssl 包注释的约定 1。
		if isUnknownResult(err) {
			slog.WarnContext(ctx, "上游调用结果未知，订单停在 submitting 等待补偿",
				"order_no", o.OrderNo, "error", err)
			return fmt.Errorf("%w: %v", ErrUpstreamUnknown, err)
		}

		// 确定性拒绝：上游明确没建单，可以放心解冻并置失败。
		if failErr := s.failOrder(ctx, o, err); failErr != nil {
			return failErr
		}
		return err
	}

	return s.completeSubmit(ctx, o, resp)
}

// completeSubmit 在上游受理之后落库。
func (s *Service) completeSubmit(
	ctx context.Context, o *Order, resp *foxssl.CreateOrderResponse,
) error {
	// 先拉域名验证材料，再进事务。
	//
	// 拉取失败不算提交失败：订单已经在上游建好了，钱也冻结着，
	// 因为「拿不到 DCV 材料」就把整单判失败是过度反应。
	// 域名材料可以在用户访问域名接口时补拉（见 ListDomains）。
	var domains []Domain
	listed, listErr := s.upstream.ListDomains(ctx, resp.UpstreamOrderNo)
	if listErr != nil {
		slog.WarnContext(ctx, "拉取上游域名材料失败，稍后由域名接口补拉",
			"order_no", o.OrderNo, "upstream_order_no", resp.UpstreamOrderNo, "error", listErr)
	} else {
		domains = toDomains(o.OrderNo, listed.Domains)
	}

	return s.txm.Run(ctx, func(ctx context.Context) error {
		locked, err := s.repo.LockByNo(ctx, o.OrderNo)
		if err != nil {
			return err
		}

		if err := s.repo.SetUpstream(ctx, o.OrderNo,
			resp.UpstreamOrderNo, resp.Cost, resp.Status); err != nil {
			return err
		}

		// 冻结转实扣。金额等于冻结额，因为两者都取自订单的零售价。
		if err := s.settle(ctx, locked); err != nil {
			return err
		}

		if len(domains) > 0 {
			if err := s.repo.ReplaceDomains(ctx, o.OrderNo, domains); err != nil {
				return err
			}
		}

		target := stateFromUpstream(resp.Status, orderstate.WaitingDcv)
		if _, err := s.advanceTo(ctx, o.OrderNo, locked.Status, target); err != nil {
			return err
		}
		return nil
	})
}

// failOrder 解冻并置失败，全过程在一个事务里。
func (s *Service) failOrder(ctx context.Context, o *Order, cause error) error {
	return s.txm.Run(ctx, func(ctx context.Context) error {
		locked, err := s.repo.LockByNo(ctx, o.OrderNo)
		if err != nil {
			return err
		}

		// 解冻与状态变更必须原子：只解冻不改状态会让订单停在 submitting
		// 而钱已经退回，补偿流程再来一次就会重复解冻（幂等键挡住）
		// 或者把一笔已经退钱的订单判成功。
		if _, err := s.advanceTo(ctx, o.OrderNo, locked.Status, orderstate.Failed); err != nil {
			return err
		}
		if err := s.repo.MarkFailed(ctx, o.OrderNo, cause.Error()); err != nil {
			return err
		}
		// 这里是 unfreeze 而不是 refund：本函数只在 submit 的确定性拒绝
		// 分支上被调用，那一刻订单必然停在 submitting，结算还没发生。
		return s.unfreeze(ctx, locked)
	})
}

// RetrySubmit 重新提交一张卡在 submitting 的订单。
//
// 这是补偿入口，供后台任务或人工调用。
//
// **它的安全性取决于上游幂等，而真实上游不幂等。** 在 Mock 上游下，
// 无论上一次到底成没成功，重试都会收敛到同一个结果；真实上游下，
// 直接重试会真的再买一张证书。所以真实上游启用后，这个入口必须先接
// foxssl.Client.FindOrder 反查（确认上游没建单才重试），在那之前
// 它只能用于 Mock 上游——见 foxssl 包注释的约定 1。
func (s *Service) RetrySubmit(ctx context.Context, orderNo string) error {
	o, err := s.repo.GetByNo(ctx, orderNo)
	if err != nil {
		return mapError(err)
	}
	if o.Status != orderstate.Submitting {
		return errs.Newf(errs.CodeInvalidOrderState,
			"订单 %s 当前状态为 %s，不需要重新提交", orderNo, o.Status)
	}

	domains, err := s.repo.ListDomains(ctx, orderNo)
	if err != nil {
		return mapError(err)
	}
	o.Domains = domains

	// 补偿要用下单时那个验证方式，而平台没把它存在订单上，只能重新
	// 从产品配置读。产品已下架时读不到——这时**不猜一个方式发出去**：
	// 猜错会让上游侧记录的方式与用户看到的产品声明对不上，而补偿
	// 卡住只是钱多冻一会儿，人工可以重新上架产品或手工处理。
	p, err := s.products.Get(ctx, o.ProductID)
	if err != nil {
		return mapError(fmt.Errorf(
			"补偿重试需要读取产品 %d 的验证方式配置: %w", o.ProductID, err))
	}

	return mapError(s.submit(ctx, o, p.Capability()))
}

// initialDcvMethod 选出下单时要报给上游的验证方式。
//
// 上游下单接口把 dcvMethod 列为必填，而平台把「选哪种方式」放在域名
// 验证阶段，两者错位。所以下单时先给一个初始值。
//
// 取产品声明的**第一个**可用方式，而不是写死 dns：产品声明是运营配置的，
// 顺序由他们控制，把哪种方式排在最前就等于表达了「这个产品的默认方式」。
// 写死 dns 会让只支持邮件验证的产品直接下单失败（上游返回 6004）。
//
// 产品一个方式都没声明时返回空串，适配层会据此明确拒绝。这比猜一个
// 方式发出去好：猜错的话，用户拿到的验证材料是另一种方式的，
// 而界面上显示的又是产品声明的那几种，两边对不上，用户会以为自己配错了。
func initialDcvMethod(cap rules.Capability) rules.DcvMethod {
	if len(cap.DcvMethods) == 0 {
		return ""
	}
	return cap.DcvMethods[0]
}

// ── 查询 ──────────────────────────────────────────

// Get 读取订单详情，并校验归属。
//
// 含域名、联系人与企业信息。访问他人订单返回 404 而不是 403：
// 后者等于确认「这个订单号存在」，可以拿来做订单号枚举。
func (s *Service) Get(ctx context.Context, userID int64, orderNo string) (*Order, error) {
	return s.reload(ctx, userID, orderNo)
}

// List 分页读取当前用户的订单。
func (s *Service) List(ctx context.Context, userID int64, f Filter) (*Page, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	page, err := s.repo.ListByUser(ctx, userID, f)
	if err != nil {
		return nil, mapError(err)
	}
	if page.Items == nil {
		page.Items = []Order{}
	}

	// 列表项要展示域名，而 ListByUser 刻意不加载关联数据。
	// 整页一次批量查询，避免「每个订单查一次域名表」的 N+1。
	orderNos := make([]string, 0, len(page.Items))
	for _, o := range page.Items {
		orderNos = append(orderNos, o.OrderNo)
	}
	if page.DomainNames, err = s.repo.ListDomainNames(ctx, orderNos); err != nil {
		return nil, mapError(err)
	}
	return page, nil
}

// reload 读取订单及其关联数据并校验归属。
func (s *Service) reload(ctx context.Context, userID int64, orderNo string) (*Order, error) {
	o, err := s.repo.GetByNo(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	if o.UserID != userID {
		return nil, errs.New(errs.CodeNotFound)
	}

	domains, err := s.repo.ListDomains(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	o.Domains = domains

	contact, err := s.repo.GetContact(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	o.Contact = contact

	org, err := s.repo.GetOrganization(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	o.Organization = org

	return o, nil
}

// ── 取消 ──────────────────────────────────────────

// Cancel 取消订单并退回金额。
//
// **顺序是刻意的：先向上游发起取消，上游确认后才退钱。**
// 反过来做的话，上游仍在跑而钱已经退给用户，等于平台白送一张证书。
// 上游取消失败时订单保持原状、余额不动，返回错误让用户重试——
// 让平台承担不确定性比让用户多试一次贵得多。
//
// 退钱的动作取决于订单走到了哪一步：还在提交阶段的解冻即可，
// 已经进入域名验证（钱已实扣）的必须退款。见 releaseActionOf。
func (s *Service) Cancel(
	ctx context.Context, userID int64, orderNo, reason string,
) (*Order, error) {
	o, err := s.repo.GetByNo(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	if o.UserID != userID {
		return nil, errs.New(errs.CodeNotFound)
	}

	// 产品能力取自快照之外的产品记录。这里读产品是必要的：
	// 「这个产品能不能取消」是产品属性，不是订单状态。
	// 产品已被下架时按「不允许取消」处理——保守方向，宁可让用户找客服。
	cap, err := s.capability(ctx, o)
	if err != nil {
		return nil, err
	}
	if !cap.CancelSupported {
		return nil, errs.Newf(errs.CodeInvalidOrderState, "%s", ErrCancelNotSupported).
			WithCause(ErrCancelNotSupported)
	}

	if o.Status.IsTerminal() {
		return nil, errs.Newf(errs.CodeInvalidOrderState,
			"订单当前状态为 %s，不能取消", o.Status)
	}
	if o.Status.Path(orderstate.Cancelled) == nil {
		return nil, errs.Newf(errs.CodeInvalidOrderState,
			"订单当前状态为 %s，不能取消", o.Status)
	}

	// 上游已建单才需要通知上游。停在 paid 的订单（尚未提交）
	// 直接走本地取消即可，去调上游只会得到一个「订单不存在」。
	if o.UpstreamOrderNo != "" {
		if err := s.upstream.CancelOrder(ctx, o.UpstreamOrderNo); err != nil {
			return nil, mapError(fmt.Errorf("上游取消失败: %w", err))
		}
	}

	err = s.txm.Run(ctx, func(ctx context.Context) error {
		locked, err := s.repo.LockByNo(ctx, orderNo)
		if err != nil {
			return err
		}
		if _, err := s.advanceTo(ctx, orderNo, locked.Status, orderstate.Cancelled); err != nil {
			return err
		}
		if err := s.repo.MarkCancelled(ctx, orderNo, reason); err != nil {
			return err
		}
		return s.release(ctx, locked)
	})
	if err != nil {
		return nil, mapError(err)
	}

	return s.reload(ctx, userID, orderNo)
}

// ── 重签 ──────────────────────────────────────────

// Reissue 申请重签。
//
// **重签不额外扣费。** CA 允许在证书有效期内免费重签，平台把这次重签
// 算在原订单的结算里。重签不改变订单状态——issued 是终态，
// 重签成功后更新的是订单上的上游重签状态与新的证书信息。
func (s *Service) Reissue(
	ctx context.Context, userID int64, orderNo, reason string,
) (*Order, error) {
	o, err := s.repo.GetByNo(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	if o.UserID != userID {
		return nil, errs.New(errs.CodeNotFound)
	}

	cap, err := s.capability(ctx, o)
	if err != nil {
		return nil, err
	}
	if !cap.ReissueSupported {
		return nil, errs.Newf(errs.CodeInvalidOrderState, "%s", ErrReissueNotSupported).
			WithCause(ErrReissueNotSupported)
	}
	if o.Status != orderstate.Issued {
		return nil, errs.Newf(errs.CodeInvalidOrderState,
			"只有已签发的订单可以重签，当前状态为 %s", o.Status)
	}

	// 上游把重签的 csr 与 dcvMethod 都列为必填，不接受「沿用原证书的 CSR」
	// 这种省略（省略会换回 6006「csr 不合法」）。所以这里要把订单上存的
	// CSR 原文重新提交一次，并带上验证方式。
	if err := s.upstream.Reissue(ctx, foxssl.ReissueRequest{
		UpstreamOrderNo: o.UpstreamOrderNo,
		CSR:             o.CSR,
		DcvMethod:       string(initialDcvMethod(cap)),
		Domains:         o.DomainNames(),
		Reason:          reason,
	}); err != nil {
		return nil, mapError(fmt.Errorf("上游重签失败: %w", err))
	}

	// 重签后上游会生成新证书。拉一次把新证书落库，
	// 失败不阻塞返回：证书接口会按需再拉。
	if err := s.syncCertificate(ctx, o.OrderNo, o.UpstreamOrderNo); err != nil {
		slog.WarnContext(ctx, "重签后同步证书失败，稍后由证书接口补拉",
			"order_no", o.OrderNo, "error", err)
	}

	return s.reload(ctx, userID, orderNo)
}

// ── 域名验证 ──────────────────────────────────────

// ListDomains 读取订单的域名验证材料。
//
// 本地没有域名行而订单已经有上游订单号时，会向上游补拉一次。
// 这条兜底路径是必要的：上游受理之后、拉取域名材料之前进程崩溃，
// 订单就会停在「有上游订单号、没有域名材料」的状态，
// 而用户此时最需要看到的就是验证材料。
//
// 两条返回路径都会填好 AllowedMethods，见 withAllowedMethods。
func (s *Service) ListDomains(ctx context.Context, userID int64, orderNo string) ([]Domain, error) {
	o, err := s.repo.GetByNo(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	if o.UserID != userID {
		return nil, errs.New(errs.CodeNotFound)
	}

	domains, err := s.repo.ListDomains(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	if len(domains) == 0 && o.UpstreamOrderNo != "" {
		if domains, err = s.syncDomains(ctx, orderNo, o.UpstreamOrderNo); err != nil {
			return nil, err
		}
	}
	return s.withAllowedMethods(ctx, o.ProductID, domains)
}

// withAllowedMethods 为每个域名填上「这张订单允许提交的验证方式」。
//
// 它与 VerifyDomains 接受什么是同一个判据的两面，所以必须在这里统一计算，
// 不能在 handler 里各算一份：界面上列出的方式用户选了却被 400 拒掉，
// 是这个域最让人费解的故障——用户会以为是自己哪里配错了。
//
// domains 必须是订单的**全部**域名：产品声明是按整张证书算的
// （见 declaredDcvMethods），少传一个域名就可能多放行一种验证方式。
func (s *Service) withAllowedMethods(
	ctx context.Context, productID int64, domains []Domain,
) ([]Domain, error) {
	if len(domains) == 0 {
		return domains, nil
	}

	declared, err := s.declaredDcvMethods(ctx, productID, domains)
	if err != nil {
		return nil, mapError(err)
	}
	for i := range domains {
		domains[i].AllowedMethods = NarrowToDeclared(AvailableMethods(domains[i]), declared)
	}
	return domains, nil
}

// VerifyDomains 提交域名验证。
func (s *Service) VerifyDomains(
	ctx context.Context, userID int64, orderNo string,
	domains []string, method rules.DcvMethod,
) ([]Domain, error) {
	o, all, targets, err := s.loadForDcv(ctx, userID, orderNo, domains)
	if err != nil {
		return nil, err
	}
	if !method.Valid() {
		return nil, errs.Newf(errs.CodeInvalidParam, "未知的验证方式: %q", method).
			WithField("method", "未知的验证方式")
	}

	// 产品侧先判。判据取订单的**全部**域名而不是本次提交的那些：
	// 「含通配符的证书不支持文件验证」约束的是整张证书，只看本次提交的域名，
	// 会让「只提交非通配符那个域名」绕过这条规则。
	declared, err := s.declaredDcvMethods(ctx, o.ProductID, all)
	if err != nil {
		return nil, mapError(err)
	}
	if declared != nil && !containsMethod(declared, method) {
		return nil, errs.Newf(errs.CodeInvalidParam,
			"该产品与这些域名不支持所选验证方式，可用：%s", joinMethods(declared)).
			WithField("method", "该产品与这些域名不支持所选验证方式")
	}

	// 材料侧判据取**本次提交的**域名：验证方式是逐域名生效的，
	// 同一张证书上 A 走 DNS、B 走文件是正常用法，按整张证书取交集会把
	// 这种用法一并拒掉（契约里也写明「必须是这些域名 availableMethods 的交集」）。
	//
	// 与 withAllowedMethods 的关系：那边是逐域名取交集，这边是对本次提交的
	// 域名再取一次交集。所以「在每个提交的域名上都列出 M」等价于「提交 M 会被接受」——
	// 这正是前端能直接用那个字段渲染选项的前提。
	material := IntersectMethods(targets)
	if !containsMethod(material, method) {
		return nil, errs.Newf(errs.CodeInvalidParam,
			"所选验证方式对这些域名不可用，可用：%s", joinMethods(material)).
			WithField("method", "所选验证方式对这些域名不可用")
	}

	if err := s.upstream.VerifyDomains(ctx, foxssl.VerifyDomainsRequest{
		UpstreamOrderNo: o.UpstreamOrderNo,
		Domains:         domains,
		Method:          string(method),
	}); err != nil {
		return nil, mapError(fmt.Errorf("提交域名验证失败: %w", err))
	}

	// 本地记录选定的方式与「校验中」状态。上游返回的最终状态由
	// 后续事件或下一次读取覆盖。
	err = s.txm.Run(ctx, func(ctx context.Context) error {
		for _, d := range targets {
			if err := s.repo.UpdateDomainMethod(ctx, orderNo, d.Domain, method, DomainVerifying); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, mapError(err)
	}

	return s.ListDomains(ctx, userID, orderNo)
}

// ResendDcvEmail 重发域名验证邮件。
func (s *Service) ResendDcvEmail(
	ctx context.Context, userID int64, orderNo string, domains []string,
) ([]Domain, error) {
	// 重发邮件不关心订单的全部域名，只关心本次要重发的那几个。
	o, _, targets, err := s.loadForDcv(ctx, userID, orderNo, domains)
	if err != nil {
		return nil, err
	}
	for _, d := range targets {
		if !MethodAllowed(d, rules.Email) {
			return nil, errs.Newf(errs.CodeInvalidParam,
				"域名 %s 未使用邮件验证", d.Domain).
				WithField("domains", fmt.Sprintf("域名 %s 未使用邮件验证", d.Domain))
		}
	}

	if err := s.upstream.ResendDcvEmail(ctx, o.UpstreamOrderNo, domains); err != nil {
		return nil, mapError(fmt.Errorf("重发验证邮件失败: %w", err))
	}
	return s.ListDomains(ctx, userID, orderNo)
}

// RegenerateDcvToken 重新生成验证 token。
func (s *Service) RegenerateDcvToken(
	ctx context.Context, userID int64, orderNo string,
) ([]Domain, error) {
	o, err := s.repo.GetByNo(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	if o.UserID != userID {
		return nil, errs.New(errs.CodeNotFound)
	}
	if o.Status != orderstate.WaitingDcv {
		return nil, errs.Newf(errs.CodeInvalidOrderState,
			"只有等待域名验证的订单可以重新生成 token，当前状态为 %s", o.Status).
			WithCause(ErrOrderNotVerifiable)
	}

	// 已经验证通过的域名不允许被重置。
	//
	// 重新生成 token 会换掉 DNS 记录值与文件路径，对已验证的域名来说
	// 等于把验证结果作废——用户得重新配一遍。而重新生成的正当理由是
	// 「token 泄漏」，此时该做的是联系客服换发，不是把已通过的域名拖下水。
	existing, err := s.repo.ListDomains(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	for _, d := range existing {
		if d.Status == DomainVerified {
			return nil, errs.Newf(errs.CodeInvalidOrderState,
				"域名 %s 已验证通过，重新生成 token 会使其失效，请联系客服处理", d.Domain).
				WithCause(ErrRegenerateLimitExceeded)
		}
	}

	listed, err := s.upstream.RegenerateDcvToken(ctx, o.UpstreamOrderNo)
	if err != nil {
		return nil, mapError(fmt.Errorf("重新生成验证 token 失败: %w", err))
	}

	domains := toDomains(orderNo, listed.Domains)
	err = s.txm.Run(ctx, func(ctx context.Context) error {
		return s.repo.ReplaceDomains(ctx, orderNo, domains)
	})
	if err != nil {
		return nil, mapError(err)
	}

	return s.ListDomains(ctx, userID, orderNo)
}

// loadForDcv 读取订单并校验域名验证操作的前置条件。
//
// 返回订单、订单的**全部**域名，以及本次要操作的那些域名。
// 全部域名是给产品规则用的：「含通配符的证书不支持文件验证」约束的是整张证书，
// 只拿本次提交的域名去判断，会让「只提交非通配符那个域名」绕过这条规则。
func (s *Service) loadForDcv(
	ctx context.Context, userID int64, orderNo string, domains []string,
) (o *Order, all, targets []Domain, err error) {
	o, err = s.repo.GetByNo(ctx, orderNo)
	if err != nil {
		return nil, nil, nil, mapError(err)
	}
	if o.UserID != userID {
		return nil, nil, nil, errs.New(errs.CodeNotFound)
	}
	// 只有等待域名验证的订单能操作验证。已签发的订单再去提交验证
	// 除了消耗上游的验证次数之外没有任何效果。
	if o.Status != orderstate.WaitingDcv {
		return nil, nil, nil, errs.Newf(errs.CodeInvalidOrderState,
			"订单当前状态为 %s，不能操作域名验证", o.Status).
			WithCause(ErrOrderNotVerifiable)
	}
	if o.UpstreamOrderNo == "" {
		return nil, nil, nil, errs.Newf(errs.CodeInvalidOrderState,
			"订单尚未提交到上游，不能操作域名验证").WithCause(ErrOrderNotVerifiable)
	}
	if len(domains) == 0 {
		return nil, nil, nil, errs.New(errs.CodeInvalidParam).
			WithField("domains", "至少需要一个域名")
	}

	all, err = s.repo.ListDomains(ctx, orderNo)
	if err != nil {
		return nil, nil, nil, mapError(err)
	}
	byName := make(map[string]Domain, len(all))
	for _, d := range all {
		byName[d.Domain] = d
	}

	targets = make([]Domain, 0, len(domains))
	for _, raw := range domains {
		name := strings.ToLower(strings.TrimSpace(raw))
		d, ok := byName[name]
		if !ok {
			return nil, nil, nil, errs.Newf(errs.CodeInvalidParam,
				"域名 %s 不属于该订单", name).
				WithField("domains", fmt.Sprintf("域名 %s 不属于该订单", name)).
				WithCause(ErrDomainNotInOrder)
		}
		targets = append(targets, d)
	}
	return o, all, targets, nil
}

// declaredDcvMethods 返回产品为这张订单声明可用的验证方式。
//
// 这是「我们卖的是什么」。产品页写着 AlphaSSL 不支持邮件验证，接口就必须拒绝它——
// docs/06 第 4.3 节把这几条列为**后端必须校验**的规则，验收标准第 6 条也写明
// 「产品规则在后端校验，不能依赖前端绕过」。产品页写着不支持、接口却接受，
// 产品配置就成了纯装饰。
//
// **判据取订单的全部域名，而不是本次提交的那些。** 规则 2（含通配符的证书
// 不支持文件验证）约束的是整张证书：只看本次提交的域名，会让「只提交非通配符
// 那个域名」绕过它。
//
// 返回 nil 表示没有产品侧约束，调用方据此跳过产品侧判断。
// 产品读不到（已下架）属于这种情况：我们无从知道当初卖的是什么，但用户已经
// 付过钱、上游也已备好材料，卡住验证只会让一张已付款的订单烂在那里。
// 这与 Cancel / Reissue 的保守方向不同——那两件事判断错了会多花钱，
// 而这里判断错了只是让一个合法的验证提交不了。
func (s *Service) declaredDcvMethods(
	ctx context.Context, productID int64, orderDomains []Domain,
) ([]rules.DcvMethod, error) {
	p, err := s.products.Get(ctx, productID)
	if err != nil {
		var e *errs.Error
		if errors.As(err, &e) && e.Code == errs.CodeNotFound {
			return nil, nil
		}
		return nil, err
	}

	names := make([]string, 0, len(orderDomains))
	for _, d := range orderDomains {
		names = append(names, d.Domain)
	}
	return rules.AllowedDcvMethods(p.Capability(), names), nil
}

// ── 证书 ──────────────────────────────────────────

// Certificate 读取已签发的证书。
func (s *Service) Certificate(
	ctx context.Context, userID int64, orderNo string,
) (*Certificate, error) {
	o, err := s.repo.GetByNo(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	if o.UserID != userID {
		return nil, errs.New(errs.CodeNotFound)
	}
	if o.Status != orderstate.Issued {
		return nil, errs.Newf(errs.CodeInvalidOrderState,
			"订单当前状态为 %s，证书尚未签发", o.Status).WithCause(ErrCertificateNotReady)
	}

	// 先读本地缓存。证书是不可变的，已经落库的没必要每次重新下载——
	// 上游的下载接口通常还有频率限制。
	cached, err := s.repo.GetCertificate(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	if cached != nil && cached.Certificate != "" {
		return cached, nil
	}

	if err := s.syncCertificate(ctx, orderNo, o.UpstreamOrderNo); err != nil {
		return nil, mapError(err)
	}
	got, err := s.repo.GetCertificate(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	if got == nil || got.Certificate == "" {
		return nil, errs.Newf(errs.CodeInternal, "证书已签发但未能取到内容").
			WithCause(ErrCertificateNotReady)
	}
	return got, nil
}

// syncCertificate 从上游下载证书并落库。
func (s *Service) syncCertificate(ctx context.Context, orderNo, upstreamOrderNo string) error {
	cert, err := s.upstream.DownloadCertificate(ctx, upstreamOrderNo)
	if err != nil {
		return fmt.Errorf("下载证书失败: %w", err)
	}

	rec := &Certificate{
		OrderNo:      orderNo,
		CertID:       cert.CertID,
		Status:       cert.Status,
		CommonName:   cert.CommonName,
		Domains:      cert.Domains,
		KeyAlgorithm: rules.KeyAlgorithm(cert.KeyAlgorithm),
		SerialNumber: cert.SerialNumber,
		Certificate:  cert.Certificate,
		CABundle:     cert.CABundle,
		IssuedAt:     zeroToNil(cert.IssuedAt),
		ExpiresAt:    zeroToNil(cert.ExpiresAt),
	}
	if len(rec.Domains) == 0 {
		rec.Domains = []string{rec.CommonName}
	}

	return s.txm.Run(ctx, func(ctx context.Context) error {
		if err := s.repo.UpsertCertificate(ctx, rec); err != nil {
			return err
		}
		return s.repo.SyncUpstream(ctx, orderNo, UpstreamSnapshot{
			CertID:    cert.CertID,
			IssuedAt:  rec.IssuedAt,
			ExpiresAt: rec.ExpiresAt,
		})
	})
}

// ── 上游回调 ──────────────────────────────────────

// HandleWebhook 处理一次上游事件回调。
//
// 全流程：验签 → 事件落库（幂等）→ 同步订单 → 标记事件处理结果。
//
// **验签失败时一行数据库都不会碰。** 不是「验签失败后回滚」，
// 而是根本没有任何东西需要回滚。这既是安全要求，也避免了
// 「攻击者用伪造报文占满事件表、把真实事件挤出幂等窗口」。
func (s *Service) HandleWebhook(ctx context.Context, raw []byte, signature string) error {
	notif, err := s.upstream.ParseNotification(raw, signature)
	if err != nil {
		return mapError(err)
	}

	event := &WebhookEvent{
		Provider:        providerFoxSSL,
		EventKey:        buildEventKey(notif),
		EventType:       notif.EventType,
		UpstreamOrderNo: notif.UpstreamOrderNo,
		UpstreamStatus:  notif.Status,
		PayloadHash:     hashPayload(raw),
		Payload:         truncateBytes(raw, payloadMaxLen),
		OccurredAt:      zeroToNil(notif.OccurredAt),
	}

	duplicate, err := s.repo.InsertEvent(ctx, event)
	if err != nil {
		return mapError(err)
	}

	if duplicate {
		// 上游重推。如果上一次处理没成功（或还没处理），这次重推正好是
		// 一次重试机会——直接跳过会把一次可用的重试白白丢掉。
		stored, err := s.repo.GetEventByKey(ctx, event.EventKey)
		if err != nil {
			return mapError(err)
		}
		if stored == nil || stored.ProcessStatus == EventDone {
			slog.InfoContext(ctx, "上游事件重推，已处理过，跳过",
				"event_key", event.EventKey, "upstream_order_no", notif.UpstreamOrderNo)
			return nil
		}
		slog.InfoContext(ctx, "上游事件重推，上次未处理成功，重新处理",
			"event_key", event.EventKey, "process_status", stored.ProcessStatus)
		event.ID = stored.ID
	}

	// 处理失败不回错误给上游。事件已经落库，重推也只会撞上同一个
	// event_key，让上游反复重试没有意义。事件上的 process_status
	// 记录了它需要人工或后台任务处理。
	if err := s.applyEvent(ctx, notif); err != nil {
		if markErr := s.repo.MarkEventProcessed(ctx, event.ID, EventFailed, err.Error()); markErr != nil {
			slog.ErrorContext(ctx, "记录事件处理失败状态时又出错了",
				"event_id", event.ID, "error", markErr)
		}
		slog.ErrorContext(ctx, "上游事件处理失败",
			"event_id", event.ID, "event_key", event.EventKey, "error", err)
		return nil
	}

	return s.repo.MarkEventProcessed(ctx, event.ID, EventDone, "")
}

// Ack 返回上游约定的成功应答体。
func (s *Service) Ack() foxssl.Ack { return s.upstream.Ack() }

// applyEvent 把上游事件反映到本地订单上。
func (s *Service) applyEvent(ctx context.Context, n *foxssl.Notification) error {
	// 事件里带的是**上游**订单号，不是平台订单号。用错查找键会让每一次
	// 事件处理都报「订单不存在」，而事件看起来又收到并落库了。
	o, err := s.repo.GetByUpstreamOrderNo(ctx, n.UpstreamOrderNo)
	if err != nil {
		if errors.Is(err, ErrOrderNotFound) {
			return fmt.Errorf("%w: 上游订单号 %s", ErrEventUnknownOrder, n.UpstreamOrderNo)
		}
		return err
	}

	// 同步域名状态。域名对不上时只记录，不影响订单状态同步——
	// 上游可能推送平台还不认识的域名（人工在上游加了 SAN）。
	if len(n.Domains) > 0 {
		for _, ds := range n.Domains {
			status := mapUpstreamDomainStatus(ds.Status)
			if err := s.repo.SyncDomainStatus(ctx, o.OrderNo, ds.Domain, status, nil); err != nil {
				slog.WarnContext(ctx, "同步域名状态失败，跳过该域名",
					"order_no", o.OrderNo, "domain", ds.Domain, "error", err)
			}
		}
	}

	target := stateFromUpstream(n.Status, "")
	if target == "" {
		// 上游状态未识别：只更新原始状态字段，不动本地状态机。
		// 猜一个状态比不动更危险——猜成 issued 会让未签发的订单可下载。
		slog.WarnContext(ctx, "上游事件携带了未识别的状态，仅记录不改状态",
			"order_no", o.OrderNo, "upstream_status", n.Status)
		return s.repo.SyncUpstream(ctx, o.OrderNo, UpstreamSnapshot{
			OrderStatus: n.Status,
			CertID:      n.CertID,
		})
	}

	err = s.txm.Run(ctx, func(ctx context.Context) error {
		locked, err := s.repo.LockByNo(ctx, o.OrderNo)
		if err != nil {
			return err
		}

		// 乱序投递的保护在这里：Path 对回退返回 nil，
		// 一条迟到的「等待验证」事件不会把已签发的订单改回去。
		if locked.Status.Path(target) == nil {
			slog.InfoContext(ctx, "上游事件与本地状态不构成合法迁移，跳过",
				"order_no", o.OrderNo,
				"current", locked.Status, "upstream_target", target)
			return s.repo.SyncUpstream(ctx, o.OrderNo, UpstreamSnapshot{
				OrderStatus: n.Status,
				CertID:      n.CertID,
			})
		}

		if _, err := s.advanceTo(ctx, o.OrderNo, locked.Status, target); err != nil {
			return err
		}

		// 上游判定失败时把钱退回给用户。
		//
		// 与状态变更放在同一个事务里：只改状态会让订单显示失败而钱留在平台，
		// 只退钱会让订单还停在「签发中」而用户已经拿到退款。
		//
		// 传 locked 而不是重新读：退款动作取决于**变更前**的状态，
		// 已经实扣的要退款、只冻结的要解冻、失败过的不能再动。
		if target == orderstate.Failed {
			if err := s.release(ctx, locked); err != nil {
				return err
			}
		}

		return s.repo.SyncUpstream(ctx, o.OrderNo, UpstreamSnapshot{
			OrderStatus: n.Status,
			CertID:      n.CertID,
		})
	})
	if err != nil {
		return err
	}

	// 已签发时把证书拉下来。失败不回滚状态——证书接口会按需补拉，
	// 而把「已签发」改回「签发中」只会让用户更困惑。
	if target == orderstate.Issued {
		if err := s.syncCertificate(ctx, o.OrderNo, o.UpstreamOrderNo); err != nil {
			slog.WarnContext(ctx, "事件触发证书下载失败，稍后由证书接口补拉",
				"order_no", o.OrderNo, "error", err)
		}
	}
	return nil
}

// syncDomains 从上游补拉域名材料并落库。
func (s *Service) syncDomains(
	ctx context.Context, orderNo, upstreamOrderNo string,
) ([]Domain, error) {
	listed, err := s.upstream.ListDomains(ctx, upstreamOrderNo)
	if err != nil {
		return nil, mapError(fmt.Errorf("拉取上游域名材料失败: %w", err))
	}
	domains := toDomains(orderNo, listed.Domains)

	err = s.txm.Run(ctx, func(ctx context.Context) error {
		return s.repo.ReplaceDomains(ctx, orderNo, domains)
	})
	if err != nil {
		return nil, mapError(err)
	}
	// 直接返回刚写入的数据，不再绕回 ListDomains：
	// 那条路径带归属校验，而补拉场景（事件驱动）没有 userID。
	return domains, nil
}

// ── 内部辅助 ──────────────────────────────────────

// advance 做一次单步状态推进。
func (s *Service) advance(
	ctx context.Context, orderNo string, from, to orderstate.State,
) error {
	if _, err := s.advanceTo(ctx, orderNo, from, to); err != nil {
		return err
	}
	return nil
}

// advanceTo 沿状态机把订单推进到目标状态，途经的中间状态依次写入。
//
// 用 Path 而不是逐级 Transition：上游不保证推送每一个中间状态，
// 本地还停在 waiting_dcv 而事件直接说「已签发」是正常的。
// Path 同时承担乱序保护——对回退返回 nil。
func (s *Service) advanceTo(
	ctx context.Context, orderNo string, from, to orderstate.State,
) (orderstate.State, error) {
	path := from.Path(to)
	if path == nil {
		return from, fmt.Errorf("%w: %s → %s", ErrStateUnreachable, from, to)
	}

	cur := from
	for _, next := range path {
		if err := s.repo.AdvanceStatus(ctx, orderNo, cur, next); err != nil {
			return cur, err
		}
		cur = next
	}
	return cur, nil
}

// freeze 冻结订单金额。免费订单不产生任何资金动作。
func (s *Service) freeze(ctx context.Context, o *Order) error {
	if o.IsFree() {
		return nil
	}
	_, err := s.wallet.Freeze(ctx, wallet.ChangeInput{
		UserID:  o.UserID,
		Amount:  o.Amount,
		BizType: bizTypeOrder,
		BizNo:   o.OrderNo,
		Remark:  "下单冻结 " + o.ProductName,
	})
	return err
}

// settle 把冻结转为实扣。
func (s *Service) settle(ctx context.Context, o *Order) error {
	if o.IsFree() {
		return nil
	}
	_, err := s.wallet.Settle(ctx, wallet.ChangeInput{
		UserID:  o.UserID,
		Amount:  o.Amount,
		BizType: bizTypeOrder,
		BizNo:   o.OrderNo,
		Remark:  "证书签发扣款 " + o.ProductName,
	})
	return err
}

// unfreeze 把冻结退回可用余额。
//
// 用 unfreeze 而不是「先扣款再退款」：后者会在账本里留下一对
// settle/refund 流水，把失败订单算进平台收入，污染财务报表。
// 冻结与解冻的净效果为零，失败订单在收入报表里根本不出现。
func (s *Service) unfreeze(ctx context.Context, o *Order) error {
	if o.IsFree() {
		return nil
	}
	_, err := s.wallet.Unfreeze(ctx, wallet.ChangeInput{
		UserID:  o.UserID,
		Amount:  o.Amount,
		BizType: bizTypeOrder,
		BizNo:   o.OrderNo,
		Remark:  "订单取消或失败退回",
	})
	return err
}

// refund 退还**已经实扣**的订单金额。
//
// 与解冻必须分开：解冻只对「冻结着但还没实扣」的订单有意义，
// 对已经结算过的订单会直接报「冻结余额不足」——
// 也就是「取消一张已进入验证阶段的订单」会永远失败。
//
// 退款在账本里是一条独立流水（op=refund，与 settle 不同的幂等键），
// 这是必要的：这笔钱确实收过又退了，财务报表要能看见它。
func (s *Service) refund(ctx context.Context, o *Order) error {
	if o.IsFree() {
		return nil
	}
	_, err := s.wallet.Refund(ctx, wallet.ChangeInput{
		UserID:  o.UserID,
		Amount:  o.Amount,
		BizType: bizTypeOrder,
		BizNo:   o.OrderNo,
		Remark:  "订单取消或失败退款 " + o.ProductName,
	})
	return err
}

// releaseAction 描述「把订单的钱退回给用户」该走哪个动作。
type releaseAction int

const (
	// releaseNone 表示不需要任何资金动作。
	releaseNone releaseAction = iota
	// releaseUnfreeze 表示钱只被冻结、还没实扣。
	releaseUnfreeze
	// releaseRefund 表示钱已经实扣，必须退款。
	releaseRefund
)

// releaseActionOf 根据订单状态判断该走哪个退款动作。
//
// 订单的资金状态有三种，做错任何一种都会算错钱：
//
//	pending_payment / paid / submitting —— 只冻结未实扣，解冻即可。
//	  解冻的净效果为零，账本里不会留下这笔支出。
//	waiting_dcv / issuing —— 已实扣，必须退款。
//	  这两种状态下解冻会直接失败，因为根本没有冻结余额可解。
//	failed —— 钱已经在上游拒绝或事件驱动失败时退过了，再退一次就是重复退款。
//
// 分界线是 completeSubmit：它在同一个事务里完成结算并把订单推进到
// waiting_dcv（或更远），因此「状态在 waiting_dcv 及其之后」等价于「已实扣」。
//
// 用状态推断而不是在订单上再存一个「是否已结算」的标志位：那个标志位
// 与状态是同一次事务写入的，多存一份就多一个可能不一致的地方。
func releaseActionOf(s orderstate.State) releaseAction {
	switch s {
	case orderstate.PendingPayment, orderstate.Paid, orderstate.Submitting:
		return releaseUnfreeze
	case orderstate.WaitingDcv, orderstate.Issuing:
		return releaseRefund
	default:
		return releaseNone
	}
}

// release 按订单当前状态把已扣的钱退回给用户。
//
// 调用方必须传**变更前**的状态：状态一旦改成 cancelled / failed，
// 就判断不出这笔钱当初是冻着的还是已经扣掉了。
func (s *Service) release(ctx context.Context, o *Order) error {
	switch releaseActionOf(o.Status) {
	case releaseUnfreeze:
		return s.unfreeze(ctx, o)
	case releaseRefund:
		return s.refund(ctx, o)
	default:
		return nil
	}
}

// capability 读取订单对应产品的能力。
//
// 产品已被下架时返回一个「什么都不支持」的能力：这是保守方向——
// 宁可让用户找客服，也不要在产品已经不可售之后还允许取消或重签。
func (s *Service) capability(ctx context.Context, o *Order) (rules.Capability, error) {
	p, err := s.products.Get(ctx, o.ProductID)
	if err != nil {
		var e *errs.Error
		if errors.As(err, &e) && e.Code == errs.CodeNotFound {
			return rules.Capability{}, nil
		}
		return rules.Capability{}, err
	}
	return p.Capability(), nil
}

// findPrice 找出指定年限的价格。
func findPrice(p *product.Product, years int) (product.Price, bool) {
	for _, price := range p.Prices {
		if price.Years == years {
			return price, true
		}
	}
	return product.Price{}, false
}

// toDomains 把上游返回的域名材料转换成本域模型。
func toDomains(orderNo string, src []foxssl.Domain) []Domain {
	out := make([]Domain, 0, len(src))
	for i, d := range src {
		out = append(out, NewDomain(orderNo, d, i == 0))
	}
	return out
}

// upstreamStateMap 把上游状态原文映射成平台状态。
//
// 取值按上游文档列，同时容忍几个常见同义词。**未识别的取值返回空串**，
// 调用方据此只记录不改状态——猜错方向的代价不对称：
// 把未签发的订单猜成已签发，用户会拿到一个下载不了的证书入口；
// 把已签发的猜成签发中，用户只是多等一会儿。
//
// ── 数字码是真实上游的形态 ────────────────────────
//
// Mock 上游用 "issued" 这类字符串，**真实上游用数字码**：订单状态
// 1001-1006、证书状态 3002-3006、重签状态 5001-5004。所以两张表都要有。
//
// **需要人工介入的状态一律不映射。** 上游文档对 1003 / 1006 / 3003 / 3006
// 的说明都是「请联系客服」，对 5004 的说明是「重签需要补差价但没有补交」。
// 这几个状态不会自愈，而平台能做的自动动作只有「退款」——
// 在不知道上游会不会人工修好的情况下退款，等于替上游做了决定。
// 不映射的后果是订单停在原地、钱冻着，人工介入后仍可推进；
// 映射错了的后果是钱已经退出去、证书又被签出来，对账时才发现。
//
// 同理，1001（未支付）也不映射：它同时出现在重签场景（配 5004），
// 而重签的退款动作与首次下单不同，不能按同一套处理。
var upstreamStateMap = map[string]orderstate.State{
	// ── Mock 与上游的文字形态 ──
	"waiting_dcv":               orderstate.WaitingDcv,
	"waiting_domain_validation": orderstate.WaitingDcv,
	"pending_validation":        orderstate.WaitingDcv,
	"issuing":                   orderstate.Issuing,
	"processing":                orderstate.Issuing,
	"issued":                    orderstate.Issued,
	"success":                   orderstate.Issued,
	"cancelled":                 orderstate.Cancelled,
	"canceled":                  orderstate.Cancelled,
	"failed":                    orderstate.Failed,
	"rejected":                  orderstate.Failed,
	"error":                     orderstate.Failed,

	// ── 真实上游的数字码 ──
	// 订单状态码。1001 未支付、1003 提交 CA 异常、1006 提交 CA 超时
	// 刻意不在表里，理由见上面的说明。
	"1002": orderstate.Issuing, // 已支付
	"1004": orderstate.Cancelled,
	// 证书状态码。3003 / 3006 同 1003 / 1006，不映射。
	"3002": orderstate.Issuing, // 已支付，等待签发
	"3004": orderstate.Issued,
	"3005": orderstate.Cancelled,
	// 重签状态码。5004（需补差价未补）不映射。
	"5001": orderstate.Issuing, // 重签申请中
	"5002": orderstate.Issued,  // 重签申请成功
	"5003": orderstate.Failed,  // 重签申请失败
}

// stateFromUpstream 映射上游状态，未识别时返回 fallback。
func stateFromUpstream(status string, fallback orderstate.State) orderstate.State {
	if s, ok := upstreamStateMap[strings.ToLower(strings.TrimSpace(status))]; ok {
		return s
	}
	return fallback
}

// isUnknownResult 判断上游错误是否属于「结果未知」。
//
// 只有「服务不可用」这一类是未知的：连接超时、5xx、连接被重置，
// 都可能发生在请求已经到达上游之后。业务性错误（不支持、报文错误）
// 说明上游明确拒绝了，可以放心解冻。
func isUnknownResult(err error) bool {
	return errors.Is(err, foxssl.ErrUnavailable)
}

// buildEventKey 拼出事件的幂等键。
//
// 组成是 上游:事件类型:上游订单号:报文哈希。前两段区分「同一订单的不同
// 事件」，报文哈希区分「同一事件的重复投递」。
//
// 不用时间戳参与：上游重推时时间戳可能不同，那样就失去了幂等性。
func buildEventKey(n *foxssl.Notification) string {
	return strings.Join([]string{
		providerFoxSSL,
		n.EventType,
		n.UpstreamOrderNo,
		hashPayload(n.Raw),
	}, ":")
}

// hashPayload 计算报文哈希，作为幂等键与留档的一部分。
func hashPayload(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// truncateBytes 按字节截断，并保证不截出半个 UTF-8 字符。
//
// 直接按字节切会让最后一个字符变成无效序列，写入 MySQL 时
// 报 Incorrect string value——一个只在报文长度刚好卡在边界时才出现的错误。
func truncateBytes(raw []byte, max int) string {
	if len(raw) <= max {
		return string(raw)
	}
	return strings.ToValidUTF8(string(raw[:max]), "")
}

func containsMethod(methods []rules.DcvMethod, target rules.DcvMethod) bool {
	for _, m := range methods {
		if m == target {
			return true
		}
	}
	return false
}

func joinMethods(methods []rules.DcvMethod) string {
	parts := make([]string, 0, len(methods))
	for _, m := range methods {
		parts = append(parts, string(m))
	}
	if len(parts) == 0 {
		return "无"
	}
	return strings.Join(parts, "、")
}

// mapError 把领域哨兵错误翻译成业务错误码。
//
// 与 wallet / recharge 域同样的分层：判定发生在持有行锁的仓储层，
// 而 HTTP 语义属于服务层，映射只在这一处发生。
func mapError(err error) error {
	switch {
	case err == nil:
		return nil

	case errors.Is(err, ErrOrderNotFound), errors.Is(err, ErrProductNotFound):
		return errs.New(errs.CodeNotFound).WithCause(err)

	case errors.Is(err, wallet.ErrInsufficientAvailable):
		return errs.Newf(errs.CodeInsufficientBalance, "可用余额不足，请先充值").WithCause(err)

	case errors.Is(err, ErrStateUnreachable),
		errors.Is(err, ErrOrderNotCancellable),
		errors.Is(err, ErrOrderNotReissuable),
		errors.Is(err, ErrOrderNotVerifiable),
		errors.Is(err, ErrCancelNotSupported),
		errors.Is(err, ErrReissueNotSupported),
		errors.Is(err, ErrCertificateNotReady),
		errors.Is(err, ErrRegenerateLimitExceeded):
		return errs.Newf(errs.CodeInvalidOrderState, "%s", err.Error()).WithCause(err)

	case errors.Is(err, ErrDomainNotInOrder),
		errors.Is(err, ErrMethodNotAvailable):
		return errs.Newf(errs.CodeInvalidParam, "%s", err.Error()).WithCause(err)

	// 上游调用结果未知是 502 而不是 400：用户什么都没做错，
	// 而且这与「上游明确拒绝」的处置完全不同——订单停在 submitting、
	// 余额仍然冻结着，等补偿流程用同一个商户订单号重试。
	// 映射成 400 会让用户以为是自己参数填错了，去改参数重下一单，
	// 而真正该做的是等这一次补偿收敛。
	case errors.Is(err, ErrUpstreamUnknown):
		return errs.Newf(errs.CodeUpstream,
			"上游响应超时，订单已受理但结果待确认，请稍后在订单列表查看，不要重复下单").
			WithCause(err)

	case errors.Is(err, foxssl.ErrUnavailable):
		return errs.New(errs.CodeUpstream).WithCause(err)

	case errors.Is(err, foxssl.ErrNotSupported):
		return errs.Newf(errs.CodeInvalidOrderState, "%s", err.Error()).WithCause(err)

	// 下面两条都是**平台侧**的问题，不是用户的问题。
	//
	// 映射成 502（上游侧问题）而不是 400：用户什么都没做错，而 400 会让
	// 他去改参数重试，真正该做的却是我们这边充值或换凭证。
	// 单独列出来而不是让它们落进 default，是为了让错误信息直接说清
	// 「是平台侧的问题」——否则运维看到的是一条泛泛的内部错误。
	case errors.Is(err, foxssl.ErrPlatformBalance):
		return errs.Newf(errs.CodeUpstream,
			"上游账户余额不足，暂时无法下单，请联系客服").WithCause(err)

	case errors.Is(err, foxssl.ErrUnauthorized):
		return errs.Newf(errs.CodeUpstream,
			"上游拒绝了平台凭证，暂时无法下单，请联系客服").WithCause(err)

	case errors.Is(err, foxssl.ErrOrderNotFound):
		// 本地记了一个上游不认的订单号，是本地数据出了问题
		return errs.New(errs.CodeInternal).
			WithCause(fmt.Errorf("上游订单不存在: %w", err))

	case errors.Is(err, foxssl.ErrInvalidSignature):
		return errs.Newf(errs.CodeUnauthorized, "回调签名校验失败").WithCause(err)

	case errors.Is(err, foxssl.ErrMalformedPayload):
		return errs.Newf(errs.CodeInvalidParam, "回调报文格式错误").WithCause(err)

	default:
		// 已经是 *errs.Error 的直接放行（产品域返回的就是它）
		var e *errs.Error
		if errors.As(err, &e) {
			return err
		}
		return err
	}
}
