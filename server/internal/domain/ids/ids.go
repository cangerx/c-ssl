// Package ids 生成业务编号。
//
// 编号刻意做成「前缀 + 时间 + 随机」的形态，便于人工在日志和客服工单里辨认与排序。
// 这些编号不是主键，数据库主键仍使用自增 ID。
package ids

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"time"
)

const (
	prefixOrder    = "CS" // 证书订单
	prefixLedger   = "WL" // 钱包流水
	prefixRecharge = "RC" // 充值订单
)

// NewOrderNo 生成平台订单号，形如 CS20260916191234A7K3M9。
func NewOrderNo(t time.Time) string { return gen(prefixOrder, t) }

// NewLedgerNo 生成钱包流水号，形如 WL20260916191234B2X8Q1。
func NewLedgerNo(t time.Time) string { return gen(prefixLedger, t) }

// NewRechargeNo 生成充值订单号，形如 RC20260916191234C5N7P2。
func NewRechargeNo(t time.Time) string { return gen(prefixRecharge, t) }

// 去掉容易混淆的字符（0/O、1/I），降低人工转录出错概率
const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

func gen(prefix string, t time.Time) string {
	return prefix + t.UTC().Format("20060102150405") + randomString(6)
}

func randomString(n int) string {
	max := big.NewInt(int64(len(alphabet)))
	out := make([]byte, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			// crypto/rand 失败属于系统级异常，退回时间纳秒取模保证不 panic
			out[i] = alphabet[time.Now().UnixNano()%int64(len(alphabet))]
			continue
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out)
}

// ParseOrderNoTime 从订单号中还原生成时间，解析失败返回零值。
func ParseOrderNoTime(no string) (time.Time, error) {
	if len(no) < len(prefixOrder)+14 {
		return time.Time{}, fmt.Errorf("订单号长度不足: %q", no)
	}
	return time.ParseInLocation("20060102150405", no[len(prefixOrder):len(prefixOrder)+14], time.UTC)
}
