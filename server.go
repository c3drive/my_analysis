package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
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

	http.HandleFunc("/api/stocks", func(w http.ResponseWriter, r *http.Request) {
		db, err := openServerDB()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		// 最新株価を含めたクエリ
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
				WHERE date = (SELECT MAX(date) FROM price_db.stock_prices sp2 WHERE sp2.code = sp1.code)
			) p ON s.code = p.code
			ORDER BY s.code ASC`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

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

		// RS値を一括取得してマップに格納
		rsMap := make(map[string]float64)
		rsRows, rsErr := db.Query(`
			SELECT code, rs_rank FROM rs_db.rs_scores rs1
			WHERE date = (SELECT MAX(date) FROM rs_db.rs_scores rs2 WHERE rs2.code = rs1.code)`)
		if rsErr == nil {
			defer rsRows.Close()
			for rsRows.Next() {
				var code string
				var rank float64
				if rsRows.Scan(&code, &rank) == nil {
					rsMap[code] = rank
				}
			}
		}

		// 成長指標用の時系列データを一括ロード
		financialsMap, _ := loadAllFinancials(db)

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

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stocks)
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

		db, err := openServerDB()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		// 銘柄データと株価を取得
		rows, err := db.Query(`
			SELECT s.code, s.name, COALESCE(s.updated_at, ''),
				   COALESCE(s.net_sales, 0), COALESCE(s.operating_income, 0), COALESCE(s.net_income, 0),
				   COALESCE(s.total_assets, 0), COALESCE(s.net_assets, 0), COALESCE(s.current_assets, 0),
				   COALESCE(s.liabilities, 0), COALESCE(s.current_liabilities, 0),
				   COALESCE(s.cash_and_deposits, 0), COALESCE(s.shares_issued, 0),
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

		type OneilStock struct {
			Code        string   `json:"Code"`
			Name        string   `json:"Name"`
			Score       float64  `json:"Score"`
			LastPrice   float64  `json:"LastPrice"`
			MarketCap   int64    `json:"MarketCap"`
			NetSales    int64    `json:"NetSales"`
			NetIncome   int64    `json:"NetIncome"`
			EPS         *float64 `json:"EPS"`
			ROE         *float64 `json:"ROE"`
			PER         *float64 `json:"PER"`
			PBR         *float64 `json:"PBR"`
			EquityRatio *float64 `json:"EquityRatio"`
			RS          *float64 `json:"RS"`
			GrowthMetrics
			UpdatedAt string `json:"UpdatedAt"`
		}

		// RS値を一括取得
		rsMap := make(map[string]float64)
		rsRows, rsErr := db.Query(`
			SELECT code, rs_rank FROM rs_db.rs_scores rs1
			WHERE date = (SELECT MAX(date) FROM rs_db.rs_scores rs2 WHERE rs2.code = rs1.code)`)
		if rsErr == nil {
			defer rsRows.Close()
			for rsRows.Next() {
				var code string
				var rank float64
				if rsRows.Scan(&code, &rank) == nil {
					rsMap[code] = rank
				}
			}
		}

		// 成長指標用の時系列データを一括ロード
		financialsMap, _ := loadAllFinancials(db)

		var stocks []OneilStock
		for rows.Next() {
			var s Stock
			var lastPrice float64
			var priceDate sql.NullString
			if err := rows.Scan(&s.Code, &s.Name, &s.UpdatedAt,
				&s.NetSales, &s.OperatingIncome, &s.NetIncome,
				&s.TotalAssets, &s.NetAssets, &s.CurrentAssets,
				&s.Liabilities, &s.CurrentLiabilities,
				&s.CashAndDeposits, &s.SharesIssued,
				&lastPrice, &priceDate); err != nil {
				continue
			}

			os := OneilStock{
				Code:      s.Code,
				Name:      s.Name,
				LastPrice: lastPrice,
				NetSales:  s.NetSales,
				NetIncome: s.NetIncome,
				UpdatedAt: s.UpdatedAt,
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

		// スコア順でソート
		for i := 0; i < len(stocks)-1; i++ {
			for j := i + 1; j < len(stocks); j++ {
				if stocks[j].Score > stocks[i].Score {
					stocks[i], stocks[j] = stocks[j], stocks[i]
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stocks)
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

		// 全期間の株価データを返す（市場天井検出は長期データが必要）
		rows, err := db.Query(`
			SELECT code, date, open, high, low, close, volume
			FROM price_db.stock_prices
			WHERE code = ?
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

	db, err := openServerDB()
	if err != nil {
		log.Fatalf("DB open error: %v", err)
	}
	defer db.Close()

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
			WHERE date = (SELECT MAX(date) FROM price_db.stock_prices sp2 WHERE sp2.code = sp1.code)
		) p ON s.code = p.code
		ORDER BY s.code ASC`)
	if err != nil {
		log.Fatalf("Query error: %v", err)
	}
	defer rows.Close()

	// RS値を一括取得
	rsMap := make(map[string]float64)
	rsRows, rsErr := db.Query(`
		SELECT code, rs_rank FROM rs_db.rs_scores rs1
		WHERE date = (SELECT MAX(date) FROM rs_db.rs_scores rs2 WHERE rs2.code = rs1.code)`)
	if rsErr == nil {
		defer rsRows.Close()
		for rsRows.Next() {
			var code string
			var rank float64
			if rsRows.Scan(&code, &rank) == nil {
				rsMap[code] = rank
			}
		}
	}

	// 成長指標用の時系列データを一括ロード
	financialsMap, _ := loadAllFinancials(db)

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
