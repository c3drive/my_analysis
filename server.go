package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// --- 閲覧ロジック ---
func startServer() {
	// 旧DBからの移行
	migrateFromLegacyDB()

	// DB初期化（3ファイル構成）
	xdb, err := initXbrlDB()
	if err != nil {
		log.Printf("⚠️ xbrl.db init warning: %v", err)
	} else {
		xdb.Close()
	}
	pdb, err := initPriceDB()
	if err != nil {
		log.Printf("⚠️ stock_price.db init warning: %v", err)
	} else {
		pdb.Close()
	}
	rdb, err := initRsDB()
	if err != nil {
		log.Printf("⚠️ rs.db init warning: %v", err)
	} else {
		rdb.Close()
	}
	log.Println("✅ Database schema migrated successfully (3-DB)")

	fs := http.FileServer(http.Dir("./web"))
	http.Handle("/", fs)

	http.HandleFunc("/xbrl.db", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-sqlite3")
		http.ServeFile(w, r, "./data/xbrl.db")
	})

	// query.html (sqlite-wasm) 用に圧縮版DBを同一オリジンで配信
	http.HandleFunc("/data/xbrl.db.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		http.ServeFile(w, r, "./data/xbrl.db.gz")
	})
	http.HandleFunc("/data/stock_price.db.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		http.ServeFile(w, r, "./data/stock_price.db.gz")
	})
	http.HandleFunc("/data/rs.db.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		http.ServeFile(w, r, "./data/rs.db.gz")
	})

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

	// サイクル投資API: 業種(33)別のRS中央値・モメンタム + 構成銘柄
	http.HandleFunc("/api/cycle-ranking", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if cached, ok := cacheGet("api:cycle-ranking"); ok {
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

		type Member struct {
			Code   string  `json:"code"`
			Name   string  `json:"name"`
			RS     int     `json:"rs"`
			Price  float64 `json:"price"`
			Volume int64   `json:"volume"`
		}
		type SectorEntry struct {
			Sector       string   `json:"sector"`
			MemberCount  int      `json:"memberCount"`
			RSMedian     float64  `json:"rsMedian"`
			RSMomentum   float64  `json:"rsMomentum"` // 30日前→現在の中央値変化
			Score        float64  `json:"score"`
			Members      []Member `json:"members"`
		}

		// 1. 当日の最新 RS を一括取得
		rsNow := make(map[string]int)
		rows, err := db.Query(`
			SELECT code, rs_rank FROM rs_db.rs_scores rs1
			WHERE date = (SELECT MAX(date) FROM rs_db.rs_scores rs2 WHERE rs2.code = rs1.code)`)
		if err == nil {
			for rows.Next() {
				var c string
				var r int
				if rows.Scan(&c, &r) == nil {
					rsNow[c] = r
				}
			}
			rows.Close()
		}

		// 2. 約30日前の RS (各 code の最新日 - 30日 以前で最新)
		// 簡易実装: 全 rs_scores から最大日付を取り、その30日前を target にして集計
		var maxDate string
		db.QueryRow("SELECT MAX(date) FROM rs_db.rs_scores").Scan(&maxDate)
		past := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
		if maxDate != "" {
			t, perr := time.Parse("2006-01-02", maxDate)
			if perr == nil {
				past = t.AddDate(0, 0, -30).Format("2006-01-02")
			}
		}
		rsPast := make(map[string]int)
		rows, err = db.Query(`
			SELECT code, rs_rank FROM rs_db.rs_scores rs1
			WHERE date = (SELECT MAX(date) FROM rs_db.rs_scores rs2 WHERE rs2.code = rs1.code AND rs2.date <= ?)`, past)
		if err == nil {
			for rows.Next() {
				var c string
				var r int
				if rows.Scan(&c, &r) == nil {
					rsPast[c] = r
				}
			}
			rows.Close()
		}

		// 3. 最新株価 (出来高込み) を一括取得
		type priceData struct {
			price  float64
			volume int64
		}
		priceMap := make(map[string]priceData)
		rows, err = db.Query(`
			SELECT code, close, volume FROM price_db.stock_prices sp1
			WHERE date = (SELECT MAX(date) FROM price_db.stock_prices sp2 WHERE sp2.code = sp1.code)`)
		if err == nil {
			for rows.Next() {
				var c string
				var p float64
				var v int64
				if rows.Scan(&c, &p, &v) == nil {
					priceMap[c] = priceData{price: p, volume: v}
				}
			}
			rows.Close()
		}

		// 4. stocks から code, name, sector_33 をロード、業種別にグルーピング
		sectorMap := make(map[string][]Member)
		rows, err = db.Query(`
			SELECT code, name, COALESCE(sector_33, '') FROM stocks
			WHERE COALESCE(sector_33, '') != ''`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var code, name, sector string
			if rows.Scan(&code, &name, &sector) != nil {
				continue
			}
			rs, ok := rsNow[code]
			if !ok {
				continue // RS のない銘柄はサイクル分析対象外
			}
			pd := priceMap[code]
			sectorMap[sector] = append(sectorMap[sector], Member{
				Code: code, Name: name, RS: rs, Price: pd.price, Volume: pd.volume,
			})
		}
		rows.Close()

		// 5. 業種ごとに集計してスコア計算
		var entries []SectorEntry
		for sector, members := range sectorMap {
			if len(members) < 3 {
				continue // 構成銘柄が極端に少ない業種は除外
			}
			// RS 中央値 (現在)
			rsValues := make([]int, len(members))
			for i, m := range members {
				rsValues[i] = m.RS
			}
			sort.Ints(rsValues)
			medianNow := float64(rsValues[len(rsValues)/2])

			// RS 中央値 (過去): 同じメンバーで集計
			pastValues := make([]int, 0, len(members))
			for _, m := range members {
				if v, ok := rsPast[m.Code]; ok {
					pastValues = append(pastValues, v)
				}
			}
			medianPast := medianNow
			if len(pastValues) > 0 {
				sort.Ints(pastValues)
				medianPast = float64(pastValues[len(pastValues)/2])
			}
			momentum := medianNow - medianPast

			// メンバーを RS 降順ソート
			sort.Slice(members, func(i, j int) bool { return members[i].RS > members[j].RS })

			// スコア (100点満点)
			// - 業種RS百分位: 40 (中央値0-99 を 0-40 に正規化)
			// - 業種RSモメンタム: 30 (-50〜+50 を 0-30 に正規化)
			// - 個別最高RS: 20 (上位銘柄RS)
			// - 売買代金合計: 10 (相対評価のため後で正規化)
			score := medianNow*0.4 + (momentum+50)/100*30
			if len(members) > 0 {
				score += float64(members[0].RS) * 0.2
			}
			// 売買代金は最大値で正規化するため一旦保留 (後段で更新)

			entries = append(entries, SectorEntry{
				Sector:      sector,
				MemberCount: len(members),
				RSMedian:    medianNow,
				RSMomentum:  momentum,
				Score:       score,
				Members:     members,
			})
		}

		// 6. スコア降順
		sort.Slice(entries, func(i, j int) bool { return entries[i].Score > entries[j].Score })

		body, err := json.Marshal(entries)
		if err != nil {
			http.Error(w, "json marshal: "+err.Error(), http.StatusInternalServerError)
			return
		}
		cacheSet("api:cycle-ranking", body, 60*time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "MISS")
		w.Write(body)
	})

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
			FCFYield        *float64    `json:"FCFYield"`        // 営業益/時価総額 * 100 (%)
			EVEBIT          *float64    `json:"EVEBIT"`          // (時価+負債-現金) / 営業益
			FScore          int         `json:"FScore"`          // 0-9
			FAvailable      int         `json:"FAvailable"`      // 計算可能だった項目数
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
			DividendYield    *float64 `json:"DividendYield"`    // DPS / 株価 * 100
			PayoutRatio      *float64 `json:"PayoutRatio"`      // DPS / EPS * 100
			NoCutYears       int      `json:"NoCutYears"`       // 連続非減配年数
			DPSHistory       int      `json:"DPSHistory"`       // 過去年数 (連続性判定の母数)
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

	// 個別銘柄の財務時系列API（四半期・通期）
	http.HandleFunc("/api/financials/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		code := strings.TrimPrefix(r.URL.Path, "/api/financials/")
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

		type FinancialPoint struct {
			DocType         string `json:"doc_type"`
			SubmissionDate  string `json:"submission_date"`
			DocDescription  string `json:"doc_description"`
			NetSales        int64  `json:"net_sales"`
			OperatingIncome int64  `json:"operating_income"`
			NetIncome       int64  `json:"net_income"`
			TotalAssets     int64  `json:"total_assets"`
			NetAssets       int64  `json:"net_assets"`
			SharesIssued    int64  `json:"shares_issued"`
		}

		rows, err := db.Query(`
			SELECT doc_type, submission_date, COALESCE(doc_description, ''),
			       COALESCE(net_sales, 0), COALESCE(operating_income, 0), COALESCE(net_income, 0),
			       COALESCE(total_assets, 0), COALESCE(net_assets, 0), COALESCE(shares_issued, 0)
			FROM stock_financials
			WHERE code = ?
			ORDER BY submission_date ASC`, code)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]FinancialPoint{})
			return
		}
		defer rows.Close()

		var points []FinancialPoint
		for rows.Next() {
			var p FinancialPoint
			if err := rows.Scan(&p.DocType, &p.SubmissionDate, &p.DocDescription,
				&p.NetSales, &p.OperatingIncome, &p.NetIncome,
				&p.TotalAssets, &p.NetAssets, &p.SharesIssued); err != nil {
				continue
			}
			points = append(points, p)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(points)
	})

	// 個別銘柄の TDNET 適時開示一覧API
	http.HandleFunc("/api/disclosures/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		code := strings.TrimPrefix(r.URL.Path, "/api/disclosures/")
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

		type Disclosure struct {
			DisclosureDateTime string `json:"disclosure_datetime"`
			Title              string `json:"title"`
			DocCategory        string `json:"doc_category"`
			PdfURL             string `json:"pdf_url"`
		}

		rows, err := db.Query(`
			SELECT disclosure_datetime, COALESCE(title,''), COALESCE(doc_category,''), COALESCE(pdf_url,'')
			FROM tdnet_disclosures
			WHERE code = ?
			ORDER BY disclosure_datetime DESC
			LIMIT 50`, code)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]Disclosure{})
			return
		}
		defer rows.Close()

		var items []Disclosure
		for rows.Next() {
			var d Disclosure
			if err := rows.Scan(&d.DisclosureDateTime, &d.Title, &d.DocCategory, &d.PdfURL); err != nil {
				continue
			}
			items = append(items, d)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(items)
	})

	// 市場指数データAPI（市場天井検出用）
	http.HandleFunc("/api/market-index/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		code := strings.TrimPrefix(r.URL.Path, "/api/market-index/")
		if code == "" {
			code = "^NKX" // デフォルトは日経225
		}

		db, err := openServerDB()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		// 直近2年分に制限 (市場天井検出には十分。全期間は数十万行で重い)
		rows, err := db.Query(`
			SELECT code, date, open, high, low, close, volume
			FROM price_db.stock_prices
			WHERE code = ?
			  AND date >= date('now', '-730 days')
			ORDER BY date ASC`, code)
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

	fmt.Println("🌐 Dashboard starting at http://localhost:8080")
	fmt.Println("📂 Serving static files from ./web/")
	fmt.Println("📊 API endpoint: http://localhost:8080/api/stocks")
	fmt.Println("📈 Price API: http://localhost:8080/api/prices/{code}")
	fmt.Println("🚀 O'Neil Ranking API: http://localhost:8080/api/oneil-ranking")
	fmt.Println("📉 Market Index API: http://localhost:8080/api/market-index/{code}")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// exportJSON はDBからデータを読み込み、web/stocks.json に出力する
// deploy-pages.yml のインラインGoコードの代替
func exportJSON() {
	ensureDir()
	migrateFromLegacyDB()

	// マイグレーション (ALTER TABLE ADD COLUMN) を実行して列の欠損を補う
	// 古い release から DB を取得した場合に必須
	migDB, err := initXbrlDB()
	if err != nil {
		log.Printf("⚠️ Migration warning: %v", err)
	} else {
		migDB.Close()
	}

	db, err := openServerDB()
	if err != nil {
		log.Fatalf("DB open error: %v", err)
	}
	defer db.Close()

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
		log.Fatalf("Query error: %v", err)
	}
	defer rows.Close()

	type StockJSON struct {
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

	var stocks []StockJSON
	for rows.Next() {
		var s StockJSON
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

	jsonData, err := json.MarshalIndent(stocks, "", "  ")
	if err != nil {
		log.Fatalf("JSON marshal error: %v", err)
	}
	if err := os.WriteFile("./web/stocks.json", jsonData, 0644); err != nil {
		log.Fatalf("Write error: %v", err)
	}

	withSales, withPrice, withRS := 0, 0, 0
	for _, s := range stocks {
		if s.NetSales > 0 {
			withSales++
		}
		if s.LastPrice > 0 {
			withPrice++
		}
		if s.RS != nil {
			withRS++
		}
	}
	fmt.Printf("✅ Generated stocks.json with %d records\n", len(stocks))
	fmt.Printf("  With sales: %d, With price: %d, With RS: %d\n", withSales, withPrice, withRS)
}
