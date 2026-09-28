package goinvoicecollection

import "testing"

func TestParseMoneyAndString(t *testing.T) {
	cases := []struct {
		in      string
		units   int64
		display string
	}{
		{"100", 10000, "100.00"},
		{"100.00", 10000, "100.00"},
		{"0.01", 1, "0.01"},
		{"1,234.56", 123456, "1234.56"},
		{"-12.30", -1230, "-12.30"},
		{"0", 0, "0.00"},
		{" 99.9 ", 9990, "99.90"},
		{"100.5", 10050, "100.50"},
	}
	for _, c := range cases {
		m, err := ParseMoney(c.in)
		if err != nil {
			t.Fatalf("ParseMoney(%q): %v", c.in, err)
		}
		if int64(m) != c.units {
			t.Fatalf("ParseMoney(%q) = %d, want %d", c.in, m, c.units)
		}
		if m.String() != c.display {
			t.Fatalf("Money(%d).String() = %q, want %q", c.units, m.String(), c.display)
		}
	}
}

func TestParseMoneyInvalid(t *testing.T) {
	bad := []string{"", "abc", "1.234", "1..2", "1.", ".", "+", "-", "0.001", "12.3.4"}
	for _, s := range bad {
		if _, err := ParseMoney(s); err == nil {
			t.Fatalf("ParseMoney(%q) expected error", s)
		}
	}
}

func TestMoneyArithmetic(t *testing.T) {
	a := MustParseMoney("10.00")
	b := MustParseMoney("3.50")
	if got := a.Sub(b); got.String() != "6.50" {
		t.Fatalf("sub = %s", got)
	}
	if got := a.Add(b); got.String() != "13.50" {
		t.Fatalf("add = %s", got)
	}
}
