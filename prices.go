package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

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

	// 最近取得済みの銘柄は差分スキップ対象（3日以内の株価がある銘柄）
	recentCutoff := time.Now().AddDate(0, 0, -3).Format("2006-01-02")
	recentRows, _ := priceDB.Query(`SELECT DISTINCT code FROM stock_prices WHERE date >= ?`, recentCutoff)
	recentMap := make(map[string]bool)
	if recentRows != nil {
		for recentRows.Next() {
			var code string
			if recentRows.Scan(&code) == nil {
				recentMap[code] = true
			}
		}
		recentRows.Close()
	}

	fmt.Printf("📈 Fetching stock prices for %d stocks (skipping %d recent)...\n", len(codes), len(recentMap))

	successCount := 0
	errorCount := 0
	skippedCount := 0
	consecutiveBlocks := 0 // Stooq 側のブロック検出カウンタ
	retryAttempted := false

	isBlockError := func(err error) bool {
		msg := err.Error()
		return strings.Contains(msg, "apikey") || strings.Contains(msg, "Get your")
	}

	for i, code := range codes {
		// 最近取得済みの銘柄はスキップ（再実行時の差分更新）
		if recentMap[code] {
			skippedCount++
			continue
		}

		prices, err := fetchPricesFromStooq(code)
		// Stooqがブロック・空データを返した場合、Yahoo Finance にフォールバック
		if err != nil || len(prices) == 0 {
			yahooPrices, yerr := fetchPricesFromYahoo(code)
			if yerr == nil && len(yahooPrices) > 0 {
				fmt.Printf("  🔁 %s: Stooq失敗→Yahoo成功 ", code)
				prices = yahooPrices
				err = nil
				consecutiveBlocks = 0
			}
		}
		if err != nil {
			fmt.Printf("  ❌ %s: %v\n", code, err)
			errorCount++

			if isBlockError(err) {
				consecutiveBlocks++
				if consecutiveBlocks >= 20 {
					if !retryAttempted {
						fmt.Printf("\n⏸️ Stooq のブロック検出（%d回連続）。60秒待機して再開を試みます...\n", consecutiveBlocks)
						time.Sleep(60 * time.Second)
						retryAttempted = true
						consecutiveBlocks = 0
						continue
					}
					fmt.Printf("\n⚠️ リトライ後も Stooq+Yahoo がブロックを継続しています\n")
					fmt.Printf("   株価取得を中止します。既存の stock_price.db は保持されます。\n")
					fmt.Printf("   しばらく時間をおいてから再実行してください（差分スキップで続きから再開します）。\n")
					break
				}
			} else {
				consecutiveBlocks = 0
			}
			continue
		}
		consecutiveBlocks = 0

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

	fmt.Printf("\n📊 完了: 成功 %d, エラー %d, スキップ %d\n", successCount, errorCount, skippedCount)
}

