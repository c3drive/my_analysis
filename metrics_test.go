package main

import (
	"math"
	"testing"
	"time"
)

func mustDate(t *testing.T, s string) time.Time {
	t.Helper()
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("invalid date %q: %v", s, err)
	}
	return d
}

func TestCalcMetrics_BasicRatios(t *testing.T) {
	// 株価1000円, 1000万株, 純利益1億, 純資産10億, 総資産20億
	m := calcMetrics(
		1000,                 // lastPrice
		10_000_000,           // sharesIssued
		100_000_000,          // netIncome
		1_000_000_000,        // netAssets
		2_000_000_000,        // totalAssets
		800_000_000,          // currentAssets
		800_000_000,          // liabilities
	)

	wantMC := int64(10_000_000_000)
	if m.MarketCap != wantMC {
		t.Errorf("MarketCap = %d, want %d", m.MarketCap, wantMC)
	}
	// PER = 時価100億 / 純利益1億 = 100倍
	if m.PER == nil || math.Abs(*m.PER-100.0) > 0.001 {
		t.Errorf("PER = %v, want 100.0", m.PER)
	}
	// PBR = 100億 / 10億 = 10倍
	if m.PBR == nil || math.Abs(*m.PBR-10.0) > 0.001 {
		t.Errorf("PBR = %v, want 10.0", m.PBR)
	}
	// EPS = 1億 / 1000万株 = 10円
	if m.EPS == nil || math.Abs(*m.EPS-10.0) > 0.001 {
		t.Errorf("EPS = %v, want 10.0", m.EPS)
	}
	// ROE = 1億 / 10億 * 100 = 10%
	if m.ROE == nil || math.Abs(*m.ROE-10.0) > 0.001 {
		t.Errorf("ROE = %v, want 10.0", m.ROE)
	}
	// EquityRatio = 10億 / 20億 * 100 = 50%
	if m.EquityRatio == nil || math.Abs(*m.EquityRatio-50.0) > 0.001 {
		t.Errorf("EquityRatio = %v, want 50.0", m.EquityRatio)
	}
	// NetNetRatio = (8億 - 8億) / 100億 = 0
	if m.NetNetRatio == nil || math.Abs(*m.NetNetRatio-0.0) > 0.001 {
		t.Errorf("NetNetRatio = %v, want 0.0", m.NetNetRatio)
	}
}

func TestCalcMetrics_ZeroDivisorReturnsNil(t *testing.T) {
	// shares=0 → MarketCap=0 → PER/PBR=nil
	m := calcMetrics(1000, 0, 100, 1000, 2000, 800, 800)
	if m.PER != nil {
		t.Errorf("PER should be nil when MarketCap=0, got %v", *m.PER)
	}
	if m.PBR != nil {
		t.Errorf("PBR should be nil when MarketCap=0, got %v", *m.PBR)
	}
}

func TestCalcMetrics_NegativeIncomeReturnsNil(t *testing.T) {
	// 赤字: netIncome<=0 → PER, EPS, ROE は計算されない (nil)
	m := calcMetrics(1000, 1_000_000, -100_000, 100_000_000, 200_000_000, 80_000_000, 80_000_000)
	if m.PER != nil {
		t.Errorf("PER should be nil for negative income")
	}
	if m.EPS != nil {
		t.Errorf("EPS should be nil for negative income")
	}
	if m.ROE != nil {
		t.Errorf("ROE should be nil for negative income")
	}
}

