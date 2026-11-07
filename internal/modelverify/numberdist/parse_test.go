package numberdist

import "testing"

func TestParse_Cases(t *testing.T) {
	cases := []struct {
		name       string
		visible    string
		wantNum    int
		wantOK     bool
		wantStrict bool
	}{
		{"plain", "217", 217, true, true},
		{"surrounding_ws", "  217 \n", 217, true, true},
		{"leading_words", "我选择217", 217, true, false},
		{"two_ints_takes_first", "217 19", 217, true, false},
		{"negative_dashes_ignored", "-7", 7, true, false},
		{"float_truncates", "1.5", 1, true, false},
		{"leading_zero", "0355", 355, true, true},
		{"boundary_low", "1", 1, true, true},
		{"boundary_high", "355", 355, true, true},
		{"zero_then_more_is_invalid", "0，然后217", 0, false, false},
		{"above_range", "356", 0, false, false},
		{"overflow", "999999999999999999999", 0, false, false},
		{"fullwidth_rejected", "２１７", 0, false, false},
		{"empty", "", 0, false, false},
		{"whitespace_only", "   \n\t", 0, false, false},
		{"no_digits", "没有数字", 0, false, false},
		{"answer_with_period", "217.", 217, true, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Parse(c.visible)
			if got.Valid != c.wantOK {
				t.Fatalf("Parse(%q).Valid = %v, want %v (reason=%q)", c.visible, got.Valid, c.wantOK, got.Reason)
			}
			if got.Number != c.wantNum {
				t.Fatalf("Parse(%q).Number = %d, want %d", c.visible, got.Number, c.wantNum)
			}
			if got.Valid && got.StrictInteger != c.wantStrict {
				t.Fatalf("Parse(%q).StrictInteger = %v, want %v", c.visible, got.StrictInteger, c.wantStrict)
			}
			if !got.Valid && got.Reason == "" {
				t.Fatalf("Parse(%q) invalid without reason", c.visible)
			}
		})
	}
}

// 全角数字不应被当作数字：上游 \d 不带 u 标志，只匹配 ASCII 0-9。
func TestParse_FullWidthNotMatched(t *testing.T) {
	got := Parse("我选１２３")
	if got.Valid {
		t.Fatalf("全角数字被误判为有效: %+v", got)
	}
}
