package main

import (
	"testing"
)

func TestCompareFieldNULL(t *testing.T) {
	cfg := NumericCompareConfig{AbsTol: 1e-6, RelTol: 1e-9}
	cases := []struct {
		name      string
		exp, act  string
		kind      NumericKind
		wantMatch bool
	}{
		{"两者皆NULL", nullMarker, nullMarker, NumericNone, true},
		{"NULL对非NULL", nullMarker, "100", NumericDecimal, false},
		{"非NULL对NULL", "100", nullMarker, NumericDecimal, false},
		{"NULL对空串-严格区分", nullMarker, "", NumericNone, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, matched := compareField("t", 1, 1, "col", c.exp, c.act, c.kind, cfg)
			if matched != c.wantMatch {
				t.Errorf("matched=%v want %v", matched, c.wantMatch)
			}
		})
	}
}

func TestCompareFieldByKind(t *testing.T) {
	cfg := NumericCompareConfig{AbsTol: 1e-6, RelTol: 1e-9}
	cases := []struct {
		name      string
		exp, act  string
		kind      NumericKind
		wantMatch bool
	}{
		{"整数精确相等", "100", "100", NumericInteger, true},
		{"整数大数末位不同", "9007199254740993", "9007199254740992", NumericInteger, false},
		{"定点小数末尾零相等", "100.00", "100", NumericDecimal, true},
		{"浮点容差内", "100", "100.0000001", NumericFloat, true},
		{"浮点容差外", "100", "100.1", NumericFloat, false},
		{"文本相等", "abc", "abc", NumericNone, true},
		{"文本不等", "abc", "abd", NumericNone, false},
		{"时间文本一致", "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z", NumericNone, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, matched := compareField("t", 1, 1, "col", c.exp, c.act, c.kind, cfg)
			if matched != c.wantMatch {
				t.Errorf("matched=%v want %v", matched, c.wantMatch)
			}
		})
	}
}

func TestCompareFieldDiffDetail(t *testing.T) {
	cfg := NumericCompareConfig{AbsTol: 0, RelTol: 0}
	d, matched := compareField("orders", 2, 42, "amount", "100.50", "100.40", NumericDecimal, cfg)
	if matched {
		t.Fatal("应当判定为不一致")
	}
	if d.Table != "orders" || d.Round != 2 || d.Key != 42 || d.Column != "amount" {
		t.Errorf("差异元信息不正确: %+v", d)
	}
	if d.Expected != "100.50" || d.Actual != "100.40" {
		t.Errorf("差异值不正确: %+v", d)
	}
	if d.Reason == "" {
		t.Error("差异原因不应为空")
	}
}
