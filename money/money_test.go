package money

import "testing"

func TestParseAndStringRoundTrip(t *testing.T) {
	cases := []string{"0.0000", "213.0000", "-0.7881", "1.5000", "-99999.9999"}
	for _, s := range cases {
		m, err := Parse(s)
		if err != nil {
			t.Fatalf("Parse(%q): %v", s, err)
		}
		if got := m.String(); got != s {
			t.Errorf("Parse(%q).String() = %q, want %q", s, got, s)
		}
	}
}

func TestParseRejectsTooManyFractionalDigits(t *testing.T) {
	if _, err := Parse("1.00001"); err == nil {
		t.Fatal("expected error for 5 fractional digits, got nil")
	}
}

// TestMulRateExactFee is the canonical precision case: a 213.00 order
// at a 0.37% fee rate must produce exactly 0.7881 — not 0.78 and not
// 0.7800, which are the two classic float/ORM-rounding failure modes.
func TestMulRateExactFee(t *testing.T) {
	amount := MustParse("213.00")
	rate := MustParse("0.0037")
	if got, want := amount.MulRate(rate), MustParse("0.7881"); got.Cmp(want) != 0 {
		t.Errorf("213.00 * 0.0037 = %s, want %s", got, want)
	}
}

func TestMulRateRoundsHalfAwayFromZero(t *testing.T) {
	// 1.1111 * 0.5 = 0.55555 exactly; half rounds away from zero.
	if got, want := MustParse("1.1111").MulRate(MustParse("0.5")), MustParse("0.5556"); got.Cmp(want) != 0 {
		t.Errorf("1.1111 * 0.5 = %s, want %s", got, want)
	}
	if got, want := MustParse("-1.1111").MulRate(MustParse("0.5")), MustParse("-0.5556"); got.Cmp(want) != 0 {
		t.Errorf("-1.1111 * 0.5 = %s, want %s", got, want)
	}
}

func TestScaledSurvivesStorageRoundTrip(t *testing.T) {
	original := MustParse("0.7881")
	restored := FromScaled(original.Scaled())
	if original.Cmp(restored) != 0 {
		t.Errorf("round-trip through Scaled() changed the value: %s -> %s", original, restored)
	}
}
