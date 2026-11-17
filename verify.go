package dbmigration

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"
)

const nullMarker = "<NULL>"

// countRows 统计 table 中满足 condition（可为空）的记录数。
func countRows(ctx context.Context, db *sql.DB, table, condition string) (int64, error) {
	q := fmt.Sprintf("SELECT COUNT(*) FROM %s", qIdent(table))
	if strings.TrimSpace(condition) != "" {
		q += " WHERE (" + condition + ")"
	}
	var n int64
	err := db.QueryRowContext(ctx, q).Scan(&n)
	return n, err
}

// sumFields 对指定数值字段做求和，结果用字符串接收入库，再解析为精确数值。
// 关键：绝不让 float64 在中途介入整数/定点小数的累加，否则大数与小数会丢精度。
func sumFields(ctx context.Context, db *sql.DB, table, condition string, fields []string, kinds map[string]NumericKind) (map[string]*ExactNumber, error) {
	out := make(map[string]*ExactNumber, len(fields))
	if len(fields) == 0 {
		return out, nil
	}
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = fmt.Sprintf("COALESCE(SUM(%s),0)", qIdent(f))
	}
	q := fmt.Sprintf("SELECT %s FROM %s", strings.Join(parts, ", "), qIdent(table))
	if strings.TrimSpace(condition) != "" {
		q += " WHERE (" + condition + ")"
	}

	vals := make([]sql.NullString, len(fields))
	ptrs := make([]interface{}, len(fields))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := db.QueryRowContext(ctx, q).Scan(ptrs...); err != nil {
		return nil, fmt.Errorf("求和查询失败: %w", err)
	}
	for i, f := range fields {
		text := "0"
		if vals[i].Valid {
			text = vals[i].String
		}
		kind, ok := kinds[strings.ToLower(f)]
		if !ok || kind == NumericNone {
			// 不再退化为“按小数处理”：SUM 作用在非数值列上会被数据库隐式转 0，
			// 那样即使字段配错也会“通过”核对，属于静默漏检。
			return nil, fmt.Errorf("字段 %q 不是数值类型或缺少列元数据，不能做求和核对", f)
		}
		n, err := ParseExactNumber(text, kind)
		if err != nil {
			return nil, fmt.Errorf("解析字段 %s 的求和值 %q 失败: %w", f, text, err)
		}
		out[f] = n
	}
	return out, nil
}

// nonemptyCounts 统计指定字段的非空数量。
func nonemptyCounts(ctx context.Context, db *sql.DB, table, condition string, fields []string) (map[string]int64, error) {
	out := make(map[string]int64, len(fields))
	if len(fields) == 0 {
		return out, nil
	}
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = fmt.Sprintf("COUNT(%s)", qIdent(f))
	}
	q := fmt.Sprintf("SELECT %s FROM %s", strings.Join(parts, ", "), qIdent(table))
	if strings.TrimSpace(condition) != "" {
		q += " WHERE (" + condition + ")"
	}
	vals := make([]int64, len(fields))
	ptrs := make([]interface{}, len(fields))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := db.QueryRowContext(ctx, q).Scan(ptrs...); err != nil {
		return nil, fmt.Errorf("非空统计查询失败: %w", err)
	}
	for i, f := range fields {
		out[f] = vals[i]
	}
	return out, nil
}

// MinMaxValue 保存单字段的最小值与最大值。数值字段保留精确数值以便正确比对，
// 非数值字段用文本比对。
type MinMaxValue struct {
	Kind    NumericKind
	MinText string
	MaxText string
	MinNum  *ExactNumber
	MaxNum  *ExactNumber
}

// minMaxFields 统计指定字段的最小值与最大值。
func minMaxFields(ctx context.Context, db *sql.DB, table, condition string, fields []string, kinds map[string]NumericKind) (map[string]*MinMaxValue, error) {
	out := make(map[string]*MinMaxValue, len(fields))
	if len(fields) == 0 {
		return out, nil
	}
	for _, f := range fields {
		kind := kinds[strings.ToLower(f)]
		q := fmt.Sprintf("SELECT MIN(%s), MAX(%s) FROM %s", qIdent(f), qIdent(f), qIdent(table))
		if strings.TrimSpace(condition) != "" {
			q += " WHERE (" + condition + ")"
		}
		var minV, maxV sql.NullString
		if err := db.QueryRowContext(ctx, q).Scan(&minV, &maxV); err != nil {
			return nil, fmt.Errorf("最值查询字段 %s 失败: %w", f, err)
		}
		v := &MinMaxValue{Kind: kind}
		if minV.Valid {
			v.MinText = minV.String
		}
		if maxV.Valid {
			v.MaxText = maxV.String
		}
		if kind != NumericNone && minV.Valid {
			if n, err := ParseExactNumber(v.MinText, kind); err == nil {
				v.MinNum = n
			}
		}
		if kind != NumericNone && maxV.Valid {
			if n, err := ParseExactNumber(v.MaxText, kind); err == nil {
				v.MaxNum = n
			}
		}
		out[f] = v
	}
	return out, nil
}

