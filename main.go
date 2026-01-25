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

func main() {
	mode := flag.String("mode", "run", "execution mode: run (fetch data), serve (web dashboard), or test-parse")
	dateFlag := flag.String("date", "2025-12-25", "target date for run mode (YYYY-MM-DD)")
	flag.Parse()

	switch *mode {
	case "test-parse":
		testLocalParse()
	case "run":
		runCollector(*dateFlag)
	case "serve":
		startServer()
	default:
		log.Fatalf("Unknown mode: %s", *mode)
	}
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

	db, err := initDB()
	if err != nil {
		log.Fatalf("Critical Error: Database init failed: %v", err)
	}
	defer db.Close()

	for _, doc := range edinetRes.Results {
		if doc.SecCode != "" {
			shortCode := doc.SecCode[:4]
			fmt.Printf("🔍 ターゲット捕捉: %s (%s) DocID: %s\n", doc.EntityName, shortCode, doc.DocID)
			fmt.Printf("🎯 Analyzing: %s (%s)\n", doc.EntityName, shortCode)

			// XBRLをダウンロードして解析
			data, err := downloadAndParseXBRL(doc.DocID)
			if err != nil {
				log.Printf("⚠️ Skip %s: %v", doc.EntityName, err)
				data = FinancialData{} // 空データで進める
			}

			// DBへ保存
			err = saveStock(db, shortCode, doc.EntityName, doc.SubmissionDate, data)
			if err != nil {
				log.Printf("⚠️ DB save failed for %s: %v", shortCode, err)
			}
		}
	}
	fmt.Println("🔥 All processes completed. Check your dashboard!")
}

