package main

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func ensureDir() {
	if _, err := os.Stat("data"); os.IsNotExist(err) {
		os.Mkdir("data", 0755)
	}
}

type EdinetResponse struct {
	Results []struct {
		DocID          string `json:"docID"`
		EntityName     string `json:"filerName"`
		SecCode        string `json:"secCode"`
		SubmissionDate string `json:"submissionDateTime"`
		DocTypeCode    string `json:"docTypeCode"`
		DocDescription string `json:"docDescription"`
	} `json:"results"`
}

// Stock は銘柄の財務データを保持する構造体
type Stock struct {
	Code      string `json:"Code"`
	Name      string `json:"Name"`
	UpdatedAt string `json:"UpdatedAt"`
	// 売上・利益
	NetSales        int64 `json:"NetSales"`        // 売上高
	OperatingIncome int64 `json:"OperatingIncome"` // 営業利益
	NetIncome       int64 `json:"NetIncome"`       // 純利益
	// 資産・負債
	TotalAssets        int64 `json:"TotalAssets"`        // 総資産
	NetAssets          int64 `json:"NetAssets"`          // 純資産
	CurrentAssets      int64 `json:"CurrentAssets"`      // 流動資産
	Liabilities        int64 `json:"Liabilities"`        // 負債合計
	CurrentLiabilities int64 `json:"CurrentLiabilities"` // 流動負債
	// その他
	CashAndDeposits int64 `json:"CashAndDeposits"` // 現金及び預金
	SharesIssued    int64 `json:"SharesIssued"`    // 発行済株式数
}

// FinancialData はXBRLから抽出した財務データ
type FinancialData struct {
	NetSales           int64
	OperatingIncome    int64
	NetIncome          int64
	TotalAssets        int64
	NetAssets          int64
	CurrentAssets      int64
	Liabilities        int64
	CurrentLiabilities int64
	CashAndDeposits    int64
	SharesIssued       int64
}

// StockPrice は株価データを保持する構造体
type StockPrice struct {
	Code   string  `json:"Code"`
	Date   string  `json:"Date"`
	Open   float64 `json:"Open"`
	High   float64 `json:"High"`
	Low    float64 `json:"Low"`
	Close  float64 `json:"Close"`
	Volume int64   `json:"Volume"`
}

func main() {
	mode := flag.String("mode", "run", "execution mode: run, batch, serve, fetch-prices, or test-parse")
	dateFlag := flag.String("date", time.Now().Format("2006-01-02"), "target date for run mode (YYYY-MM-DD)")
	fromFlag := flag.String("from", "", "start date for batch mode (YYYY-MM-DD)")
	toFlag := flag.String("to", "", "end date for batch mode (YYYY-MM-DD)")
	flag.Parse()

	switch *mode {
	case "test-parse":
		testLocalParse()
	case "run":
		runCollector(*dateFlag)
	case "batch":
		runBatch(*fromFlag, *toFlag)
	case "serve":
		startServer()
	case "fetch-prices":
		fetchStockPrices()
	default:
		log.Fatalf("Unknown mode: %s", *mode)
	}
}

// runBatch は過去の日付範囲を一括で取得するバッチモード
func runBatch(fromStr, toStr string) {
	if fromStr == "" || toStr == "" {
		log.Fatalf("batch mode requires -from and -to flags. Example: -mode=batch -from=2025-04-01 -to=2026-02-22")
	}

	fromDate, err := time.Parse("2006-01-02", fromStr)
	if err != nil {
		log.Fatalf("Invalid -from date: %v", err)
	}
	toDate, err := time.Parse("2006-01-02", toStr)
	if err != nil {
		log.Fatalf("Invalid -to date: %v", err)
	}

	if fromDate.After(toDate) {
		log.Fatalf("-from date must be before -to date")
	}

	totalDays := int(toDate.Sub(fromDate).Hours()/24) + 1
	fmt.Printf("🚀 バッチモード: %s 〜 %s (%d日間)\n\n", fromStr, toStr, totalDays)

	totalProcessed := 0
	totalErrors := 0

	for d := fromDate; !d.After(toDate); d = d.AddDate(0, 0, 1) {
		dateStr := d.Format("2006-01-02")
		// 土日はEDINET提出なしのためスキップ
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			fmt.Printf("⏭️ %s (%s) スキップ（休日）\n", dateStr, d.Weekday())
			continue
		}

		fmt.Printf("\n━━━ %s (%s) ━━━\n", dateStr, d.Weekday())
		runCollector(dateStr)
		totalProcessed++

		// EDINET APIレートリミット対策
		time.Sleep(1 * time.Second)
	}

	fmt.Printf("\n🔥 バッチ完了! 処理日数=%d, エラー=%d\n", totalProcessed, totalErrors)
}