// RowSnapshot 是迁移前对某行做的字段级快照，附带每个字段的数值类别。
type RowSnapshot struct {
	Key    int64
	Values map[string]string // 列名(小写) -> 规范化文本
	Kinds  map[string]NumericKind
}

// takeSnapshots 抽取 rounds 轮样本，每轮最多 size 条记录，返回每轮的整行快照。
//
// 抽样必须按“行”均匀，而不能按“主键值”均匀：
// 旧实现在 [min,max] 上随机取 target、再取 >= target 的第一条，某行的被选中概率正比于
// 它前面的空隙宽度。历史删除造成大空洞时，空洞后面那一行会独占总抽样的绝大部分，
// 稠密区间里的行几乎抽不到，核对形同虚设；而按主键区间分层也同样救不了极端空洞
// （空洞跨越的区间整段落空，最后只剩两三条样本）。
//
// 因此这里用一次顺序扫描 + 蓄水池抽样（Algorithm R）：
//   - 顺序扫一遍主键索引，每读到一行就以“已见行数”为分母做一次替换判定；
//   - 每个子集被选中的概率严格相等，结果无偏；
//   - 代价是一次索引扫描（O(N) 顺序读），可预测，不像深 OFFSET 那样随偏移量放大。
//
// 多轮样本在这一次扫描里一并抽取：池容量取 rounds*size，扫完后打散再切成 rounds 份。
// 这样轮与轮之间不重叠，总覆盖行数更多，发现不一致的概率高于独立的重复抽样。
func takeSnapshots(ctx context.Context, db *sql.DB, table, keyCol, condition string, rounds, size int, rng *rand.Rand) ([][]*RowSnapshot, error) {
	if rounds <= 0 || size <= 0 {
		return nil, nil
	}
	pool, err := reservoirKeys(ctx, db, table, keyCol, condition, rounds*size, rng)
	if err != nil {
		return nil, err
	}
	rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	chunks := splitPool(pool, rounds)
	out := make([][]*RowSnapshot, 0, rounds)
	for _, keys := range chunks {
		snaps, err := readRowsByKeys(ctx, db, table, keyCol, keys)
		if err != nil {
			return nil, err
		}
		out = append(out, snaps)
	}
	return out, nil
}

// reservoirKeys 顺序扫描主键索引，用蓄水池抽样取 want 个互不相同的主键。
// 内存占用 O(want)，扫描过程中不做随机访问，适合千万级表。
func reservoirKeys(ctx context.Context, db *sql.DB, table, keyCol, condition string, want int, rng *rand.Rand) ([]int64, error) {
	if want <= 0 {
		return nil, nil
	}
	q := fmt.Sprintf("SELECT %s FROM %s", qIdent(keyCol), qIdent(table))
	if strings.TrimSpace(condition) != "" {
		q += " WHERE (" + condition + ")"
	}
	q += fmt.Sprintf(" ORDER BY %s", qIdent(keyCol))

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("扫描主键失败: %w", err)
	}
	defer rows.Close()

	pool := make([]int64, 0, want)
	var seen int64
	for rows.Next() {
		var k int64
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		seen++
		offerReservoir(&pool, k, seen, want, rng)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return pool, nil
}

// offerReservoir 是蓄水池抽样的单步判定：池未满则直接入池，
// 否则以 want/seen 的概率替换池中随机一项，保证每行入选概率相等。
// 单独拆出来便于用内存序列验证均匀性，不必依赖数据库。
func offerReservoir(pool *[]int64, k int64, seen int64, want int, rng *rand.Rand) {
	if want <= 0 || seen <= 0 {
		return
	}
	if len(*pool) < want {
		*pool = append(*pool, k)
		return
	}
	if j := rng.Int63n(seen); j < int64(want) {
		(*pool)[j] = k
	}
}

// splitPool 把打散后的主键池切成 rounds 份，轮间不重叠。
// 行数不足时前面几轮各分 1 条，其余轮为空（由调用方在报告里体现实际抽样条数）。
func splitPool(pool []int64, rounds int) [][]int64 {
	out := make([][]int64, rounds)
	if rounds <= 0 || len(pool) == 0 {
		return out
	}
	if len(pool) < rounds {
		for i := 0; i < len(pool); i++ {
			out[i] = pool[i : i+1]
		}
		return out
	}
	per := len(pool) / rounds
	rem := len(pool) % rounds
	idx := 0
	for r := 0; r < rounds; r++ {
		n := per
		if r < rem {
			n++
		}
		out[r] = pool[idx : idx+n]
		idx += n
	}
	return out
}