func TestYoyPctFloat(t *testing.T) {
	cases := []struct {
		current, prior, want float64
	}{
		{120, 100, 20.0},
		{80, 100, -20.0},
		{200, 100, 100.0},
		{-50, 100, -150.0},
		{50, -100, 150.0}, // 赤字→黒字転換 (50 - (-100)) / |-100| * 100 = +150%
	}
	for _, c := range cases {
		got := yoyPctFloat(c.current, c.prior)
		if got == nil {
			t.Errorf("yoyPctFloat(%v, %v) = nil", c.current, c.prior)
			continue
		}
		if math.Abs(*got-c.want) > 0.001 {
			t.Errorf("yoyPctFloat(%v, %v) = %v, want %v", c.current, c.prior, *got, c.want)
		}
	}
	// prior=0 → nil
	if v := yoyPctFloat(100, 0); v != nil {
		t.Errorf("prior=0 should return nil, got %v", *v)
	}
}

func TestFindNearestByDate(t *testing.T) {
	records := []financialRecord{
		{submissionDate: mustDate(t, "2024-06-15")},
		{submissionDate: mustDate(t, "2024-09-30")},
		{submissionDate: mustDate(t, "2025-06-20")},
	}
	target := mustDate(t, "2025-06-15")

	// tolerance 30日 → 2025-06-20 (5日差) がマッチ
	got := findNearestByDate(records, target, 30)
	if got == nil || !got.submissionDate.Equal(mustDate(t, "2025-06-20")) {
		t.Errorf("expected 2025-06-20, got %v", got)
	}

	// tolerance 3日 → どれもマッチしない
	got = findNearestByDate(records, target, 3)
	if got != nil {
		t.Errorf("expected nil for tight tolerance, got %v", got.submissionDate)
	}
}

func TestCalcGrowthMetrics_QuarterlyYoY(t *testing.T) {
	// 直近Q (2026-04-15) と1年前 (2025-04-12) の四半期から Q0 EPS YoY を計算
	records := []financialRecord{
		{docType: "140", submissionDate: mustDate(t, "2026-04-15"), netIncome: 200_000_000, sharesIssued: 1_000_000, netSales: 1_500_000_000},
		{docType: "140", submissionDate: mustDate(t, "2025-04-12"), netIncome: 100_000_000, sharesIssued: 1_000_000, netSales: 1_000_000_000},
		// 通期データ
		{docType: "120", submissionDate: mustDate(t, "2026-06-25"), netIncome: 500_000_000, sharesIssued: 1_000_000, netSales: 4_000_000_000},
		{docType: "120", submissionDate: mustDate(t, "2025-06-20"), netIncome: 250_000_000, sharesIssued: 1_000_000, netSales: 2_000_000_000},
	}

	m := calcGrowthMetrics(records)

	// Q0 EPS = 200/1M = 200, prior = 100 → +100%
	if m.Q0EPSYoY == nil || math.Abs(*m.Q0EPSYoY-100.0) > 0.1 {
		t.Errorf("Q0EPSYoY = %v, want 100.0", m.Q0EPSYoY)
	}
	// Q0 売上 1500M → 1000M で +50%
	if m.Q0SalesYoY == nil || math.Abs(*m.Q0SalesYoY-50.0) > 0.1 {
		t.Errorf("Q0SalesYoY = %v, want 50.0", m.Q0SalesYoY)
	}
	// Y0 EPS = 500 vs 250 → +100%
	if m.Y0EPSYoY == nil || math.Abs(*m.Y0EPSYoY-100.0) > 0.1 {
		t.Errorf("Y0EPSYoY = %v, want 100.0", m.Y0EPSYoY)
	}
}

func TestCalcGrowthMetrics_EmptyReturnsNoMetrics(t *testing.T) {
	m := calcGrowthMetrics(nil)
	if m.Q0EPSYoY != nil || m.Y0EPSYoY != nil || m.EPS3YCAGR != nil {
		t.Errorf("empty records should return all-nil metrics, got %+v", m)
	}
}