// --- 収集ロジック ---
func runCollector(targetDate string) {
	apiKey := os.Getenv("EDINET_API_KEY")

	var body []byte
	var err error

	if apiKey == "" {
		fmt.Println("⚠️ EDINET_API_KEY not set. Using MOCK MODE...")
		body, err = os.ReadFile("test_data.json")
		if err != nil {
			log.Fatalf("Critical Error: Failed to read mock file: %v", err)
		}
	} else {
		fmt.Printf("🚀 Fetching from EDINET API for: %s\n", targetDate)
		url := fmt.Sprintf("https://api.edinet-fsa.go.jp/api/v2/documents.json?date=%s&type=2", targetDate)

		body, err = fetchFromAPI(url, apiKey)
		if err != nil {
			log.Fatalf("Critical Error: API request failed: %v", err)
		}
	}

	var edinetRes EdinetResponse
	if err := json.Unmarshal(body, &edinetRes); err != nil {
		log.Fatalf("Critical Error: Failed to parse JSON: %v\nRaw Body: %s", err, string(body))
	}

	db, err := initXbrlDB()
	if err != nil {
		log.Fatalf("Critical Error: Database init failed: %v", err)
	}
	defer db.Close()

	// 財務データを含む書類タイプ
	// 120=有価証券報告書, 130=訂正有価証券報告書, 140=四半期報告書, 160=半期報告書
	financialDocTypes := map[string]bool{
		"120": true, // 有価証券報告書
		"130": true, // 訂正有価証券報告書
		"140": true, // 四半期報告書
		"160": true, // 半期報告書
	}

	processedCount := 0
	skippedCount := 0
	errorCount := 0
	emptyDataCount := 0

	// パース成功率トラッキング
	fieldStats := map[string]int{
		"NetSales": 0, "OperatingIncome": 0, "NetIncome": 0,
		"TotalAssets": 0, "NetAssets": 0, "CurrentAssets": 0,
		"Liabilities": 0, "CurrentLiabilities": 0, "CashAndDeposits": 0, "SharesIssued": 0,
	}
	totalParsed := 0

	for _, doc := range edinetRes.Results {
		if doc.SecCode == "" {
			continue
		}

		// 財務データを含まない書類タイプはスキップ
		if !financialDocTypes[doc.DocTypeCode] {
			skippedCount++
			continue
		}

		shortCode := doc.SecCode[:4]
		fmt.Printf("🔍 [%s] %s (%s) - %s\n", doc.DocTypeCode, doc.EntityName, shortCode, doc.DocDescription)

		// XBRLをダウンロードして解析
		data, err := downloadAndParseXBRL(doc.DocID)
		if err != nil {
			log.Printf("⚠️ Skip %s: %v", doc.EntityName, err)
			errorCount++
			continue // 空データでは保存しない
		}

		// パース成功率を記録
		totalParsed++
		if data.NetSales > 0 {
			fieldStats["NetSales"]++
		}
		if data.OperatingIncome > 0 {
			fieldStats["OperatingIncome"]++
		}
		if data.NetIncome > 0 {
			fieldStats["NetIncome"]++
		}
		if data.TotalAssets > 0 {
			fieldStats["TotalAssets"]++
		}
		if data.NetAssets > 0 {
			fieldStats["NetAssets"]++
		}
		if data.CurrentAssets > 0 {
			fieldStats["CurrentAssets"]++
		}
		if data.Liabilities > 0 {
			fieldStats["Liabilities"]++
		}
		if data.CurrentLiabilities > 0 {
			fieldStats["CurrentLiabilities"]++
		}
		if data.CashAndDeposits > 0 {
			fieldStats["CashAndDeposits"]++
		}
		if data.SharesIssued > 0 {
			fieldStats["SharesIssued"]++
		}

		// DBへ保存
		err = saveStock(db, shortCode, doc.EntityName, doc.SubmissionDate, data)
		if err != nil {
			log.Printf("⚠️ DB save failed for %s: %v", shortCode, err)
			errorCount++
		} else {
			processedCount++
		}
	}

	// パース成功率レポート
	fmt.Printf("\n🔥 完了! 処理=%d件, スキップ=%d件, エラー=%d件, 空データ=%d件\n", processedCount, skippedCount, errorCount, emptyDataCount)
	if totalParsed > 0 {
		fmt.Println("📊 パース成功率:")
		for _, field := range []string{"NetSales", "OperatingIncome", "NetIncome", "TotalAssets", "NetAssets", "CurrentAssets", "Liabilities", "CurrentLiabilities", "CashAndDeposits", "SharesIssued"} {
			rate := float64(fieldStats[field]) / float64(totalParsed) * 100
			fmt.Printf("  %s: %d/%d (%.1f%%)\n", field, fieldStats[field], totalParsed, rate)
		}
	}
}

