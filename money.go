package goinvoicecollection

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// Money 表示一笔精确金额，以最小货币单位（如“分”）保存为 int64。
// 全程不使用浮点，避免二进制误差；对外可通过 ParseMoney / String 与十进制字符串互转。
type Money int64

// ParseMoney 将十进制字符串解析为 Money，支持千分位逗号。
// 小数位最多两位；多于两位将报错，避免静默截断。
// 例如 "100"、"100.00"、"1,234.56"、"-12.30"。
func ParseMoney(s string) (Money, error) {
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty amount")
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	}
	if s == "" {
		return 0, errors.New("invalid amount")
	}

	var whole, frac string
	if i := strings.IndexByte(s, '.'); i >= 0 {
		whole, frac = s[:i], s[i+1:]
		if strings.IndexByte(frac, '.') >= 0 || frac == "" || len(frac) > 2 {
			return 0, errors.New("invalid amount: must have 1 or 2 decimal places")
		}
	} else {
		whole = s
	}
	if whole == "" {
		whole = "0"
	}
	for _, r := range whole {
		if r < '0' || r > '9' {
			return 0, errors.New("invalid amount: not a number")
		}
	}
	for _, r := range frac {
		if r < '0' || r > '9' {
			return 0, errors.New("invalid amount: not a number")
		}
	}

	var units int64
	for i := 0; i < len(whole); i++ {
		d := int64(whole[i] - '0')
		if units > (math.MaxInt64-d)/10 {
			return 0, errors.New("amount out of range")
		}
		units = units*10 + d
	}
	for i := 0; i < 2; i++ {
		d := int64(0)
		if i < len(frac) {
			d = int64(frac[i] - '0')
		}
		units = units * 10
		if units > math.MaxInt64-d {
			return 0, errors.New("amount out of range")
		}
		units += d
	}
	if neg {
		units = -units
	}
	return Money(units), nil
}

// MustParseMoney 解析失败时 panic，适用于测试与常量构造。
func MustParseMoney(s string) Money {
	m, err := ParseMoney(s)
	if err != nil {
		panic(err)
	}
	return m
}

// String 返回十进制字符串，负数带负号，金额为负时同样保留两位小数。
func (m Money) String() string {
	neg := m < 0
	v := int64(m)
	if neg {
		v = -v
	}
	whole, frac := v/100, v%100
	s := strconv.FormatInt(whole, 10) + "." +
		string(rune('0'+frac/10)) + string(rune('0'+frac%10))
	if neg {
		return "-" + s
	}
	return s
}

// Units 返回最小货币单位的原始整数值。
func (m Money) Units() int64 { return int64(m) }

// Add / Sub 提供精确加减。
func (m Money) Add(n Money) Money { return Money(int64(m) + int64(n)) }
func (m Money) Sub(n Money) Money { return Money(int64(m) - int64(n)) }

// Abs 返回绝对值。
func (m Money) Abs() Money {
	if int64(m) < 0 {
		return Money(-int64(m))
	}
	return m
}

// IsPositive / IsZero 等语义化判断。
func (m Money) IsPositive() bool { return int64(m) > 0 }
func (m Money) IsZero() bool     { return int64(m) == 0 }
func (m Money) IsNegative() bool { return int64(m) < 0 }

// Min 返回两者中较小者，用于自动分配。
func moneyMin(a, b Money) Money {
	if a < b {
		return a
	}
	return b
}
