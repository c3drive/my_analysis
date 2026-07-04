package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

func registerOneilRanking() {
	// オニール成長株スクリーニングAPI
	http.HandleFunc("/api/oneil-ranking", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// キャッシュチェック (60秒TTL)
		if cached, ok := cacheGet("api:oneil-ranking"); ok {
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

		type OneilStock struct {
			Code          string   `json:"Code"`
			Name          string   `json:"Name"`
			Score         float64  `json:"Score"`
			LastPrice     float64  `json:"LastPrice"`
			MarketCap     int64    `json:"MarketCap"`
			NetSales      int64    `json:"NetSales"`
			NetIncome     int64    `json:"NetIncome"`
			EPS           *float64 `json:"EPS"`
			ROE           *float64 `json:"ROE"`
			PER           *float64 `json:"PER"`
			PBR           *float64 `json:"PBR"`
			EquityRatio   *float64 `json:"EquityRatio"`
			RS            *float64 `json:"RS"`
			MarketSegment string   `json:"MarketSegment,omitempty"`
			Sector33      string   `json:"Sector33,omitempty"`
			Sector17      string   `json:"Sector17,omitempty"`
			GrowthMetrics
			UpdatedAt string `json:"UpdatedAt"`
		}

		// SetMaxOpenConns(1) のためメインクエリ前に補助データを完全ロードする
		// RS値を一括取得
		rsMap := make(map[string]float64)
		rsRows, rsErr := db.Query(`
			SELECT code, rs_rank FROM rs_db.rs_scores rs1
			WHERE date = (SELECT MAX(date) FROM rs_db.rs_scores rs2 WHERE rs2.code = rs1.code)`)
		if rsErr == nil {
			for rsRows.Next() {
				var code string
				var rank float64
				if rsRows.Scan(&code, &rank) == nil {
					rsMap[code] = rank
				}
			}
			rsRows.Close()
		}

		// 成長指標用の時系列データを一括ロード
		financialsMap, _ := loadAllFinancials(db)

		// メインクエリ (補助データロード後)
		rows, err := db.Query(`
			SELECT s.code, s.name, COALESCE(s.updated_at, ''),
				   COALESCE(s.net_sales, 0), COALESCE(s.operating_income, 0), COALESCE(s.net_income, 0),
				   COALESCE(s.total_assets, 0), COALESCE(s.net_assets, 0), COALESCE(s.current_assets, 0),
				   COALESCE(s.liabilities, 0), COALESCE(s.current_liabilities, 0),
				   COALESCE(s.cash_and_deposits, 0), COALESCE(s.shares_issued, 0),
				   COALESCE(s.market_segment, ''), COALESCE(s.sector_33, ''), COALESCE(s.sector_17, ''),
				   COALESCE(p.close, 0) as last_price,
				   p.date as price_date
			FROM stocks s
			LEFT JOIN (
				SELECT code, close, date FROM price_db.stock_prices sp1
				WHERE date = (SELECT MAX(date) FROM price_db.stock_prices sp2 WHERE sp2.code = sp1.code)
			) p ON s.code = p.code
			WHERE s.net_sales > 0 OR s.net_income > 0
			ORDER BY s.code ASC`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var stocks []OneilStock
		for rows.Next() {
			var s Stock
			var lastPrice float64
			var priceDate sql.NullString
			var marketSegment, sector33, sector17 string
			if err := rows.Scan(&s.Code, &s.Name, &s.UpdatedAt,
				&s.NetSales, &s.OperatingIncome, &s.NetIncome,
				&s.TotalAssets, &s.NetAssets, &s.CurrentAssets,
				&s.Liabilities, &s.CurrentLiabilities,
				&s.CashAndDeposits, &s.SharesIssued,
				&marketSegment, &sector33, &sector17,
				&lastPrice, &priceDate); err != nil {
				continue
			}

			os := OneilStock{
				Code:          s.Code,
				Name:          s.Name,
				LastPrice:     lastPrice,
				NetSales:      s.NetSales,
				NetIncome:     s.NetIncome,
				MarketSegment: marketSegment,
				Sector33:      sector33,
				Sector17:      sector17,
				UpdatedAt:     s.UpdatedAt,
			}

			m := calcMetrics(lastPrice, s.SharesIssued, s.NetIncome, s.NetAssets, s.TotalAssets, s.CurrentAssets, s.Liabilities)
			os.MarketCap = m.MarketCap
			os.EPS = m.EPS
			os.ROE = m.ROE
			os.PER = m.PER
			os.PBR = m.PBR
			os.EquityRatio = m.EquityRatio

			if rank, ok := rsMap[s.Code]; ok {
				rs := rank
				os.RS = &rs
			}

			// 成長指標（Q0/Q1/Y0/3Y CAGR）
			if records, ok := financialsMap[s.Code]; ok {
				os.GrowthMetrics = calcGrowthMetrics(records)
			}

			// スコア計算（オニール成長株に近いウェイト付け）
			// ROE, PER, PBR, EquityRatio, RS に加え Q0 EPS YoY を重視
			score := 50.0

			if os.ROE != nil {
				if *os.ROE > 20 {
					score += 15
				} else if *os.ROE > 15 {
					score += 10
				} else if *os.ROE > 10 {
					score += 5
				}
			}

			if os.PER != nil {
				if *os.PER < 10 {
					score += 10
				} else if *os.PER < 15 {
					score += 7
				} else if *os.PER < 20 {
					score += 3
				}
			}

			if os.PBR != nil {
				if *os.PBR < 1 {
					score += 8
				} else if *os.PBR < 1.5 {
					score += 4
				}
			}

			if os.EquityRatio != nil {
				if *os.EquityRatio > 50 {
					score += 7
				} else if *os.EquityRatio > 30 {
					score += 3
				}
			}

			if os.RS != nil {
				if *os.RS >= 85 {
					score += 15
				} else if *os.RS >= 70 {
					score += 10
				} else if *os.RS >= 50 {
					score += 5
				}
			}

			// Q0 EPS YoY: 四半期成長率（オニールでは25%以上が推奨）
			if os.Q0EPSYoY != nil {
				if *os.Q0EPSYoY >= 50 {
					score += 20
				} else if *os.Q0EPSYoY >= 25 {
					score += 15
				} else if *os.Q0EPSYoY >= 10 {
					score += 8
				} else if *os.Q0EPSYoY < 0 {
					score -= 10
				}
			}

			// Q1 EPS YoY: 加速を評価
			if os.Q1EPSYoY != nil && os.Q0EPSYoY != nil {
				if *os.Q0EPSYoY > *os.Q1EPSYoY {
					score += 5 // 成長加速
				}
			}

			// 3年 CAGR: 長期成長性
			if os.EPS3YCAGR != nil {
				if *os.EPS3YCAGR >= 25 {
					score += 10
				} else if *os.EPS3YCAGR >= 15 {
					score += 5
				}
			}

			os.Score = score
			stocks = append(stocks, os)
		}

		// スコア順でソート (バブルソート → sort.Slice で O(n²) → O(n log n))
		sort.Slice(stocks, func(i, j int) bool { return stocks[i].Score > stocks[j].Score })

		body, err := json.Marshal(stocks)
		if err != nil {
			http.Error(w, "json marshal: "+err.Error(), http.StatusInternalServerError)
			return
		}
		cacheSet("api:oneil-ranking", body, 60*time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "MISS")
		w.Write(body)
	})

}