func TestCalcPiotroskiF9_PerfectScore(t *testing.T) {
	// 全項目で改善している優良企業
	records := []financialRecord{
		// 当期 (2026)
		{docType: "120", submissionDate: mustDate(t, "2026-06-25"),
			netIncome: 200_000_000, netSales: 3_000_000_000, sharesIssued: 1_000_000,
			totalAssets: 2_000_000_000, nonCurrentLiabilities: 200_000_000,
			currentAssets: 1_500_000_000, currentLiabilities: 500_000_000,
			operatingCashFlow: 300_000_000, grossProfit: 1_200_000_000},
		// 前期 (2025)
		{docType: "120", submissionDate: mustDate(t, "2025-06-20"),
			netIncome: 100_000_000, netSales: 2_500_000_000, sharesIssued: 1_000_000,
			totalAssets: 1_900_000_000, nonCurrentLiabilities: 300_000_000,
			currentAssets: 1_200_000_000, currentLiabilities: 600_000_000,
			operatingCashFlow: 150_000_000, grossProfit: 800_000_000},
	}
	f := calcPiotroskiF9(records)
	if f.Score != 9 {
		t.Errorf("Score = %d, want 9. detail=%+v", f.Score, f)
	}
	if f.Available != 9 {
		t.Errorf("Available = %d, want 9", f.Available)
	}
}

func TestCalcPiotroskiF9_AllFail(t *testing.T) {
	// 当期は赤字・CFOマイナス・粗利減・希薄化・負債増・流動比率悪化・回転率低下
	records := []financialRecord{
		{docType: "120", submissionDate: mustDate(t, "2026-06-25"),
			netIncome: -50_000_000, netSales: 2_000_000_000, sharesIssued: 1_500_000,
			totalAssets: 2_500_000_000, nonCurrentLiabilities: 800_000_000,
			currentAssets: 800_000_000, currentLiabilities: 1_000_000_000,
			operatingCashFlow: -100_000_000, grossProfit: 400_000_000},
		{docType: "120", submissionDate: mustDate(t, "2025-06-20"),
			netIncome: 100_000_000, netSales: 2_500_000_000, sharesIssued: 1_000_000,
			totalAssets: 2_000_000_000, nonCurrentLiabilities: 400_000_000,
			currentAssets: 1_200_000_000, currentLiabilities: 500_000_000,
			operatingCashFlow: 200_000_000, grossProfit: 1_000_000_000},
	}
	f := calcPiotroskiF9(records)
	if f.Score != 0 {
		t.Errorf("Score = %d, want 0. detail=%+v", f.Score, f)
	}
	// AccrualsGood: CFO=-100M, NetIncome=-50M → -100M > -50M = false
	if f.AccrualsGood {
		t.Errorf("AccrualsGood should be false")
	}
}

func TestCalcPiotroskiF9_NoAnnual(t *testing.T) {
	// 通期がない (四半期だけ) → 何も計算できない
	records := []financialRecord{
		{docType: "140", submissionDate: mustDate(t, "2026-04-15"),
			netIncome: 100, totalAssets: 1000, operatingCashFlow: 200},
	}
	f := calcPiotroskiF9(records)
	if f.Score != 0 || f.Available != 0 {
		t.Errorf("expected zero values when no annual, got Score=%d Available=%d", f.Score, f.Available)
	}
}

func TestCalcPiotroskiF9_OnlyOneYear(t *testing.T) {
	// 1期分のみ → ROA/CFO/Accruals の3項目のみ計算可能
	records := []financialRecord{
		{docType: "120", submissionDate: mustDate(t, "2026-06-25"),
			netIncome: 100_000_000, totalAssets: 1_000_000_000,
			operatingCashFlow: 200_000_000},
	}
	f := calcPiotroskiF9(records)
	// Available = ROA(1) + CFO(1) + Accruals(1) = 3
	if f.Available != 3 {
		t.Errorf("Available = %d, want 3", f.Available)
	}
	if !f.ROAPositive || !f.CFOPositive || !f.AccrualsGood {
		t.Errorf("expected ROA/CFO/Accruals all true, got %+v", f)
	}
	if f.Score != 3 {
		t.Errorf("Score = %d, want 3", f.Score)
	}
}
