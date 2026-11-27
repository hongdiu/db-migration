package main

import "testing"

func TestNumericKindFromDBType(t *testing.T) {
	cases := map[string]NumericKind{
		"bigint":   NumericInteger,
		"INT":      NumericInteger,
		"tinyint":  NumericInteger,
		"decimal":  NumericDecimal,
		"NUMERIC":  NumericDecimal,
		"double":   NumericFloat,
		"FLOAT":    NumericFloat,
		"varchar":  NumericNone,
		"datetime": NumericNone,
	}
	for typ, want := range cases {
		if got := NumericKindFromDBType(typ); got != want {
			t.Errorf("%s: 期望 %v，实际 %v", typ, want, got)
		}
	}
}

func TestExactBigNumbers(t *testing.T) {
	// 无符号 BIGINT 最大值，20 位，远超 float64 的精确范围。
	a := TextValue("18446744073709551615")
	b := TextValue("18446744073709551615")
	if ok, _ := CompareNumbers(a, b, NumericInteger, 0.0001, 0.00001); !ok {
		t.Fatal("相同的 20 位整数应精确相等")
	}
	// 差 1 必须判失败。
	c := TextValue("18446744073709551614")
	if ok, _ := CompareNumbers(a, c, NumericInteger, 0.0001, 0.00001); ok {
		t.Fatal("差 1 的整数不应相等（零容差）")
	}
	// 超长定点小数。
	d := TextValue("12345678901234567890.12345678901234567890")
	e := TextValue("12345678901234567890.12345678901234567890")
	if ok, _ := CompareNumbers(d, e, NumericDecimal, 0.0001, 0.00001); !ok {
		t.Fatal("相同的超长定点小数应精确相等")
	}
	f := TextValue("12345678901234567890.12345678901234567891")
	if ok, _ := CompareNumbers(d, f, NumericDecimal, 0.0001, 0.00001); ok {
		t.Fatal("末位不同的定点小数不应相等（零容差）")
	}
}

func TestDecimalFormatNormalization(t *testing.T) {
	// 1.0 / 1.00 / 01.0 写法不同但数值相等。
	pairs := [][2]string{
		{"1.0", "1.00"},
		{"01.0", "1"},
		{"-0.50", "-0.5"},
		{"0", "0.000"},
	}
	for _, p := range pairs {
		ok, msg := CompareNumbers(TextValue(p[0]), TextValue(p[1]), NumericDecimal, 0.0001, 0.00001)
		if !ok {
			t.Errorf("%s 与 %s 应判等，实际: %s", p[0], p[1], msg)
		}
	}
}

func TestNullStrictness(t *testing.T) {
	// NULL 与 NULL 相等。
	if ok, _ := CompareNumbers(NullValue(), NullValue(), NumericDecimal, 0.0001, 0.00001); !ok {
		t.Fatal("NULL 与 NULL 应相等")
	}
	// NULL 与 0 不等。
	if ok, _ := CompareNumbers(NullValue(), TextValue("0"), NumericInteger, 0.0001, 0.00001); ok {
		t.Fatal("NULL 与 0 不应相等")
	}
	// NULL 与空串不等。
	if ok, _ := CompareNumbers(NullValue(), TextValue(""), NumericDecimal, 0.0001, 0.00001); ok {
		t.Fatal("NULL 与空串不应相等")
	}
	// 反方向。
	if ok, _ := CompareNumbers(TextValue("1"), NullValue(), NumericInteger, 0.0001, 0.00001); ok {
		t.Fatal("非 NULL 与 NULL 不应相等")
	}
}

func TestFloatTolerance(t *testing.T) {
	const absTol = 0.0001
	const relTol = 0.00001
	// 小数值，差值 0.00005 < 绝对容差，通过。
	if ok, _ := CompareNumbers(TextValue("1.00005"), TextValue("1.0"), NumericFloat, absTol, relTol); !ok {
		t.Fatal("绝对容差内的浮点应通过")
	}
	// 差值 0.001 超出容差，失败。
	if ok, _ := CompareNumbers(TextValue("1.001"), TextValue("1.0"), NumericFloat, absTol, relTol); ok {
		t.Fatal("超出容差的浮点应失败")
	}
	// 大数值靠相对容差：1e12 的量级，差 1e7 = 1e-5 相对，恰好在边界。
	if ok, _ := CompareNumbers(TextValue("10000000000000"), TextValue("10000100000000"), NumericFloat, absTol, relTol); !ok {
		t.Fatal("相对容差边界内的浮点应通过")
	}
	// 大数值差 1e8，超出相对容差。
	if ok, _ := CompareNumbers(TextValue("10000000000000"), TextValue("10001000000000"), NumericFloat, absTol, relTol); ok {
		t.Fatal("超出相对容差的浮点应失败")
	}
}

func TestInvalidInput(t *testing.T) {
	if ok, msg := CompareNumbers(TextValue("abc"), TextValue("1"), NumericInteger, 0.0001, 0.00001); ok {
		t.Fatalf("非法整数应判失败: %s", msg)
	}
	if ok, msg := CompareNumbers(TextValue("abc"), TextValue("1"), NumericFloat, 0.0001, 0.00001); ok {
		t.Fatalf("非法浮点应判失败: %s", msg)
	}
}
