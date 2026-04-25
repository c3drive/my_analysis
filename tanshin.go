package main

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// parseTanshinForDate は指定日のTDNET決算短信PDFを取得・パースしてstock_financialsに保存する
// 注意:
// - PDFは一時ファイルとして処理し、保存しない (著作権・容量考慮)
// - poppler-utils (pdftotext) コマンドが必要
// - 決算短信のフォーマット差異により抽出精度は完璧ではない
func parseTanshinForDate(targetDate string) {
	// pdftotext コマンドの存在確認
	if _, err := exec.LookPath("pdftotext"); err != nil {
		log.Fatalf("pdftotext が見つかりません。Dockerコンテナを再ビルドしてください: docker compose build")
	}

	db, err := initXbrlDB()
	if err != nil {
		log.Fatalf("DB init failed: %v", err)
	}
	defer db.Close()

	// 対象: 指定日に投稿された「決算短信」を含む開示
	rows, err := db.Query(`
		SELECT code, name, disclosure_datetime, title, pdf_url
		FROM tdnet_disclosures
		WHERE disclosure_datetime LIKE ? || '%'
		  AND title LIKE '%決算短信%'
		  AND pdf_url != ''`, targetDate)
	if err != nil {
		log.Fatalf("Query failed: %v (先に task fetch-tdnet を実行してください)", err)
	}
	defer rows.Close()

	type target struct {
		code, name, dt, title, url string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.code, &t.name, &t.dt, &t.title, &t.url); err == nil {
			targets = append(targets, t)
		}
	}

	if len(targets) == 0 {
		fmt.Printf("⚠️ %s に決算短信の開示はありません (先に task fetch-tdnet DATE=%s で取得が必要)\n", targetDate, targetDate)
		return
	}

	fmt.Printf("📄 %s の決算短信 %d件をパース...\n", targetDate, len(targets))

	successCount := 0
	failCount := 0
	parseStats := map[string]int{
		"NetSales": 0, "OperatingIncome": 0, "NetIncome": 0, "TotalAssets": 0, "NetAssets": 0,
	}

	for i, t := range targets {
		fmt.Printf("  [%d/%d] %s %s ... ", i+1, len(targets), t.code, t.name)

		pdfPath, err := downloadPDF(t.url)
		if err != nil {
			fmt.Printf("DL失敗: %v\n", err)
			failCount++
			continue
		}

		text, err := extractPDFText(pdfPath)
		os.Remove(pdfPath) // 一時ファイル削除
		if err != nil {
			fmt.Printf("テキスト抽出失敗: %v\n", err)
			failCount++
			continue
		}

		data := parseTanshinText(text)

		// 抽出統計
		if data.NetSales > 0 {
			parseStats["NetSales"]++
		}
		if data.OperatingIncome != 0 {
			parseStats["OperatingIncome"]++
		}
		if data.NetIncome != 0 {
			parseStats["NetIncome"]++
		}
		if data.TotalAssets > 0 {
			parseStats["TotalAssets"]++
		}
		if data.NetAssets > 0 {
			parseStats["NetAssets"]++
		}

		// 何も取れなかったらスキップ
		if data.NetSales == 0 && data.NetIncome == 0 && data.OperatingIncome == 0 {
			fmt.Println("抽出データなし")
			failCount++
			continue
		}

		// stock_financials に保存 (doc_type=SHORT_REPORT で区別)
		err = saveStockFinancial(db, t.code, "SHORT_REPORT", t.dt, t.title, data)
		if err != nil {
			fmt.Printf("DB保存失敗: %v\n", err)
			failCount++
			continue
		}

		fmt.Printf("✅ 売上=%d 営利=%d 純利=%d\n", data.NetSales, data.OperatingIncome, data.NetIncome)
		successCount++

		// レート制限対策
		time.Sleep(500 * time.Millisecond)
	}

	fmt.Printf("\n✅ 完了: 成功=%d, 失敗=%d\n", successCount, failCount)
	fmt.Println("📊 抽出成功率:")
	for _, key := range []string{"NetSales", "OperatingIncome", "NetIncome", "TotalAssets", "NetAssets"} {
		rate := float64(parseStats[key]) / float64(len(targets)) * 100
		fmt.Printf("  %s: %d/%d (%.1f%%)\n", key, parseStats[key], len(targets), rate)
	}
}

