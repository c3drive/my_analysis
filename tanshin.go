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
	var partialFailures []string // 部分失敗 (売上はあるが利益0など) の銘柄リスト

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

		// 既存 stocks テーブルの売上値と比較して妥当性チェック + 単位補正
		// EDINET XBRL 由来の値が「正」なので、決算短信の値とズレてたら単位誤判定の可能性
		var existingSales int64
		db.QueryRow("SELECT COALESCE(net_sales, 0) FROM stocks WHERE code = ?", t.code).Scan(&existingSales)
		if existingSales > 0 && data.NetSales > 0 {
			ratio := float64(data.NetSales) / float64(existingSales)

			// 比率がほぼ1000倍 (±10%) なら単位誤判定 → 1/1000 補正
			if ratio > 900 && ratio < 1100 {
				data.NetSales /= 1000
				data.OperatingIncome /= 1000
				data.NetIncome /= 1000
				data.TotalAssets /= 1000
				data.NetAssets /= 1000
				fmt.Printf("🔧 単位補正 (1/1000): 比率%.1f → ", ratio)
			}
			// 比率がほぼ 1/1000 (0.0009〜0.0011) なら逆方向の単位誤判定 → 1000倍補正
			if ratio < 0.0011 && ratio > 0.0009 {
				data.NetSales *= 1000
				data.OperatingIncome *= 1000
				data.NetIncome *= 1000
				data.TotalAssets *= 1000
				data.NetAssets *= 1000
				fmt.Printf("🔧 単位補正 (×1000): 比率%.4f → ", ratio)
			}

			// 補正後に再チェック
			ratio = float64(data.NetSales) / float64(existingSales)
			if ratio > 10 || ratio < 0.1 {
				fmt.Printf("⚠️ 売上妥当性NG (既存%d vs 抽出%d, 比率%.2f) → スキップ\n", existingSales, data.NetSales, ratio)
				failCount++
				continue
			}
		}

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

		// 部分失敗の検出 (売上はあるが純利益が 0)
		if data.NetSales > 0 && data.NetIncome == 0 {
			partialFailures = append(partialFailures, fmt.Sprintf("%s %s (純利益取得失敗)", t.code, t.name))
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

	// 部分失敗銘柄の表示 (デバッグ参考用)
	if len(partialFailures) > 0 {
		fmt.Printf("\n⚠️ 部分抽出失敗 %d件 (debug-tanshin で個別調査推奨):\n", len(partialFailures))
		for _, p := range partialFailures {
			fmt.Printf("  %s\n", p)
		}
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
		// 通常の決算短信
		`(?m)^[\s　]*売上高[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
		// 四半期短信
		`(?m)^[\s　]*(?:第[一二三四1234]四半期)?売上高[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
		// IFRS 適用企業
		`(?m)^[\s　]*売上収益[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
		// 鉄道、運輸、サービス業
		`(?m)^[\s　]*営業収益[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
		// 銀行・金融
		`(?m)^[\s　]*経常収益[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
		// 保険業
		`(?m)^[\s　]*経常収入[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
	}) * multiplier

	d.OperatingIncome = findFirst([]string{
		`(?m)^[\s　]*営業利益[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
		// 「営業利益又は営業損失（△）」「営業利益(△は損失)」等の表記
		`(?m)^[\s　]*営業利益(?:又は営業損失|\(△は損失\))?[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
		// 銀行: 経常利益が事業利益の代理
		`(?m)^[\s　]*経常利益[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
		// IFRS 営業利益
		`(?m)^[\s　]*事業利益[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
		`(?m)^[\s　]*営業損失[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
	}) * multiplier

	d.NetIncome = findFirst([]string{
		// 通期決算（最も一般的）
		`(?m)^[\s　]*親会社株主に帰属する当期純利益[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
		// 四半期決算
		`(?m)^[\s　]*親会社株主に帰属する四半期純利益[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
		// IFRS 通期
		`(?m)^[\s　]*親会社の所有者に帰属する当期利益[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
		// IFRS 四半期
		`(?m)^[\s　]*親会社の所有者に帰属する四半期利益[^\n]*?[\s　]([0-9,△▲\-]{2,})`,
		// 単独決算など
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
		// IFRS
		`(?m)^[\s　]*資本合計[^\n]*?[\s　]([0-9,△▲\-]{4,})`,
	}) * multiplier

	// 妥当性チェック: 異常値の除外
	// 上限: 日本最大企業の年間売上 (Toyota 約45兆円) を考慮し 50兆円を上限
	const maxSalesYen = 50_000_000_000_000

	// 1. 売上が負の値は誤抽出 (前年比△XX% を誤読した典型例)
	if d.NetSales < 0 {
		return FinancialData{}
	}
	// 2. 売上が 50兆円超は誤抽出 (PDF表崩れによる別項目混入)
	if d.NetSales > maxSalesYen {
		return FinancialData{}
	}
	// 3. 総資産が 1000兆円超 (実質メガバンク級でも 400兆円) も同様
	if d.TotalAssets > 1000_000_000_000_000 {
		return FinancialData{}
	}
	// 3. 純利益の絶対値が売上の3倍を超える場合は誤抽出 (純資産混入等)
	if d.NetSales > 0 && abs64(d.NetIncome) > d.NetSales*3 {
		d.NetIncome = 0
	}
	// 4. 営業利益の絶対値が売上の2倍を超える場合も同様
	if d.NetSales > 0 && abs64(d.OperatingIncome) > d.NetSales*2 {
		d.OperatingIncome = 0
	}
	// 5. 純資産が総資産より大きい場合は誤抽出
	if d.TotalAssets > 0 && d.NetAssets > d.TotalAssets {
		d.NetAssets = 0
	}

	return d
}

// debugTanshin は単一銘柄の決算短信PDFを取得し、テキスト抽出 + パース結果を表示する
// 正規表現の調整やトラブルシュート用
func debugTanshin(code, date string) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		log.Fatalf("pdftotext が必要です")
	}

	db, err := initXbrlDB()
	if err != nil {
		log.Fatalf("DB init: %v", err)
	}
	defer db.Close()

	var url, title string
	err = db.QueryRow(`
		SELECT pdf_url, title FROM tdnet_disclosures
		WHERE code = ? AND disclosure_datetime LIKE ? || '%' AND title LIKE '%決算短信%'
		ORDER BY disclosure_datetime DESC LIMIT 1`, code, date).Scan(&url, &title)
	if err != nil {
		log.Fatalf("該当する決算短信が tdnet_disclosures にありません: %v", err)
	}

	fmt.Printf("📄 %s %s\n   URL: %s\n", code, title, url)

	pdfPath, err := downloadPDF(url)
	if err != nil {
		log.Fatalf("DL失敗: %v", err)
	}
	defer os.Remove(pdfPath)

	text, err := extractPDFText(pdfPath)
	if err != nil {
		log.Fatalf("抽出失敗: %v", err)
	}

	// テキスト先頭4000文字を出力
	fmt.Println("\n--- 抽出テキスト (先頭4000文字) ---")
	limit := len(text)
	if limit > 4000 {
		limit = 4000
	}
	fmt.Println(text[:limit])

	fmt.Println("\n--- パース結果 ---")
	d := parseTanshinText(text)
	fmt.Printf("売上高:       %d 円 (%.2f 億円)\n", d.NetSales, float64(d.NetSales)/1e8)
	fmt.Printf("営業利益:     %d 円 (%.2f 億円)\n", d.OperatingIncome, float64(d.OperatingIncome)/1e8)
	fmt.Printf("純利益:       %d 円 (%.2f 億円)\n", d.NetIncome, float64(d.NetIncome)/1e8)
	fmt.Printf("総資産:       %d 円 (%.2f 億円)\n", d.TotalAssets, float64(d.TotalAssets)/1e8)
	fmt.Printf("純資産:       %d 円 (%.2f 億円)\n", d.NetAssets, float64(d.NetAssets)/1e8)
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
