package main

import (
	"database/sql"
	"math"
	"time"
)

// GrowthMetrics は時系列財務データから算出する成長指標
type GrowthMetrics struct {
	Q0EPSYoY   *float64 `json:"Q0EPSYoY"`   // 直近四半期 EPS 前年同期比 (%)
	Q1EPSYoY   *float64 `json:"Q1EPSYoY"`   // 1四半期前 EPS 前年同期比 (%)
	Q0SalesYoY *float64 `json:"Q0SalesYoY"` // 直近四半期 売上 前年同期比 (%)
	Y0EPSYoY   *float64 `json:"Y0EPSYoY"`   // 直近通期 EPS 前年比 (%)
	EPS3YCAGR  *float64 `json:"EPS3YCAGR"`  // 3年 EPS CAGR (%)
}

type financialRecord struct {
	docType               string
	submissionDate        time.Time
	netIncome             int64
	netSales              int64
	sharesIssued          int64
	totalAssets           int64
	nonCurrentLiabilities int64
	currentAssets         int64
	currentLiabilities    int64
	operatingCashFlow     int64
	grossProfit           int64
	dividendPerShare      float64
}

func (r financialRecord) eps() float64 {
	if r.sharesIssued <= 0 {
		return 0
	}
	return float64(r.netIncome) / float64(r.sharesIssued)
}

