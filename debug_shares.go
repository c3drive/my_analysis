//go:build ignore
// +build ignore

package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
)

func main() {
	apiKey := os.Getenv("EDINET_API_KEY")
	if apiKey == "" {
		fmt.Println("❌ EDINET_API_KEY環境変数を設定してください")
		os.Exit(1)
	}

	// 東京一番フーズの有価証券報告書を解析
	docID := "S100XCE6" // 有価証券報告書の例
	if len(os.Args) > 1 {
		docID = os.Args[1]
	}

	fmt.Printf("📄 DocID: %s\n", docID)

	url := fmt.Sprintf("https://api.edinet-fsa.go.jp/api/v2/documents/%s?type=1", docID)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Ocp-Apim-Subscription-Key", apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Printf("❌ Error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	zipReader, _ := zip.NewReader(bytes.NewReader(data), int64(len(data)))

	for _, f := range zipReader.File {
		if !strings.HasSuffix(f.Name, ".xbrl") {
			continue
		}
		// メインのXBRLファイルのみ（監査報告書等をスキップ）
		if strings.Contains(f.Name, "jpaud") {
			continue
		}

		parts := strings.Split(f.Name, "/")
		shortName := parts[len(parts)-1]
		fmt.Printf("\n📁 %s\n\n", shortName)

		rc, _ := f.Open()
		content, _ := io.ReadAll(rc)
		rc.Close()
		contentStr := string(content)

		// 発行済株式数関連のすべてのタグを探す
		patterns := []struct {
			Name string
			Pat  *regexp.Regexp
		}{
			{"TotalNumberOfIssued*", regexp.MustCompile(`<[^>]*TotalNumberOfIssued[^>]*>[^<]+</[^>]+>`)},
			{"NumberOfIssued*", regexp.MustCompile(`<[^>]*NumberOfIssued[^>]*>[^<]+</[^>]+>`)},
			{"IssuedShares*", regexp.MustCompile(`<[^>]*IssuedShares[^>]*>[^<]+</[^>]+>`)},
			{"NumberOfSharesEPS*", regexp.MustCompile(`<[^>]*(?:EarningsPerShare|NumberOfShares|ShareUnit)[^>]*>[^<]+</[^>]+>`)},
			{"BookValuePerShareOfShare", regexp.MustCompile(`<[^>]*BookValuePerShare[^>]*>[^<]+</[^>]+>`)},
			{"EPS系", regexp.MustCompile(`<[^>]*(?:BasicEarningsLossPerShare|DilutedEarningsPerShare)[^>]*>[^<]+</[^>]+>`)},
		}

		for _, p := range patterns {
			matches := p.Pat.FindAllString(contentStr, 20)
			if len(matches) > 0 {
				fmt.Printf("✅ %s: %d件\n", p.Name, len(matches))
				for i, m := range matches {
					if i >= 5 {
						fmt.Printf("   ... (残り%d件)\n", len(matches)-5)
						break
					}
					if len(m) > 300 {
						m = m[:300] + "..."
					}
					fmt.Printf("   [%d] %s\n", i+1, m)
				}
			}
		}
	}
}
