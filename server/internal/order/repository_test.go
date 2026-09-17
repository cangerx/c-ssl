package order

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
	"github.com/cangerx/c-ssl/server/internal/domain/orderstate"
	"github.com/cangerx/c-ssl/server/internal/platform/mysql"
	"github.com/cangerx/c-ssl/server/internal/product/rules"
	"github.com/cangerx/c-ssl/server/internal/testutil"
	"github.com/cangerx/c-ssl/server/internal/upstream/foxssl"
)

// 本文件覆盖订单仓储层里那些**只有真实数据库才能验证**的不变量：
// 唯一索引、CHECK 约束、带条件的 UPDATE、批量查询的排序。
//
// 这些约束的价值恰恰在于它们是数据库层的：应用层代码写错时它们会拦住，
// 而「拦住」这件事只有在真实数据库上才成立。用 mock 仓储测这些
// 等于测「我以为数据库会怎么做」。

type repoFixture struct {
	db     *sql.DB
	repo   *Repository
	userID int64
}

func newRepoFixture(t *testing.T) *repoFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	return &repoFixture{db: db, repo: NewRepository(db), userID: testutil.CreateTestUser(t, db)}
}

// newOrder 造一张最小可用的订单行。
func (f *repoFixture) newOrder(t *testing.T, orderNo, upstreamOrderNo string) *Order {
	t.Helper()
	o := &Order{
		OrderNo:           orderNo,
		UserID:            f.userID,
		ProductID:         productDV,
		ProductName:       "AlphaSSL DV 单域名",
		Brand:             "AlphaSSL",
		ValidationType:    rules.DV,
		UpstreamProductID: 101,
		Years:             1,
		KeyAlgorithm:      rules.RSA,
		Amount:            productDVRetail,
		Status:            orderstate.PendingPayment,
		UpstreamOrderNo:   upstreamOrderNo,
		Contact:           &Contact{Name: "张三", Email: "a@example.com", Phone: "+86.1"},
		Domains:           []Domain{NewPendingDomain(orderNo, "example.com", true)},
	}
	if err := f.repo.Create(context.Background(), o); err != nil {
		t.Fatalf("创建订单 %s 失败: %v", orderNo, err)
	}
	return o
}

// ── 唯一索引 ──────────────────────────────────────

// TestUpstreamOrderNoIsUnique 验证同一个上游订单号不能被两条本地订单引用。
//
// 这是本地侧的第二道闸门。第一道是向上游传本地 order_no 做幂等键；
// 但补偿流程或人工操作出错时，重复引用同一个上游订单会让平台
// 对同一张证书收两次钱。
func TestUpstreamOrderNoIsUnique(t *testing.T) {
	f := newRepoFixture(t)
	const upstreamNo = "FX-20260917-000001"

	f.newOrder(t, "CS20260917120000AAAAAA", upstreamNo)

	err := f.repo.Create(context.Background(), &Order{
		OrderNo:           "CS20260917120000BBBBBB",
		UserID:            f.userID,
		ProductID:         productDV,
		ProductName:       "AlphaSSL DV 单域名",
		Brand:             "AlphaSSL",
		ValidationType:    rules.DV,
		UpstreamProductID: 101,
		Years:             1,
		KeyAlgorithm:      rules.RSA,
		Amount:            productDVRetail,
		Status:            orderstate.PendingPayment,
		UpstreamOrderNo:   upstreamNo,
		Contact:           &Contact{Name: "李四", Email: "b@example.com", Phone: "+86.2"},
	})
	if err == nil {
		t.Fatal("重复引用同一个上游订单号应被唯一索引拦住")
	}
	if !mysql.IsDuplicateKey(err) {
		t.Errorf("应当是唯一键冲突，实际 %v", err)
	}
}

