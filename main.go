package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
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

type Stock struct {
	Code      string
	Name      string
	NetSales  int64 // 売上高
	UpdatedAt string
}

func main() {
	// モード切り替え用の引数を追加
	mode := flag.String("mode", "run", "execution mode: run (fetch data), serve (web dashboard), or test-parse")
	dateFlag := flag.String("date", "2025-12-25", "target date for run mode (YYYY-MM-DD)")
	flag.Parse()

	switch *mode {
	case "test-parse":
		// ★ ここで手元の「本物」を解析する
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

	// 1. データソースの切り替えと取得
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

	// 2. JSONデコード
	var edinetRes EdinetResponse
	if err := json.Unmarshal(body, &edinetRes); err != nil {
		log.Fatalf("Critical Error: Failed to parse JSON: %v\nRaw Body: %s", err, string(body))
	}

	// 3. DB処理
	if err := saveToDatabase(edinetRes); err != nil {
		log.Fatalf("Critical Error: Database operation failed: %v", err)
	}

	db, _ := initDB()
	defer db.Close()

	for _, doc := range edinetRes.Results {
		if doc.SecCode != "" {
			shortCode := doc.SecCode[:4]
			fmt.Printf("🔍 ターゲット捕捉: %s (%s) DocID: %s\n", doc.EntityName, shortCode, doc.DocID)
			fmt.Printf("🎯 Analyzing: %s (%s)\n", doc.EntityName, shortCode)

			// 1. ZIPをダウンロードして解析する（自作した関数を呼び出す）
			amount, err := downloadAndParseXBRL(doc.DocID) // 関数名を合わせたぞ
			if err != nil {
				log.Printf("⚠️ Skip %s: %v", doc.EntityName, err)
				amount = 0 // 取れなかった場合は 0 で進める
			}

			// 2. DBへ保存
			_, err = db.Exec(`INSERT OR REPLACE INTO stocks (code, name, updated_at, net_sales) 
                             VALUES (?, ?, ?, ?)`,
				shortCode, doc.EntityName, doc.SubmissionDate, amount)
		}
	}
	fmt.Println("🔥 All processes completed. Check your dashboard!")
}

// --- 閲覧ロジック ---
func startServer() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		db, _ := sql.Open("sqlite", "./data/stock_data.db")
		defer db.Close()

		// net_sales も取得するようにSQLを変更
		rows, err := db.Query("SELECT code, name, updated_at, net_sales FROM stocks ORDER BY code ASC")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var stocks []Stock
		for rows.Next() {
			var s Stock
			// Scanの引数に &s.NetSales を追加
			rows.Scan(&s.Code, &s.Name, &s.UpdatedAt, &s.NetSales)
			stocks = append(stocks, s)
		}

		tmpl := `
		<!DOCTYPE html>
		<html>
		<head>
			<title>Stock Dashboard</title>
			<style>table { width: 100%; border-collapse: collapse; } th, td { padding: 8px; text-align: left; border: 1px solid #ddd; }</style>
		</head>
		<body>
			<h1>Stock Analysis Dashboard</h1>
			<table>
				<tr><th>Code</th><th>Name</th><th>Net Sales (Yen)</th><th>Updated At</th></tr>
				{{range .}}
				<tr>
					<td>{{.Code}}</td>
					<td>{{.Name}}</td>
					<td>{{.NetSales}}</td>
					<td>{{.UpdatedAt}}</td>
				</tr>
				{{end}}
			</table>
		</body>
		</html>`
		t := template.Must(template.New("web").Parse(tmpl))
		t.Execute(w, stocks)
	})

	fmt.Println("🌐 Dashboard starting at http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// fetchFromAPI はHTTPリクエストを処理し、エラーがあれば詳細を返す
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

// saveToDatabase はトランザクションを管理し、SQLiteへ保存する
func saveToDatabase(res EdinetResponse) error {
	db, err := sql.Open("sqlite", "./data/stock_data.db")
	if err != nil {
		return fmt.Errorf("db open error: %w", err)
	}
	defer db.Close()

	// 初期設定
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS stocks (
		code TEXT PRIMARY KEY, 
		name TEXT, 
		updated_at DATETIME
	);`)
	if err != nil {
		return fmt.Errorf("table creation error: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("transaction begin error: %w", err)
	}
	// エラー時にロールバックを保証
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO stocks (code, name, updated_at) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("statement preparation error: %w", err)
	}
	defer stmt.Close()

	count := 0
	for _, doc := range res.Results {
		if doc.SecCode != "" {
			// 下1桁を除去して4桁の証券コードにする処理
			shortCode := doc.SecCode[:4]
			if _, err := stmt.Exec(shortCode, doc.EntityName, doc.SubmissionDate); err != nil {
				log.Printf("Warning: Failed to insert code %s: %v", shortCode, err)
				continue
			}
			count++
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("transaction commit error: %w", err)
	}

	fmt.Printf("✅ Successfully processed %d records to SQLite.\n", count)
	return nil
}

// XBRL解析用の関数（まずはダウンロードの準備）
func downloadAndParseXBRL(docID string) (int64, error) {
	apiKey := os.Getenv("EDINET_API_KEY")
	if apiKey == "" {
		return 5000000000, nil // モック用
	}

	url := fmt.Sprintf("https://api.edinet-fsa.go.jp/api/v2/documents/%s?type=1", docID)

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Ocp-Apim-Subscription-Key", apiKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("API error: %d", resp.StatusCode)
	}

	// 1. ZIP全体をメモリに読み込む
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	// 2. メモリ上のバイナリを ZIP として開く
	zipReader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return 0, err
	}

	targetPattern := regexp.MustCompile(`(OperatingRevenues|NetSales).+contextRef="InterimDuration"`)

	// 3. ZIP内のファイルを走査
	for _, f := range zipReader.File {
		if strings.HasSuffix(f.Name, ".xbrl") {
			rc, err := f.Open()
			if err != nil {
				continue
			}

			scanner := bufio.NewScanner(rc)
			for scanner.Scan() {
				line := scanner.Text()
				if targetPattern.MatchString(line) {
					valStr := extractValue(line)
					if valStr != "" {
						amount, _ := strconv.ParseInt(valStr, 10, 64)
						rc.Close()
						return amount, nil // 数値が見つかれば即座に返す
					}
				}
			}
			rc.Close()
		}
	}

	return 0, fmt.Errorf("target financial data not found in ZIP")
}

// テスト用関数を末尾に追加しろ
func testLocalParse() {
	db, err := initDB()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// お前が ls で確認したパスを正確に指定しろ
	// コンテナから見えるパス（./data/S100WYZE/...）にする必要がある
	targetFile := "./data/S100WYZE/XBRL/PublicDoc/jpcrp040300-ssr-001_E02144-000_2025-09-30_01_2025-11-13.xbrl"

	fmt.Println("🚀 Starting local XBRL parse test...")

	// 前に作った解析ロジックを呼び出す（関数名は適宜合わせろ）
	// もし関数がなければ、ここで直接 extractValue を呼び出すループを書け
	amount, err := parseLocalFile(targetFile)
	if err != nil {
		log.Fatalf("❌ Parse failed: %v", err)
	}

	fmt.Printf("💰 Extracted Amount: %d\n", amount)

	// DBに「テストデータ」として保存
	_, err = db.Exec("INSERT OR REPLACE INTO stocks (code, name, updated_at, net_sales) VALUES (?, ?, ?, ?)",
		"7203", "トヨタ自動車（TEST）", "2025-11-13", amount)
	if err != nil {
		log.Fatalf("❌ DB update failed: %v", err)
	}

	fmt.Println("✅ Success! Check your dashboard.")
}

// DBの初期化（テーブルがなければ作る）
func initDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite", "./data/stock_data.db")
	if err != nil {
		return nil, err
	}

	// 確実に net_sales カラムを含んだ stocks テーブルを作成する
	sqlStmt := `
	CREATE TABLE IF NOT EXISTS stocks (
		code TEXT PRIMARY KEY, 
		name TEXT, 
		updated_at DATETIME,
		net_sales INTEGER
	);`

	if _, err = db.Exec(sqlStmt); err != nil {
		return nil, fmt.Errorf("テーブル作成失敗: %w", err)
	}
	return db, nil
}

// ローカルのXBRLファイルを解析する
func parseLocalFile(filePath string) (int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	// トヨタの結果に基づいた「営業収益」または「売上高」を狙う正規表現
	// contextRef="InterimDuration"（今期累計）を条件にするのがコツだ
	targetPattern := regexp.MustCompile(`(OperatingRevenues|NetSales).+contextRef="InterimDuration"`)
	valuePattern := regexp.MustCompile(`>(\d+)</`)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if targetPattern.MatchString(line) {
			match := valuePattern.FindStringSubmatch(line)
			if len(match) > 1 {
				val, _ := strconv.ParseInt(match[1], 10, 64)
				return val, nil
			}
		}
	}

	return 0, fmt.Errorf("target tag not found in XBRL")
}

// 抽出用のスナイパー関数。タグに囲まれた数字だけを抜き出す。
func extractValue(line string) string {
	// <タグ名 ...>数字</タグ名> の構造から数字だけを抽出する
	re := regexp.MustCompile(`>(\d+)</`)
	match := re.FindStringSubmatch(line)
	if len(match) > 1 {
		return match[1]
	}
	return ""
}
