package recharge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/ids"
	"github.com/cangerx/c-ssl/server/internal/platform/mysql"
	"github.com/cangerx/c-ssl/server/internal/platform/tx"
	"github.com/cangerx/c-ssl/server/internal/upstream/payment"
	"github.com/cangerx/c-ssl/server/internal/wallet"
)

// Wallet 是本域需要的钱包能力。
//
// 刻意定义成本域自己的窄接口，而不是直接依赖 *wallet.Service：
// 充值只需要「加款」这一个动作，接口收窄后钱包新增的方法不会自动
// 成为充值可以调用的能力。跨域依赖的面越小，两边改起来越不容易互相牵连。
type Wallet interface {
	Recharge(ctx context.Context, in wallet.ChangeInput) (*wallet.Result, error)
}

// Service 是充值域的业务入口。
type Service struct {
	repo   *Repository
	txm    *tx.Manager
	wallet Wallet
	// channels 是渠道注册表，键为渠道标识。
	channels       map[string]payment.Channel
	defaultChannel string
	baseURL        string
	// clock 可被测试替换，让「订单过期」这类与时间相关的分支能被确定性地覆盖。
	clock func() time.Time
}

// NewService 构造服务。
//
// channels 以切片而不是 map 传入：map 的遍历顺序随机，
// 若用 map 构造注册表，重复的渠道标识会随机决定谁生效。
// 切片让「后注册的覆盖先注册的」成为确定的语义。
func NewService(
	repo *Repository,
	walletSvc Wallet,
	channels []payment.Channel,
	defaultChannel string,
	baseURL string,
) *Service {
	registry := make(map[string]payment.Channel, len(channels))
	for _, ch := range channels {
		registry[ch.Name()] = ch
	}
	return &Service{
		repo:           repo,
		txm:            tx.NewManager(repo.db),
		wallet:         walletSvc,
		channels:       registry,
		defaultChannel: defaultChannel,
		baseURL:        strings.TrimRight(baseURL, "/"),
		clock:          time.Now,
	}
}

// ── 用户侧 ────────────────────────────────────────

// Create 创建一张充值订单。
//
// 本方法不产生任何余额变动。加款只发生在渠道回调通过验签之后。
func (s *Service) Create(ctx context.Context, in CreateInput) (*Order, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	channelName := in.Channel
	if channelName == "" {
		channelName = s.defaultChannel
	}
	ch, ok := s.channels[channelName]
	if !ok {
		return nil, errs.New(errs.CodeInvalidParam).WithField("channel", "不支持的支付渠道")
	}

	now := s.clock().UTC()
	order := &Order{
		OrderNo:   ids.NewRechargeNo(now),
		UserID:    in.UserID,
		Amount:    in.Amount,
		Status:    StatusPending,
		Channel:   channelName,
		ExpiresAt: now.Add(OrderTTL),
	}

	// 先在渠道侧发起支付，再落库。
	//
	// 顺序是刻意的：反过来的话，落库成功但渠道调用失败会留下一张
	// 用户点开却付不了款的订单；而现在的顺序，最坏情况是渠道侧多了一笔
	// 用户从未见过的支付——payUrl 没有返回给任何人，不会被支付。
	//
	// 另一个不能反的理由：渠道调用是网络 I/O，绝不能放进数据库事务，
	// 一次慢响应会长时间占住事务与连接。
	created, err := ch.CreatePayment(ctx, payment.CreateRequest{
		OrderNo:   order.OrderNo,
		Amount:    order.Amount,
		Subject:   "账户充值",
		NotifyURL: s.notifyURL(channelName),
		ExpireAt:  order.ExpiresAt,
	})
	if err != nil {
		return nil, errs.New(errs.CodeUpstream).WithCause(err)
	}
	order.ChannelOrderNo = created.ChannelOrderNo
	order.PayURL = created.PayURL

	if err := s.repo.Create(ctx, order); err != nil {
		return nil, err
	}
	return order, nil
}

// Get 按单号读取订单，并校验归属。
func (s *Service) Get(ctx context.Context, userID int64, orderNo string) (*Order, error) {
	order, err := s.repo.GetByNo(ctx, orderNo)
	if err != nil {
		return nil, mapError(err)
	}
	if order.UserID != userID {
		// 返回 404 而不是 403：告诉调用方「这张单存在但不属于你」，
		// 等于确认了单号有效，可以被用来枚举别人的订单。
		return nil, errs.New(errs.CodeNotFound)
	}
	return order, nil
}

