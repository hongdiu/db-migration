package main

import (
	"testing"
)

func TestParseExactNumber(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		kind NumericKind
		want string // RatString
	}{
		{"普通整数", "123", NumericInteger, "123"},
		{"负整数", "-42", NumericInteger, "-42"},
		{"大整数超出int64", "9223372036854775808", NumericInteger, "9223372036854775808"},
		{"定点小数", "123.45", NumericDecimal, "2469/20"},
		{"定点小数末尾零", "100.00", NumericDecimal, "100"},
		{"科学计数法", "1.5e3", NumericDecimal, "1500"},
		{"负科学计数法", "2.5e-2", NumericDecimal, "1/40"},
		{"浮点文本", "0.1", NumericFloat, "1/10"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, err := ParseExactNumber(c.raw, c.kind)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got := n.Rat.RatString(); got != c.want {
				t.Errorf("RatString=%s, want %s", got, c.want)
			}
		})
	}
}

func TestParseExactNumberInvalid(t *testing.T) {
	for _, raw := range []string{"", "abc", "1/3", "1.2.3"} {
		if _, err := ParseExactNumber(raw, NumericDecimal); err == nil {
			t.Errorf("解析 %q 应当失败", raw)
		}
	}
}

func TestNumericKindFromDBType(t *testing.T) {
	cases := map[string]NumericKind{
		"int": NumericInteger, "bigint": NumericInteger, "tinyint": NumericInteger,
		"BOOLEAN": NumericInteger, "bit": NumericInteger,
		"decimal": NumericDecimal, "NUMERIC": NumericDecimal,
		"float": NumericFloat, "double": NumericFloat, "real": NumericFloat,
		"varchar": NumericNone, "datetime": NumericNone, "text": NumericNone,
	}
	for typ, want := range cases {
		if got := NumericKindFromDBType(typ); got != want {
			t.Errorf("类型 %q: got %v, want %v", typ, got, want)
		}
	}
}

func TestCompareExact(t *testing.T) {
	cfg := NumericCompareConfig{AbsTol: 0, RelTol: 0}
	cases := []struct {
		name string
		a, b string
		kind NumericKind
		want bool
	}{
		{"整数相等", "100", "100", NumericInteger, true},
		{"整数大数不同-关键精度用例", "9007199254740993", "9007199254740992", NumericInteger, false},
		{"整数精确相等大数", "9223372036854775807", "9223372036854775807", NumericInteger, true},
		{"定点小数末尾零相等", "100.00", "100", NumericDecimal, true},
		{"定点小数精度不同", "0.000000000000000001", "0", NumericDecimal, false},
		{"定点小数浮点陷阱-精确相等", "0.3", "0.30", NumericDecimal, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, _ := ParseExactNumber(c.a, c.kind)
			b, _ := ParseExactNumber(c.b, c.kind)
			got, detail := a.Compare(b, cfg)
			if got != c.want {
				t.Errorf("Compare(%s,%s)=%v want %v; detail=%s", c.a, c.b, got, c.want, detail)
			}
		})
	}
}

func TestCompareFloatTolerance(t *testing.T) {
	cases := []struct {
		name   string
		a, b   string
		absTol float64
		relTol float64
		want   bool
	}{
		{"完全相等", "100", "100", 0, 0, true},
		{"零容差下微小差异不通过", "100", "100.0000001", 0, 0, false},
		{"绝对容差内通过", "100", "100.0000001", 0.000001, 0, true},
		{"绝对容差外不通过", "100", "100.1", 0.000001, 0, false},
		{"相对容差内通过-大数", "1e12", "1.0000000001e12", 0, 1e-9, true},
		{"相对容差外不通过-大数", "1e12", "1.001e12", 0, 1e-9, false},
		{"正负无穷不通过", "+Inf", "-Inf", 1e-6, 1e-9, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, errA := ParseExactNumber(c.a, NumericFloat)
			b, errB := ParseExactNumber(c.b, NumericFloat)
			if errA != nil || errB != nil {
				t.Fatalf("解析失败: a=%v b=%v", errA, errB)
			}
			got, detail := a.Compare(b, NumericCompareConfig{AbsTol: c.absTol, RelTol: c.relTol})
			if got != c.want {
				t.Errorf("Compare(%s,%s)=%v want %v; detail=%s", c.a, c.b, got, c.want, detail)
			}
		})
	}
}
