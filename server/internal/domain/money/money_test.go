package money

import (
	"errors"
	"math"
	"testing"
)

func TestParseYuan(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Amount
		wantErr bool
	}{
		{name: "两位小数", in: "29.80", want: 2980},
		{name: "整数", in: "100", want: 10000},
		{name: "零", in: "0", want: 0},
		{name: "一位小数补齐", in: "0.5", want: 50},
		{name: "负数", in: "-12.34", want: -1234},
		{name: "前后空格", in: "  8.08  ", want: 808},
		{name: "多于两位小数直接截断", in: "1.999", want: 199},
		{name: "省略整数部分", in: ".5", want: 50},
		{name: "空字符串", in: "", wantErr: true},
		{name: "非数字", in: "abc", wantErr: true},
		{name: "多个小数点", in: "1.2.3", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseYuan(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseYuan(%q) 期望报错，实际得到 %v", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseYuan(%q) 意外报错: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseYuan(%q) = %d，期望 %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseYuanAvoidsFloatError(t *testing.T) {
	// 0.1 + 0.2 用浮点会得到 0.30000000000000004，走字符串解析必须精确
	a, err := ParseYuan("0.1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseYuan("0.2")
	if err != nil {
		t.Fatal(err)
	}
	sum, err := a.Add(b)
	if err != nil {
		t.Fatal(err)
	}
	if sum != 30 {
		t.Errorf("0.1 + 0.2 = %d 分，期望 30 分", sum)
	}
}

func TestAmountString(t *testing.T) {
	tests := []struct {
		in   Amount
		want string
	}{
		{in: 2980, want: "29.80"},
		{in: 0, want: "0.00"},
		{in: 5, want: "0.05"},
		{in: 100, want: "1.00"},
		{in: -1234, want: "-12.34"},
		{in: -5, want: "-0.05"},
	}

	for _, tt := range tests {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("Amount(%d).String() = %q，期望 %q", tt.in, got, tt.want)
		}
	}
}

func TestArithmetic(t *testing.T) {
	a := Amount(1000)
	b := Amount(250)

	if got, err := a.Add(b); err != nil || got != 1250 {
		t.Errorf("Add = %d, %v；期望 1250", got, err)
	}
	if got, err := a.Sub(b); err != nil || got != 750 {
		t.Errorf("Sub = %d, %v；期望 750", got, err)
	}
	if got, err := a.Mul(3); err != nil || got != 3000 {
		t.Errorf("Mul = %d, %v；期望 3000", got, err)
	}
}

func TestArithmeticOverflow(t *testing.T) {
	max := Amount(math.MaxInt64)
	min := Amount(math.MinInt64)

	if _, err := max.Add(1); !errors.Is(err, ErrOverflow) {
		t.Errorf("MaxInt64 + 1 应报溢出，实际 err = %v", err)
	}
	if _, err := min.Sub(1); !errors.Is(err, ErrOverflow) {
		t.Errorf("MinInt64 - 1 应报溢出，实际 err = %v", err)
	}
	if _, err := max.Mul(2); !errors.Is(err, ErrOverflow) {
		t.Errorf("MaxInt64 * 2 应报溢出，实际 err = %v", err)
	}
	if _, err := max.Mul(0); err != nil {
		t.Errorf("MaxInt64 * 0 不应报错，实际 err = %v", err)
	}
}

func TestPredicates(t *testing.T) {
	if !Amount(0).IsZero() {
		t.Error("0 应判定为零")
	}
	if !Amount(-1).IsNegative() {
		t.Error("-1 应判定为负")
	}
	if !Amount(1).IsPositive() {
		t.Error("1 应判定为正")
	}
	if !Amount(1).LessThan(2) {
		t.Error("1 应小于 2")
	}

	if err := Amount(-1).ValidateNonNegative(); !errors.Is(err, ErrNegative) {
		t.Errorf("负数应校验失败，实际 err = %v", err)
	}
	if err := Amount(0).ValidateNonNegative(); err != nil {
		t.Errorf("零应校验通过，实际 err = %v", err)
	}
}
