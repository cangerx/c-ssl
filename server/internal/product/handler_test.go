package product

import (
	"encoding/json"
	"testing"

	"github.com/cangerx/c-ssl/server/internal/domain/money"
	"github.com/cangerx/c-ssl/server/internal/product/rules"
)

func sampleProduct() Product {
	minSans, maxSans := 1, 3
	upstreamID := 103
	return Product{
		ID:                   3,
		Name:                 "DigiCert Secure Site OV",
		Brand:                "DigiCert",
		ValidationType:       rules.OV,
		WildcardSupported:    true,
		IPSupported:          true,
		MultiDomainSupported: true,
		MinSans:              &minSans,
		MaxSans:              &maxSans,
		KeyAlgorithms:        []rules.KeyAlgorithm{rules.RSA, rules.ECC},
		DcvMethods:           []rules.DcvMethod{rules.DnsTxt, rules.Email},
		ReissueSupported:     true,
		CancelSupported:      true,
		RequireOrgInfo:       true,
		RecommendTag:         "推荐",
		UpstreamProductID:    &upstreamID,
		Status:               StatusActive,
		SortOrder:            30,
		Prices: []Price{
			{Years: 1, CostPrice: money.Amount(98000), RetailPrice: money.Amount(218000)},
			{Years: 2, CostPrice: money.Amount(178000), RetailPrice: money.Amount(398000)},
		},
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

// 这是本域最重要的一条测试：上游成本价绝不能出现在任何面向用户的响应里。
func TestToDTOHidesInternalFields(t *testing.T) {
	m, _ := marshalToMap(t, toDTO(sampleProduct()))

	forbidden := []string{
		"costPrice",         // 上游成本价
		"upstreamProductId", // FoxSSL 产品 ID
		"status",            // 上下架状态，用户端不需要知道
		"sortOrder",         // 排序权重
		"createdAt",
		"updatedAt",
	}
	for _, key := range forbidden {
		if _, ok := m[key]; ok {
			t.Errorf("DTO 不应包含内部字段 %s", key)
		}
	}

	// 字段名可能被改名，数值本身也不能漏出去。
	// 按数值精确比对——用子串匹配会误报（98000 是 398000 的子串）。
	numbers := make(map[float64]bool)
	collectNumbers(m, numbers)

	for _, cost := range []float64{98000, 178000} {
		if numbers[cost] {
			t.Errorf("成本价 %v 分出现在响应中", cost)
		}
	}
}

// collectNumbers 收集 JSON 树中的所有数值。
func collectNumbers(v any, out map[float64]bool) {
	switch node := v.(type) {
	case map[string]any:
		for _, child := range node {
			collectNumbers(child, out)
		}
	case []any:
		for _, child := range node {
			collectNumbers(child, out)
		}
	case float64:
		out[node] = true
	}
}

// 契约字段必须齐全，缺一个前端就会拿到 undefined。
func TestToDTOContainsAllContractFields(t *testing.T) {
	m, _ := marshalToMap(t, toDTO(sampleProduct()))

	required := []string{
		"id", "name", "brand", "validationType",
		"wildcardSupported", "ipSupported", "multiDomainSupported",
		"minSans", "maxSans", "years", "keyAlgorithms", "dcvMethods",
		"reissueSupported", "cancelSupported", "requireOrganizationInfo",
		"recommendTag", "prices",
	}
	for _, key := range required {
		if _, ok := m[key]; !ok {
			t.Errorf("DTO 缺少契约字段 %s", key)
		}
	}
}

func TestToDTOPriceShape(t *testing.T) {
	m, _ := marshalToMap(t, toDTO(sampleProduct()))

	prices, ok := m["prices"].([]any)
	if !ok {
		t.Fatalf("prices 不是数组: %T", m["prices"])
	}
	if len(prices) != 2 {
		t.Fatalf("应有 2 条价格，实际 %d 条", len(prices))
	}

	first := prices[0].(map[string]any)
	if first["retailPrice"].(float64) != 218000 {
		t.Errorf("零售价应为 218000 分，实际 %v", first["retailPrice"])
	}
	// 无划线价时应为 null，而不是 0——0 会被前端渲染成"原价 0 元"
	if first["originalPrice"] != nil {
		t.Errorf("无划线价时应为 null，实际 %v", first["originalPrice"])
	}
}

// 空数组必须序列化成 []，不能是 null，否则前端 .map() 会崩。
func TestToDTOEmptySlicesAreArrays(t *testing.T) {
	p := Product{
		ID:             1,
		Brand:          "Test",
		ValidationType: rules.DV,
	}
	m, _ := marshalToMap(t, toDTO(p))

	for _, key := range []string{"years", "keyAlgorithms", "dcvMethods", "prices"} {
		if m[key] == nil {
			t.Errorf("%s 应序列化为 []，实际为 null", key)
		}
		if _, ok := m[key].([]any); !ok {
			t.Errorf("%s 应为数组，实际 %T", key, m[key])
		}
	}
}

func TestProductIsFree(t *testing.T) {
	free := Product{Prices: []Price{{Years: 1, RetailPrice: 0}}}
	if !free.IsFree() {
		t.Error("零售价为 0 应判定为免费产品")
	}

	paid := Product{Prices: []Price{{Years: 1, RetailPrice: 100}}}
	if paid.IsFree() {
		t.Error("零售价非 0 不应判定为免费产品")
	}

	empty := Product{}
	if empty.IsFree() {
		t.Error("无价格数据不应判定为免费产品")
	}
}
