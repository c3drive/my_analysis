package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

func registerDividendRanking() {
	// 高配当API: 配当利回り + 持続性 (100点満点)
	// 配点: 配当利回り(40) + 配当性向(20) + 連続非減配年数(20) + 自己資本率(10) + ROE(10)
	http.HandleFunc("/api/dividend-ranking", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if cached, ok := cacheGet("api:dividend-ranking"); ok {
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

		type DivStock struct {
			Code             string   `json:"Code"`
			Name             string   `json:"Name"`
			Score            float64  `json:"Score"`
			LastPrice        float64  `json:"LastPrice"`
			MarketCap        int64    `json:"MarketCap"`
			NetIncome        int64    `json:"NetIncome"`
			DividendPerShare float64  `json:"DividendPerShare"`
			DividendYield    *float64 `json:"DividendYield"` // DPS / 株価 * 100
			PayoutRatio      *float64 `json:"PayoutRatio"`   // DPS / EPS * 100
			NoCutYears       int      `json:"NoCutYears"`    // 連続非減配年数
			DPSHistory       int      `json:"DPSHistory"`    // 過去年数 (連続性判定の母数)
			EPS              *float64 `json:"EPS"`
			ROE              *float64 `json:"ROE"`
			PER              *float64 `json:"PER"`
			PBR              *float64 `json:"PBR"`
			EquityRatio      *float64 `json:"EquityRatio"`
			MarketSegment    string   `json:"MarketSegment,omitempty"`
			Sector33         string   `json:"Sector33,omitempty"`
			Sector17         string   `json:"Sector17,omitempty"`
			UpdatedAt        string   `json:"UpdatedAt"`
		}

		// 連続非減配年数を時系列から計算するため事前ロード
		financialsMap, _ := loadAllFinancials(db)

		rows, err := db.Query(`
			SELECT s.code, s.name, COALESCE(s.updated_at, ''),
				   COALESCE(s.net_income, 0),
				   COALESCE(s.total_assets, 0), COALESCE(s.net_assets, 0), COALESCE(s.current_assets, 0),
				   COALESCE(s.liabilities, 0),
				   COALESCE(s.shares_issued, 0),
				   COALESCE(s.dividend_per_share, 0.0),
				   COALESCE(s.market_segment, ''), COALESCE(s.sector_33, ''), COALESCE(s.sector_17, ''),
				   COALESCE(p.close, 0) as last_price
			FROM stocks s
			LEFT JOIN (
				SELECT code, close FROM price_db.stock_prices sp1
				WHERE date = (SELECT MAX(date) FROM price_db.stock_prices sp2 WHERE sp2.code = sp1.code)
			) p ON s.code = p.code
			WHERE s.dividend_per_share > 0
			ORDER BY s.code ASC`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var stocks []DivStock
		for rows.Next() {
			var s Stock
			var lastPrice, dps float64
			var marketSegment, sector33, sector17 string
			if err := rows.Scan(&s.Code, &s.Name, &s.UpdatedAt,
				&s.NetIncome,
				&s.TotalAssets, &s.NetAssets, &s.CurrentAssets,
				&s.Liabilities,
				&s.SharesIssued,
				&dps,
				&marketSegment, &sector33, &sector17,
				&lastPrice); err != nil {
				continue
			}

			ds := DivStock{
				Code:             s.Code,
				Name:             s.Name,
				LastPrice:        lastPrice,
				NetIncome:        s.NetIncome,
				DividendPerShare: dps,
				MarketSegment:    marketSegment,
				Sector33:         sector33,
				Sector17:         sector17,
				UpdatedAt:        s.UpdatedAt,
			}

			m := calcMetrics(lastPrice, s.SharesIssued, s.NetIncome, s.NetAssets, s.TotalAssets, s.CurrentAssets, s.Liabilities)
			ds.MarketCap = m.MarketCap
			ds.EPS = m.EPS
			ds.ROE = m.ROE
			ds.PER = m.PER
			ds.PBR = m.PBR
			ds.EquityRatio = m.EquityRatio

			// 配当利回り
			if lastPrice > 0 && dps > 0 {
				v := dps / lastPrice * 100
				ds.DividendYield = &v
			}

			// 配当性向
			if ds.EPS != nil && *ds.EPS > 0 && dps > 0 {
				v := dps / *ds.EPS * 100
				ds.PayoutRatio = &v
			}

			// 連続非減配年数 (時系列から、通期のみ)
			if records, ok := financialsMap[s.Code]; ok {
				var annual []financialRecord
				for _, r := range records {
					if r.docType == "120" || r.docType == "130" {
						annual = append(annual, r)
					}
				}
				ds.DPSHistory = len(annual)
				// records は DESC ソート: annual[0] が最新
				// 最新から遡って前年DPS以上を維持している連続年数をカウント
				noCut := 0
				for i := 0; i+1 < len(annual); i++ {
					curDPS := annual[i].dividendPerShare
					prevDPS := annual[i+1].dividendPerShare
					if curDPS <= 0 || prevDPS <= 0 {
						break
					}
					if curDPS >= prevDPS {
						noCut++
					} else {
						break
					}
				}
				ds.NoCutYears = noCut
			}

			// スコア計算 (100点満点)
			score := 0.0

			// 配当利回り 40点
			if ds.DividendYield != nil {
				switch {
				case *ds.DividendYield >= 5:
					score += 40
				case *ds.DividendYield >= 4:
					score += 32
				case *ds.DividendYield >= 3:
					score += 22
				case *ds.DividendYield >= 2:
					score += 12
				case *ds.DividendYield >= 1:
					score += 5
				}
			}

			// 配当性向 20点 (30-60% が安定的、80%超は減配リスク)
			if ds.PayoutRatio != nil {
				p := *ds.PayoutRatio
				switch {
				case p >= 30 && p <= 60:
					score += 20
				case p >= 20 && p < 30:
					score += 14
				case p > 60 && p <= 80:
					score += 12
				case p > 0 && p < 20:
					score += 6
				case p > 80 && p <= 100:
					score += 4
					// >100% は加点なし (フリーキャッシュからの取り崩し)
				}
			}

			// 連続非減配年数 20点
			switch {
			case ds.NoCutYears >= 5:
				score += 20
			case ds.NoCutYears >= 3:
				score += 14
			case ds.NoCutYears >= 2:
				score += 8
			case ds.NoCutYears >= 1:
				score += 3
			}

			// 自己資本率 10点
			if ds.EquityRatio != nil {
				switch {
				case *ds.EquityRatio > 50:
					score += 10
				case *ds.EquityRatio > 40:
					score += 7
				case *ds.EquityRatio > 30:
					score += 3
				}
			}

			// ROE 10点
			if ds.ROE != nil {
				switch {
				case *ds.ROE >= 12:
					score += 10
				case *ds.ROE >= 8:
					score += 6
				case *ds.ROE >= 5:
					score += 3
				}
			}

			ds.Score = score
			stocks = append(stocks, ds)
		}

		sort.Slice(stocks, func(i, j int) bool { return stocks[i].Score > stocks[j].Score })

		body, err := json.Marshal(stocks)
		if err != nil {
			http.Error(w, "json marshal: "+err.Error(), http.StatusInternalServerError)
			return
		}
		cacheSet("api:dividend-ranking", body, 60*time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "MISS")
		w.Write(body)
	})

}
