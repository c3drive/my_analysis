//go:build ignore
// +build ignore

package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
)

type EdinetDoc struct {
	DocID          string `json:"docID"`
	SecCode        string `json:"secCode"`
	FilerName      string `json:"filerName"`
	DocDescription string `json:"docDescription"`
	DocTypeCode    string `json:"docTypeCode"`
	// docTypeCode: 120=有価証券報告書, 130=四半期報告書, 140=決算短信,
	// 150=半期報告書, 350=臨時報告書, 360=大量保有報告書
}

type EdinetResult struct {
	Results []EdinetDoc `json:"results"`
}

func main() {
	apiKey := os.Getenv("EDINET_API_KEY")
	if apiKey == "" {
		fmt.Println("❌ EDINET_API_KEY環境変数を設定してください")
		os.Exit(1)
	}

	date := "2025-12-25"
	if len(os.Args) > 1 {
		date = os.Args[1]
	}

	fmt.Printf("📅 日付: %s\n", date)
	fmt.Printf("🔍 EDINET APIから書類一覧を取得中...\n\n")

	url := fmt.Sprintf("https://api.edinet-fsa.go.jp/api/v2/documents.json?date=%s&type=2", date)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Ocp-Apim-Subscription-Key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("❌ API error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result EdinetResult
	json.Unmarshal(body, &result)

	// 書類タイプごとにカウント
	typeCounts := map[string]int{}
	var financialDocs []EdinetDoc

	for _, doc := range result.Results {
		desc := doc.DocDescription
		if desc == "" {
			desc = "不明"
		}
		typeCounts[doc.DocTypeCode+" "+desc]++

		// 財務データが含まれる書類タイプを選定
		// 120=有価証券報告書, 130=四半期報告書, 140=半期報告書
		// 160=有価証券届出書
		if doc.SecCode != "" && (strings.Contains(desc, "有価証券報告書") ||
			strings.Contains(desc, "四半期報告書") ||
			strings.Contains(desc, "半期報告書") ||
			strings.Contains(desc, "決算短信") ||
			doc.DocTypeCode == "120" || doc.DocTypeCode == "130" || doc.DocTypeCode == "140") {
			financialDocs = append(financialDocs, doc)
		}
	}

	fmt.Printf("📋 全書類数: %d件\n", len(result.Results))
	fmt.Printf("\n📊 書類タイプ別集計:\n")
	for t, c := range typeCounts {
		fmt.Printf("   %s: %d件\n", t, c)
	}

	fmt.Printf("\n🎯 財務データ含む書類: %d件\n\n", len(financialDocs))

	if len(financialDocs) == 0 {
		fmt.Println("⚠️ この日付では有価証券報告書・四半期報告書が見つかりませんでした。")
		fmt.Println("   別の日付でお試しください（例: 平日の日付）")

		// SecCode付きの書類をとにかく全部見る
		fmt.Printf("\n📋 SecCode付き全書類:\n")
		for _, doc := range result.Results {
			if doc.SecCode != "" {
				fmt.Printf("   %s %s [%s] DocType:%s DocID:%s\n",
					doc.SecCode, doc.FilerName, doc.DocDescription, doc.DocTypeCode, doc.DocID)
			}
		}
		return
	}

	// 最初の3件のXBRLを詳細調査
	maxDocs := 3
	if len(financialDocs) < maxDocs {
		maxDocs = len(financialDocs)
	}

	for i := 0; i < maxDocs; i++ {
		doc := financialDocs[i]
		fmt.Printf("═══════════════════════════════════════════════════\n")
		fmt.Printf("📄 [%d/%d] %s (%s)\n", i+1, maxDocs, doc.FilerName, doc.SecCode)
		fmt.Printf("   書類: %s (DocType:%s)\n", doc.DocDescription, doc.DocTypeCode)
		fmt.Printf("   DocID: %s\n", doc.DocID)
		fmt.Printf("═══════════════════════════════════════════════════\n")

		analyzeXBRL(doc.DocID, apiKey)
		fmt.Println()
	}
}

func analyzeXBRL(docID, apiKey string) {
	url := fmt.Sprintf("https://api.edinet-fsa.go.jp/api/v2/documents/%s?type=1", docID)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Ocp-Apim-Subscription-Key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("  ❌ ダウンロードエラー: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Printf("  ❌ HTTPステータス: %d\n", resp.StatusCode)
		return
	}

	data, _ := io.ReadAll(resp.Body)
	zipReader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		fmt.Printf("  ❌ ZIP解析エラー: %v\n", err)
		return
	}

	// ファイル一覧
	fmt.Printf("\n  📁 ZIPファイル一覧:\n")
	for _, f := range zipReader.File {
		ext := ""
		parts := strings.Split(f.Name, ".")
		if len(parts) > 1 {
			ext = parts[len(parts)-1]
		}
		if ext == "xbrl" || ext == "xml" || ext == "htm" {
			fmt.Printf("     %s (%d bytes)\n", f.Name, f.UncompressedSize64)
		}
	}

	// XBRLファイルを解析
	for _, f := range zipReader.File {
		if !strings.HasSuffix(f.Name, ".xbrl") {
			continue
		}

		parts := strings.Split(f.Name, "/")
		shortName := parts[len(parts)-1]
		fmt.Printf("\n  🔬 解析: %s\n", shortName)

		rc, _ := f.Open()
		content, _ := io.ReadAll(rc)
		rc.Close()
		contentStr := string(content)

		fmt.Printf("  📏 サイズ: %d bytes\n\n", len(content))

		// 広範なパターンで検索
		searchPatterns := []struct {
			Name    string
			Pattern *regexp.Regexp
		}{
			// 売上
			{"NetSales", regexp.MustCompile(`<[^>]*NetSales[^>]*>[^<]+</`)},
			{"Revenues", regexp.MustCompile(`<[^>]*Revenues?[^>]*>[^<]+</`)},
			{"OperatingRevenue", regexp.MustCompile(`(?i)<[^>]*OperatingRevenue[^>]*>[^<]+</`)},
			{"Sales", regexp.MustCompile(`<[^>]*(?:^|:)Sales[^A-Z][^>]*>[^<]+</`)},

			// 利益
			{"ProfitLoss", regexp.MustCompile(`<[^>]*ProfitLoss[^>]*>[^<]+</`)},
			{"NetIncome", regexp.MustCompile(`<[^>]*NetIncome[^>]*>[^<]+</`)},
			{"OperatingIncome", regexp.MustCompile(`<[^>]*OperatingIncome[^>]*>[^<]+</`)},
			{"OrdinaryIncome", regexp.MustCompile(`<[^>]*OrdinaryIncome[^>]*>[^<]+</`)},
			{"Profit (generic)", regexp.MustCompile(`<[^>]*:Profit[^A-Za-z][^>]*>[^<]+</`)},
			{"ProfitAttributable", regexp.MustCompile(`<[^>]*ProfitAttributable[^>]*>[^<]+</`)},

			// 資産
			{"Assets (standalone)", regexp.MustCompile(`<[^>]*:Assets[\s>][^>]*>[^<]+</`)},
			{"TotalAssets", regexp.MustCompile(`<[^>]*TotalAssets[^>]*>[^<]+</`)},
			{"NetAssets", regexp.MustCompile(`<[^>]*NetAssets[^>]*>[^<]+</`)},
			{"CurrentAssets", regexp.MustCompile(`<[^>]*CurrentAssets[^>]*>[^<]+</`)},

			// 負債
			{"Liabilities (standalone)", regexp.MustCompile(`<[^>]*:Liabilities[\s>][^>]*>[^<]+</`)},
			{"CurrentLiabilities", regexp.MustCompile(`<[^>]*CurrentLiabilities[^>]*>[^<]+</`)},

			// キャッシュ
			{"CashAndDeposits", regexp.MustCompile(`<[^>]*CashAndDeposits[^>]*>[^<]+</`)},
			{"CashAndCashEquivalents", regexp.MustCompile(`<[^>]*CashAndCashEquivalents[^>]*>[^<]+</`)},

			// 株式数
			{"NumberOfShares", regexp.MustCompile(`<[^>]*(?:NumberOfIssued|TotalNumberOfIssued|SharesIssued|IssuedShares)[^>]*>[^<]+</`)},
		}

		for _, sp := range searchPatterns {
			matches := sp.Pattern.FindAllString(contentStr, 10)
			if len(matches) > 0 {
				fmt.Printf("  ✅ %s: %d件\n", sp.Name, len(matches))
				for j, m := range matches {
					if j >= 3 {
						fmt.Printf("      ... (残り%d件)\n", len(matches)-3)
						break
					}
					if len(m) > 250 {
						m = m[:250] + "..."
					}
					fmt.Printf("      [%d] %s\n", j+1, m)
				}
			}
		}

		// contextRefの種類を調査
		ctxPattern := regexp.MustCompile(`contextRef="([^"]+)"`)
		ctxMatches := ctxPattern.FindAllStringSubmatch(contentStr, -1)
		ctxSet := make(map[string]bool)
		for _, m := range ctxMatches {
			ctxSet[m[1]] = true
		}
		fmt.Printf("\n  📌 contextRef一覧 (%d種類):\n", len(ctxSet))
		for ctx := range ctxSet {
			fmt.Printf("      %s\n", ctx)
		}
		fmt.Println()

		// 最初の500文字を表示（タグ構造を確認）
		preview := contentStr
		if len(preview) > 800 {
			preview = preview[:800]
		}
		fmt.Printf("  📝 先頭プレビュー:\n%s\n...\n", preview)
	}
}
