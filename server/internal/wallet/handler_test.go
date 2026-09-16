package wallet

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
)

func sampleEntry() Entry {
	return Entry{
		ID:             1024,
		AccountID:      7,
		UserID:         42,
		EntryNo:        "cert_order:O20260916000001:freeze",
		Op:             OpFreeze,
		BizType:        "cert_order",
		BizNo:          "O20260916000001",
		AvailableDelta: money.Amount(-21800),
		FrozenDelta:    money.Amount(21800),
		AvailableAfter: money.Amount(78200),
		FrozenAfter:    money.Amount(21800),
		Remark:         "下单冻结",
		CreatedAt:      time.Date(2026, 9, 16, 20, 30, 0, 0, time.UTC),
	}
}

func marshalToMap(t *testing.T, v any) (map[string]any, string) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	return m, string(raw)
}

// 幂等键是服务端的实现细节，下发它没有用途，反而多一个可被外部构造的标识。
func TestToEntryDTOHidesInternalFields(t *testing.T) {
	m, _ := marshalToMap(t, toEntryDTO(sampleEntry()))

	forbidden := []string{
		"entryNo",   // 幂等键
		"accountId", // 钱包账户主键，用户不需要知道
		"userId",    // 当前用户身份由令牌确定，响应里重复下发没有意义
	}
	for _, key := range forbidden {
		if _, ok := m[key]; ok {
			t.Errorf("DTO 不应包含内部字段 %s", key)
		}
	}
}

// 契约字段必须齐全，缺一个前端就会拿到 undefined。
func TestToEntryDTOContainsAllContractFields(t *testing.T) {
	m, _ := marshalToMap(t, toEntryDTO(sampleEntry()))

	required := []string{
		"id", "op", "bizType", "bizNo",
		"availableDelta", "frozenDelta",
		"availableAfter", "frozenAfter",
		"remark", "createdAt",
	}
	for _, key := range required {
		if _, ok := m[key]; !ok {
			t.Errorf("DTO 缺少契约字段 %s", key)
		}
	}
}

// 金额必须是「分」的整数。一旦有人把它改成浮点或字符串，
// 前端的展示与求和会同时出问题，所以在这里钉死。
func TestToEntryDTOAmountsAreIntegerCents(t *testing.T) {
	m, _ := marshalToMap(t, toEntryDTO(sampleEntry()))

	cases := map[string]float64{
		"availableDelta": -21800,
		"frozenDelta":    21800,
		"availableAfter": 78200,
		"frozenAfter":    21800,
	}
	for key, want := range cases {
		got, ok := m[key].(float64)
		if !ok {
			t.Errorf("%s 应为数值，实际 %T", key, m[key])
			continue
		}
		if got != want {
			t.Errorf("%s 应为 %v，实际 %v", key, want, got)
		}
		if got != float64(int64(got)) {
			t.Errorf("%s 含小数，金额不应出现浮点", key)
		}
	}
}

// 时间统一用 RFC 3339，前端才能直接 new Date() 解析。
func TestToEntryDTOTimeFormat(t *testing.T) {
	m, _ := marshalToMap(t, toEntryDTO(sampleEntry()))

	raw, _ := m["createdAt"].(string)
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("createdAt 不是 RFC 3339 格式: %q (%v)", raw, err)
	}
	if !parsed.Equal(sampleEntry().CreatedAt) {
		t.Errorf("createdAt 应还原为 %v，实际 %v", sampleEntry().CreatedAt, parsed)
	}
}

func TestToAccountDTO(t *testing.T) {
	account := &Account{
		AvailableBalance: money.Amount(78200),
		FrozenBalance:    money.Amount(21800),
	}
	m, _ := marshalToMap(t, toAccountDTO(account))

	if m["availableBalance"].(float64) != 78200 {
		t.Errorf("可用余额应为 78200 分，实际 %v", m["availableBalance"])
	}
	if m["frozenBalance"].(float64) != 21800 {
		t.Errorf("冻结余额应为 21800 分，实际 %v", m["frozenBalance"])
	}
	// totalBalance 是冗余下发的，必须恒等于两者之和，否则前端会出现两个对不上的数字
	if m["totalBalance"].(float64) != 100000 {
		t.Errorf("总余额应为 100000 分，实际 %v", m["totalBalance"])
	}
}

// 零余额也要正常序列化出三个 0，而不是 null——新用户看到的第一屏就是这个。
func TestToAccountDTOZeroBalance(t *testing.T) {
	m, _ := marshalToMap(t, toAccountDTO(&Account{}))

	for _, key := range []string{"availableBalance", "frozenBalance", "totalBalance"} {
		got, ok := m[key].(float64)
		if !ok {
			t.Errorf("%s 应为数值 0，实际 %T", key, m[key])
			continue
		}
		if got != 0 {
			t.Errorf("%s 应为 0，实际 %v", key, got)
		}
	}
}

// 空账本必须序列化成 []，不能是 null，否则前端 .map() 会崩。
func TestEmptyLedgerSerializesAsArray(t *testing.T) {
	page := &EntryPage{Items: []Entry{}}
	dto := ledgerDTO{Items: make([]entryDTO, 0, len(page.Items))}
	for _, entry := range page.Items {
		dto.Items = append(dto.Items, toEntryDTO(entry))
	}

	m, _ := marshalToMap(t, dto)
	if m["items"] == nil {
		t.Fatal("items 应序列化为 []，实际为 null")
	}
	if _, ok := m["items"].([]any); !ok {
		t.Errorf("items 应为数组，实际 %T", m["items"])
	}
	// 没有下一页时 nextCursor 必须是 null，前端据此判断到底
	if m["nextCursor"] != nil {
		t.Errorf("无下一页时 nextCursor 应为 null，实际 %v", m["nextCursor"])
	}
}

func TestLedgerNextCursorIsNullOnLastPage(t *testing.T) {
	cursor := int64(1005)
	dto := ledgerDTO{Items: []entryDTO{}, NextCursor: &cursor}

	m, _ := marshalToMap(t, dto)
	if m["nextCursor"].(float64) != 1005 {
		t.Errorf("有下一页时 nextCursor 应为 1005，实际 %v", m["nextCursor"])
	}
}

// 契约里 op 是枚举，服务端下发未知取值会让前端映射表落空。
func TestEntryOpIsWithinContractEnum(t *testing.T) {
	allowed := map[string]bool{
		"recharge": true, "consume": true, "freeze": true,
		"settle": true, "unfreeze": true, "refund": true,
	}

	for _, op := range AllOps() {
		if !allowed[string(op)] {
			t.Errorf("操作类型 %q 不在契约枚举内", op)
		}
	}

	m, _ := marshalToMap(t, toEntryDTO(sampleEntry()))
	if !allowed[m["op"].(string)] {
		t.Errorf("DTO 下发的 op 取值 %v 不在契约枚举内", m["op"])
	}
}
