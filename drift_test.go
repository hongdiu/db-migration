package main

import (
	"testing"
)

func mustNum(t *testing.T, raw string, kind NumericKind) *ExactNumber {
	t.Helper()
	n, err := ParseExactNumber(raw, kind)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", raw, err)
	}
	return n
}

// TestDriftDiffs 覆盖 P2-2 的判定逻辑：只有真正变化的指标才产生差异项。
func TestDriftDiffs(t *testing.T) {
	v := &VerifyConfig{
		NonemptyCount:    true,
		MinMax:           true,
		NumericSumFields: []string{"amount"},
		NonemptyFields:   []string{"note"},
		MinMaxFields:     []string{"id"},
		AbsTolerance:     1e-6,
		RelTolerance:     1e-9,
	}
	base := &baseline{
		count:    100,
		sums:     map[string]*ExactNumber{"amount": mustNum(t, "1234.56", NumericDecimal)},
		nonempty: map[string]int64{"note": 90},
		minmax: map[string]*MinMaxValue{"id": {
			Kind: NumericInteger, MinText: "1", MaxText: "100",
			MinNum: mustNum(t, "1", NumericInteger), MaxNum: mustNum(t, "100", NumericInteger),
		}},
	}

	// 完全一致时不应报任何差异。
	same := &driftFingerprint{
		count:    100,
		sums:     map[string]*ExactNumber{"amount": mustNum(t, "1234.56", NumericDecimal)},
		nonempty: map[string]int64{"note": 90},
		minmax: map[string]*MinMaxValue{"id": {
			Kind: NumericInteger, MinText: "1", MaxText: "100",
			MinNum: mustNum(t, "1", NumericInteger), MaxNum: mustNum(t, "100", NumericInteger),
		}},
	}
	if diffs := driftDiffs(base, same, v); len(diffs) != 0 {
		t.Errorf("指纹一致时不应有差异，实际 %v", diffs)
	}

	// 记录数、求和、非空数、最值各自变化都应被发现。
	changed := &driftFingerprint{
		count:    101,
		sums:     map[string]*ExactNumber{"amount": mustNum(t, "1235.56", NumericDecimal)},
		nonempty: map[string]int64{"note": 91},
		minmax: map[string]*MinMaxValue{"id": {
			Kind: NumericInteger, MinText: "1", MaxText: "101",
			MinNum: mustNum(t, "1", NumericInteger), MaxNum: mustNum(t, "101", NumericInteger),
		}},
	}
	diffs := driftDiffs(base, changed, v)
	if len(diffs) != 4 {
		t.Fatalf("四项指标都变了，应报 4 条差异，实际 %d 条: %v", len(diffs), diffs)
	}

	// 只改记录数时只报 1 条，避免把未变化的项也列出来干扰判断。
	onlyCount := &driftFingerprint{
		count:    99,
		sums:     map[string]*ExactNumber{"amount": mustNum(t, "1234.56", NumericDecimal)},
		nonempty: map[string]int64{"note": 90},
		minmax:   same.minmax,
	}
	if diffs := driftDiffs(base, onlyCount, v); len(diffs) != 1 {
		t.Errorf("仅记录数变化时应只报 1 条，实际 %v", diffs)
	}
}

// TestDriftDiffsRespectsDisabledChecks 验证被显式关闭的核对项不参与漂移检测。
func TestDriftDiffsRespectsDisabledChecks(t *testing.T) {
	off := false
	v := &VerifyConfig{RowCount: &off, AbsTolerance: 1e-6, RelTolerance: 1e-9}
	base := &baseline{count: 10}
	now := &driftFingerprint{count: 20}
	if diffs := driftDiffs(base, now, v); len(diffs) != 0 {
		t.Errorf("row_count 已关闭时不应比较记录数，实际 %v", diffs)
	}
}

// TestUseSourceDriftCheckDefault 确认漂移检测默认开启，只有显式 false 才关闭。
func TestUseSourceDriftCheckDefault(t *testing.T) {
	v := &VerifyConfig{}
	if !v.UseSourceDriftCheck() {
		t.Error("未配置时漂移检测应默认开启")
	}
	off := false
	v.DetectSourceDrift = &off
	if v.UseSourceDriftCheck() {
		t.Error("显式 false 应关闭漂移检测")
	}
	on := true
	v.DetectSourceDrift = &on
	if !v.UseSourceDriftCheck() {
		t.Error("显式 true 应开启漂移检测")
	}
}
