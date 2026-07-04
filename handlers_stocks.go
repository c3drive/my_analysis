package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

func registerStockHandlers() {
	http.HandleFunc("/api/stocks", func(w http.ResponseWriter, r *http.Request) {
		// キャッシュチェック (60秒TTL)
		if cached, ok := cacheGet("api:stocks"); ok {
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

		type StockWithPrice struct {
			Stock
			LastPrice   float64  `json:"LastPrice"`
			PriceDate   *string  `json:"PriceDate"`
			MarketCap   int64    `json:"MarketCap"`
			PER         *float64 `json:"PER"`
			PBR         *float64 `json:"PBR"`
			EPS         *float64 `json:"EPS"`
			ROE         *float64 `json:"ROE"`
			EquityRatio *float64 `json:"EquityRatio"`
			NetNetRatio *float64 `json:"NetNetRatio"`
			RS          *float64 `json:"RS"`
			GrowthMetrics
		}

		// 重要: SetMaxOpenConns(1) のため、メインの rows を開く前に
		// 補助マップ (rsMap, financialsMap) を完全にロードしてから次へ進む
		// (rows を開いたまま別 Query を呼ぶとデッドロックする)

		// RS値を一括取得してマップに格納
		rsMap := make(map[string]float64)
		rsRows, rsErr := db.Query(`
			SELECT code, rs_rank FROM rs_db.rs_scores rs1
			WHERE date = (SELECT MAX(date) FROM rs_db.rs_scores rs2 WHERE rs2.code = rs1.code)`)
		if rsErr != nil {
			log.Printf("⚠️ /api/stocks RS query error: %v", rsErr)
		} else {
			scanErrCount := 0
			for rsRows.Next() {
				var code string
				var rank float64
				if err := rsRows.Scan(&code, &rank); err != nil {
					scanErrCount++
					if scanErrCount <= 3 {
						log.Printf("⚠️ /api/stocks RS scan error: %v", err)
					}
					continue
				}
				rsMap[code] = rank
			}
			rsRows.Close() // メインクエリを開く前に明示的にClose
			log.Printf("📊 /api/stocks RS map loaded: %d entries (scan errors: %d)", len(rsMap), scanErrCount)
		}

		// 成長指標用の時系列データを一括ロード (内部で Query→Close 完結)
		financialsMap, _ := loadAllFinancials(db)

		// メインクエリ (補助データロード後に実行)
		rows, err := db.Query(`
			SELECT s.code, s.name, COALESCE(s.updated_at, ''),
				   COALESCE(s.net_sales, 0), COALESCE(s.operating_income, 0), COALESCE(s.net_income, 0),
				   COALESCE(s.total_assets, 0), COALESCE(s.net_assets, 0), COALESCE(s.current_assets, 0),
				   COALESCE(s.liabilities, 0), COALESCE(s.current_liabilities, 0),
				   COALESCE(s.cash_and_deposits, 0), COALESCE(s.shares_issued, 0),
				   COALESCE(s.investment_securities, 0), COALESCE(s.securities, 0),
				   COALESCE(s.accounts_receivable, 0), COALESCE(s.inventories, 0),
				   COALESCE(s.non_current_liabilities, 0), COALESCE(s.shareholders_equity, 0),
				   COALESCE(s.market_segment, ''), COALESCE(s.sector_33, ''), COALESCE(s.sector_17, ''),
				   COALESCE(p.close, 0) as last_price,
				   p.date as price_date
			FROM stocks s
			LEFT JOIN (
				SELECT code, close, date FROM price_db.stock_prices sp1
				WHERE date = (SELECT MAX(date) FROM price_db.stock_prices sp2 WHERE sp2.code = sp1.code)
			) p ON s.code = p.code
			ORDER BY s.code ASC`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var stocks []StockWithPrice
		for rows.Next() {
			var s StockWithPrice
			var priceDate sql.NullString
			if err := rows.Scan(&s.Code, &s.Name, &s.UpdatedAt,
				&s.NetSales, &s.OperatingIncome, &s.NetIncome,
				&s.TotalAssets, &s.NetAssets, &s.CurrentAssets,
				&s.Liabilities, &s.CurrentLiabilities,
				&s.CashAndDeposits, &s.SharesIssued,
				&s.InvestmentSecurities, &s.Securities,
				&s.AccountsReceivable, &s.Inventories,
				&s.NonCurrentLiabilities, &s.ShareholdersEquity,
				&s.MarketSegment, &s.Sector33, &s.Sector17,
				&s.LastPrice, &priceDate); err != nil {
				log.Printf("⚠️ Scan error: %v", err)
				continue
			}

			if priceDate.Valid {
				s.PriceDate = &priceDate.String
			}

			m := calcMetrics(s.LastPrice, s.SharesIssued, s.NetIncome, s.NetAssets, s.TotalAssets, s.CurrentAssets, s.Liabilities)
			s.MarketCap = m.MarketCap
			s.PER = m.PER
			s.PBR = m.PBR
			s.EPS = m.EPS
			s.ROE = m.ROE
			s.EquityRatio = m.EquityRatio
			s.NetNetRatio = m.NetNetRatio

			if rank, ok := rsMap[s.Code]; ok {
				rs := rank
				s.RS = &rs
			}

			if records, ok := financialsMap[s.Code]; ok {
				s.GrowthMetrics = calcGrowthMetrics(records)
			}

			stocks = append(stocks, s)
		}

		body, err := json.Marshal(stocks)
		if err != nil {
			http.Error(w, "json marshal: "+err.Error(), http.StatusInternalServerError)
			return
		}
		cacheSet("api:stocks", body, 60*time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "MISS")
		w.Write(body)
	})

	// 個別銘柄の株価履歴API
	http.HandleFunc("/api/prices/", func(w http.ResponseWriter, r *http.Request) {
		code := strings.TrimPrefix(r.URL.Path, "/api/prices/")
		if code == "" {
			http.Error(w, "code required", http.StatusBadRequest)
			return
		}

		db, err := openServerDB()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		rows, err := db.Query(`
			SELECT code, date, open, high, low, close, volume
			FROM price_db.stock_prices
			WHERE code = ?
			ORDER BY date DESC
			LIMIT 365`, code)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var prices []StockPrice
		for rows.Next() {
			var p StockPrice
			if err := rows.Scan(&p.Code, &p.Date, &p.Open, &p.High, &p.Low, &p.Close, &p.Volume); err != nil {
				continue
			}
			prices = append(prices, p)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(prices)
	})

	// RS推移履歴API
	http.HandleFunc("/api/rs/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		code := strings.TrimPrefix(r.URL.Path, "/api/rs/")
		if code == "" {
			http.Error(w, "code required", http.StatusBadRequest)
			return
		}

		db, err := openServerDB()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		type RSPoint struct {
			Date   string  `json:"date"`
			RSRank float64 `json:"rs_rank"`
		}

		rows, err := db.Query(`
			SELECT date, rs_rank FROM rs_db.rs_scores
			WHERE code = ?
			ORDER BY date ASC
			LIMIT 365`, code)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]RSPoint{})
			return
		}
		defer rows.Close()

		var points []RSPoint
		for rows.Next() {
			var p RSPoint
			if err := rows.Scan(&p.Date, &p.RSRank); err != nil {
				continue
			}
			points = append(points, p)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(points)
	})

	// 特定日基準のスナップショットAPI（ネットネット分析の遡及計算用）
	// 指定日以前の最新株価を採用し、ネットネット比率等を再計算する
	http.HandleFunc("/api/stocks-as-of/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		dateStr := strings.TrimPrefix(r.URL.Path, "/api/stocks-as-of/")
		if dateStr == "" {
			http.Error(w, "date required (YYYY-MM-DD)", http.StatusBadRequest)
			return
		}
		// 簡易バリデーション
		if len(dateStr) != 10 {
			http.Error(w, "invalid date format, expected YYYY-MM-DD", http.StatusBadRequest)
			return
		}

		db, err := openServerDB()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		// 各銘柄の指定日以前の最新株価をJOIN
		rows, err := db.Query(`
			SELECT s.code, s.name, COALESCE(s.updated_at, ''),
				   COALESCE(s.net_sales, 0), COALESCE(s.operating_income, 0), COALESCE(s.net_income, 0),
				   COALESCE(s.total_assets, 0), COALESCE(s.net_assets, 0), COALESCE(s.current_assets, 0),
				   COALESCE(s.liabilities, 0), COALESCE(s.current_liabilities, 0),
				   COALESCE(s.cash_and_deposits, 0), COALESCE(s.shares_issued, 0),
				   COALESCE(s.investment_securities, 0), COALESCE(s.securities, 0),
				   COALESCE(s.accounts_receivable, 0), COALESCE(s.inventories, 0),
				   COALESCE(s.non_current_liabilities, 0), COALESCE(s.shareholders_equity, 0),
				   COALESCE(p.close, 0) as last_price,
				   p.date as price_date
			FROM stocks s
			LEFT JOIN (
				SELECT code, close, date FROM price_db.stock_prices sp1
				WHERE date <= ?
				  AND date = (
					SELECT MAX(date) FROM price_db.stock_prices sp2
					WHERE sp2.code = sp1.code AND sp2.date <= ?
				  )
			) p ON s.code = p.code
			ORDER BY s.code ASC`, dateStr, dateStr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		type StockSnapshot struct {
			Stock
			LastPrice   float64  `json:"LastPrice"`
			PriceDate   *string  `json:"PriceDate"`
			MarketCap   int64    `json:"MarketCap"`
			PER         *float64 `json:"PER"`
			PBR         *float64 `json:"PBR"`
			EPS         *float64 `json:"EPS"`
			ROE         *float64 `json:"ROE"`
			EquityRatio *float64 `json:"EquityRatio"`
			NetNetRatio *float64 `json:"NetNetRatio"`
			AsOfDate    string   `json:"AsOfDate"`
		}

		var stocks []StockSnapshot
		for rows.Next() {
			var s StockSnapshot
			var priceDate sql.NullString
			if err := rows.Scan(&s.Code, &s.Name, &s.UpdatedAt,
				&s.NetSales, &s.OperatingIncome, &s.NetIncome,
				&s.TotalAssets, &s.NetAssets, &s.CurrentAssets,
				&s.Liabilities, &s.CurrentLiabilities,
				&s.CashAndDeposits, &s.SharesIssued,
				&s.InvestmentSecurities, &s.Securities,
				&s.AccountsReceivable, &s.Inventories,
				&s.NonCurrentLiabilities, &s.ShareholdersEquity,
				&s.LastPrice, &priceDate); err != nil {
				continue
			}

			if priceDate.Valid {
				s.PriceDate = &priceDate.String
			}
			s.AsOfDate = dateStr

			m := calcMetrics(s.LastPrice, s.SharesIssued, s.NetIncome, s.NetAssets, s.TotalAssets, s.CurrentAssets, s.Liabilities)
			s.MarketCap = m.MarketCap
			s.PER = m.PER
			s.PBR = m.PBR
			s.EPS = m.EPS
			s.ROE = m.ROE
			s.EquityRatio = m.EquityRatio
			s.NetNetRatio = m.NetNetRatio

			stocks = append(stocks, s)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stocks)
	})

	// 利用可能な銘柄コード一覧API（市場天井検出のプルダウン用）
	http.HandleFunc("/api/available-codes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")

		db, err := openServerDB()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		rows, err := db.Query(`
			SELECT code, COUNT(*) as cnt, MIN(date) as from_date, MAX(date) as to_date
			FROM price_db.stock_prices
			GROUP BY code
			HAVING cnt >= 30
			ORDER BY cnt DESC`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		type CodeInfo struct {
			Code     string `json:"code"`
			Count    int    `json:"count"`
			FromDate string `json:"from_date"`
			ToDate   string `json:"to_date"`
		}
		var codes []CodeInfo
		for rows.Next() {
			var c CodeInfo
			if err := rows.Scan(&c.Code, &c.Count, &c.FromDate, &c.ToDate); err != nil {
				continue
			}
			codes = append(codes, c)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(codes)
	})
}
