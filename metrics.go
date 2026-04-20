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
	docType        string
	submissionDate time.Time
	netIncome      int64
	netSales       int64
	sharesIssued   int64
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
		       COALESCE(net_income, 0), COALESCE(net_sales, 0), COALESCE(shares_issued, 0)
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
		if err := rows.Scan(&code, &r.docType, &dateStr, &r.netIncome, &r.netSales, &r.sharesIssued); err != nil {
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
