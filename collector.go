package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"
)

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
		"InvestmentSecurities": 0, "Securities": 0, "AccountsReceivable": 0, "Inventories": 0,
		"NonCurrentLiabilities": 0, "ShareholdersEquity": 0,
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
		if data.InvestmentSecurities > 0 {
			fieldStats["InvestmentSecurities"]++
		}
		if data.Securities > 0 {
			fieldStats["Securities"]++
		}
		if data.AccountsReceivable > 0 {
			fieldStats["AccountsReceivable"]++
		}
		if data.Inventories > 0 {
			fieldStats["Inventories"]++
		}
		if data.NonCurrentLiabilities > 0 {
			fieldStats["NonCurrentLiabilities"]++
		}
		if data.ShareholdersEquity > 0 {
			fieldStats["ShareholdersEquity"]++
		}

		// DBへ保存（最新サマリ）
		err = saveStock(db, shortCode, doc.EntityName, doc.SubmissionDate, data)
		if err != nil {
			log.Printf("⚠️ DB save failed for %s: %v", shortCode, err)
			errorCount++
		} else {
			processedCount++
		}

		// 時系列テーブルにも保存（四半期・通期データ蓄積）
		if err := saveStockFinancial(db, shortCode, doc.DocTypeCode, doc.SubmissionDate, doc.DocDescription, data); err != nil {
			log.Printf("⚠️ Financials save failed for %s: %v", shortCode, err)
		}
	}

	// パース成功率レポート
	fmt.Printf("\n🔥 完了! 処理=%d件, スキップ=%d件, エラー=%d件, 空データ=%d件\n", processedCount, skippedCount, errorCount, emptyDataCount)
	if totalParsed > 0 {
		fmt.Println("📊 パース成功率:")
		for _, field := range []string{"NetSales", "OperatingIncome", "NetIncome", "TotalAssets", "NetAssets", "CurrentAssets", "Liabilities", "CurrentLiabilities", "CashAndDeposits", "SharesIssued", "InvestmentSecurities", "Securities", "AccountsReceivable", "Inventories", "NonCurrentLiabilities", "ShareholdersEquity"} {
			rate := float64(fieldStats[field]) / float64(totalParsed) * 100
			fmt.Printf("  %s: %d/%d (%.1f%%)\n", field, fieldStats[field], totalParsed, rate)
		}
	}
}
