package dbmigration

import (
	"fmt"
	"math"
	"math/big"
	"strings"
)

// NumericKind 表示数据库字段的数值类别。不同类别采用不同的核对规则，
// 这是"精确可靠"的核心：整数与定点小数零容差，浮点才允许容差。
type NumericKind int

const (
	NumericNone    NumericKind = iota // 非数值类型
	NumericInteger                    // 整数类：TINYINT/SMALLINT/MEDIUMINT/INT/BIGINT/BOOLEAN/BIT，精确
	NumericDecimal                    // 定点小数：DECIMAL/NUMERIC，精确
	NumericFloat                      // 浮点：FLOAT/DOUBLE/REAL，近似，需容差
)

func (k NumericKind) String() string {
	switch k {
	case NumericInteger:
		return "整数(精确)"
	case NumericDecimal:
		return "定点小数(精确)"
	case NumericFloat:
		return "浮点(容差)"
	default:
		return "非数值"
	}
}

// NumericKindFromDBType 把 information_schema.COLUMNS.DATA_TYPE 映射为数值类别。
func NumericKindFromDBType(dataType string) NumericKind {
	switch strings.ToLower(strings.TrimSpace(dataType)) {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "bool", "boolean", "bit", "year":
		return NumericInteger
	case "decimal", "numeric", "fixed", "dec", "newdecimal":
		return NumericDecimal
	case "float", "double", "double precision", "real":
		return NumericFloat
	default:
		return NumericNone
	}
}

// FloatSpecial 表示浮点的特殊取值（正负无穷、非数字）。
type FloatSpecial int

const (
	FloatNone   FloatSpecial = iota
	FloatPosInf              // 正无穷
	FloatNegInf              // 负无穷
	FloatNaN                 // 非数字
)

// ExactNumber 是任意精度的数值。用有理数保存，避免 float64 在大数/小数上的精度丢失。
type ExactNumber struct {
	Kind    NumericKind
	Rat     *big.Rat
	Raw     string
	Special FloatSpecial // 仅浮点类别可能为非 FloatNone
}

// floatValue 返回数值的 float64 近似，供浮点容差比对使用。
func (n *ExactNumber) floatValue() float64 {
	switch n.Special {
	case FloatPosInf:
		return math.Inf(1)
	case FloatNegInf:
		return math.Inf(-1)
	case FloatNaN:
		return math.NaN()
	default:
		f, _ := n.Rat.Float64()
		return f
	}
}

// ParseExactNumber 从数据库返回的文本精确解析数值。
// 支持普通十进制、负号与科学计数法（如 1.23e-5）。整数与定点小数不会丢失任何精度。
func ParseExactNumber(raw string, kind NumericKind) (*ExactNumber, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("空数值")
	}
	n := &ExactNumber{Kind: kind, Raw: raw}
	// 浮点类别识别特殊值；整数与定点小数不允许特殊值。
	if kind == NumericFloat {
		switch strings.ToLower(s) {
		case "+inf", "inf", "infinity", "+infinity":
			n.Special = FloatPosInf
			return n, nil
		case "-inf", "-infinity":
			n.Special = FloatNegInf
			return n, nil
		case "nan":
			n.Special = FloatNaN
			return n, nil
		}
	}
	// big.Rat.SetString 支持十进制与可选指数；拒绝分数写法以免误配。
	if strings.ContainsAny(s, "/") {
		return nil, fmt.Errorf("非法数值格式 %q", raw)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("无法解析数值 %q", raw)
	}
	n.Rat = r
	return n, nil
}

// NumericCompareConfig 是浮点比对容差。整数与定点小数忽略容差，始终零容差。
type NumericCompareConfig struct {
	AbsTol float64 // 绝对容差
	RelTol float64 // 相对容差
}

// Compare 比对迁移前后数值，返回是否通过及可读说明。
//   - 整数 / 定点小数：按有理数精确相等判定，差值非零即失败。
//   - 浮点：绝对容差与相对容差满足任一即通过；两者为 0 时退化为精确相等。
func (a *ExactNumber) Compare(b *ExactNumber, cfg NumericCompareConfig) (bool, string) {
	if a.Kind != b.Kind {
		return false, fmt.Sprintf("数值类别不一致: %s vs %s", a.Kind, b.Kind)
	}

	if a.Kind == NumericFloat {
		return compareFloat(a, b, cfg)
	}

	// 整数与定点小数：零容差精确比对。
	if a.Rat.Cmp(b.Rat) == 0 {
		return true, fmt.Sprintf("%s 精确相等 (%s)", a.Kind, a.Rat.RatString())
	}
	diff := new(big.Rat).Sub(a.Rat, b.Rat)
	return false, fmt.Sprintf("%s 不一致: 迁移前=%s 迁移后=%s 差值=%s",
		a.Kind, a.Rat.RatString(), b.Rat.RatString(), diff.RatString())
}

func compareFloat(a, b *ExactNumber, cfg NumericCompareConfig) (bool, string) {
	fa := a.floatValue()
	fb := b.floatValue()

	// 非有限值：两者同为 +Inf 或同为 -Inf 视为相等；其余情况不通过。
	if math.IsInf(fa, 0) || math.IsInf(fb, 0) || math.IsNaN(fa) || math.IsNaN(fb) {
		ok := fa == fb
		return ok, fmt.Sprintf("浮点特殊值比对: 迁移前=%v 迁移后=%v 结论=%v", fa, fb, ok)
	}

	diff := math.Abs(fa - fb)
	scale := math.Max(math.Abs(fa), math.Abs(fb))
	absOK := cfg.AbsTol > 0 && diff <= cfg.AbsTol
	relOK := cfg.RelTol > 0 && scale > 0 && diff <= cfg.RelTol*scale
	exactOK := diff == 0

	ok := absOK || relOK || exactOK
	return ok, fmt.Sprintf("浮点比对: 迁移前=%v 迁移后=%v 绝对差=%g (绝对容差=%g, 相对容差=%g)",
		fa, fb, diff, cfg.AbsTol, cfg.RelTol)
}

// ParseSumValue 解析数据库 SUM 聚合结果。整数/定点小数按精确文本处理；
// 浮点按浮点类别处理并施加容差。decimalText 来自 sql 的字符串扫描，避免 float64 中途介入。
func ParseSumValue(decimalText string, kind NumericKind) (*ExactNumber, error) {
	return ParseExactNumber(decimalText, kind)
}
