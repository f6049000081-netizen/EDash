package money

import "testing"

func TestBasisPointsRoundsHalfUp(t *testing.T) {
	cases := []struct {
		amount, bps, want int64
	}{
		{18500, 50, 93}, // ETB 185.00 at 0.5% = 92.5 santim → 93
		{18400, 50, 92}, // 92.0
		{100, 50, 1},    // 0.5 → 1
		{99, 50, 0},     // 0.495 → 0
		{0, 50, 0},
		{1_000_000, 0, 0},
		{MaxAmountMinor, 10_000, MaxAmountMinor}, // 100% of the cap, no overflow
	}
	for _, c := range cases {
		if got := BasisPoints(c.amount, c.bps); got != c.want {
			t.Errorf("BasisPoints(%d, %d) = %d, want %d", c.amount, c.bps, got, c.want)
		}
	}
}

func TestAddRejectsMixedCurrencies(t *testing.T) {
	if _, err := New(100, ETB).Add(New(100, USD)); err != ErrCurrencyMismatch {
		t.Fatalf("want ErrCurrencyMismatch, got %v", err)
	}
	sum, err := New(150, ETB).Add(New(250, ETB))
	if err != nil || sum.AmountMinor != 400 {
		t.Fatalf("got %v, %v", sum, err)
	}
}

func TestString(t *testing.T) {
	if s := New(18500, ETB).String(); s != "ETB 185.00" {
		t.Errorf("got %q", s)
	}
	if s := New(-5, ETB).String(); s != "ETB -0.05" {
		t.Errorf("got %q", s)
	}
}

func TestValid(t *testing.T) {
	if !ETB.Valid() || Currency("XYZ").Valid() {
		t.Fatal("currency validation wrong")
	}
}

func TestDecimalRoundTrip(t *testing.T) {
	ok := map[string]int64{"100.00": 10000, "100": 10000, "100.5": 10050, "0.01": 1, "1234567.89": 123456789}
	for s, want := range ok {
		m, err := ParseDecimal(s, ETB)
		if err != nil || m.AmountMinor != want {
			t.Errorf("%q: %v %d", s, err, m.AmountMinor)
		}
	}
	for _, s := range []string{"", ".5", "1.", "1.234", "-1.00", "+1", "1e3", "1,000.00", " 1.00", "1.0a", "99999999999999.00"} {
		if _, err := ParseDecimal(s, ETB); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if d := New(10050, ETB).Decimal(); d != "100.50" {
		t.Errorf("Decimal: %s", d)
	}
}

func TestDisplay(t *testing.T) {
	for _, c := range []struct {
		m    Money
		want string
	}{
		{New(0, ETB), "ETB 0.00"},
		{New(5, ETB), "ETB 0.05"},
		{New(99_999, ETB), "ETB 999.99"},
		{New(125_000, ETB), "ETB 1,250.00"},
		{New(123_456_789, ETB), "ETB 1,234,567.89"},
		{New(-2_500, ETB), "ETB -25.00"},
	} {
		if got := c.m.Display(); got != c.want {
			t.Errorf("%d: %q, want %q", c.m.AmountMinor, got, c.want)
		}
	}
}
