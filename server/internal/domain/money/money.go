// Package money 处理金额。
//
// 全局约定：金额一律使用 int64 表示，单位为「分」。
// 不使用 float64 参与任何金额运算——浮点误差在资金场景下不可接受。
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Amount 是以「分」为单位的金额。
type Amount int64

var (
	// ErrOverflow 表示运算结果超出 int64 表示范围。
	ErrOverflow = errors.New("金额运算溢出")
	// ErrNegative 表示金额为负，出现在不允许为负的场景。
	ErrNegative = errors.New("金额不能为负")
)

// FromYuan 把「元」转换为「分」。仅用于解析外部输入，内部运算不要经过浮点。
func FromYuan(yuan float64) Amount {
	return Amount(math.Round(yuan * 100))
}

// ParseYuan 解析形如 "29.80" 的元字符串，避免浮点误差。
func ParseYuan(s string) (Amount, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("金额为空")
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}

	intPart, fracPart, _ := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	// 补齐到两位小数，多于两位直接截断（不四舍五入，避免凭空多出金额）
	for len(fracPart) < 2 {
		fracPart += "0"
	}
	fracPart = fracPart[:2]

	yuan, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("金额格式不正确: %q", s)
	}
	cents, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("金额格式不正确: %q", s)
	}

	total, err := safeMul(yuan, 100)
	if err != nil {
		return 0, err
	}
	total, err = safeAdd(total, cents)
	if err != nil {
		return 0, err
	}
	if neg {
		total = -total
	}
	return Amount(total), nil
}

// Yuan 返回以「元」表示的浮点值。仅用于展示或对账，不要用于再次计算。
func (a Amount) Yuan() float64 { return float64(a) / 100 }

// Cents 返回以「分」表示的原始值。
func (a Amount) Cents() int64 { return int64(a) }

// String 返回形如 "29.80" 的字符串。
func (a Amount) String() string {
	sign := ""
	v := int64(a)
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

// Add 相加，溢出时返回错误。
func (a Amount) Add(b Amount) (Amount, error) { return wrap(safeAdd(int64(a), int64(b))) }

// Sub 相减，溢出时返回错误。
func (a Amount) Sub(b Amount) (Amount, error) { return wrap(safeSub(int64(a), int64(b))) }

// Mul 乘以整数倍数，溢出时返回错误。
func (a Amount) Mul(n int64) (Amount, error) { return wrap(safeMul(int64(a), n)) }

// IsZero 判断是否为零。
func (a Amount) IsZero() bool { return a == 0 }

// IsNegative 判断是否为负。
func (a Amount) IsNegative() bool { return a < 0 }

// IsPositive 判断是否为正。
func (a Amount) IsPositive() bool { return a > 0 }

// LessThan 判断是否小于 b。
func (a Amount) LessThan(b Amount) bool { return a < b }

// ValidateNonNegative 校验金额不为负。
func (a Amount) ValidateNonNegative() error {
	if a.IsNegative() {
		return ErrNegative
	}
	return nil
}

func wrap(v int64, err error) (Amount, error) {
	if err != nil {
		return 0, err
	}
	return Amount(v), nil
}

func safeAdd(a, b int64) (int64, error) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, ErrOverflow
	}
	return sum, nil
}

func safeSub(a, b int64) (int64, error) {
	diff := a - b
	if (b < 0 && diff < a) || (b > 0 && diff > a) {
		return 0, ErrOverflow
	}
	return diff, nil
}

func safeMul(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	product := a * b
	if product/b != a {
		return 0, ErrOverflow
	}
	return product, nil
}
