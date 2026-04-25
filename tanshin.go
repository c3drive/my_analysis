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

	// 単位判定: 文書先頭(連結経営成績の前まで)で最初に見つかる単位を採用
	// 「(百万円)」と「(千円)」が混在する場合、上部の表の単位を採用
	multiplier := int64(1_000_000) // デフォルト百万円
	headerLen := len(text)
	if headerLen > 4000 {
		headerLen = 4000 // 先頭4000文字に制限 (連結経営成績の表ヘッダ範囲)
	}
	header := text[:headerLen]
	yenIdx := regexp.MustCompile(`\(百万円\)`).FindStringIndex(header)
	thouIdx := regexp.MustCompile(`\(千円\)`).FindStringIndex(header)
	if yenIdx != nil && thouIdx != nil {
		if thouIdx[0] < yenIdx[0] {
			multiplier = 1_000
		}
	} else if thouIdx != nil {
		multiplier = 1_000
	}

	// 主要項目の抽出ヘルパー
	// 行末まで or 改行までの数字を厳格にマッチ (隣接行混入を防ぐ)
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

	// 行頭の項目名 + 同一行内の最初の数値 を要求 (`[\s　]*` は半角/全角スペース)
	// 行頭アンカー (?m) でマルチライン対応
	d.NetSales = findFirst([]string{
		`(?m)^[\s　]*売上高[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
		`(?m)^[\s　]*営業収益[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
		`(?m)^[\s　]*売上収益[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
		`(?m)^[\s　]*経常収益[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
	}) * multiplier

	d.OperatingIncome = findFirst([]string{
		`(?m)^[\s　]*営業利益[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
		`(?m)^[\s　]*営業損失[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
	}) * multiplier

	d.NetIncome = findFirst([]string{
		`(?m)^[\s　]*親会社株主に帰属する当期純利益[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
		`(?m)^[\s　]*親会社の所有者に帰属する当期利益[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
		`(?m)^[\s　]*当期純利益[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
		`(?m)^[\s　]*四半期純利益[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
	}) * multiplier

	d.TotalAssets = findFirst([]string{
		`(?m)^[\s　]*総資産[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
		`(?m)^[\s　]*資産合計[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
	}) * multiplier

	d.NetAssets = findFirst([]string{
		`(?m)^[\s　]*純資産[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
		`(?m)^[\s　]*純資産合計[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
	}) * multiplier

	// 妥当性チェック: 異常値の除外
	// 1. 純利益の絶対値が売上の3倍を超える場合は誤抽出と判定 (純資産混入等)
	if d.NetSales > 0 && abs64(d.NetIncome) > d.NetSales*3 {
		d.NetIncome = 0
	}
	// 2. 営業利益の絶対値が売上の2倍を超える場合も同様
	if d.NetSales > 0 && abs64(d.OperatingIncome) > d.NetSales*2 {
		d.OperatingIncome = 0
	}
	// 3. 純資産が総資産より大きい場合は誤抽出
	if d.TotalAssets > 0 && d.NetAssets > d.TotalAssets {
		d.NetAssets = 0
	}

	return d
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
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