// TestMultipleUnsubmittedOrdersAreAllowed 是上一条的对照。
//
// MySQL 的唯一索引允许多个 NULL，所以「尚未提交上游」的订单不会互相冲突。
// 若实现里把空上游订单号写成空串而不是 NULL，第二条未提交的订单就会
// 因为唯一索引而插不进去——一个「只有下第二单时才会出现」的故障。
func TestMultipleUnsubmittedOrdersAreAllowed(t *testing.T) {
	f := newRepoFixture(t)

	for _, no := range []string{"CS20260917120000CCCCCC", "CS20260917120000DDDDDD"} {
		f.newOrder(t, no, "")
	}

	var n int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM certificate_orders WHERE user_id = ?`, f.userID).Scan(&n); err != nil {
		t.Fatalf("统计订单失败: %v", err)
	}
	if n != 2 {
		t.Errorf("两张未提交的订单都应存在，实际 %d 张", n)
	}
}

// TestDuplicateDomainInSameOrderIsRejected 验证同一订单里域名不能重复。
func TestDuplicateDomainInSameOrderIsRejected(t *testing.T) {
	f := newRepoFixture(t)
	o := f.newOrder(t, "CS20260917120000EEEEEE", "")

	err := f.repo.insertDomains(context.Background(), o.OrderNo, []Domain{
		NewPendingDomain(o.OrderNo, "example.com", false),
	})
	if err == nil {
		t.Fatal("同一订单里重复的域名应被唯一索引拦住")
	}
	if !mysql.IsDuplicateKey(err) {
		t.Errorf("应当是唯一键冲突，实际 %v", err)
	}
}

// ── 带条件的状态推进 ──────────────────────────────

// TestAdvanceStatusIsConditional 验证状态推进带 from 条件。
//
// 无条件 UPDATE 会让两个同时操作同一张订单的流程互相覆盖：
// 一个刚把它推进到 waiting_dcv，另一个按自己读到的旧状态又改回去。
// 带 from 条件后，后者会失败——这才是想要的结果。
func TestAdvanceStatusIsConditional(t *testing.T) {
	f := newRepoFixture(t)
	o := f.newOrder(t, "CS20260917120000FFFFFF", "")
	ctx := context.Background()

	// 正确的推进
	if err := f.repo.AdvanceStatus(ctx, o.OrderNo, orderstate.PendingPayment, orderstate.Paid); err != nil {
		t.Fatalf("合法推进失败: %v", err)
	}

	// 用一个已经过期的 from 再推一次
	err := f.repo.AdvanceStatus(ctx, o.OrderNo, orderstate.PendingPayment, orderstate.Paid)
	if err == nil {
		t.Fatal("from 与当前状态不符时应失败，而不是静默更新")
	}
	// 断言报错里点明了这次迁移，而不只是「出了个错」：
	// 影响行数为 0 的原因可能是状态被改过，也可能是订单不存在，
	// 两者的排查方向完全不同。
	if !strings.Contains(err.Error(), "pending_payment → paid") {
		t.Errorf("报错应点明失败的迁移，实际 %v", err)
	}

	got, err := f.repo.GetByNo(ctx, o.OrderNo)
	if err != nil {
		t.Fatalf("读取订单失败: %v", err)
	}
	if got.Status != orderstate.Paid {
		t.Errorf("被拒绝的推进不应改动状态，实际 %s", got.Status)
	}
}

// TestAdvanceStatusRejectsMissingOrder 验证对不存在的订单推进会报错。
//
// 影响行数为 0 时静默返回 nil 是最容易犯的错：调用方以为状态改成功了，
// 后续所有基于状态的判断都建立在错误的前提上。
func TestAdvanceStatusRejectsMissingOrder(t *testing.T) {
	f := newRepoFixture(t)

	err := f.repo.AdvanceStatus(context.Background(),
		"CS20260917120000ZZZZZZ", orderstate.PendingPayment, orderstate.Paid)
	if err == nil {
		t.Fatal("对不存在的订单推进状态应报错")
	}
}

// ── 域名替换 ──────────────────────────────────────

// TestReplaceDomainsRemovesOldRows 验证整体替换而不是逐条更新。
//
// 逐字段更新容易漏掉某个字段，留下「记录名是新的、值是旧的」这种组合。
// 重新生成 token 会同时改变 DNS 记录值与文件路径，正是这种场景。
func TestReplaceDomainsRemovesOldRows(t *testing.T) {
	f := newRepoFixture(t)
	o := f.newOrder(t, "CS20260917120000GGGGGG", "")
	ctx := context.Background()

	before, err := f.repo.ListDomains(ctx, o.OrderNo)
	if err != nil {
		t.Fatalf("读取域名失败: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("初始应有 1 个域名，实际 %d 个", len(before))
	}

	replaced := []Domain{
		NewDomain(o.OrderNo, upstreamDomain("example.com", "NEWTOKEN"), true),
		NewDomain(o.OrderNo, upstreamDomain("www.example.com", "NEWTOKEN2"), false),
	}
	if err := f.repo.ReplaceDomains(ctx, o.OrderNo, replaced); err != nil {
		t.Fatalf("替换域名失败: %v", err)
	}

	after, err := f.repo.ListDomains(ctx, o.OrderNo)
	if err != nil {
		t.Fatalf("读取域名失败: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("替换后应有 2 个域名，实际 %d 个", len(after))
	}
	if after[0].Domain != "example.com" || after[0].Record.Value != "NEWTOKEN" {
		t.Errorf("替换后主域名应是 example.com 且记录值为新值，实际 %+v", after[0])
	}
}

// upstreamDomain 造一份上游返回的域名材料。
//
// FileDcvPath 刻意带上占位符：这样这条测试顺带钉住了「存储层存的是
// 展开后的路径」——NewDomain 会把 {FQDN} 换成裸域名。
func upstreamDomain(domain, token string) foxssl.Domain {
	return foxssl.Domain{
		Domain:         domain,
		Status:         "pending",
		DnsRecordType:  "TXT",
		DnsRecordName:  "_dnsauth." + BaseDomain(domain),
		DnsRecordValue: token,
		FileDcvPath:    "/.well-known/pki-validation/" + foxssl.PlaceholderFQDN + "/" + token + ".txt",
		FileContent:    token,
	}
}

// ── 批量读取域名 ──────────────────────────────────

// TestListDomainNamesGroupsByOrderAndPutsPrimaryFirst 验证批量查询的排序与分组。
//
// 列表页与详情页展示的是同一个域名集合，两处顺序不同会让用户
// 在列表里看到 SAN 在前、点进去又变成主域名在前。
func TestListDomainNamesGroupsByOrderAndPutsPrimaryFirst(t *testing.T) {
	f := newRepoFixture(t)
	ctx := context.Background()

	// 先插入 SAN，再插入主域名，确认排序靠的是 is_primary 而不是插入顺序
	withSAN := &Order{
		OrderNo: "CS20260917120000HHHHHH", UserID: f.userID,
		ProductID: productDV, ProductName: "p", Brand: "b",
		ValidationType: rules.DV, UpstreamProductID: 101,
		Years: 1, KeyAlgorithm: rules.RSA, Amount: 0,
		Status:  orderstate.PendingPayment,
		Contact: &Contact{Name: "张三", Email: "a@example.com", Phone: "+86.1"},
		Domains: []Domain{
			NewPendingDomain("CS20260917120000HHHHHH", "www.example.com", false),
			NewPendingDomain("CS20260917120000HHHHHH", "example.com", true),
		},
	}
	if err := f.repo.Create(ctx, withSAN); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	f.newOrder(t, "CS20260917120000IIIIII", "")

	got, err := f.repo.ListDomainNames(ctx, []string{
		"CS20260917120000HHHHHH", "CS20260917120000IIIIII",
	})
	if err != nil {
		t.Fatalf("批量查询域名失败: %v", err)
	}

	multi := got["CS20260917120000HHHHHH"]
	if len(multi) != 2 || multi[0] != "example.com" {
		t.Errorf("主域名应排在最前，实际 %v", multi)
	}
	if single := got["CS20260917120000IIIIII"]; len(single) != 1 || single[0] != "example.com" {
		t.Errorf("单域名订单应返回一个域名，实际 %v", single)
	}
}

// TestListDomainNamesHandlesEmptyInput 验证空输入不会拼出非法 SQL。
//
// `IN ()` 是语法错误。翻到最后一页之后前端仍可能带着空列表请求，
// 那种情况不该让服务报 500。
func TestListDomainNamesHandlesEmptyInput(t *testing.T) {
	f := newRepoFixture(t)

	got, err := f.repo.ListDomainNames(context.Background(), nil)
	if err != nil {
		t.Fatalf("空输入不应报错: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("空输入应返回空 map，实际 %v", got)
	}
}

// ── 分页与筛选 ────────────────────────────────────

// TestListByUserPaginatesWithoutGapsOrDuplicates 验证游标分页不漏不重。
//
// 用 ID 而非 offset 分页的原因就在这里：订单持续新增时，
// offset 会让后面的页跳过中间插入的行。
func TestListByUserPaginatesWithoutGapsOrDuplicates(t *testing.T) {
	f := newRepoFixture(t)
	ctx := context.Background()

	const total = 5
	want := make(map[string]bool, total)
	for i := range total {
		no := "CS2026091712000" + string(rune('A'+i)) + "00000"
		f.newOrder(t, no, "")
		want[no] = true
	}

	seen := make(map[string]bool, total)
	cursor := int64(0)
	for range total + 2 { // 多转两圈，确认末页之后能正常收敛
		page, err := f.repo.ListByUser(ctx, f.userID, Filter{Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatalf("分页查询失败: %v", err)
		}
		for _, o := range page.Items {
			if seen[o.OrderNo] {
				t.Fatalf("翻页出现重复订单: %s", o.OrderNo)
			}
			seen[o.OrderNo] = true
		}
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}

	if len(seen) != total {
		t.Errorf("翻页共应看到 %d 张订单，实际 %d 张", total, len(seen))
	}
	for no := range want {
		if !seen[no] {
			t.Errorf("翻页漏掉了订单 %s", no)
		}
	}
}

func TestListByUserFiltersByStatus(t *testing.T) {
	f := newRepoFixture(t)
	ctx := context.Background()

	paid := f.newOrder(t, "CS20260917120000JJJJJJ", "")
	if err := f.repo.AdvanceStatus(ctx, paid.OrderNo,
		orderstate.PendingPayment, orderstate.Paid); err != nil {
		t.Fatalf("推进状态失败: %v", err)
	}
	f.newOrder(t, "CS20260917120000KKKKKK", "") // 仍是 pending_payment

	page, err := f.repo.ListByUser(ctx, f.userID, Filter{Status: orderstate.Paid, Limit: 10})
	if err != nil {
		t.Fatalf("按状态查询失败: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].OrderNo != paid.OrderNo {
		t.Errorf("按 paid 筛选应只返回 1 张订单，实际 %+v", page.Items)
	}
}

// ── 金额约束 ──────────────────────────────────────

// TestZeroAmountOrderIsAccepted 验证免费证书能真的落库。
//
// CHECK 写成 > 0 的话免费证书根本无法下单，而且报的是数据库约束错误，
// 排查起来很绕——错误信息只会说「违反约束」，不会说「你写了 > 0」。
func TestZeroAmountOrderIsAccepted(t *testing.T) {
	f := newRepoFixture(t)

	o := &Order{
		OrderNo: "CS20260917120000LLLLLL", UserID: f.userID,
		ProductID: productFree, ProductName: "免费 DV 证书", Brand: "Let's Encrypt",
		ValidationType: rules.DV, UpstreamProductID: 106,
		Years: 1, KeyAlgorithm: rules.RSA,
		Amount:  money.Amount(0),
		Status:  orderstate.PendingPayment,
		Contact: &Contact{Name: "张三", Email: "a@example.com", Phone: "+86.1"},
	}
	if err := f.repo.Create(context.Background(), o); err != nil {
		t.Fatalf("免费证书（金额 0）应能落库: %v", err)
	}
}

// ── 上游订单号反查 ────────────────────────────────

// TestMarkCancelledAcceptsEmptyReason 验证不带原因取消不会失败。
//
// MySQL 的 UPDATE 在**新值与旧值相同**时报告 0 影响行。往一个已经是
// 空串的列里再写一次空串正好命中这种情况，而「用户没填取消原因」
// 是契约明确允许的（请求体 required: false）。
// 把它当成「订单不存在」会让所有不带原因的取消返回 500。
func TestMarkCancelledAcceptsEmptyReason(t *testing.T) {
	f := newRepoFixture(t)
	o := f.newOrder(t, "CS20260917120000NNNNNN", "")
	ctx := context.Background()

	if err := f.repo.MarkCancelled(ctx, o.OrderNo, ""); err != nil {
		t.Fatalf("不带原因取消应被接受，实际 %v", err)
	}

	// 非空原因仍然要真的写进去
	if err := f.repo.MarkCancelled(ctx, o.OrderNo, "用户改主意了"); err != nil {
		t.Fatalf("带原因取消应写入成功，实际 %v", err)
	}
	got, err := f.repo.GetByNo(ctx, o.OrderNo)
	if err != nil {
		t.Fatalf("读取订单失败: %v", err)
	}
	if got.CancelReason != "用户改主意了" {
		t.Errorf("取消原因应被记录，实际 %q", got.CancelReason)
	}
}

// TestGetByUpstreamOrderNo 验证按上游订单号反查。
//
// 上游事件里带的是上游订单号，不是平台订单号。这个反查是事件处理的入口，
// 用错查找键会让每一次事件处理都报「订单不存在」，
// 而事件看起来又收到并落库了——故障表现得非常隐蔽。
func TestGetByUpstreamOrderNo(t *testing.T) {
	f := newRepoFixture(t)
	ctx := context.Background()

	const upstreamNo = "FX-20260917-000042"
	o := f.newOrder(t, "CS20260917120000MMMMMM", upstreamNo)

	got, err := f.repo.GetByUpstreamOrderNo(ctx, upstreamNo)
	if err != nil {
		t.Fatalf("按上游订单号反查失败: %v", err)
	}
	if got.OrderNo != o.OrderNo {
		t.Errorf("反查结果应为 %s，实际 %s", o.OrderNo, got.OrderNo)
	}

	if _, err := f.repo.GetByUpstreamOrderNo(ctx, "FX-NOPE"); !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("查不到时应返回 ErrOrderNotFound，实际 %v", err)
	}
}

// ── 事件幂等 ──────────────────────────────────────

// TestInsertEventReportsDuplicateWithoutOverwriting 验证事件幂等键的行为。
//
// 断言的是「原行没被覆盖」而不只是「返回了 duplicate」：
// 一个用 ON DUPLICATE KEY UPDATE 覆盖旧行的实现同样会返回 duplicate，
// 但那样会把首次的处理结果冲掉——包括 process_status 已经 done 这个事实，
// 于是重推会被当成一条全新的待处理事件再跑一遍。
func TestInsertEventReportsDuplicateWithoutOverwriting(t *testing.T) {
	f := newRepoFixture(t)
	ctx := context.Background()
	cleanupEvents(t, f.db, "FX-20260917-000077")

	first := &WebhookEvent{
		Provider:        providerFoxSSL,
		EventKey:        "foxssl:order_status:FX-20260917-000077:hash-one",
		EventType:       "order_status",
		UpstreamOrderNo: "FX-20260917-000077",
		UpstreamStatus:  "issued",
		PayloadHash:     "hash-one",
		Payload:         `{"event":"order_status"}`,
	}

	duplicate, err := f.repo.InsertEvent(ctx, first)
	if err != nil {
		t.Fatalf("首次写入事件失败: %v", err)
	}
	if duplicate {
		t.Error("首次写入不应被判成重复")
	}
	if err := f.repo.MarkEventProcessed(ctx, first.ID, EventDone, ""); err != nil {
		t.Fatalf("标记事件已处理失败: %v", err)
	}

	second := *first
	second.UpstreamStatus = "cancelled" // 同一幂等键、不同内容
	duplicate, err = f.repo.InsertEvent(ctx, &second)
	if err != nil {
		t.Fatalf("重复写入事件应返回 duplicate 而不是错误: %v", err)
	}
	if !duplicate {
		t.Fatal("同一个幂等键应被判成重复")
	}

	stored, err := f.repo.GetEventByKey(ctx, first.EventKey)
	if err != nil {
		t.Fatalf("读取事件失败: %v", err)
	}
	if stored.ProcessStatus != EventDone {
		t.Errorf("重复写入不应冲掉首次的处理状态，实际 %s", stored.ProcessStatus)
	}
	if stored.UpstreamStatus != "issued" {
		t.Errorf("重复写入不应覆盖首次的内容，实际 %q", stored.UpstreamStatus)
	}
}

// TestEventIsRecordedForUnknownUpstreamOrder 验证事件表刻意不对订单建外键。
//
// 上游可能推送平台不认识的订单号（订单还没落库、或已被归档），
// 此时事件仍然必须记下来——丢了就再也查不到上游到底推过什么。
// 加了外键会让这类插入直接失败，把排查线索丢掉。
func TestEventIsRecordedForUnknownUpstreamOrder(t *testing.T) {
	f := newRepoFixture(t)
	ctx := context.Background()
	const unknown = "FX-19700101-000001"
	cleanupEvents(t, f.db, unknown)

	duplicate, err := f.repo.InsertEvent(ctx, &WebhookEvent{
		Provider:        providerFoxSSL,
		EventKey:        "foxssl:order_status:" + unknown + ":hash-x",
		EventType:       "order_status",
		UpstreamOrderNo: unknown,
		UpstreamStatus:  "issued",
		PayloadHash:     "hash-x",
		Payload:         `{}`,
		OccurredAt:      ptrTime(time.Now().UTC()),
	})
	if err != nil {
		t.Fatalf("平台不认识的订单号也应能写入事件: %v", err)
	}
	if duplicate {
		t.Error("首次写入不应被判成重复")
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