// saveStock は銘柄データをDBに保存する（UPSERT: 既存の有効データを空データで上書きしない）
func saveStock(db *sql.DB, code, name, updatedAt string, data FinancialData) error {
	_, err := db.Exec(`
		INSERT INTO stocks (
			code, name, updated_at,
			net_sales, operating_income, net_income,
			total_assets, net_assets, current_assets,
			liabilities, current_liabilities, cash_and_deposits, shares_issued
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(code) DO UPDATE SET
			name = excluded.name,
			updated_at = CASE WHEN excluded.updated_at != '' THEN excluded.updated_at ELSE stocks.updated_at END,
			net_sales = CASE WHEN excluded.net_sales > 0 THEN excluded.net_sales ELSE stocks.net_sales END,
			operating_income = CASE WHEN excluded.operating_income > 0 THEN excluded.operating_income ELSE stocks.operating_income END,
			net_income = CASE WHEN excluded.net_income > 0 THEN excluded.net_income ELSE stocks.net_income END,
			total_assets = CASE WHEN excluded.total_assets > 0 THEN excluded.total_assets ELSE stocks.total_assets END,
			net_assets = CASE WHEN excluded.net_assets > 0 THEN excluded.net_assets ELSE stocks.net_assets END,
			current_assets = CASE WHEN excluded.current_assets > 0 THEN excluded.current_assets ELSE stocks.current_assets END,
			liabilities = CASE WHEN excluded.liabilities > 0 THEN excluded.liabilities ELSE stocks.liabilities END,
			current_liabilities = CASE WHEN excluded.current_liabilities > 0 THEN excluded.current_liabilities ELSE stocks.current_liabilities END,
			cash_and_deposits = CASE WHEN excluded.cash_and_deposits > 0 THEN excluded.cash_and_deposits ELSE stocks.cash_and_deposits END,
			shares_issued = CASE WHEN excluded.shares_issued > 0 THEN excluded.shares_issued ELSE stocks.shares_issued END
	`,
		code, name, updatedAt,
		data.NetSales, data.OperatingIncome, data.NetIncome,
		data.TotalAssets, data.NetAssets, data.CurrentAssets,
		data.Liabilities, data.CurrentLiabilities, data.CashAndDeposits, data.SharesIssued,
	)
	return err
}

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
			SELECT s.code, s.name, s.updated_at,
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
			MarketCap   int64    `json:"MarketCap"`   // 時価総額 = 株価 × 発行済株式数
			PER         *float64 `json:"PER"`         // 株価収益率 = 時価総額 ÷ 純利益
			PBR         *float64 `json:"PBR"`         // 株価純資産倍率 = 時価総額 ÷ 純資産
			EPS         *float64 `json:"EPS"`         // 1株当たり利益 = 純利益 ÷ 発行済株式数
			ROE         *float64 `json:"ROE"`         // 自己資本利益率 = 純利益 ÷ 純資産 × 100
			EquityRatio *float64 `json:"EquityRatio"` // 自己資本比率 = 純資産 ÷ 総資産 × 100
			NetNetRatio *float64 `json:"NetNetRatio"` // ネットネット値 = (流動資産 - 負債) ÷ 時価総額
		}

		var stocks []StockWithPrice
		for rows.Next() {
			var s StockWithPrice
			var priceDate sql.NullString
			rows.Scan(&s.Code, &s.Name, &s.UpdatedAt,
				&s.NetSales, &s.OperatingIncome, &s.NetIncome,
				&s.TotalAssets, &s.NetAssets, &s.CurrentAssets,
				&s.Liabilities, &s.CurrentLiabilities,
				&s.CashAndDeposits, &s.SharesIssued,
				&s.LastPrice, &priceDate)

			if priceDate.Valid {
				s.PriceDate = &priceDate.String
			}

			// 時価総額を計算（株価 × 発行済株式数）
			if s.LastPrice > 0 && s.SharesIssued > 0 {
				s.MarketCap = int64(s.LastPrice * float64(s.SharesIssued))
			}

			// 投資指標を計算
			// PER = 時価総額 ÷ 純利益
			if s.MarketCap > 0 && s.NetIncome > 0 {
				per := float64(s.MarketCap) / float64(s.NetIncome)
				s.PER = &per
			}

			// PBR = 時価総額 ÷ 純資産
			if s.MarketCap > 0 && s.NetAssets > 0 {
				pbr := float64(s.MarketCap) / float64(s.NetAssets)
				s.PBR = &pbr
			}

			// EPS = 純利益 ÷ 発行済株式数
			if s.NetIncome > 0 && s.SharesIssued > 0 {
				eps := float64(s.NetIncome) / float64(s.SharesIssued)
				s.EPS = &eps
			}

			// ROE = 純利益 ÷ 純資産 × 100
			if s.NetIncome > 0 && s.NetAssets > 0 {
				roe := float64(s.NetIncome) / float64(s.NetAssets) * 100
				s.ROE = &roe
			}

			// 自己資本比率 = 純資産 ÷ 総資産 × 100
			if s.TotalAssets > 0 && s.NetAssets > 0 {
				equityRatio := float64(s.NetAssets) / float64(s.TotalAssets) * 100
				s.EquityRatio = &equityRatio
			}

			// ネットネット値 = (流動資産 - 負債) ÷ 時価総額
			if s.MarketCap > 0 && s.CurrentAssets > 0 {
				netNet := float64(s.CurrentAssets-s.Liabilities) / float64(s.MarketCap)
				s.NetNetRatio = &netNet
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

		db, err := sql.Open("sqlite", "./data/stock_price.db")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		rows, err := db.Query(`
			SELECT code, date, open, high, low, close, volume 
			FROM stock_prices 
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
			rows.Scan(&p.Code, &p.Date, &p.Open, &p.High, &p.Low, &p.Close, &p.Volume)
			prices = append(prices, p)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(prices)
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
			SELECT s.code, s.name, s.updated_at,
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
			Score       float64  `json:"Score"`       // 総合スコア（0-100）
			LastPrice   float64  `json:"LastPrice"`   // 株価
			MarketCap   int64    `json:"MarketCap"`   // 時価総額
			NetSales    int64    `json:"NetSales"`    // 売上高
			NetIncome   int64    `json:"NetIncome"`   // 純利益
			EPS         *float64 `json:"EPS"`         // 1株当たり利益
			ROE         *float64 `json:"ROE"`         // 自己資本利益率
			PER         *float64 `json:"PER"`         // PER
			PBR         *float64 `json:"PBR"`         // PBR
			EquityRatio *float64 `json:"EquityRatio"` // 自己資本比率
			RS          *float64 `json:"RS"`          // 相対力（簡易版）
			UpdatedAt   string   `json:"UpdatedAt"`
		}

		var stocks []OneilStock
		for rows.Next() {
			var s Stock
			var lastPrice float64
			var priceDate sql.NullString
			rows.Scan(&s.Code, &s.Name, &s.UpdatedAt,
				&s.NetSales, &s.OperatingIncome, &s.NetIncome,
				&s.TotalAssets, &s.NetAssets, &s.CurrentAssets,
				&s.Liabilities, &s.CurrentLiabilities,
				&s.CashAndDeposits, &s.SharesIssued,
				&lastPrice, &priceDate)

			os := OneilStock{
				Code:      s.Code,
				Name:      s.Name,
				LastPrice: lastPrice,
				NetSales:  s.NetSales,
				NetIncome: s.NetIncome,
				UpdatedAt: s.UpdatedAt,
			}

			// 時価総額
			if lastPrice > 0 && s.SharesIssued > 0 {
				os.MarketCap = int64(lastPrice * float64(s.SharesIssued))
			}

			// EPS = 純利益 / 発行済株式数
			if s.NetIncome > 0 && s.SharesIssued > 0 {
				eps := float64(s.NetIncome) / float64(s.SharesIssued)
				os.EPS = &eps
			}

			// ROE = 純利益 / 純資産 × 100
			if s.NetAssets > 0 && s.NetIncome > 0 {
				roe := float64(s.NetIncome) / float64(s.NetAssets) * 100
				os.ROE = &roe
			}

			// PER = 時価総額 / 純利益
			if os.MarketCap > 0 && s.NetIncome > 0 {
				per := float64(os.MarketCap) / float64(s.NetIncome)
				os.PER = &per
			}

			// PBR = 時価総額 / 純資産
			if os.MarketCap > 0 && s.NetAssets > 0 {
				pbr := float64(os.MarketCap) / float64(s.NetAssets)
				os.PBR = &pbr
			}

			// 自己資本比率 = 純資産 / 総資産 × 100
			if s.TotalAssets > 0 && s.NetAssets > 0 {
				equityRatio := float64(s.NetAssets) / float64(s.TotalAssets) * 100
				os.EquityRatio = &equityRatio
			}

			// スコア計算（シンプル版）
			// 高ROE、低PER、低PBR、高自己資本比率でスコアを増加
			score := 50.0 // ベーススコア

			if os.ROE != nil {
				if *os.ROE > 20 {
					score += 20
				} else if *os.ROE > 15 {
					score += 15
				} else if *os.ROE > 10 {
					score += 10
				}
			}

			if os.PER != nil {
				if *os.PER < 10 {
					score += 15
				} else if *os.PER < 15 {
					score += 10
				} else if *os.PER < 20 {
					score += 5
				}
			}

			if os.PBR != nil {
				if *os.PBR < 1 {
					score += 10
				} else if *os.PBR < 1.5 {
					score += 5
				}
			}

			if os.EquityRatio != nil {
				if *os.EquityRatio > 50 {
					score += 10
				} else if *os.EquityRatio > 30 {
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

	// 市場指数データAPI（市場天井検出用）
	http.HandleFunc("/api/market-index/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		code := strings.TrimPrefix(r.URL.Path, "/api/market-index/")
		if code == "" {
			code = "^NKX" // デフォルトは日経225
		}

		db, err := sql.Open("sqlite", "./data/stock_price.db")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		// 全期間の株価データを返す（市場天井検出は長期データが必要）
		rows, err := db.Query(`
			SELECT code, date, open, high, low, close, volume
			FROM stock_prices
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
			rows.Scan(&p.Code, &p.Date, &p.Open, &p.High, &p.Low, &p.Close, &p.Volume)
			prices = append(prices, p)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(prices)
	})

	// 利用可能な銘柄コード一覧API（市場天井検出のプルダウン用）
	http.HandleFunc("/api/available-codes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")

		db, err := sql.Open("sqlite", "./data/stock_price.db")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		rows, err := db.Query(`
			SELECT code, COUNT(*) as cnt, MIN(date) as from_date, MAX(date) as to_date
			FROM stock_prices
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
			rows.Scan(&c.Code, &c.Count, &c.FromDate, &c.ToDate)
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

func fetchFromAPI(url, apiKey string) ([]byte, error) {
	client := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Ocp-Apim-Subscription-Key", apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned non-200 status: %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// --- DB初期化（3ファイル構成） ---

// initXbrlDB は財務データ用DB（xbrl.db）を初期化する
func initXbrlDB() (*sql.DB, error) {
	ensureDir()
	db, err := sql.Open("sqlite", "./data/xbrl.db")
	if err != nil {
		return nil, err
	}

	sqlStmt := `
	CREATE TABLE IF NOT EXISTS stocks (
		code TEXT PRIMARY KEY, 
		name TEXT, 
		updated_at DATETIME,
		-- 売上・利益
		net_sales INTEGER,
		operating_income INTEGER,
		net_income INTEGER,
		-- 資産・負債
		total_assets INTEGER,
		net_assets INTEGER,
		current_assets INTEGER,
		liabilities INTEGER,
		current_liabilities INTEGER,
		-- その他
		cash_and_deposits INTEGER,
		shares_issued INTEGER
	);`

	if _, err = db.Exec(sqlStmt); err != nil {
		return nil, fmt.Errorf("テーブル作成失敗: %w", err)
	}

	// マイグレーション（既存カラム追加）
	alterStatements := []string{
		"ALTER TABLE stocks ADD COLUMN operating_income INTEGER",
		"ALTER TABLE stocks ADD COLUMN net_income INTEGER",
		"ALTER TABLE stocks ADD COLUMN total_assets INTEGER",
		"ALTER TABLE stocks ADD COLUMN net_assets INTEGER",
		"ALTER TABLE stocks ADD COLUMN current_assets INTEGER",
		"ALTER TABLE stocks ADD COLUMN liabilities INTEGER",
		"ALTER TABLE stocks ADD COLUMN current_liabilities INTEGER",
		"ALTER TABLE stocks ADD COLUMN cash_and_deposits INTEGER",
		"ALTER TABLE stocks ADD COLUMN shares_issued INTEGER",
	}
	for _, stmt := range alterStatements {
		db.Exec(stmt)
	}

	return db, nil
}

// initPriceDB は株価データ用DB（stock_price.db）を初期化する
func initPriceDB() (*sql.DB, error) {
	ensureDir()
	db, err := sql.Open("sqlite", "./data/stock_price.db")
	if err != nil {
		return nil, err
	}

	sqlStmt := `
	CREATE TABLE IF NOT EXISTS stock_prices (
		code TEXT,
		date TEXT,
		open REAL,
		high REAL,
		low REAL,
		close REAL,
		volume INTEGER,
		PRIMARY KEY (code, date)
	);`
	if _, err = db.Exec(sqlStmt); err != nil {
		return nil, fmt.Errorf("株価テーブル作成失敗: %w", err)
	}

	return db, nil
}

// initRsDB はリラティブストレングス用DB（rs.db）を初期化する
func initRsDB() (*sql.DB, error) {
	ensureDir()
	db, err := sql.Open("sqlite", "./data/rs.db")
	if err != nil {
		return nil, err
	}

	sqlStmt := `
	CREATE TABLE IF NOT EXISTS rs_scores (
		code TEXT,
		date TEXT,
		rs_score REAL,
		rs_rank INTEGER,
		PRIMARY KEY (code, date)
	);`
	if _, err = db.Exec(sqlStmt); err != nil {
		return nil, fmt.Errorf("RSテーブル作成失敗: %w", err)
	}

	return db, nil
}

// openServerDB はサーバー用に全DBをATTACHした接続を返す
func openServerDB() (*sql.DB, error) {
	ensureDir()
	// メインはxbrl.db
	db, err := sql.Open("sqlite", "./data/xbrl.db")
	if err != nil {
		return nil, err
	}

	// 株価DBをアタッチ
	_, err = db.Exec(`ATTACH DATABASE './data/stock_price.db' AS price_db`)
	if err != nil {
		// stock_price.db が存在しなければ無視
		log.Printf("⚠️ stock_price.db attach: %v", err)
	}

	// RS DBをアタッチ
	_, err = db.Exec(`ATTACH DATABASE './data/rs.db' AS rs_db`)
	if err != nil {
		log.Printf("⚠️ rs.db attach: %v", err)
	}

	return db, nil
}

// migrateFromLegacyDB は旧 stock_data.db からデータを移行する
func migrateFromLegacyDB() {
	legacyPath := "./data/stock_data.db"
	if _, err := os.Stat(legacyPath); os.IsNotExist(err) {
		return // 旧DBなし
	}

	// xbrl.db が既にあればスキップ
	if _, err := os.Stat("./data/xbrl.db"); err == nil {
		var count int
		xdb, err := sql.Open("sqlite", "./data/xbrl.db")
		if err == nil {
			defer xdb.Close()
			xdb.QueryRow("SELECT COUNT(*) FROM stocks").Scan(&count)
			if count > 0 {
				fmt.Printf("📋 xbrl.db already has %d records, skipping migration\n", count)
				return
			}
		}
	}

	fmt.Println("🔄 Migrating from legacy stock_data.db...")

	legacyDB, err := sql.Open("sqlite", legacyPath)
	if err != nil {
		log.Printf("⚠️ Legacy DB open failed: %v", err)
		return
	}
	defer legacyDB.Close()

	// 財務データを移行
	xbrlDB, err := initXbrlDB()
	if err != nil {
		log.Printf("⚠️ xbrl.db init failed: %v", err)
		return
	}
	defer xbrlDB.Close()

	rows, err := legacyDB.Query(`SELECT code, name, COALESCE(updated_at, ''),
		COALESCE(net_sales,0), COALESCE(operating_income,0), COALESCE(net_income,0),
		COALESCE(total_assets,0), COALESCE(net_assets,0), COALESCE(current_assets,0),
		COALESCE(liabilities,0), COALESCE(current_liabilities,0),
		COALESCE(cash_and_deposits,0), COALESCE(shares_issued,0)
		FROM stocks`)
	if err != nil {
		log.Printf("⚠️ Legacy stocks query failed: %v", err)
		return
	}
	defer rows.Close()

	stockCount := 0
	for rows.Next() {
		var d FinancialData
		var code, name, updatedAt string
		rows.Scan(&code, &name, &updatedAt,
			&d.NetSales, &d.OperatingIncome, &d.NetIncome,
			&d.TotalAssets, &d.NetAssets, &d.CurrentAssets,
			&d.Liabilities, &d.CurrentLiabilities,
			&d.CashAndDeposits, &d.SharesIssued)
		saveStock(xbrlDB, code, name, updatedAt, d)
		stockCount++
	}
	fmt.Printf("  ✅ Migrated %d stocks to xbrl.db\n", stockCount)

	// 株価データを移行
	priceDB, err := initPriceDB()
	if err != nil {
		log.Printf("⚠️ stock_price.db init failed: %v", err)
		return
	}
	defer priceDB.Close()

	pRows, err := legacyDB.Query(`SELECT code, date, open, high, low, close, volume FROM stock_prices`)
	if err != nil {
		log.Printf("⚠️ Legacy prices query failed: %v", err)
		return
	}
	defer pRows.Close()

	priceCount := 0
	for pRows.Next() {
		var p StockPrice
		pRows.Scan(&p.Code, &p.Date, &p.Open, &p.High, &p.Low, &p.Close, &p.Volume)
		savePricesToDB(priceDB, p.Code, []StockPrice{p})
		priceCount++
	}
	fmt.Printf("  ✅ Migrated %d price records to stock_price.db\n", priceCount)

	// RS DB初期化
	rsDB, err := initRsDB()
	if err != nil {
		log.Printf("⚠️ rs.db init failed: %v", err)
	} else {
		rsDB.Close()
		fmt.Println("  ✅ Created rs.db")
	}

	fmt.Println("🔄 Migration complete!")
}

// XBRLタグと対応するフィールドのマッピング
// EDINETのXBRL形式:
//   - 経営指標サマリー: jpcrp_cor:XXXSummaryOfBusinessResults (contextRef="CurrentYearDuration/Instant")
//   - 財務諸表本体: jppfs_cor:XXX (contextRef="CurrentYearDuration/Instant")
//   - 四半期: contextRef="CurrentQuarterDuration" or "CurrentYTDDuration"
//   - 非連結: contextRefに "_NonConsolidatedMember" サフィックス
var xbrlTagPatterns = map[string]*regexp.Regexp{
	// ====== 売上高 ======
	// サマリー（連結・年度）
	"NetSales": regexp.MustCompile(`<jpcrp_cor:NetSalesSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration"[^>]*>(\d+)</`),
	// サマリー（非連結含む）
	"NetSalesFallback": regexp.MustCompile(`<jpcrp_cor:NetSalesSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(\d+)</`),
	// 財務諸表本体
	"NetSalesFallback2": regexp.MustCompile(`<jppfs_cor:NetSales[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(\d+)</`),
	// 四半期累計
	"NetSalesFallback3": regexp.MustCompile(`<jpcrp_cor:NetSalesSummaryOfBusinessResults[^>]*contextRef="CurrentYTDDuration[^"]*"[^>]*>(\d+)</`),
	// IFRS適用企業の売上収益
	"NetSalesFallback4": regexp.MustCompile(`<jpcrp_cor:RevenueIFRSSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(\d+)</`),
	// 営業収益（銀行・保険など）
	"OperatingRevenues": regexp.MustCompile(`<jpcrp_cor:OperatingRevenue[12]SummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(\d+)</`),
	// 四半期営業収益
	"OperatingRevenuesFallback": regexp.MustCompile(`<jpcrp_cor:OperatingRevenue[12]SummaryOfBusinessResults[^>]*contextRef="CurrentYTDDuration[^"]*"[^>]*>(\d+)</`),

	// ====== 営業利益 ======
	// サマリー（連結）
	"OperatingIncome": regexp.MustCompile(`<jpcrp_cor:OperatingIncomeLossSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration"[^>]*>(-?\d+)</`),
	// サマリー（非連結含む）
	"OperatingIncomeFallback": regexp.MustCompile(`<jpcrp_cor:OperatingIncomeLossSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	// 財務諸表本体
	"OperatingIncomeFallback2": regexp.MustCompile(`<jppfs_cor:OperatingIncome[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	// 四半期累計
	"OperatingIncomeFallback3": regexp.MustCompile(`<jpcrp_cor:OperatingIncomeLossSummaryOfBusinessResults[^>]*contextRef="CurrentYTDDuration[^"]*"[^>]*>(-?\d+)</`),

	// ====== 経常利益 ======
	"OrdinaryIncome": regexp.MustCompile(`<jpcrp_cor:OrdinaryIncomeLossSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	"OrdinaryIncomeFallback": regexp.MustCompile(`<jpcrp_cor:OrdinaryIncomeLossSummaryOfBusinessResults[^>]*contextRef="CurrentYTDDuration[^"]*"[^>]*>(-?\d+)</`),

	// ====== 純利益 ======
	// 親会社株主帰属 サマリー（連結）
	"NetIncome": regexp.MustCompile(`<jpcrp_cor:ProfitLossAttributableToOwnersOfParentSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration"[^>]*>(-?\d+)</`),
	// 親会社株主帰属 サマリー（非連結含む）
	"NetIncomeFallback": regexp.MustCompile(`<jpcrp_cor:ProfitLossAttributableToOwnersOfParentSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	// 財務諸表本体 当期純利益
	"NetIncomeFallback2": regexp.MustCompile(`<jppfs_cor:ProfitLoss[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	// 非連結 NetIncomeLoss
	"NetIncomeFallback3": regexp.MustCompile(`<jpcrp_cor:NetIncomeLossSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	// 四半期累計 純利益
	"NetIncomeFallback4": regexp.MustCompile(`<jpcrp_cor:ProfitLossAttributableToOwnersOfParentSummaryOfBusinessResults[^>]*contextRef="CurrentYTDDuration[^"]*"[^>]*>(-?\d+)</`),
	// IFRS 親会社帰属利益
	"NetIncomeFallback5": regexp.MustCompile(`<jpcrp_cor:ProfitLossAttributableToOwnersOfParentIFRSSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),

	// ====== 総資産 ======
	// サマリー（連結）
	"TotalAssets": regexp.MustCompile(`<jpcrp_cor:TotalAssetsSummaryOfBusinessResults[^>]*contextRef="CurrentYearInstant"[^>]*>(\d+)</`),
	// サマリー（非連結含む）
	"TotalAssetsFallback": regexp.MustCompile(`<jpcrp_cor:TotalAssetsSummaryOfBusinessResults[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	// 財務諸表本体
	"TotalAssetsFallback2": regexp.MustCompile(`<jppfs_cor:Assets[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	// 四半期末時点
	"TotalAssetsFallback3": regexp.MustCompile(`<jpcrp_cor:TotalAssetsSummaryOfBusinessResults[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 純資産 ======
	// サマリー（連結）
	"NetAssets": regexp.MustCompile(`<jpcrp_cor:NetAssetsSummaryOfBusinessResults[^>]*contextRef="CurrentYearInstant"[^>]*>(\d+)</`),
	// サマリー（非連結含む）
	"NetAssetsFallback": regexp.MustCompile(`<jpcrp_cor:NetAssetsSummaryOfBusinessResults[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	// 財務諸表本体
	"NetAssetsFallback2": regexp.MustCompile(`<jppfs_cor:NetAssets[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	// 四半期末時点
	"NetAssetsFallback3": regexp.MustCompile(`<jpcrp_cor:NetAssetsSummaryOfBusinessResults[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),
	// 株主資本（EquityAttributableToOwnersOfParent - IFRS用）
	"NetAssetsFallback4": regexp.MustCompile(`<jpcrp_cor:EquityAttributableToOwnersOfParentIFRSSummaryOfBusinessResults[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 流動資産 ======
	"CurrentAssets": regexp.MustCompile(`<jppfs_cor:CurrentAssets[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"CurrentAssetsFallback": regexp.MustCompile(`<jppfs_cor:CurrentAssets[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 負債合計 ======
	"Liabilities": regexp.MustCompile(`<jppfs_cor:Liabilities[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"LiabilitiesFallback": regexp.MustCompile(`<jppfs_cor:Liabilities[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 流動負債 ======
	"CurrentLiabilities": regexp.MustCompile(`<jppfs_cor:CurrentLiabilities[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"CurrentLiabilitiesFallback": regexp.MustCompile(`<jppfs_cor:CurrentLiabilities[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 現金預金 ======
	"CashAndDeposits": regexp.MustCompile(`<jppfs_cor:CashAndDeposits[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"CashAndDepositsFallback": regexp.MustCompile(`<jppfs_cor:CashAndDeposits[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 発行済株式数 ======
	// サマリー（contextRefにNonConsolidatedMember等が付く場合あり）
	"SharesIssued": regexp.MustCompile(`<jpcrp_cor:TotalNumberOfIssuedSharesSummaryOfBusinessResults[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	// 四半期末時点
	"SharesIssuedFallback": regexp.MustCompile(`<jpcrp_cor:TotalNumberOfIssuedSharesSummaryOfBusinessResults[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),
	// 提出日時点の発行済株式数
	"SharesIssuedFallback2": regexp.MustCompile(`<jpcrp_cor:NumberOfIssuedSharesAsOfFilingDateEtcTotalNumberOfSharesEtc[^>]*>(\d+)</`),
}

// downloadAndParseXBRL はXBRLをダウンロードして財務データを抽出する
func downloadAndParseXBRL(docID string) (FinancialData, error) {
	apiKey := os.Getenv("EDINET_API_KEY")
	if apiKey == "" {
		// モック用のデータを返す
		return FinancialData{
			NetSales:        5000000000,
			OperatingIncome: 500000000,
			NetIncome:       300000000,
			TotalAssets:     10000000000,
			NetAssets:       5000000000,
			CurrentAssets:   3000000000,
			Liabilities:     5000000000,
		}, nil
	}

	url := fmt.Sprintf("https://api.edinet-fsa.go.jp/api/v2/documents/%s?type=1", docID)

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Ocp-Apim-Subscription-Key", apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return FinancialData{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return FinancialData{}, fmt.Errorf("API error: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return FinancialData{}, err
	}

	zipReader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return FinancialData{}, err
	}

	return parseXBRLFromZip(zipReader)
}

// getBaseTagName はフォールバックタグ名からベースタグ名を取得する
func getBaseTagName(tagName string) string {
	// Fallback5 → Fallback4 → ... → Fallback → base の順で除去
	for _, suffix := range []string{"Fallback5", "Fallback4", "Fallback3", "Fallback2", "Fallback"} {
		if strings.HasSuffix(tagName, suffix) {
			return strings.TrimSuffix(tagName, suffix)
		}
	}
	return tagName
}

// applyXBRLValue は抽出した値をFinancialDataに設定する
func applyXBRLValue(data *FinancialData, found map[string]bool, baseName string, value int64) {
	switch baseName {
	case "NetSales", "OperatingRevenues":
		if !found["NetSales"] {
			data.NetSales = value
			found["NetSales"] = true
		}
	case "OperatingIncome":
		if !found["OperatingIncome"] {
			data.OperatingIncome = value
			found["OperatingIncome"] = true
		}
	case "OrdinaryIncome":
		// 経常利益 → OperatingIncomeが0なら代用
		if !found["OperatingIncome"] {
			data.OperatingIncome = value
		}
	case "NetIncome":
		if !found["NetIncome"] {
			data.NetIncome = value
			found["NetIncome"] = true
		}
	case "TotalAssets":
		if !found["TotalAssets"] {
			data.TotalAssets = value
			found["TotalAssets"] = true
		}
	case "NetAssets":
		if !found["NetAssets"] {
			data.NetAssets = value
			found["NetAssets"] = true
		}
	case "CurrentAssets":
		if !found["CurrentAssets"] {
			data.CurrentAssets = value
			found["CurrentAssets"] = true
		}
	case "Liabilities":
		if !found["Liabilities"] {
			data.Liabilities = value
			found["Liabilities"] = true
		}
	case "CurrentLiabilities":
		if !found["CurrentLiabilities"] {
			data.CurrentLiabilities = value
			found["CurrentLiabilities"] = true
		}
	case "CashAndDeposits":
		if !found["CashAndDeposits"] {
			data.CashAndDeposits = value
			found["CashAndDeposits"] = true
		}
	case "SharesIssued":
		if !found["SharesIssued"] {
			data.SharesIssued = value
			found["SharesIssued"] = true
		}
	}
}

// parseXBRLFromZip はZIP内のXBRLファイルを解析して財務データを抽出
func parseXBRLFromZip(zipReader *zip.Reader) (FinancialData, error) {
	var data FinancialData
	found := make(map[string]bool)

	for _, f := range zipReader.File {
		if !strings.HasSuffix(f.Name, ".xbrl") {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			continue
		}

		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			continue
		}

		contentStr := string(content)

		// 各タグパターンを検索
		for tagName, pattern := range xbrlTagPatterns {
			baseName := getBaseTagName(tagName)

			// 既にベースタグで取得済みならスキップ
			if found[baseName] {
				continue
			}

			matches := pattern.FindStringSubmatch(contentStr)
			if len(matches) >= 2 {
				value, _ := strconv.ParseInt(matches[1], 10, 64)
				// 売上・資産系はプラスのみ、利益系はマイナスも許容
				isProfit := baseName == "OperatingIncome" || baseName == "OrdinaryIncome" || baseName == "NetIncome"
				if value > 0 || (isProfit && value != 0) {
					applyXBRLValue(&data, found, baseName, value)
				}
			}
		}
	}

	// 何かデータが取れたかチェック（1つでもあればOK）
	if data.NetSales == 0 && data.TotalAssets == 0 && data.NetAssets == 0 &&
		data.NetIncome == 0 && data.OperatingIncome == 0 && data.SharesIssued == 0 {
		return data, fmt.Errorf("no financial data found in XBRL")
	}

	fmt.Printf("    📊 抽出: 売上=%d, 営業利益=%d, 純利益=%d, 総資産=%d, 純資産=%d, 株式数=%d\n",
		data.NetSales, data.OperatingIncome, data.NetIncome, data.TotalAssets, data.NetAssets, data.SharesIssued)

	return data, nil
}

// テスト用関数
func testLocalParse() {
	db, err := initXbrlDB()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	targetFile := "./data/S100WYZE/XBRL/PublicDoc/jpcrp040300-ssr-001_E02144-000_2025-09-30_01_2025-11-13.xbrl"

	fmt.Println("🚀 Starting local XBRL parse test...")

	data, err := parseLocalFile(targetFile)
	if err != nil {
		log.Fatalf("❌ Parse failed: %v", err)
	}

	fmt.Printf("💰 Extracted Data:\n")
	fmt.Printf("    売上高: %d\n", data.NetSales)
	fmt.Printf("    営業利益: %d\n", data.OperatingIncome)
	fmt.Printf("    純利益: %d\n", data.NetIncome)
	fmt.Printf("    総資産: %d\n", data.TotalAssets)
	fmt.Printf("    純資産: %d\n", data.NetAssets)
	fmt.Printf("    流動資産: %d\n", data.CurrentAssets)
	fmt.Printf("    負債: %d\n", data.Liabilities)

	// DBに保存
	err = saveStock(db, "7203", "トヨタ自動車（TEST）", "2025-11-13", data)
	if err != nil {
		log.Fatalf("❌ DB update failed: %v", err)
	}

	fmt.Println("✅ Success! Check your dashboard.")
}

// ローカルのXBRLファイルを解析する
func parseLocalFile(filePath string) (FinancialData, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return FinancialData{}, err
	}

	var data FinancialData
	contentStr := string(content)
	found := make(map[string]bool)

	for tagName, pattern := range xbrlTagPatterns {
		baseName := getBaseTagName(tagName)

		// 既にベースタグで取得済みならスキップ
		if found[baseName] {
			continue
		}

		matches := pattern.FindStringSubmatch(contentStr)
		if len(matches) >= 2 {
			value, _ := strconv.ParseInt(matches[1], 10, 64)
			isProfit := baseName == "OperatingIncome" || baseName == "OrdinaryIncome" || baseName == "NetIncome"
			if value > 0 || (isProfit && value != 0) {
				applyXBRLValue(&data, found, baseName, value)
			}
		}
	}

	return data, nil
}

// extractValue は後方互換性のために残す
func extractValue(line string) string {
	re := regexp.MustCompile(`>(\d+)</`)
	match := re.FindStringSubmatch(line)
	if len(match) > 1 {
		return match[1]
	}
	return ""
}

// fetchStockPrices はStooqから株価データを取得してDBに保存する
func fetchStockPrices() {
	// 銘柄一覧はxbrl.dbから取得
	xbrlDB, err := initXbrlDB()
	if err != nil {
		log.Fatalf("xbrl.db初期化失敗: %v", err)
	}
	defer xbrlDB.Close()

	// 株価はstock_price.dbに保存
	priceDB, err := initPriceDB()
	if err != nil {
		log.Fatalf("stock_price.db初期化失敗: %v", err)
	}
	defer priceDB.Close()

	// xbrl.dbから証券コード一覧を取得
	rows, err := xbrlDB.Query("SELECT code FROM stocks ORDER BY code")
	if err != nil {
		log.Fatalf("銘柄コード取得失敗: %v", err)
	}
	defer rows.Close()

	var codes []string
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err == nil {
			codes = append(codes, code)
		}
	}

	fmt.Printf("📈 Fetching stock prices for %d stocks...\n", len(codes))

	successCount := 0
	errorCount := 0

	for i, code := range codes {
		prices, err := fetchPricesFromStooq(code)
		if err != nil {
			fmt.Printf("  ❌ %s: %v\n", code, err)
			errorCount++
			continue
		}

		// DBに保存
		savedCount, err := savePricesToDB(priceDB, code, prices)
		if err != nil {
			fmt.Printf("  ❌ %s: DB保存失敗 %v\n", code, err)
			errorCount++
			continue
		}

		if savedCount > 0 {
			fmt.Printf("  ✅ [%d/%d] %s: %d件保存\n", i+1, len(codes), code, savedCount)
			successCount++
		} else {
			fmt.Printf("  ⏭️ [%d/%d] %s: 新規データなし\n", i+1, len(codes), code)
		}

		// レート制限対策（1秒待機）
		time.Sleep(1 * time.Second)
	}

	fmt.Printf("\n📊 完了: 成功 %d, エラー %d\n", successCount, errorCount)
}

// fetchPricesFromStooq はStooqから株価を取得
func fetchPricesFromStooq(code string) ([]StockPrice, error) {
	// 証券コードの調整（4桁なら.jpを付ける）
	stooqCode := code
	if len(code) == 4 {
		stooqCode = code + ".jp"
	}

	url := fmt.Sprintf("https://stooq.com/q/d/l/?s=%s&i=d", stooqCode)

	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("HTTP error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read error: %w", err)
	}

	lines := strings.Split(string(body), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("no data returned")
	}

	// ヘッダー確認
	header := strings.TrimSpace(lines[0])
	if !strings.Contains(header, "Date") {
		return nil, fmt.Errorf("invalid format: %s", header)
	}

	var prices []StockPrice
	oneYearAgo := time.Now().AddDate(-1, 0, 0).Format("2006-01-02")

	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Split(line, ",")
		if len(fields) < 6 {
			continue
		}

		// 日付をチェック（1年以内のデータのみ）
		date := fields[0]
		if date < oneYearAgo {
			continue
		}

		open, _ := strconv.ParseFloat(fields[1], 64)
		high, _ := strconv.ParseFloat(fields[2], 64)
		low, _ := strconv.ParseFloat(fields[3], 64)
		closePrice, _ := strconv.ParseFloat(fields[4], 64)
		volume, _ := strconv.ParseInt(fields[5], 10, 64)

		prices = append(prices, StockPrice{
			Code:   code,
			Date:   date,
			Open:   open,
			High:   high,
			Low:    low,
			Close:  closePrice,
			Volume: volume,
		})
	}

	return prices, nil
}

// savePricesToDB は株価をDBに保存（UPSERT）
func savePricesToDB(db *sql.DB, code string, prices []StockPrice) (int, error) {
	stmt, err := db.Prepare(`
		INSERT OR REPLACE INTO stock_prices (code, date, open, high, low, close, volume)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	count := 0
	for _, p := range prices {
		_, err := stmt.Exec(code, p.Date, p.Open, p.High, p.Low, p.Close, p.Volume)
		if err == nil {
			count++
		}
	}

	return count, nil
}