// saveStock は銘柄データをDBに保存する
func saveStock(db *sql.DB, code, name, updatedAt string, data FinancialData) error {
	_, err := db.Exec(`
		INSERT OR REPLACE INTO stocks (
			code, name, updated_at,
			net_sales, operating_income, net_income,
			total_assets, net_assets, current_assets,
			liabilities, current_liabilities, cash_and_deposits, shares_issued
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		code, name, updatedAt,
		data.NetSales, data.OperatingIncome, data.NetIncome,
		data.TotalAssets, data.NetAssets, data.CurrentAssets,
		data.Liabilities, data.CurrentLiabilities, data.CashAndDeposits, data.SharesIssued,
	)
	return err
}

// --- 閲覧ロジック ---
func startServer() {
	// DBマイグレーション実行（新しいカラムを追加）
	migrateDB, err := initDB()
	if err != nil {
		log.Printf("⚠️ DB migration warning: %v", err)
	} else {
		migrateDB.Close()
		log.Println("✅ Database schema migrated successfully")
	}

	fs := http.FileServer(http.Dir("./web"))
	http.Handle("/", fs)

	http.HandleFunc("/stock_data.db", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-sqlite3")
		http.ServeFile(w, r, "./data/stock_data.db")
	})

	http.HandleFunc("/api/stocks", func(w http.ResponseWriter, r *http.Request) {
		db, err := sql.Open("sqlite", "./data/stock_data.db")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		rows, err := db.Query(`
			SELECT code, name, updated_at,
				   COALESCE(net_sales, 0), COALESCE(operating_income, 0), COALESCE(net_income, 0),
				   COALESCE(total_assets, 0), COALESCE(net_assets, 0), COALESCE(current_assets, 0),
				   COALESCE(liabilities, 0), COALESCE(current_liabilities, 0),
				   COALESCE(cash_and_deposits, 0), COALESCE(shares_issued, 0)
			FROM stocks ORDER BY code ASC`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var stocks []Stock
		for rows.Next() {
			var s Stock
			rows.Scan(&s.Code, &s.Name, &s.UpdatedAt,
				&s.NetSales, &s.OperatingIncome, &s.NetIncome,
				&s.TotalAssets, &s.NetAssets, &s.CurrentAssets,
				&s.Liabilities, &s.CurrentLiabilities,
				&s.CashAndDeposits, &s.SharesIssued)
			stocks = append(stocks, s)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(stocks)
	})

	fmt.Println("🌐 Dashboard starting at http://localhost:8080")
	fmt.Println("📂 Serving static files from ./web/")
	fmt.Println("📊 API endpoint: http://localhost:8080/api/stocks")
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

// DBの初期化（拡張されたスキーマ）
func initDB() (*sql.DB, error) {
	ensureDir()
	db, err := sql.Open("sqlite", "./data/stock_data.db")
	if err != nil {
		return nil, err
	}

	// 拡張されたスキーマ
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

	// 既存テーブルに新しいカラムがない場合は追加（マイグレーション）
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
		db.Exec(stmt) // エラーは無視（既にカラムがある場合）
	}

	return db, nil
}

// XBRLタグと対応するフィールドのマッピング
var xbrlTagPatterns = map[string]*regexp.Regexp{
	"NetSales":           regexp.MustCompile(`(jppfs_cor:NetSales|jpcrp_cor:NetSales|NetSales)[^>]*contextRef="[^"]*Duration[^"]*"[^>]*>(\d+)</`),
	"OperatingRevenues":  regexp.MustCompile(`(OperatingRevenues)[^>]*contextRef="[^"]*Duration[^"]*"[^>]*>(\d+)</`),
	"OperatingIncome":    regexp.MustCompile(`(jppfs_cor:OperatingIncome|OperatingIncome)[^>]*contextRef="[^"]*Duration[^"]*"[^>]*>(\d+)</`),
	"NetIncome":          regexp.MustCompile(`(jppfs_cor:ProfitLoss|ProfitLoss|NetIncome)[^>]*contextRef="[^"]*Duration[^"]*"[^>]*>(\d+)</`),
	"TotalAssets":        regexp.MustCompile(`(jppfs_cor:Assets|Assets|TotalAssets)[^>]*contextRef="[^"]*Instant[^"]*"[^>]*>(\d+)</`),
	"NetAssets":          regexp.MustCompile(`(jppfs_cor:NetAssets|NetAssets)[^>]*contextRef="[^"]*Instant[^"]*"[^>]*>(\d+)</`),
	"CurrentAssets":      regexp.MustCompile(`(jppfs_cor:CurrentAssets|CurrentAssets)[^>]*contextRef="[^"]*Instant[^"]*"[^>]*>(\d+)</`),
	"Liabilities":        regexp.MustCompile(`(jppfs_cor:Liabilities|Liabilities)[^>]*contextRef="[^"]*Instant[^"]*"[^>]*>(\d+)</`),
	"CurrentLiabilities": regexp.MustCompile(`(jppfs_cor:CurrentLiabilities|CurrentLiabilities)[^>]*contextRef="[^"]*Instant[^"]*"[^>]*>(\d+)</`),
	"CashAndDeposits":    regexp.MustCompile(`(jppfs_cor:CashAndDeposits|CashAndDeposits)[^>]*contextRef="[^"]*Instant[^"]*"[^>]*>(\d+)</`),
	"SharesIssued":       regexp.MustCompile(`(jpcrp_cor:TotalNumberOfIssuedSharesSummaryOfBusinessResults|NumberOfIssuedShares)[^>]*>(\d+)</`),
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
			if found[tagName] {
				continue
			}

			matches := pattern.FindStringSubmatch(contentStr)
			if len(matches) >= 3 {
				value, _ := strconv.ParseInt(matches[2], 10, 64)
				if value > 0 {
					switch tagName {
					case "NetSales", "OperatingRevenues":
						if data.NetSales == 0 {
							data.NetSales = value
							found["NetSales"] = true
						}
					case "OperatingIncome":
						data.OperatingIncome = value
						found[tagName] = true
					case "NetIncome":
						data.NetIncome = value
						found[tagName] = true
					case "TotalAssets":
						data.TotalAssets = value
						found[tagName] = true
					case "NetAssets":
						data.NetAssets = value
						found[tagName] = true
					case "CurrentAssets":
						data.CurrentAssets = value
						found[tagName] = true
					case "Liabilities":
						data.Liabilities = value
						found[tagName] = true
					case "CurrentLiabilities":
						data.CurrentLiabilities = value
						found[tagName] = true
					case "CashAndDeposits":
						data.CashAndDeposits = value
						found[tagName] = true
					case "SharesIssued":
						data.SharesIssued = value
						found[tagName] = true
					}
				}
			}
		}
	}

	// 何かデータが取れたかチェック
	if data.NetSales == 0 && data.TotalAssets == 0 && data.NetAssets == 0 {
		return data, fmt.Errorf("no financial data found in XBRL")
	}

	fmt.Printf("    📊 抽出: 売上=%d, 営業利益=%d, 純利益=%d, 総資産=%d, 純資産=%d\n",
		data.NetSales, data.OperatingIncome, data.NetIncome, data.TotalAssets, data.NetAssets)

	return data, nil
}

// テスト用関数
func testLocalParse() {
	db, err := initDB()
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

	for tagName, pattern := range xbrlTagPatterns {
		matches := pattern.FindStringSubmatch(contentStr)
		if len(matches) >= 3 {
			value, _ := strconv.ParseInt(matches[2], 10, 64)
			if value > 0 {
				switch tagName {
				case "NetSales", "OperatingRevenues":
					if data.NetSales == 0 {
						data.NetSales = value
					}
				case "OperatingIncome":
					data.OperatingIncome = value
				case "NetIncome":
					data.NetIncome = value
				case "TotalAssets":
					data.TotalAssets = value
				case "NetAssets":
					data.NetAssets = value
				case "CurrentAssets":
					data.CurrentAssets = value
				case "Liabilities":
					data.Liabilities = value
				case "CurrentLiabilities":
					data.CurrentLiabilities = value
				case "CashAndDeposits":
					data.CashAndDeposits = value
				case "SharesIssued":
					data.SharesIssued = value
				}
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
