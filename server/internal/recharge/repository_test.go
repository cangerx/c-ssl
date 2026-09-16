package recharge

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/errs"
	"github.com/cangerx/c-ssl/server/internal/domain/money"
	"github.com/cangerx/c-ssl/server/internal/platform/tx"
	"github.com/cangerx/c-ssl/server/internal/testutil"
	"github.com/cangerx/c-ssl/server/internal/upstream/payment"
	"github.com/cangerx/c-ssl/server/internal/wallet"
)

// 本文件的测试全部打真实数据库。
//
// 充值域的验收标准——「充值回调重复提交不会重复入账」「Webhook 验签失败
// 不会修改订单状态」——依赖唯一索引、行锁与事务回滚的真实行为，mock 验证不了：
// mock 只能证明「代码按我设想的方式调用了接口」，而设想本身可能才是错的。
//
// 钱包用的是真实的 wallet.Service 而不是替身：本域最关键的一条性质是
// 「改单状态、写渠道流水、加款、写账本在一个事务里原子完成」，
// 用替身钱包就绕开了这个性质本身。

const testSecret = "test-payment-webhook-secret-0123456789"

type testEnv struct {
	svc     *Service
	wallet  *wallet.Service
	channel *payment.MockChannel
	db      *sql.DB
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	db := testutil.OpenTestDB(t)

	ch, err := payment.NewMockChannel(testSecret, "http://localhost:8080")
	if err != nil {
		t.Fatalf("构造 Mock 支付渠道失败: %v", err)
	}

	walletSvc := wallet.NewService(wallet.NewRepository(db))
	svc := NewService(
		NewRepository(db),
		walletSvc,
		[]payment.Channel{ch},
		payment.NameMock,
		"http://localhost:8080",
	)

	return &testEnv{svc: svc, wallet: walletSvc, channel: ch, db: db}
}

// ── 辅助 ──────────────────────────────────────────

func (e *testEnv) createOrder(t *testing.T, userID int64, amount money.Amount) *Order {
	t.Helper()
	order, err := e.svc.Create(context.Background(), CreateInput{
		UserID: userID, Amount: amount,
	})
	if err != nil {
		t.Fatalf("创建充值订单失败: %v", err)
	}
	return order
}

// notify 走完整回调路径：编码报文 → 签名 → 交给 Service 处理（含验签）。
func (e *testEnv) notify(t *testing.T, n *payment.Notification) error {
	t.Helper()
	raw, signature, err := e.channel.EncodeNotification(n)
	if err != nil {
		t.Fatalf("编码回调报文失败: %v", err)
	}
	return e.svc.HandleNotification(context.Background(), payment.NameMock, raw, signature)
}

// paidNotification 构造一条「金额与订单一致」的成功回调。
func (e *testEnv) paidNotification(order *Order, tradeNo string) *payment.Notification {
	return &payment.Notification{
		Channel:        payment.NameMock,
		ChannelTradeNo: tradeNo,
		ChannelOrderNo: order.ChannelOrderNo,
		OrderNo:        order.OrderNo,
		Amount:         order.Amount,
		Status:         payment.StatusSuccess,
		PaidAt:         time.Now().UTC(),
	}
}

func (e *testEnv) availableBalance(t *testing.T, userID int64) money.Amount {
	t.Helper()
	account, err := e.wallet.Get(context.Background(), userID)
	if err != nil {
		t.Fatalf("查询钱包失败: %v", err)
	}
	return account.AvailableBalance
}

func (e *testEnv) orderStatus(t *testing.T, orderNo string) Status {
	t.Helper()
	var status string
	if err := e.db.QueryRow(
		`SELECT status FROM recharge_orders WHERE order_no = ?`, orderNo).Scan(&status); err != nil {
		t.Fatalf("查询订单状态失败: %v", err)
	}
	return Status(status)
}

func (e *testEnv) countTransactions(t *testing.T, orderNo string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(
		`SELECT COUNT(*) FROM payment_transactions WHERE order_no = ?`, orderNo).Scan(&n); err != nil {
		t.Fatalf("统计渠道流水失败: %v", err)
	}
	return n
}