// readRowsByKeys 用 IN 批量读取指定主键的整行，返回按键排序的快照。
func readRowsByKeys(ctx context.Context, db *sql.DB, table, keyCol string, keys []int64) ([]*RowSnapshot, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	ph := make([]string, len(keys))
	args := make([]interface{}, len(keys))
	for i, k := range keys {
		ph[i] = "?"
		args[i] = k
	}
	q := fmt.Sprintf("SELECT * FROM %s WHERE %s IN (%s)",
		qIdent(table), qIdent(keyCol), strings.Join(ph, ","))

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	lowerName := make([]string, len(cols))
	for i, c := range cols {
		lowerName[i] = strings.ToLower(c)
	}

	raw := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}

	out := make([]*RowSnapshot, 0, len(keys))
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		vals := make(map[string]string, len(cols))
		kinds := make(map[string]NumericKind, len(cols))
		for i, name := range lowerName {
			if raw[i] == nil {
				vals[name] = nullMarker
			} else {
				vals[name] = normalizeValue(raw[i])
			}
			// 依据数据库类型名推断数值类别，用于逐字段比对。
			kinds[name] = NumericKindFromDBType(colTypes[i].DatabaseTypeName())
		}
		keyText := vals[strings.ToLower(keyCol)]
		k, err := strconv.ParseInt(keyText, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("解析主键列 %q 的值 %q 失败: %w", keyCol, keyText, err)
		}
		out = append(out, &RowSnapshot{Key: k, Values: vals, Kinds: kinds})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func normalizeValue(v interface{}) string {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case string:
		return x
	case time.Time:
		return x.Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// FieldDiff 描述一条抽样记录的字段差异。
type FieldDiff struct {
	Table    string
	Round    int
	Key      int64
	Column   string
	Expected string
	Actual   string
	Kind     NumericKind
	Reason   string
}

// compareSnapshots 把迁移前的快照与历史表中同主键的行逐字段比较。
func compareSnapshots(ctx context.Context, db *sql.DB, historyTable, keyCol string, snaps []*RowSnapshot, cfg NumericCompareConfig, tableName string, round int) ([]FieldDiff, error) {
	if len(snaps) == 0 {
		return nil, nil
	}
	keys := make([]int64, len(snaps))
	for i, s := range snaps {
		keys[i] = s.Key
	}
	actualList, err := readRowsByKeys(ctx, db, historyTable, keyCol, keys)
	if err != nil {
		return nil, err
	}
	actual := make(map[int64]*RowSnapshot, len(actualList))
	for _, a := range actualList {
		actual[a.Key] = a
	}

	var diffs []FieldDiff
	for _, snap := range snaps {
		act, ok := actual[snap.Key]
		if !ok {
			diffs = append(diffs, FieldDiff{Table: tableName, Round: round, Key: snap.Key, Column: "<行>", Expected: "存在", Actual: "缺失", Reason: "历史表缺少该主键"})
			continue
		}
		cols := make([]string, 0, len(snap.Values))
		for c := range snap.Values {
			cols = append(cols, c)
		}
		sort.Strings(cols)
		for _, col := range cols {
			exp := snap.Values[col]
			av, ok := act.Values[col]
			if !ok {
				diffs = append(diffs, FieldDiff{Table: tableName, Round: round, Key: snap.Key, Column: col, Expected: exp, Actual: "<列缺失>", Reason: "历史表缺少该列"})
				continue
			}
			kind := snap.Kinds[col]
			if diff, matched := compareField(tableName, round, snap.Key, col, exp, av, kind, cfg); !matched {
				diffs = append(diffs, diff)
			}
		}
	}
	return diffs, nil
}

// compareField 按字段类别比对两个值。返回差异描述与是否一致。
// 数值类别使用精确/容差规则；NULL 与空串严格区分；其余按文本比对。
func compareField(table string, round int, key int64, col, exp, act string, kind NumericKind, cfg NumericCompareConfig) (FieldDiff, bool) {
	makeDiff := func(reason string) (FieldDiff, bool) {
		return FieldDiff{Table: table, Round: round, Key: key, Column: col, Expected: exp, Actual: act, Kind: kind, Reason: reason}, false
	}
	ok := func() (FieldDiff, bool) { return FieldDiff{}, true }

	if exp == nullMarker || act == nullMarker {
		if exp == act {
			return ok()
		}
		return makeDiff("NULL 与非 NULL 不一致")
	}

	if kind != NumericNone {
		a, errA := ParseExactNumber(exp, kind)
		b, errB := ParseExactNumber(act, kind)
		if errA == nil && errB == nil {
			passed, detail := a.Compare(b, cfg)
			if passed {
				return ok()
			}
			return makeDiff(detail)
		}
		// 数值类别但解析失败：退化为文本比对，避免误判。
		if exp == act {
			return ok()
		}
		return makeDiff("数值解析失败且文本不一致")
	}

	if exp == act {
		return ok()
	}
	return makeDiff("文本不一致")
}