// List 分页读取当前用户的充值订单。
func (s *Service) List(ctx context.Context, userID int64, filter Filter) (*Page, error) {
	page, err := s.repo.ListByUser(ctx, userID, filter)
	if err != nil {
		return nil, err
	}
	if page.Items == nil {
		// 保证序列化成 []，前端 .map() 不会因为 null 崩掉
		page.Items = []Order{}
	}
	return page, nil
}

// Ack 返回渠道约定的成功应答体。渠道未注册时第二个返回值为 false。
func (s *Service) Ack(channelName string) (payment.Ack, bool) {
	ch, ok := s.channels[channelName]
	if !ok {
		return payment.Ack{}, false
	}
	return ch.Ack(), true
}

// notifyURL 拼接渠道回调地址。
func (s *Service) notifyURL(channel string) string {
	return s.baseURL + "/api/v1/payments/webhook/" + url.PathEscape(channel)
}

// ── 渠道回调 ──────────────────────────────────────

// HandleNotification 处理一次支付渠道回调。
//
// 全流程是：验签 → 幂等 → 校验 → 改单状态 + 钱包加款 + 写账本，
// 后三步在同一个事务里完成。
func (s *Service) HandleNotification(
	ctx context.Context,
	channelName string,
	raw []byte,
	signature string,
) error {
	if err := s.handle(ctx, channelName, raw, signature); err != nil {
		return mapError(err)
	}
	return nil
}

func (s *Service) handle(
	ctx context.Context,
	channelName string,
	raw []byte,
	signature string,
) error {
	ch, ok := s.channels[channelName]
	if !ok {
		return fmt.Errorf("%w: %s", payment.ErrUnknownChannel, channelName)
	}

	// 验签先于一切。验签失败时下面一行数据库都不会碰——
	// 「Webhook 验签失败不会修改订单状态」这条验收标准就是这样实现的：
	// 不是「验签失败后回滚」，而是根本没有任何东西需要回滚。
	notif, err := ch.ParseNotification(raw, signature)
	if err != nil {
		return err
	}

	// 事务边界在这里。改单状态、写渠道流水、加款、写账本必须原子完成，
	// 任何一步失败都不能留下半成品——尤其是「钱加了但订单还是待支付」。
	//
	// 反向验证过它的必要性：把这一层去掉后，金额不符与订单过期这两条
	// 拒绝路径会各自提交一条渠道流水，于是那个交易号被永久占住，
	// 渠道随后的重试会被判成重放而静默返回成功——
	// 结果是钱收了、账上却没有，而且不报错。
	// 由 TestRetryAfterFailureSucceeds 与 TestWrongAmountIsRejected 覆盖。
	return s.txm.Run(ctx, func(ctx context.Context) error {
		return s.apply(ctx, notif)
	})
}

func (s *Service) apply(ctx context.Context, n *payment.Notification) error {
	// 1. 锁定订单。
	//
	// 先锁订单再落渠道流水，是为了让「订单不存在」得到一个明确的业务错误，
	// 而不是外键约束失败。锁本身也是必需的：状态校验与改状态必须在同一个
	// 锁下完成，否则两个并发回调会各自读到 pending、各自认为可以入账。
	order, err := s.repo.LockByNo(ctx, n.OrderNo)
	if err != nil {
		return err
	}

	// 2. 幂等闸门。
	replay, err := s.recordTransaction(ctx, order, n)
	if err != nil {
		return err
	}
	if replay {
		// 渠道在重试同一次通知。直接返回成功让它停止重试，
		// 不再改动订单，也不再动钱包。
		return nil
	}

	// 3. 金额必须逐分相等。
	//
	// 这是防「用 1 分钱的支付回调核销一张 1000 元充值单」的关键一步。
	// 校验放在落库之后，但失败会回滚整笔事务，渠道流水不会留下——
	// 这是刻意的：幂等键只能在操作真正成功时提交。
	// 否则一次失败的尝试会永久占住那个交易号，后续合法的回调会被
	// 当成重放而丢掉，变成「钱收了但没入账」。
	if order.Amount != n.Amount {
		return fmt.Errorf("%w: 订单 %d 分，回执 %d 分",
			ErrAmountMismatch, order.Amount, n.Amount)
	}

	// 4. 过期订单不入账。
	if s.clock().After(order.ExpiresAt) {
		return fmt.Errorf("%w: 过期于 %s",
			ErrOrderExpired, order.ExpiresAt.UTC().Format(time.RFC3339))
	}

	target := StatusFailed
	if n.Status == payment.StatusSuccess {
		target = StatusPaid
	}
	if !order.Status.CanTransitionTo(target) {
		return fmt.Errorf("%w: %s → %s", ErrOrderNotPayable, order.Status, target)
	}

	if target == StatusFailed {
		return s.repo.MarkFailed(ctx, order.OrderNo)
	}

	paidAt := n.PaidAt
	if paidAt.IsZero() {
		// 渠道没给支付时间时用本地时间兜底。订单状态与账本在同一个事务里，
		// 时间偏差最多是这一次请求的耗时，不影响对账。
		paidAt = s.clock().UTC()
	}

	// 5. 加款。走钱包域，加入当前事务。
	if _, err := s.wallet.Recharge(ctx, wallet.ChangeInput{
		UserID:  order.UserID,
		Amount:  order.Amount,
		BizType: bizTypeRechargeOrder,
		BizNo:   order.OrderNo,
		Remark:  "充值到账",
	}); err != nil {
		return err
	}

	return s.repo.MarkPaid(ctx, order.OrderNo, n.ChannelTradeNo, paidAt)
}

