// 数值类型分类与比对。
//
// 规则（方案定稿）：
//   - 整数、定点小数：零容差精确比对，全程用字符串 + 大数，绝不让浮点数介入，避免精度丢失。
//   - 浮点：绝对容差 + 相对容差双阈值。
//   - NULL 与 0、空字符串严格区分，三者互不相等。
package main

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

type NumericKind int

const (
	NumericNone    NumericKind = iota // 非数值列，不参与数值比对
	NumericInteger                    // 整数：TINYINT ~ BIGINT，含无符号
	NumericDecimal                    // 定点小数：DECIMAL / NUMERIC
	NumericFloat                      // 单精度浮点：FLOAT，约 7 位有效数字
	NumericDouble                     // 双精度浮点：DOUBLE / REAL，约 15 位有效数字
)

func (k NumericKind) String() string {
	switch k {
	case NumericInteger:
		return "整数"
	case NumericDecimal:
		return "定点小数"
	case NumericFloat:
		return "浮点(FLOAT)"
	case NumericDouble:
		return "浮点(DOUBLE)"
	default:
		return "非数值"
	}
}

// NumericKindFromDBType 根据 information_schema 里的 DATA_TYPE 判定类别。
// FLOAT 与 DOUBLE 必须分开：两者有效数字相差一倍以上，用同一组容差时，
// 要么把 DOUBLE 的真实差异放过，要么把 FLOAT 的固有误差判成错误。
func NumericKindFromDBType(dataType string) NumericKind {
	switch strings.ToLower(strings.TrimSpace(dataType)) {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint":
		return NumericInteger
	case "decimal", "numeric", "fixed":
		return NumericDecimal
	case "float":
		return NumericFloat
	case "double", "real", "double precision":
		return NumericDouble
	default:
		return NumericNone
	}
}

// Tolerance 是一组浮点比对阈值。
type Tolerance struct {
	Abs float64 // 绝对容差
	Rel float64 // 相对容差
}

// Tolerances 按浮点类型分别保存阈值。
// FLOAT 约 7 位有效数字，DOUBLE 约 15 位，两者精度差一个数量级以上，
// 所以不能共用一组数值。
type Tolerances struct {
	Float  Tolerance
	Double Tolerance
}

// For 返回某类字段应使用的阈值。
// 整数与定点小数始终零容差比对，不读取这里的值，返回零值即可。
func (t Tolerances) For(kind NumericKind) Tolerance {
	if kind == NumericDouble {
		return t.Double
	}
	return t.Float
}

// NumericValue 是从数据库读出的一个值。
// IsNull 为 true 时，无论 Raw 是什么都按 NULL 处理，不与 0 或空串相等。
type NumericValue struct {
	IsNull bool
	Raw    string
}

// NullValue 构造一个 NULL 值。
func NullValue() NumericValue { return NumericValue{IsNull: true} }

// TextValue 构造一个非 NULL 的文本值。
func TextValue(s string) NumericValue { return NumericValue{Raw: s} }

// CompareNumbers 按类别比对两个值。
// 返回 (是否相等, 判定说明)。说明用于写进复查报告，让人看懂为什么判等或判不等。
func CompareNumbers(a, b NumericValue, kind NumericKind, tols Tolerances) (bool, string) {
	// NULL 只和 NULL 相等；NULL 与 0、空串严格区分。
	if a.IsNull || b.IsNull {
		if a.IsNull && b.IsNull {
			return true, "两侧均为 NULL"
		}
		return false, fmt.Sprintf("NULL 与非 NULL 不等（源=%s，目标=%s）", nullLabel(a), nullLabel(b))
	}

	switch kind {
	case NumericInteger, NumericDecimal:
		return compareExact(a.Raw, b.Raw, kind)
	case NumericFloat, NumericDouble:
		return compareFloat(a.Raw, b.Raw, kind, tols.For(kind))
	default:
		// 非数值列按字符串精确比对，用于最值等场景。
		if a.Raw == b.Raw {
			return true, "字符串相等"
		}
		return false, fmt.Sprintf("字符串不等：源=%q，目标=%q", a.Raw, b.Raw)
	}
}

func nullLabel(v NumericValue) string {
	if v.IsNull {
		return "NULL"
	}
	return fmt.Sprintf("%q", v.Raw)
}

// compareExact 精确比对整数或定点小数。
// big.Rat 以有理数表示，1.0 与 1.00 会归一为同一个值，写法差异不算差异；
// 精度只受字符串本身的位数限制，不受 float64 的 15 位有效数字限制。
func compareExact(a, b string, kind NumericKind) (bool, string) {
	ra, okA := new(big.Rat).SetString(a)
	rb, okB := new(big.Rat).SetString(b)
	if !okA || !okB {
		return false, fmt.Sprintf("无法解析为%s：源=%q，目标=%q", kind, a, b)
	}
	if ra.Cmp(rb) == 0 {
		return true, fmt.Sprintf("%s精确相等（%s == %s）", kind, ra.RatString(), rb.RatString())
	}
	return false, fmt.Sprintf("%s不相等：源=%s，目标=%s（零容差）", kind, ra.RatString(), rb.RatString())
}

// compareFloat 按绝对 + 相对双容差比对浮点数。
// 判定式：|a-b| <= absTol + relTol * max(|a|, |b|)。
// 说明里注明用的是 FLOAT 还是 DOUBLE 阈值，运维从日志就能确认走的是哪一组。
func compareFloat(a, b string, kind NumericKind, tol Tolerance) (bool, string) {
	fa, errA := strconv.ParseFloat(a, 64)
	fb, errB := strconv.ParseFloat(b, 64)
	if errA != nil || errB != nil {
		return false, fmt.Sprintf("无法解析为%s：源=%q，目标=%q", kind, a, b)
	}
	if math.IsNaN(fa) || math.IsNaN(fb) {
		// NaN 与任何值（含自身）都不等，单独说明。
		return false, fmt.Sprintf("出现 NaN：源=%s，目标=%s", a, b)
	}
	diff := math.Abs(fa - fb)
	scale := math.Max(math.Abs(fa), math.Abs(fb))
	limit := tol.Abs + tol.Rel*scale
	if diff <= limit {
		return true, fmt.Sprintf("%s 在容差内：差值=%g，阈值=%g（绝对 %g + 相对 %g × %g）", kind, diff, limit, tol.Abs, tol.Rel, scale)
	}
	return false, fmt.Sprintf("%s 超出容差：差值=%g，阈值=%g（绝对 %g + 相对 %g × %g）", kind, diff, limit, tol.Abs, tol.Rel, scale)
}
