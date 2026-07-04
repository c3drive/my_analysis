package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

func registerValueRanking() {
	// バリュー投資API: グレアム + Piotroski F9 ハイブリッド (100点満点)
	// 配点: PER(15) + PBR(15) + FCF利回り(10) + EV/EBIT(10) + F9(42) + 自己資本比率(8)
	http.HandleFunc("/api/value-ranking", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if cached, ok := cacheGet("api:value-ranking"); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Write(cached)
			return
		}

		db, err := openServerDB()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		type ValueStock struct {
			Code            string      `json:"Code"`
			Name            string      `json:"Name"`
			Score           float64     `json:"Score"`
			LastPrice       float64     `json:"LastPrice"`
			MarketCap       int64       `json:"MarketCap"`
			NetSales        int64       `json:"NetSales"`
			OperatingIncome int64       `json:"OperatingIncome"`
			NetIncome       int64       `json:"NetIncome"`
			EPS             *float64    `json:"EPS"`
			ROE             *float64    `json:"ROE"`
			PER             *float64    `json:"PER"`
			PBR             *float64    `json:"PBR"`
			EquityRatio     *float64    `json:"EquityRatio"`
			FCFYield        *float64    `json:"FCFYield"`   // 営業益/時価総額 * 100 (%)
			EVEBIT          *float64    `json:"EVEBIT"`     // (時価+負債-現金) / 営業益
			FScore          int         `json:"FScore"`     // 0-9
			FAvailable      int         `json:"FAvailable"` // 計算可能だった項目数
			FDetail         PiotroskiF9 `json:"FDetail"`
			MarketSegment   string      `json:"MarketSegment,omitempty"`
			Sector33        string      `json:"Sector33,omitempty"`
			Sector17        string      `json:"Sector17,omitempty"`
			UpdatedAt       string      `json:"UpdatedAt"`
		}

		// 補助データ事前ロード
		financialsMap, _ := loadAllFinancials(db)

		rows, err := db.Query(`
			SELECT s.code, s.name, COALESCE(s.updated_at, ''),
				   COALESCE(s.net_sales, 0), COALESCE(s.operating_income, 0), COALESCE(s.net_income, 0),
				   COALESCE(s.total_assets, 0), COALESCE(s.net_assets, 0), COALESCE(s.current_assets, 0),
				   COALESCE(s.liabilities, 0), COALESCE(s.current_liabilities, 0),
				   COALESCE(s.cash_and_deposits, 0), COALESCE(s.shares_issued, 0),
				   COALESCE(s.market_segment, ''), COALESCE(s.sector_33, ''), COALESCE(s.sector_17, ''),
				   COALESCE(p.close, 0) as last_price
			FROM stocks s
			LEFT JOIN (
				SELECT code, close FROM price_db.stock_prices sp1
				WHERE date = (SELECT MAX(date) FROM price_db.stock_prices sp2 WHERE sp2.code = sp1.code)
			) p ON s.code = p.code
			WHERE s.net_sales > 0 OR s.net_income > 0
			ORDER BY s.code ASC`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var stocks []ValueStock
		for rows.Next() {
			var s Stock
			var lastPrice float64
			var marketSegment, sector33, sector17 string
			if err := rows.Scan(&s.Code, &s.Name, &s.UpdatedAt,
				&s.NetSales, &s.OperatingIncome, &s.NetIncome,
				&s.TotalAssets, &s.NetAssets, &s.CurrentAssets,
				&s.Liabilities, &s.CurrentLiabilities,
				&s.CashAndDeposits, &s.SharesIssued,
				&marketSegment, &sector33, &sector17,
				&lastPrice); err != nil {
				continue
			}

			vs := ValueStock{
				Code:            s.Code,
				Name:            s.Name,
				LastPrice:       lastPrice,
				NetSales:        s.NetSales,
				OperatingIncome: s.OperatingIncome,
				NetIncome:       s.NetIncome,
				MarketSegment:   marketSegment,
				Sector33:        sector33,
				Sector17:        sector17,
				UpdatedAt:       s.UpdatedAt,
			}

			m := calcMetrics(lastPrice, s.SharesIssued, s.NetIncome, s.NetAssets, s.TotalAssets, s.CurrentAssets, s.Liabilities)
			vs.MarketCap = m.MarketCap
			vs.EPS = m.EPS
			vs.ROE = m.ROE
			vs.PER = m.PER
			vs.PBR = m.PBR
			vs.EquityRatio = m.EquityRatio

			// FCF利回り (代用): 営業益 / 時価総額 * 100
			if m.MarketCap > 0 && s.OperatingIncome > 0 {
				v := float64(s.OperatingIncome) / float64(m.MarketCap) * 100
				vs.FCFYield = &v
			}

			// EV/EBIT 簡易: (時価 + 負債 - 現金) / 営業益
			if m.MarketCap > 0 && s.OperatingIncome > 0 {
				ev := float64(m.MarketCap) + float64(s.Liabilities) - float64(s.CashAndDeposits)
				if ev > 0 {
					v := ev / float64(s.OperatingIncome)
					vs.EVEBIT = &v
				}
			}

			// F9 計算 (時系列が必要)
			if records, ok := financialsMap[s.Code]; ok {
				f := calcPiotroskiF9(records)
				vs.FScore = f.Score
				vs.FAvailable = f.Available
				vs.FDetail = f
			}

			// スコア計算 (0-100)
			score := 0.0

			// PER (15点満点): <8で満点、12でゼロ
			if vs.PER != nil && *vs.PER > 0 {
				switch {
				case *vs.PER < 8:
					score += 15
				case *vs.PER < 10:
					score += 12
				case *vs.PER < 12:
					score += 8
				case *vs.PER < 15:
					score += 4
				}
			}

			// PBR (15点満点): <0.7で満点、1.0でゼロ
			if vs.PBR != nil && *vs.PBR > 0 {
				switch {
				case *vs.PBR < 0.7:
					score += 15
				case *vs.PBR < 1.0:
					score += 10
				case *vs.PBR < 1.5:
					score += 5
				}
			}

			// FCF利回り (10点満点): >10%で満点
			if vs.FCFYield != nil {
				switch {
				case *vs.FCFYield >= 10:
					score += 10
				case *vs.FCFYield >= 7:
					score += 7
				case *vs.FCFYield >= 5:
					score += 4
				}
			}

			// EV/EBIT (10点満点): <6で満点、12でゼロ
			if vs.EVEBIT != nil && *vs.EVEBIT > 0 {
				switch {
				case *vs.EVEBIT < 6:
					score += 10
				case *vs.EVEBIT < 8:
					score += 7
				case *vs.EVEBIT < 12:
					score += 4
				}
			}

			// F9 (42点満点): 1点 = 4.667 点換算
			if vs.FAvailable > 0 {
				score += float64(vs.FScore) * 42.0 / 9.0
			}

			// 自己資本比率 (8点満点): >40%で満点
			if vs.EquityRatio != nil {
				switch {
				case *vs.EquityRatio > 50:
					score += 8
				case *vs.EquityRatio > 40:
					score += 6
				case *vs.EquityRatio > 30:
					score += 3
				}
			}

			vs.Score = score
			stocks = append(stocks, vs)
		}

		sort.Slice(stocks, func(i, j int) bool { return stocks[i].Score > stocks[j].Score })

		body, err := json.Marshal(stocks)
		if err != nil {
			http.Error(w, "json marshal: "+err.Error(), http.StatusInternalServerError)
			return
		}
		cacheSet("api:value-ranking", body, 60*time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "MISS")
		w.Write(body)
	})

}