// recordTransaction 写入渠道流水，返回本次回调是否为一次重放。
//
// 幂等靠数据库的唯一索引拦下，而不是「先查有没有、没有就插」——
// 后者中间有竞态窗口，并发的两次重复回调会双双通过检查，然后各加一次款。
func (s *Service) recordTransaction(
	ctx context.Context,
	order *Order,
	n *payment.Notification,
) (bool, error) {
	var paidAt *time.Time
	if !n.PaidAt.IsZero() {
		t := n.PaidAt
		paidAt = &t
	}

	err := s.repo.InsertTransaction(ctx, &Transaction{
		Channel:        n.Channel,
		ChannelTradeNo: n.ChannelTradeNo,
		OrderNo:        order.OrderNo,
		UserID:         order.UserID,
		Amount:         n.Amount,
		Status:         n.Status,
		PaidAt:         paidAt,
		RawPayload:     truncatePayload(n.Raw),
	})
	if err == nil {
		return false, nil
	}
	if !mysql.IsDuplicateKey(err) {
		return false, err
	}

	// 撞唯一索引 = 这个渠道交易号已经处理过。
	//
	// 但必须确认「是同一件事」：同一个交易号被用在另一张订单或另一个金额上，
	// 说明渠道串单或有人在伪造。静默返回成功会把问题藏到对账时才暴露，
	// 而那时钱已经对不上了。
	stored, findErr := s.repo.FindTransaction(ctx, n.Channel, n.ChannelTradeNo)
	if findErr != nil {
		return false, findErr
	}
	if stored == nil {
		return false, fmt.Errorf("渠道交易号 %s 冲突但读不到已有记录", n.ChannelTradeNo)
	}
	if stored.OrderNo != order.OrderNo || stored.Amount != n.Amount {
		return false, fmt.Errorf("%w: channel=%s trade_no=%s 已用于订单 %s（%d 分）",
			ErrTransactionReused, n.Channel, n.ChannelTradeNo, stored.OrderNo, stored.Amount)
	}

	slog.InfoContext(ctx, "支付回调重放，跳过入账",
		"channel", n.Channel,
		"channel_trade_no", n.ChannelTradeNo,
		"order_no", order.OrderNo)
	return true, nil
}

// mapError 把领域哨兵错误翻译成业务错误码。
//
// 与 wallet 域同样的分层：判定发生在持有行锁的仓储层，
// 而 HTTP 语义属于服务层，映射只在这一处发生。
func mapError(err error) error {
	switch {
	case err == nil:
		return nil

	case errors.Is(err, ErrOrderNotFound):
		return errs.New(errs.CodeNotFound).WithCause(err)

	case errors.Is(err, ErrAmountMismatch), errors.Is(err, ErrTransactionReused):
		return errs.Newf(errs.CodeInvalidParam, "%s", err.Error()).WithCause(err)

	case errors.Is(err, ErrOrderExpired), errors.Is(err, ErrOrderNotPayable):
		return errs.Newf(errs.CodeInvalidOrderState, "%s", err.Error()).WithCause(err)

	case errors.Is(err, payment.ErrUnknownChannel):
		return errs.New(errs.CodeNotFound).WithCause(err)

	case errors.Is(err, payment.ErrInvalidSignature):
		return errs.Newf(errs.CodeUnauthorized, "回调签名校验失败").WithCause(err)

	case errors.Is(err, payment.ErrMalformedPayload):
		return errs.Newf(errs.CodeInvalidParam, "回调报文格式错误").WithCause(err)

	default:
		return err
	}
}