// fetchPricesFromStooq はStooqから株価を取得
func fetchPricesFromStooq(code string) ([]StockPrice, error) {
	// 証券コードの調整（4桁なら.jpを付ける）
	stooqCode := code
	if len(code) == 4 {
		stooqCode = code + ".jp"
	}

	url := fmt.Sprintf("https://stooq.com/q/d/l/?s=%s&i=d", stooqCode)

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("request error: %w", err)
	}
	// ボット判定回避のためブラウザ風ヘッダを付加
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/csv,text/plain,*/*")
	req.Header.Set("Referer", "https://stooq.com/")

	resp, err := client.Do(req)
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

// fetchPricesFromYahoo は Yahoo Finance から株価を取得（Stooqフォールバック用）
// 4桁証券コード + .T で東証銘柄を指定。直近1年分の日足を返す
func fetchPricesFromYahoo(code string) ([]StockPrice, error) {
	if len(code) != 4 {
		return nil, fmt.Errorf("yahoo: requires 4-digit code, got %s", code)
	}
	yahooSym := code + ".T"
	end := time.Now().Unix()
	start := end - 365*86400 // 1年前

	url := fmt.Sprintf("https://query1.finance.yahoo.com/v8/finance/chart/%s?period1=%d&period2=%d&interval=1d&events=history",
		yahooSym, start, end)

	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("request error: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json,text/javascript,*/*")

	resp, err := client.Do(req)
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

	var data struct {
		Chart struct {
			Result []struct {
				Timestamp  []int64 `json:"timestamp"`
				Indicators struct {
					Quote []struct {
						Open   []float64 `json:"open"`
						High   []float64 `json:"high"`
						Low    []float64 `json:"low"`
						Close  []float64 `json:"close"`
						Volume []int64   `json:"volume"`
					} `json:"quote"`
				} `json:"indicators"`
			} `json:"result"`
			Error any `json:"error"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("json parse: %w", err)
	}
	if data.Chart.Error != nil {
		return nil, fmt.Errorf("yahoo error: %v", data.Chart.Error)
	}
	if len(data.Chart.Result) == 0 || len(data.Chart.Result[0].Indicators.Quote) == 0 {
		return nil, fmt.Errorf("no data")
	}

	r := data.Chart.Result[0]
	q := r.Indicators.Quote[0]

	var prices []StockPrice
	for i, ts := range r.Timestamp {
		if i >= len(q.Close) {
			break
		}
		// null値（取引なし）はスキップ
		if q.Close[i] == 0 {
			continue
		}
		prices = append(prices, StockPrice{
			Code:   code,
			Date:   time.Unix(ts, 0).Format("2006-01-02"),
			Open:   q.Open[i],
			High:   q.High[i],
			Low:    q.Low[i],
			Close:  q.Close[i],
			Volume: q.Volume[i],
		})
	}

	if len(prices) == 0 {
		return nil, fmt.Errorf("no valid prices")
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

// calculateRS はリラティブストレングス(RS)を計算してrs.dbに保存する
// RS = 各銘柄の株価パフォーマンスを全銘柄と比較したパーセンタイルランク(1-99)
// パフォーマンス = 3ヶ月騰落率×40% + 6ヶ月×20% + 9ヶ月×20% + 12ヶ月×20%
func calculateRS() {
	fmt.Println("📊 Calculating Relative Strength (RS)...")

	// 株価DBを開く
	priceDB, err := sql.Open("sqlite", "./data/stock_price.db")
	if err != nil {
		log.Fatalf("stock_price.db open failed: %v", err)
	}
	defer priceDB.Close()

	// RS DBを初期化
	rsDB, err := initRsDB()
	if err != nil {
		log.Fatalf("rs.db init failed: %v", err)
	}
	defer rsDB.Close()

	// 株価データのある全銘柄を取得
	codeRows, err := priceDB.Query(`SELECT DISTINCT code FROM stock_prices ORDER BY code`)
	if err != nil {
		log.Fatalf("Failed to get codes: %v", err)
	}
	defer codeRows.Close()

	var codes []string
	for codeRows.Next() {
		var code string
		codeRows.Scan(&code)
		codes = append(codes, code)
	}

	fmt.Printf("  📈 %d stocks with price data\n", len(codes))

	// 基準日（最新の取引日）を取得
	var baseDate string
	priceDB.QueryRow(`SELECT MAX(date) FROM stock_prices`).Scan(&baseDate)
	fmt.Printf("  📅 Base date: %s\n", baseDate)

	baseDateParsed, err := time.Parse("2006-01-02", baseDate)
	if err != nil {
		log.Fatalf("Failed to parse base date: %v", err)
	}

	// 各期間の開始日を計算
	date3m := baseDateParsed.AddDate(0, -3, 0).Format("2006-01-02")
	date6m := baseDateParsed.AddDate(0, -6, 0).Format("2006-01-02")
	date9m := baseDateParsed.AddDate(0, -9, 0).Format("2006-01-02")
	date12m := baseDateParsed.AddDate(-1, 0, 0).Format("2006-01-02")

	type stockPerf struct {
		Code  string
		Score float64 // 加重パフォーマンススコア
	}

	var performances []stockPerf
	skippedCount := 0

	for _, code := range codes {
		// 各期間の始値と基準日の終値を取得
		var latestClose float64
		err := priceDB.QueryRow(`SELECT close FROM stock_prices WHERE code = ? AND date = ?`, code, baseDate).Scan(&latestClose)
		if err != nil || latestClose <= 0 {
			skippedCount++
			continue
		}

		// 各期間のclose価格を取得（その日付以降で最も近い日）
		getClose := func(targetDate string) float64 {
			var price float64
			priceDB.QueryRow(`
				SELECT close FROM stock_prices
				WHERE code = ? AND date >= ?
				ORDER BY date ASC LIMIT 1`, code, targetDate).Scan(&price)
			return price
		}

		close3m := getClose(date3m)
		close6m := getClose(date6m)
		close9m := getClose(date9m)
		close12m := getClose(date12m)

		// 騰落率を計算（データがない期間は0%として扱う）
		var score float64
		var validPeriods int

		if close3m > 0 {
			score += (latestClose/close3m - 1) * 0.4 * 100
			validPeriods++
		}
		if close6m > 0 {
			score += (latestClose/close6m - 1) * 0.2 * 100
			validPeriods++
		}
		if close9m > 0 {
			score += (latestClose/close9m - 1) * 0.2 * 100
			validPeriods++
		}
		if close12m > 0 {
			score += (latestClose/close12m - 1) * 0.2 * 100
			validPeriods++
		}

		// 最低2期間以上のデータが必要
		if validPeriods < 2 {
			skippedCount++
			continue
		}

		performances = append(performances, stockPerf{Code: code, Score: score})
	}

	fmt.Printf("  📊 Calculated performance for %d stocks (skipped %d)\n", len(performances), skippedCount)

	if len(performances) == 0 {
		fmt.Println("⚠️ No performance data to rank")
		return
	}

	// パフォーマンスでソート（昇順）
	sort.Slice(performances, func(i, j int) bool {
		return performances[i].Score < performances[j].Score
	})

	// パーセンタイルランクを計算（1-99）
	n := len(performances)
	calcRank := func(i int) int {
		rank := int(float64(i+1)/float64(n)*98) + 1
		if rank > 99 {
			rank = 99
		}
		return rank
	}

	stmt, err := rsDB.Prepare(`
		INSERT OR REPLACE INTO rs_scores (code, date, rs_score, rs_rank)
		VALUES (?, ?, ?, ?)
	`)
	if err != nil {
		log.Fatalf("Failed to prepare RS insert: %v", err)
	}
	defer stmt.Close()

	savedCount := 0
	for i, perf := range performances {
		rank := calcRank(i)
		_, err := stmt.Exec(perf.Code, baseDate, perf.Score, rank)
		if err == nil {
			savedCount++
		}
	}

	// トップ10を表示
	fmt.Println("\n🏆 Top 10 RS Stocks:")
	for i := n - 1; i >= 0 && i >= n-10; i-- {
		perf := performances[i]
		fmt.Printf("  RS=%2d  %s  (score: %.1f%%)\n", calcRank(i), perf.Code, perf.Score)
	}

	fmt.Printf("\n✅ RS calculation complete! Saved %d scores to rs.db\n", savedCount)
}