// downloadPDF は URL から PDF をダウンロードして一時ファイルに保存し、パスを返す
func downloadPDF(url string) (string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("HTTP error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP status: %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "tdnet-*.pdf")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(tmp, resp.Body)
	tmp.Close()
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// extractPDFText は pdftotext コマンドで PDF からテキストを抽出する
func extractPDFText(pdfPath string) (string, error) {
	cmd := exec.Command("pdftotext", "-layout", "-enc", "UTF-8", pdfPath, "-")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseTanshinText は決算短信のテキストから財務データを抽出する
// 注意: 決算短信のフォーマットは企業・業種により異なるため、ベストエフォート
func parseTanshinText(text string) FinancialData {
	var d FinancialData

	// 単位判定 (「百万円」「千円」)
	multiplier := int64(1_000_000) // デフォルト百万円
	if regexp.MustCompile(`\(千円\)`).MatchString(text) {
		multiplier = 1_000
	}

	// 主要項目の抽出ヘルパー
	findFirst := func(patterns []string) int64 {
		for _, p := range patterns {
			rx := regexp.MustCompile(p)
			m := rx.FindStringSubmatch(text)
			if len(m) >= 2 {
				v := parseJPNumber(m[1])
				if v != 0 {
					return v
				}
			}
		}
		return 0
	}

	// 売上高 / 営業収益 / 売上収益(IFRS)
	d.NetSales = findFirst([]string{
		`売上高\s*[*※()（）連結個別単独]*\s*([0-9,△▲\-]+)`,
		`営業収益\s*[*※]*\s*([0-9,△▲\-]+)`,
		`売上収益\s*[*※]*\s*([0-9,△▲\-]+)`,
	}) * multiplier

	// 営業利益
	d.OperatingIncome = findFirst([]string{
		`営業利益\s*[*※]*\s*([0-9,△▲\-]+)`,
		`営業損失\s*[*※]*\s*([0-9,△▲\-]+)`,
	}) * multiplier

	// 経常利益 (使ってないがログ用に取っておく場合は別途)

	// 親会社株主に帰属する当期純利益 (連結) / 当期純利益 (単体)
	d.NetIncome = findFirst([]string{
		`親会社株主に帰属する当期純利益\s*[*※]*\s*([0-9,△▲\-]+)`,
		`当期純利益\s*[*※]*\s*([0-9,△▲\-]+)`,
	}) * multiplier

	// 総資産 / 純資産 (BS は四半期決算短信では「期末純資産」「総資産」表記)
	d.TotalAssets = findFirst([]string{
		`総資産\s*[*※]*\s*([0-9,△▲\-]+)`,
		`資産合計\s*[*※]*\s*([0-9,△▲\-]+)`,
	}) * multiplier

	d.NetAssets = findFirst([]string{
		`純資産\s*[*※]*\s*([0-9,△▲\-]+)`,
		`純資産合計\s*[*※]*\s*([0-9,△▲\-]+)`,
	}) * multiplier

	return d
}

// parseJPNumber は "1,234" "△500" "▲1,000" 等を int64 に変換
func parseJPNumber(s string) int64 {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")
	sign := int64(1)
	if strings.HasPrefix(s, "△") || strings.HasPrefix(s, "▲") || strings.HasPrefix(s, "-") {
		sign = -1
		s = strings.TrimLeft(s, "△▲-")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n * sign
}

// SQL ATTACH database for stock_financials_tdnet view (placeholder for compile)
var _ = sql.ErrNoRows