// loadAllFinancials は全銘柄の財務時系列を一括ロードする
// 返り値のマップ値は submission_date DESC でソート済み
func loadAllFinancials(db *sql.DB) (map[string][]financialRecord, error) {
	rows, err := db.Query(`
		SELECT code, doc_type, submission_date,
		       COALESCE(net_income, 0), COALESCE(net_sales, 0), COALESCE(shares_issued, 0),
		       COALESCE(total_assets, 0), COALESCE(non_current_liabilities, 0),
		       COALESCE(current_assets, 0), COALESCE(current_liabilities, 0),
		       COALESCE(operating_cash_flow, 0), COALESCE(gross_profit, 0),
		       COALESCE(dividend_per_share, 0.0)
		FROM stock_financials
		ORDER BY code ASC, submission_date DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string][]financialRecord)
	for rows.Next() {
		var code string
		var r financialRecord
		var dateStr string
		if err := rows.Scan(&code, &r.docType, &dateStr,
			&r.netIncome, &r.netSales, &r.sharesIssued,
			&r.totalAssets, &r.nonCurrentLiabilities,
			&r.currentAssets, &r.currentLiabilities,
			&r.operatingCashFlow, &r.grossProfit,
			&r.dividendPerShare); err != nil {
			continue
		}
		if len(dateStr) < 10 {
			continue
		}
		t, err := time.Parse("2006-01-02", dateStr[:10])
		if err != nil {
			continue
		}
		r.submissionDate = t
		result[code] = append(result[code], r)
	}
	return result, nil
}

// calcGrowthMetrics は財務時系列から成長指標を計算する
// records は submission_date DESC でソート済みであることを期待
func calcGrowthMetrics(records []financialRecord) GrowthMetrics {
	var quarterly, annual []financialRecord
	for _, r := range records {
		switch r.docType {
		case "140", "160": // 四半期報告書・半期報告書
			quarterly = append(quarterly, r)
		case "120", "130": // 有価証券報告書・訂正
			annual = append(annual, r)
		}
	}

	var m GrowthMetrics

	// Q0: 直近四半期 vs 約1年前の四半期
	if len(quarterly) > 0 {
		q0 := quarterly[0]
		if prior := findNearestByDate(quarterly[1:], q0.submissionDate.AddDate(-1, 0, 0), 45); prior != nil {
			if pct := yoyPctFloat(q0.eps(), prior.eps()); pct != nil {
				m.Q0EPSYoY = pct
			}
			if pct := yoyPctInt(q0.netSales, prior.netSales); pct != nil {
				m.Q0SalesYoY = pct
			}
		}
	}

	// Q1: 1四半期前 vs 約1年前
	if len(quarterly) >= 2 {
		q1 := quarterly[1]
		if prior := findNearestByDate(quarterly[2:], q1.submissionDate.AddDate(-1, 0, 0), 45); prior != nil {
			if pct := yoyPctFloat(q1.eps(), prior.eps()); pct != nil {
				m.Q1EPSYoY = pct
			}
		}
	}

	// Y0: 最新通期 vs 前年通期
	if len(annual) > 0 {
		y0 := annual[0]
		if prior := findNearestByDate(annual[1:], y0.submissionDate.AddDate(-1, 0, 0), 90); prior != nil {
			if pct := yoyPctFloat(y0.eps(), prior.eps()); pct != nil {
				m.Y0EPSYoY = pct
			}
		}
		// 3年CAGR: 最新通期 vs 3年前通期
		if prior3 := findNearestByDate(annual[1:], y0.submissionDate.AddDate(-3, 0, 0), 120); prior3 != nil {
			if y0.eps() > 0 && prior3.eps() > 0 {
				cagr := (math.Pow(y0.eps()/prior3.eps(), 1.0/3.0) - 1) * 100
				m.EPS3YCAGR = &cagr
			}
		}
	}

	return m
}

// findNearestByDate は records の中から target に最も近い日付のレコードを返す
// toleranceDays を超える差がある場合は nil
func findNearestByDate(records []financialRecord, target time.Time, toleranceDays int) *financialRecord {
	var best *financialRecord
	bestDiff := time.Duration(toleranceDays+1) * 24 * time.Hour
	for i := range records {
		diff := records[i].submissionDate.Sub(target)
		if diff < 0 {
			diff = -diff
		}
		if diff < bestDiff {
			bestDiff = diff
			best = &records[i]
		}
	}
	return best
}

// yoyPctFloat は (current - prior) / |prior| * 100 を返す
func yoyPctFloat(current, prior float64) *float64 {
	if prior == 0 {
		return nil
	}
	v := (current - prior) / math.Abs(prior) * 100
	return &v
}

// yoyPctInt は int64 版
func yoyPctInt(current, prior int64) *float64 {
	if prior == 0 {
		return nil
	}
	v := float64(current-prior) / math.Abs(float64(prior)) * 100
	return &v
}

// PiotroskiF9 は Piotroski F-Score (9点満点) の内訳
type PiotroskiF9 struct {
	Score          int  `json:"Score"`           // 0-9
	Available      int  `json:"Available"`       // 計算可能だった項目数 (0-9)
	ROAPositive    bool `json:"ROAPositive"`     // 1. ROA > 0
	ROAImproved    bool `json:"ROAImproved"`     // 2. ΔROA > 0
	CFOPositive    bool `json:"CFOPositive"`     // 3. CFO > 0
	AccrualsGood   bool `json:"AccrualsGood"`    // 4. CFO > NetIncome
	LeverageDown   bool `json:"LeverageDown"`    // 5. Δ長期負債率 < 0
	CurrentRatioUp bool `json:"CurrentRatioUp"`  // 6. Δ流動比率 > 0
	NoDilution     bool `json:"NoDilution"`      // 7. 希薄化なし (発行済株式数 ≤ 前期)
	GrossMarginUp  bool `json:"GrossMarginUp"`   // 8. Δ粗利率 > 0
	AssetTurnUp    bool `json:"AssetTurnUp"`     // 9. Δ資産回転率 > 0
}

// calcPiotroskiF9 は通期決算 (有報) の最新2期を比較して F-Score を算出する
// records は loadAllFinancials の出力 (submission_date DESC) を想定
// 通期 (docType=120/130) のみを使う。前期データが無い項目は false 扱いだが、Available にカウントされない
func calcPiotroskiF9(records []financialRecord) PiotroskiF9 {
	var annual []financialRecord
	for _, r := range records {
		if r.docType == "120" || r.docType == "130" {
			annual = append(annual, r)
		}
	}

	var f PiotroskiF9
	if len(annual) == 0 {
		return f
	}
	cur := annual[0]

	// 1. ROA > 0 (前期不要)
	if cur.totalAssets > 0 {
		f.Available++
		if float64(cur.netIncome)/float64(cur.totalAssets) > 0 {
			f.ROAPositive = true
			f.Score++
		}
	}

	// 3. CFO > 0 (前期不要)
	if cur.operatingCashFlow != 0 {
		f.Available++
		if cur.operatingCashFlow > 0 {
			f.CFOPositive = true
			f.Score++
		}
	}

	// 4. Accruals: CFO > NetIncome (前期不要、両方データが必要)
	if cur.operatingCashFlow != 0 && cur.netIncome != 0 {
		f.Available++
		if cur.operatingCashFlow > cur.netIncome {
			f.AccrualsGood = true
			f.Score++
		}
	}

	// 前期データが取れない場合は ΔXX系をスキップ
	if len(annual) < 2 {
		return f
	}
	prev := annual[1]

	// 2. ΔROA > 0
	if cur.totalAssets > 0 && prev.totalAssets > 0 {
		f.Available++
		curROA := float64(cur.netIncome) / float64(cur.totalAssets)
		prevROA := float64(prev.netIncome) / float64(prev.totalAssets)
		if curROA > prevROA {
			f.ROAImproved = true
			f.Score++
		}
	}

	// 5. Δ長期負債率 < 0
	if cur.totalAssets > 0 && prev.totalAssets > 0 && cur.nonCurrentLiabilities > 0 && prev.nonCurrentLiabilities > 0 {
		f.Available++
		curLev := float64(cur.nonCurrentLiabilities) / float64(cur.totalAssets)
		prevLev := float64(prev.nonCurrentLiabilities) / float64(prev.totalAssets)
		if curLev < prevLev {
			f.LeverageDown = true
			f.Score++
		}
	}

	// 6. Δ流動比率 > 0
	if cur.currentLiabilities > 0 && prev.currentLiabilities > 0 && cur.currentAssets > 0 && prev.currentAssets > 0 {
		f.Available++
		curCR := float64(cur.currentAssets) / float64(cur.currentLiabilities)
		prevCR := float64(prev.currentAssets) / float64(prev.currentLiabilities)
		if curCR > prevCR {
			f.CurrentRatioUp = true
			f.Score++
		}
	}

	// 7. 希薄化なし
	if cur.sharesIssued > 0 && prev.sharesIssued > 0 {
		f.Available++
		if cur.sharesIssued <= prev.sharesIssued {
			f.NoDilution = true
			f.Score++
		}
	}

	// 8. Δ粗利率 > 0
	if cur.netSales > 0 && prev.netSales > 0 && cur.grossProfit > 0 && prev.grossProfit > 0 {
		f.Available++
		curGM := float64(cur.grossProfit) / float64(cur.netSales)
		prevGM := float64(prev.grossProfit) / float64(prev.netSales)
		if curGM > prevGM {
			f.GrossMarginUp = true
			f.Score++
		}
	}

	// 9. Δ資産回転率 > 0
	if cur.totalAssets > 0 && prev.totalAssets > 0 && cur.netSales > 0 && prev.netSales > 0 {
		f.Available++
		curTO := float64(cur.netSales) / float64(cur.totalAssets)
		prevTO := float64(prev.netSales) / float64(prev.totalAssets)
		if curTO > prevTO {
			f.AssetTurnUp = true
			f.Score++
		}
	}

	return f
}