func (e *testEnv) countLedgerEntries(t *testing.T, userID int64) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(
		`SELECT COUNT(*) FROM wallet_ledger WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatalf("统计账本流水失败: %v", err)
	}
	return n
}

// assertConsistent 校验「账户余额 == 账本累加值」这条不变量。
//
// 每个涉及资金的用例结束都跑一次：它同时覆盖了「加款有没有写账本」
// 与「加款金额对不对」两个问题，比单独断言余额更强。
func (e *testEnv) assertConsistent(t *testing.T, userID int64) {
	t.Helper()
	result, err := e.wallet.Reconcile(context.Background(), userID)
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if !result.Consistent() {
		available, frozen := result.Difference()
		t.Fatalf("账户余额与账本累加值不一致：可用差 %d，冻结差 %d（账户 %d/%d，账本 %d/%d，共 %d 条流水）",
			available, frozen,
			result.AccountAvailable, result.AccountFrozen,
			result.LedgerAvailable, result.LedgerFrozen, result.EntryCount)
	}
}

// assertNoSideEffects 断言「什么都没发生」。
//
// 验签失败、金额不符这类拒绝路径要同时检查订单状态、渠道流水、余额三处，
// 只查其中一处会漏掉「订单没改但钱加了」这种最糟的组合。
func (e *testEnv) assertNoSideEffects(t *testing.T, order *Order, userID int64) {
	t.Helper()
	if got := e.orderStatus(t, order.OrderNo); got != StatusPending {
		t.Errorf("订单状态应仍为 pending，实际 %s", got)
	}
	if n := e.countTransactions(t, order.OrderNo); n != 0 {
		t.Errorf("不应留下渠道流水，实际 %d 条", n)
	}
	if got := e.availableBalance(t, userID); !got.IsZero() {
		t.Errorf("余额应仍为 0，实际 %d", got)
	}
	if n := e.countLedgerEntries(t, userID); n != 0 {
		t.Errorf("不应留下账本流水，实际 %d 条", n)
	}
}

// ── 创建订单 ──────────────────────────────────────

// TestCreateOrderDoesNotTouchWallet 验证创建订单本身不动钱。
//
// 这是充值域最容易被误解的一点：用户点了「充值 100 元」不等于账户多了 100 元，
// 钱要等渠道回调确认之后才进来。若创建即入账，用户可以不付款白拿余额。
func TestCreateOrderDoesNotTouchWallet(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)

	order := env.createOrder(t, userID, 10000)

	if order.Status != StatusPending {
		t.Errorf("新订单应为 pending，实际 %s", order.Status)
	}
	if order.PayURL == "" {
		t.Error("新订单应带支付地址")
	}
	if order.ChannelOrderNo == "" {
		t.Error("新订单应记录渠道订单号")
	}
	if !order.ExpiresAt.After(time.Now()) {
		t.Errorf("过期时间应在将来，实际 %s", order.ExpiresAt)
	}

	if got := env.availableBalance(t, userID); !got.IsZero() {
		t.Errorf("创建订单不应改变余额，实际 %d", got)
	}
	if n := env.countLedgerEntries(t, userID); n != 0 {
		t.Errorf("创建订单不应写账本，实际 %d 条", n)
	}
}

func TestCreateOrderValidatesChannel(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)

	_, err := env.svc.Create(context.Background(), CreateInput{
		UserID: userID, Amount: 10000, Channel: "not-registered",
	})
	if err == nil {
		t.Fatal("未注册的渠道应被拒绝")
	}
	if code := errs.From(err).Code; code != errs.CodeInvalidParam {
		t.Errorf("错误码期望 %d，实际 %d", errs.CodeInvalidParam, code)
	}
}

// TestCreateOrderRecordsCallbackURL 验证下单时把回调地址告诉了渠道。
//
// 用「记录参数的替身渠道」而不是 Mock 渠道：Mock 渠道不走网络，
// 忽略 NotifyURL 是合理的，但那会让这个字段永远不被验证。
func TestCreateOrderRecordsCallbackURL(t *testing.T) {
	db := testutil.OpenTestDB(t)
	userID := testutil.CreateTestUser(t, db)

	recorder := &recordingChannel{}
	svc := NewService(NewRepository(db), wallet.NewService(wallet.NewRepository(db)),
		[]payment.Channel{recorder}, "recorder", "http://localhost:8080")

	if _, err := svc.Create(context.Background(), CreateInput{UserID: userID, Amount: 10000}); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}

	want := "http://localhost:8080/api/v1/payments/webhook/recorder"
	if recorder.lastCreate.NotifyURL != want {
		t.Errorf("回调地址期望 %s，实际 %s", want, recorder.lastCreate.NotifyURL)
	}
	if recorder.lastCreate.Amount != 10000 {
		t.Errorf("下单金额期望 10000，实际 %d", recorder.lastCreate.Amount)
	}
	if recorder.lastCreate.OrderNo == "" {
		t.Error("下单时应把平台单号传给渠道")
	}
}

// recordingChannel 记录最后一次下单参数，其余方法返回固定值。
type recordingChannel struct {
	lastCreate payment.CreateRequest
}

func (c *recordingChannel) Name() string { return "recorder" }

func (c *recordingChannel) CreatePayment(_ context.Context, req payment.CreateRequest) (*payment.Payment, error) {
	c.lastCreate = req
	return &payment.Payment{ChannelOrderNo: "REC-1", PayURL: "http://example.test/pay"}, nil
}

func (c *recordingChannel) ParseNotification([]byte, string) (*payment.Notification, error) {
	return nil, payment.ErrInvalidSignature
}

func (c *recordingChannel) Ack() payment.Ack {
	return payment.Ack{ContentType: "application/json", Body: []byte(`{}`)}
}

// ── 成功回调 ──────────────────────────────────────

func TestSuccessfulCallbackCreditsWallet(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	if err := env.notify(t, env.paidNotification(order, "TRADE-OK-1")); err != nil {
		t.Fatalf("处理回调失败: %v", err)
	}

	if got := env.orderStatus(t, order.OrderNo); got != StatusPaid {
		t.Errorf("订单状态应为 paid，实际 %s", got)
	}
	if got := env.availableBalance(t, userID); got != 10000 {
		t.Errorf("余额应为 10000，实际 %d", got)
	}
	if n := env.countTransactions(t, order.OrderNo); n != 1 {
		t.Errorf("渠道流水应为 1 条，实际 %d 条", n)
	}
	env.assertConsistent(t, userID)
}

// TestCallbackRecordsOrderLinkage 验证账本流水能反查到充值单。
//
// 账本里只有「业务类型 + 业务单号」，对账时全靠这两个字段回答
// 「这笔钱是哪来的」。写错的话，钱是对的，但没人能解释它。
func TestCallbackRecordsOrderLinkage(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 5000)

	if err := env.notify(t, env.paidNotification(order, "TRADE-LINK-1")); err != nil {
		t.Fatalf("处理回调失败: %v", err)
	}

	page, err := env.wallet.ListEntries(context.Background(), userID, wallet.EntryFilter{})
	if err != nil {
		t.Fatalf("查询账本失败: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("账本应有 1 条流水，实际 %d 条", len(page.Items))
	}

	entry := page.Items[0]
	if entry.Op != wallet.OpRecharge {
		t.Errorf("操作类型应为 recharge，实际 %s", entry.Op)
	}
	if entry.BizType != bizTypeRechargeOrder {
		t.Errorf("业务类型期望 %s，实际 %s", bizTypeRechargeOrder, entry.BizType)
	}
	if entry.BizNo != order.OrderNo {
		t.Errorf("业务单号期望 %s，实际 %s", order.OrderNo, entry.BizNo)
	}
	if entry.AvailableDelta != 5000 {
		t.Errorf("可用余额变动期望 5000，实际 %d", entry.AvailableDelta)
	}
}

// ── 验收标准第 2 条：重复提交不重复入账 ────────────

// TestDuplicateCallbackCreditsOnce 是验收标准第 2 条的直接验证。
//
// 渠道在没收到成功应答时会重复投递，重复次数不确定，可能几十次。
// 三次重放必须只加一次款，且订单只被改一次。
func TestDuplicateCallbackCreditsOnce(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	// 同一个渠道交易号，投递三次
	for i := range 3 {
		if err := env.notify(t, env.paidNotification(order, "TRADE-DUP-1")); err != nil {
			t.Fatalf("第 %d 次回调不应报错（重放应返回成功让渠道停止重试）: %v", i+1, err)
		}
	}

	if got := env.availableBalance(t, userID); got != 10000 {
		t.Errorf("余额应只增加一次，期望 10000，实际 %d", got)
	}
	if n := env.countLedgerEntries(t, userID); n != 1 {
		t.Errorf("账本应只有 1 条流水，实际 %d 条", n)
	}
	if n := env.countTransactions(t, order.OrderNo); n != 1 {
		t.Errorf("渠道流水应只有 1 条，实际 %d 条", n)
	}
	if got := env.orderStatus(t, order.OrderNo); got != StatusPaid {
		t.Errorf("订单状态应为 paid，实际 %s", got)
	}
	env.assertConsistent(t, userID)
}

// TestConcurrentDuplicateCallbacksCreditOnce 验证并发重复投递也只加一次款。
//
// 单线程重放只能证明「第二次被拦住了」，证明不了闸门本身是对的：
// 「先查有没有、没有就插」的实现在单线程下表现完全正确，
// 只有并发时才会双双通过检查、各加一次款。
func TestConcurrentDuplicateCallbacksCreditOnce(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	const workers = 10
	// 先各自把报文编好，避免把编码开销算进并发窗口
	raw, signature, err := env.channel.EncodeNotification(env.paidNotification(order, "TRADE-RACE-1"))
	if err != nil {
		t.Fatalf("编码回调报文失败: %v", err)
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []error
	)
	start := make(chan struct{})

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := env.svc.HandleNotification(
				context.Background(), payment.NameMock, raw, signature); err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		}()
	}

	close(start)
	wg.Wait()

	// 重放必须返回成功：返回错误会让渠道一直重试，而重试永远不会成功
	if len(failures) > 0 {
		t.Fatalf("%d/%d 次并发重复回调失败（重放应返回成功）: %v", len(failures), workers, failures[0])
	}

	if got := env.availableBalance(t, userID); got != 10000 {
		t.Errorf("并发重复回调后余额期望 10000，实际 %d", got)
	}
	if n := env.countLedgerEntries(t, userID); n != 1 {
		t.Errorf("账本应只有 1 条流水，实际 %d 条", n)
	}
	if n := env.countTransactions(t, order.OrderNo); n != 1 {
		t.Errorf("渠道流水应只有 1 条，实际 %d 条", n)
	}
	env.assertConsistent(t, userID)
}

// TestConcurrentSameTradeNoOnDifferentOrdersCreditsOnce 是幂等闸门的正面证据。
//
// 上一条测试（同一订单并发重放）看起来像是在验证唯一索引，其实不是：
// 同一订单的行锁已经把并发回调串行化了，「先查再插」的实现同样能通过。
// 实测确认过这一点——把实现换成先查再插，那条测试依然是绿的。
//
// 唯一索引真正不可替代的场景是这一条：两张不同订单的并发回调带上了
// 同一个渠道交易号。两个事务锁的是不同的订单行，谁也拦不住谁，
// 只能靠 (channel, channel_trade_no) 上的唯一索引让其中一个失败。
//
// 期望的结果是「恰好入账一次」：赢的那个正常加款，输的那个拿到
// ErrTransactionReused 而不是静默成功——静默成功意味着有一笔钱
// 被记到了错误的订单上。
func TestConcurrentSameTradeNoOnDifferentOrdersCreditsOnce(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)

	first := env.createOrder(t, userID, 10000)
	second := env.createOrder(t, userID, 20000)

	const tradeNo = "TRADE-CROSS-ORDER"
	payloads := [][]byte{}
	signatures := []string{}
	for _, order := range []*Order{first, second} {
		raw, signature, err := env.channel.EncodeNotification(env.paidNotification(order, tradeNo))
		if err != nil {
			t.Fatalf("编码回调报文失败: %v", err)
		}
		payloads = append(payloads, raw)
		signatures = append(signatures, signature)
	}

	results := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = env.svc.HandleNotification(
				context.Background(), payment.NameMock, payloads[i], signatures[i])
		}()
	}
	close(start)
	wg.Wait()

	var succeeded, rejected int
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrTransactionReused):
			rejected++
		default:
			t.Errorf("意外的错误: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("期望恰好 1 次成功、1 次被拒，实际成功 %d 次、被拒 %d 次", succeeded, rejected)
	}

	// 恰好入账一笔，金额是两张订单之一
	balance := env.availableBalance(t, userID)
	if balance != 10000 && balance != 20000 {
		t.Errorf("余额应为 10000 或 20000（恰好入账一次），实际 %d", balance)
	}
	if n := env.countLedgerEntries(t, userID); n != 1 {
		t.Errorf("账本应只有 1 条流水，实际 %d 条", n)
	}
	env.assertConsistent(t, userID)
}

// TestConcurrentCallbacksOnDifferentOrdersAllCredit 是上一条的对照。
//
// 闸门必须只拦「同一笔支付」，不能把不同订单的并发回调也拦掉——
// 那会变成「并发时随机少入账」，比重复入账更难发现。
func TestConcurrentCallbacksOnDifferentOrdersAllCredit(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)

	const orders = 8
	created := make([]*Order, 0, orders)
	for range orders {
		created = append(created, env.createOrder(t, userID, 1000))
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errsCh := make(chan error, orders)

	for i, order := range created {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := env.notify(t, env.paidNotification(order, "TRADE-MULTI-"+strconv.Itoa(i))); err != nil {
				errsCh <- err
			}
		}()
	}

	close(start)
	wg.Wait()
	close(errsCh)

	for err := range errsCh {
		t.Errorf("不同订单的并发回调不应失败: %v", err)
	}

	want := money.Amount(1000 * orders)
	if got := env.availableBalance(t, userID); got != want {
		t.Errorf("余额期望 %d，实际 %d", want, got)
	}
	env.assertConsistent(t, userID)
}

// ── 验收标准第 7 条：验签失败不修改订单状态 ────────

// TestInvalidSignatureChangesNothing 是验收标准第 7 条的直接验证。
//
// 关键在于「什么都没发生」而不是「事后回滚」：验签在读取任何数据之前完成，
// 因此不存在需要回滚的东西。这也意味着伪造回调连一条日志之外
// 的痕迹都不会留下。
func TestInvalidSignatureChangesNothing(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	raw, goodSignature, err := env.channel.EncodeNotification(env.paidNotification(order, "TRADE-FORGED"))
	if err != nil {
		t.Fatalf("编码回调报文失败: %v", err)
	}

	cases := map[string]string{
		"没有签名":    "",
		"签名被截断":   goodSignature[:len(goodSignature)-4],
		"签名被篡改":   flipLastChar(goodSignature),
		"用别的密钥签名": signWithAnotherSecret(t, raw),
	}

	for name, signature := range cases {
		t.Run(name, func(t *testing.T) {
			err := env.svc.HandleNotification(
				context.Background(), payment.NameMock, raw, signature)
			if !errors.Is(err, payment.ErrInvalidSignature) {
				t.Fatalf("期望 ErrInvalidSignature，实际 %v", err)
			}
			// 错误码必须是 1001，HTTP 上是 401
			if code := errs.From(err).Code; code != errs.CodeUnauthorized {
				t.Errorf("错误码期望 %d，实际 %d", errs.CodeUnauthorized, code)
			}
			env.assertNoSideEffects(t, order, userID)
		})
	}
}

// TestTamperedBodyWithValidSignatureIsRejected 覆盖「签名对不上报文」的情况。
//
// 攻击者可以拿到一条合法回调的签名，然后把报文里的金额改大。
// 签名覆盖的是原始字节，改动任何一位都会验签失败。
func TestTamperedBodyWithValidSignatureIsRejected(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	raw, signature, err := env.channel.EncodeNotification(env.paidNotification(order, "TRADE-TAMPER"))
	if err != nil {
		t.Fatalf("编码回调报文失败: %v", err)
	}

	// 把报文里的金额从 10000 改成 99999999
	tampered := []byte(string(raw))
	tampered = []byte(replaceOnce(string(tampered), `"amount":10000`, `"amount":99999999`))
	if string(tampered) == string(raw) {
		t.Fatal("测试没有改到报文，断言失去意义")
	}

	err = env.svc.HandleNotification(context.Background(), payment.NameMock, tampered, signature)
	if !errors.Is(err, payment.ErrInvalidSignature) {
		t.Fatalf("篡改报文应验签失败，实际 %v", err)
	}
	env.assertNoSideEffects(t, order, userID)
}

func TestUnknownChannelIsRejected(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	err := env.svc.HandleNotification(
		context.Background(), "no-such-channel", []byte(`{}`), "sig")
	if code := errs.From(err).Code; code != errs.CodeNotFound {
		t.Errorf("未注册渠道的错误码期望 %d，实际 %d", errs.CodeNotFound, code)
	}
	env.assertNoSideEffects(t, order, userID)
}

// ── 金额与订单校验 ────────────────────────────────

// TestWrongAmountIsRejected 覆盖「用 1 分钱的回调核销一张 1000 元充值单」。
//
// 注意断言的是「不等就拒」，不是「少了才拒」：只判「回执金额小于订单金额」
// 的话，多付的钱会被当成合法回调入账，而实际入账金额取自订单，
// 用户多付的部分就凭空消失了。
func TestWrongAmountIsRejected(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	for _, amount := range []money.Amount{1, 9999, 10001, 100000} {
		t.Run(amount.String(), func(t *testing.T) {
			n := env.paidNotification(order, "TRADE-AMOUNT-"+amount.String())
			n.Amount = amount

			err := env.notify(t, n)
			if !errors.Is(err, ErrAmountMismatch) {
				t.Fatalf("金额不符应被拒绝，实际 %v", err)
			}
			if code := errs.From(err).Code; code != errs.CodeInvalidParam {
				t.Errorf("错误码期望 %d，实际 %d", errs.CodeInvalidParam, code)
			}
			env.assertNoSideEffects(t, order, userID)
		})
	}
}

func TestCallbackForUnknownOrderIsRejected(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)

	n := &payment.Notification{
		Channel:        payment.NameMock,
		ChannelTradeNo: "TRADE-GHOST",
		OrderNo:        "RC00000000000000ZZZZZZ",
		Amount:         10000,
		Status:         payment.StatusSuccess,
		PaidAt:         time.Now().UTC(),
	}

	err := env.notify(t, n)
	if !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("不存在的订单应被拒绝，实际 %v", err)
	}
	if code := errs.From(err).Code; code != errs.CodeNotFound {
		t.Errorf("错误码期望 %d，实际 %d", errs.CodeNotFound, code)
	}
	if got := env.availableBalance(t, userID); !got.IsZero() {
		t.Errorf("余额应仍为 0，实际 %d", got)
	}
}

// TestExpiredOrderIsNotCredited 验证过期订单不入账。
//
// 渠道的支付结果不可信地晚到时，自动入账会把一笔已经对不上的钱塞进账本：
// 用户可能已经重新下过单并付过款了。宁可让这笔钱停在待人工核对的状态。
func TestExpiredOrderIsNotCredited(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	// 把时钟拨到订单过期之后
	env.svc.clock = func() time.Time { return order.ExpiresAt.Add(time.Minute) }

	err := env.notify(t, env.paidNotification(order, "TRADE-LATE"))
	if !errors.Is(err, ErrOrderExpired) {
		t.Fatalf("过期订单应被拒绝，实际 %v", err)
	}
	if code := errs.From(err).Code; code != errs.CodeInvalidOrderState {
		t.Errorf("错误码期望 %d，实际 %d", errs.CodeInvalidOrderState, code)
	}
	env.assertNoSideEffects(t, order, userID)
}

// TestTradeNoReuseAcrossOrdersIsRejected 验证同一交易号不能核销另一张订单。
//
// 渠道串单或有人伪造时会出现这种情况。静默返回成功会把问题藏到对账时才暴露，
// 而那时钱已经对不上了。
func TestTradeNoReuseAcrossOrdersIsRejected(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)

	first := env.createOrder(t, userID, 10000)
	second := env.createOrder(t, userID, 20000)

	const tradeNo = "TRADE-REUSED"
	if err := env.notify(t, env.paidNotification(first, tradeNo)); err != nil {
		t.Fatalf("第一张订单的回调应成功: %v", err)
	}

	err := env.notify(t, env.paidNotification(second, tradeNo))
	if !errors.Is(err, ErrTransactionReused) {
		t.Fatalf("交易号被复用时应付错，实际 %v", err)
	}
	if code := errs.From(err).Code; code != errs.CodeInvalidParam {
		t.Errorf("错误码期望 %d，实际 %d", errs.CodeInvalidParam, code)
	}

	// 第二张订单必须原封不动，第一张的入账不受影响
	if got := env.orderStatus(t, second.OrderNo); got != StatusPending {
		t.Errorf("第二张订单应仍为 pending，实际 %s", got)
	}
	if got := env.availableBalance(t, userID); got != 10000 {
		t.Errorf("余额应只有第一张订单的 10000，实际 %d", got)
	}
	env.assertConsistent(t, userID)
}

// TestSecondPaymentOnPaidOrderIsRejected 覆盖「同一张订单被支付两次」。
//
// 用户重复付款时，第二次的回调会带一个不同的交易号。
// 订单已是终态，必须拒绝而不是再入一次账——多付的钱需要人工退款，
// 自动入账会让平台平白多欠用户一笔钱。
func TestSecondPaymentOnPaidOrderIsRejected(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	if err := env.notify(t, env.paidNotification(order, "TRADE-FIRST")); err != nil {
		t.Fatalf("第一次回调应成功: %v", err)
	}

	err := env.notify(t, env.paidNotification(order, "TRADE-SECOND"))
	if !errors.Is(err, ErrOrderNotPayable) {
		t.Fatalf("已支付订单的再次回调应被拒绝，实际 %v", err)
	}

	if got := env.availableBalance(t, userID); got != 10000 {
		t.Errorf("余额应仍为 10000，实际 %d", got)
	}
	env.assertConsistent(t, userID)
}

// ── 失败通知 ──────────────────────────────────────

func TestFailedNotificationMarksOrderFailed(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	n := env.paidNotification(order, "TRADE-FAILED")
	n.Status = payment.StatusFailed

	if err := env.notify(t, n); err != nil {
		t.Fatalf("失败通知不应报错: %v", err)
	}

	if got := env.orderStatus(t, order.OrderNo); got != StatusFailed {
		t.Errorf("订单状态应为 failed，实际 %s", got)
	}
	if got := env.availableBalance(t, userID); !got.IsZero() {
		t.Errorf("支付失败不应改变余额，实际 %d", got)
	}
	if n := env.countLedgerEntries(t, userID); n != 0 {
		t.Errorf("支付失败不应写账本，实际 %d 条", n)
	}
	env.assertConsistent(t, userID)
}

// TestDuplicateFailedNotificationIsIdempotent 验证失败通知同样幂等。
func TestDuplicateFailedNotificationIsIdempotent(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	n := env.paidNotification(order, "TRADE-FAILED-DUP")
	n.Status = payment.StatusFailed

	for i := range 3 {
		if err := env.notify(t, n); err != nil {
			t.Fatalf("第 %d 次失败通知不应报错: %v", i+1, err)
		}
	}

	if got := env.orderStatus(t, order.OrderNo); got != StatusFailed {
		t.Errorf("订单状态应为 failed，实际 %s", got)
	}
	if n := env.countTransactions(t, order.OrderNo); n != 1 {
		t.Errorf("渠道流水应只有 1 条，实际 %d 条", n)
	}
}

// ── 事务原子性 ────────────────────────────────────

// TestWalletFailureRollsBackOrderState 验证加款失败时订单状态一并回滚。
//
// 这是跨域事务的核心性质：改单状态、写渠道流水、加款、写账本要么全成、
// 要么全不成。「订单已支付但钱没到账」比「订单没改但钱到了」更难被发现，
// 因为用户看到的是「已支付」，而账上没钱。
func TestWalletFailureRollsBackOrderState(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	// 换成一个会失败的钱包
	boom := errors.New("模拟钱包不可用")
	env.svc.wallet = failingWallet{err: boom}

	err := env.notify(t, env.paidNotification(order, "TRADE-ROLLBACK"))
	if !errors.Is(err, boom) {
		t.Fatalf("钱包错误应被透传，实际 %v", err)
	}

	// 订单、渠道流水都必须回滚
	if got := env.orderStatus(t, order.OrderNo); got != StatusPending {
		t.Errorf("加款失败时订单状态应回滚为 pending，实际 %s", got)
	}
	if n := env.countTransactions(t, order.OrderNo); n != 0 {
		t.Errorf("加款失败时渠道流水应回滚，实际 %d 条", n)
	}
}

// TestRetryAfterFailureSucceeds 验证失败后重试同一个交易号仍然能入账。
//
// 这条锁住的是一个很容易写错的细节：幂等键只能在操作真正成功时提交。
// 如果渠道流水在钱包失败时被单独提交（比如用了独立的连接或事务），
// 那个交易号就被永久占住了——渠道重试会被判成重放而返回成功，
// 结果是一笔真实收款永远不会入账，而且不报错。
func TestRetryAfterFailureSucceeds(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	const tradeNo = "TRADE-RETRY"

	boom := errors.New("模拟钱包不可用")
	env.svc.wallet = failingWallet{err: boom}
	if err := env.notify(t, env.paidNotification(order, tradeNo)); !errors.Is(err, boom) {
		t.Fatalf("第一次回调应失败于钱包错误，实际 %v", err)
	}

	// 钱包恢复，渠道重试同一个交易号
	env.svc.wallet = env.wallet
	if err := env.notify(t, env.paidNotification(order, tradeNo)); err != nil {
		t.Fatalf("重试应成功入账，实际 %v", err)
	}

	if got := env.availableBalance(t, userID); got != 10000 {
		t.Errorf("重试后余额期望 10000，实际 %d", got)
	}
	if got := env.orderStatus(t, order.OrderNo); got != StatusPaid {
		t.Errorf("重试后订单状态应为 paid，实际 %s", got)
	}
	env.assertConsistent(t, userID)
}

type failingWallet struct{ err error }

func (w failingWallet) Recharge(context.Context, wallet.ChangeInput) (*wallet.Result, error) {
	return nil, w.err
}

// ── 查询 ──────────────────────────────────────────

func TestListByUserPagination(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)

	const total = 5
	created := make([]*Order, 0, total)
	for range total {
		created = append(created, env.createOrder(t, userID, 1000))
	}

	// 第一页：2 条，最新的在最前
	page, err := env.svc.List(context.Background(), userID, Filter{Limit: 2})
	if err != nil {
		t.Fatalf("查询订单列表失败: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("第一页应有 2 条，实际 %d 条", len(page.Items))
	}
	if page.Items[0].OrderNo != created[total-1].OrderNo {
		t.Errorf("最新的订单应在最前，实际 %s", page.Items[0].OrderNo)
	}
	if page.NextCursor == 0 {
		t.Fatal("还有更多数据时 NextCursor 不应为 0")
	}

	// 依次翻完
	seen := make(map[string]bool)
	for _, item := range page.Items {
		seen[item.OrderNo] = true
	}
	cursor := page.NextCursor
	for cursor != 0 {
		next, err := env.svc.List(context.Background(), userID, Filter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("翻页失败: %v", err)
		}
		for _, item := range next.Items {
			if seen[item.OrderNo] {
				t.Errorf("翻页出现重复记录: %s", item.OrderNo)
			}
			seen[item.OrderNo] = true
		}
		cursor = next.NextCursor
	}

	if len(seen) != total {
		t.Errorf("翻页共应看到 %d 条，实际 %d 条", total, len(seen))
	}
}

func TestListIsScopedToUser(t *testing.T) {
	env := newTestEnv(t)
	mine := testutil.CreateTestUser(t, env.db)
	other := testutil.CreateTestUser(t, env.db)

	env.createOrder(t, mine, 1000)
	env.createOrder(t, other, 2000)

	page, err := env.svc.List(context.Background(), mine, Filter{})
	if err != nil {
		t.Fatalf("查询订单列表失败: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("只应看到自己的 1 张订单，实际 %d 张", len(page.Items))
	}
	if page.Items[0].Amount != 1000 {
		t.Errorf("看到了别人的订单：金额 %d", page.Items[0].Amount)
	}
}

// TestGetRejectsOtherUsersOrder 验证越权查询返回 404 而不是 403。
//
// 403 等于确认「这张单存在但不属于你」，攻击者可以据此枚举有效单号。
func TestGetRejectsOtherUsersOrder(t *testing.T) {
	env := newTestEnv(t)
	owner := testutil.CreateTestUser(t, env.db)
	intruder := testutil.CreateTestUser(t, env.db)

	order := env.createOrder(t, owner, 10000)

	if _, err := env.svc.Get(context.Background(), owner, order.OrderNo); err != nil {
		t.Fatalf("本人查询应成功: %v", err)
	}

	_, err := env.svc.Get(context.Background(), intruder, order.OrderNo)
	if code := errs.From(err).Code; code != errs.CodeNotFound {
		t.Errorf("越权查询的错误码期望 %d，实际 %d", errs.CodeNotFound, code)
	}
}

// TestEmptyListSerializesAsArray 验证空列表不是 nil。
//
// nil 会被序列化成 null，前端 .map() 会直接崩掉。
func TestEmptyListSerializesAsArray(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)

	page, err := env.svc.List(context.Background(), userID, Filter{})
	if err != nil {
		t.Fatalf("查询订单列表失败: %v", err)
	}
	if page.Items == nil {
		t.Error("空列表应为空切片而不是 nil")
	}
}

// TestLockingReadSeesConcurrentCommit 固定「加锁读与普通读不一样」这个事实。
//
// 幂等判重必须用加锁读。这条测试是那个选择唯一的证据来源——
// 实测确认过：把实现换成普通读，全部业务测试依然是绿的。
// 也就是说，这个差别在常规测试里观察不到，只能直接测出来。
//
// 差别来自 REPEATABLE READ 的读视图建立时机：
//
//   - 普通读在事务里的第一次执行时建立快照，之后一直读那个快照；
//   - 加锁读（FOR SHARE / FOR UPDATE）总是读最新已提交版本。
//
// 现在的实现之所以用普通读也「碰巧」正确，是因为事务里在它之前
// 没有发生过普通读，快照恰好建立在那条流水提交之后。
// 这个前提是隐式且脆弱的：将来只要有人在事务里先做一次普通查询
// （比如为了记一条日志而读一下订单），重放就会被误判成
// 「冲突了却查不到记录」。所以 Repository 只提供加锁读一个版本。
func TestLockingReadSeesConcurrentCommit(t *testing.T) {
	env := newTestEnv(t)
	userID := testutil.CreateTestUser(t, env.db)
	order := env.createOrder(t, userID, 10000)

	const tradeNo = "TRADE-VISIBILITY"

	// 另一条独立连接（另一个连接池，因此是不同的物理连接）
	other := testutil.OpenTestDB(t)

	handle, err := env.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = handle.Rollback() }()

	// 在本事务里先做一次普通读，把快照定在此刻
	var before int
	if err := handle.QueryRow(`SELECT COUNT(*) FROM payment_transactions`).Scan(&before); err != nil {
		t.Fatalf("统计渠道流水失败: %v", err)
	}

	// 另一条连接插入并提交一条流水
	if _, err := other.Exec(
		`INSERT INTO payment_transactions
			(channel, channel_trade_no, order_no, user_id, amount, status, paid_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		payment.NameMock, tradeNo, order.OrderNo, userID, 10000, "success", time.Now()); err != nil {
		t.Fatalf("另一条连接写入流水失败: %v", err)
	}

	ctxTx := tx.WithTx(context.Background(), handle)

	// 普通读：读的是快照，看不到刚提交的那条
	var plainCount int
	if err := handle.QueryRow(
		`SELECT COUNT(*) FROM payment_transactions WHERE channel = ? AND channel_trade_no = ?`,
		payment.NameMock, tradeNo).Scan(&plainCount); err != nil {
		t.Fatalf("普通读失败: %v", err)
	}
	if plainCount != 0 {
		t.Fatalf("普通读本应看不到快照之后提交的数据，实际看到 %d 条——"+
			"如果 MySQL 的隔离行为变了，这条断言需要重新评估", plainCount)
	}

	// 加锁读：看得到
	found, err := NewRepository(env.db).FindTransaction(ctxTx, payment.NameMock, tradeNo)
	if err != nil {
		t.Fatalf("加锁读失败: %v", err)
	}
	if found == nil {
		t.Fatal("加锁读应看到另一条连接刚提交的流水，实际什么都没读到——" +
			"幂等判重会因此把重放误判成「冲突了却查不到记录」")
	}
	if found.OrderNo != order.OrderNo {
		t.Errorf("读到的流水单号不符：期望 %s，实际 %s", order.OrderNo, found.OrderNo)
	}
}

// ── 工具 ──────────────────────────────────────────

func flipLastChar(s string) string {
	if s == "" {
		return "A"
	}
	last := s[len(s)-1]
	if last == 'A' {
		last = 'B'
	} else {
		last = 'A'
	}
	return s[:len(s)-1] + string(last)
}

func signWithAnotherSecret(t *testing.T, raw []byte) string {
	t.Helper()
	other, err := payment.NewMockChannel("a-completely-different-secret", "http://localhost:8080")
	if err != nil {
		t.Fatalf("构造对照渠道失败: %v", err)
	}
	return other.Sign(raw)
}

func replaceOnce(s, old, new string) string {
	idx := indexOf(s, old)
	if idx < 0 {
		return s
	}
	return s[:idx] + new + s[idx+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
