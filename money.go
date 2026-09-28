package goinvoicecollection

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// MoneyScale 是金额支持的小数位数（2 表示精确到分）。
const MoneyScale = 2

const centsPerUnit = 100 // 10^MoneyScale

// Money 是精确金额，以最小货币单位（分）的 int64 存储，全程不使用浮点数。
// 零值即金额 0。业务允许为负的场景才会出现负值。
type Money int64

// ErrInvalidMoney 表示金额字符串无法被精确解析（非法格式或超过 MoneyScale 位小数）。
var ErrInvalidMoney = errors.New("invalid money amount")

// ParseMoney 把十进制小数字符串精确解析为 Money，最多允许 MoneyScale 位小数。
// 例如 "100" -> 10000、"100.5" -> 10050、"0.01" -> 1、"-12.30" -> -1230。
// 拒绝空串、非数字、科学计数法以及 "1.234" 这类无法用分精确表示的金额。
func ParseMoney(s string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrInvalidMoney
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
		return 0, ErrInvalidMoney
	}

	intPart, fracPart := s, ""
	hadDot := false
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
		hadDot = true
	}
	// 不接受 ".5"、"1."、"." 这类残缺写法。
	if intPart == "" || (hadDot && fracPart == "") {
		return 0, ErrInvalidMoney
	}
	if strings.IndexByte(intPart, '-') >= 0 || strings.IndexByte(intPart, '+') >= 0 {
		return 0, ErrInvalidMoney
	}
	// 小数位超出精度会丢失，直接拒绝而不是四舍五入。
	if len(fracPart) > MoneyScale {
		return 0, ErrInvalidMoney
	}
	for _, r := range intPart {
		if r < '0' || r > '9' {
			return 0, ErrInvalidMoney
		}
	}
	for _, r := range fracPart {
		if r < '0' || r > '9' {
			return 0, ErrInvalidMoney
		}
	}

	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, ErrInvalidMoney
	}
	frac := fracPart + strings.Repeat("0", MoneyScale-len(fracPart))
	cents := int64(0)
	if frac != "" {
		v, err := strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, ErrInvalidMoney
		}
		cents = v
	}

	if whole > (math.MaxInt64-cents)/centsPerUnit {
		return 0, ErrInvalidMoney
	}
	v := whole*centsPerUnit + cents
	if neg {
		v = -v
	}
	return Money(v), nil
}

// MustParseMoney 解析失败时 panic，适合在测试或常量构造中使用。
func MustParseMoney(s string) Money {
	m, err := ParseMoney(s)
	if err != nil {
		panic(err)
	}
	return m
}

// MoneyFromCents 直接用最小货币单位构造金额。
func MoneyFromCents(c int64) Money { return Money(c) }

// Cents 返回最小货币单位表示（分）。
func (m Money) Cents() int64 { return int64(m) }

// String 以固定 MoneyScale 位小数渲染，例如 150 -> "1.50"，-5 -> "-0.05"。
func (m Money) String() string {
	neg := m < 0
	v := int64(m)
	if neg {
		v = -v
	}
	whole, cents := v/centsPerUnit, v%centsPerUnit
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(strconv.FormatInt(whole, 10))
	b.WriteByte('.')
	b.WriteByte(byte('0' + (cents/10)%10))
	b.WriteByte(byte('0' + cents%10))
	return b.String()
}
