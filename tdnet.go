package main

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// TdnetDisclosure は適時開示の1件分
type TdnetDisclosure struct {
	Code               string // 4桁証券コード
	DisclosureDateTime string // YYYY-MM-DD HH:MM
	Name               string // 会社名
	Title              string // 開示表題
	DocCategory        string // 書類種別
	PdfURL             string // PDF URL
}

// fetchTdnet は指定日のTDNET開示情報をスクレイピングしてDBに保存する
// 注意: TDNETは過去31日分のみ取得可能。それより古い日付は404になる
func fetchTdnet(targetDate string) {
	t, err := time.Parse("2006-01-02", targetDate)
	if err != nil {
		log.Fatalf("Invalid date format: %v (expected YYYY-MM-DD)", err)
	}
	dateStr := t.Format("20060102") // TDNETのURL形式

	fmt.Printf("📥 Fetching TDNET disclosures for %s...\n", targetDate)

	db, err := initXbrlDB()
	if err != nil {
		log.Fatalf("DB init failed: %v", err)
	}
	defer db.Close()

	totalSaved := 0
	totalSkipped := 0
	page := 1

	for {
		url := fmt.Sprintf("https://www.release.tdnet.info/inbs/I_list_%03d_%s.html", page, dateStr)
		disclosures, hasMore, err := fetchTdnetPage(url, targetDate)
		if err != nil {
			if page == 1 {
				log.Printf("⚠️ Failed to fetch first page: %v (もしかすると土日祝で開示なし、または31日より古い日付)", err)
				return
			}
			break // 後続ページの404は終端
		}

		if len(disclosures) == 0 {
			break
		}

		for _, d := range disclosures {
			saved, err := saveTdnetDisclosure(db, d)
			if err != nil {
				log.Printf("⚠️ Save failed for %s: %v", d.Code, err)
				continue
			}
			if saved {
				totalSaved++
			} else {
				totalSkipped++
			}
		}

		fmt.Printf("  📄 Page %d: %d件取得\n", page, len(disclosures))

		if !hasMore {
			break
		}
		page++
		time.Sleep(1 * time.Second) // レート制限対策
	}

	fmt.Printf("\n✅ TDNET 取得完了: 新規=%d件, 既存=%d件\n", totalSaved, totalSkipped)
}

// fetchTdnetPage は1ページ分のHTMLを取得して開示一覧をパースする
func fetchTdnetPage(url, dateStr string) ([]TdnetDisclosure, bool, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, false, fmt.Errorf("HTTP error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("HTTP status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, err
	}

	html := string(body)
	disclosures := parseTdnetHTML(html, dateStr)

	// 「次へ」リンクの有無で次ページ判定（簡易）
	hasMore := strings.Contains(html, "次へ") || strings.Contains(html, "next")

	return disclosures, hasMore, nil
}

// parseTdnetHTML はTDNETのHTMLから開示行を抽出する
// TDNET形式: <td>HH:MM</td><td>NNNNN</td><td>会社名</td><td><a>表題</a></td><td>種別</td><td>...PDFリンク...</td>
func parseTdnetHTML(html, dateStr string) []TdnetDisclosure {
	// 各 <tr>...</tr> ブロックを抽出
	rowRegex := regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)
	cellRegex := regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
	pdfRegex := regexp.MustCompile(`href="([^"]+\.pdf)"`)
	timeRegex := regexp.MustCompile(`^\d{1,2}:\d{2}$`)
	tagStripRegex := regexp.MustCompile(`<[^>]+>`)

	var results []TdnetDisclosure
	rows := rowRegex.FindAllStringSubmatch(html, -1)
	for _, row := range rows {
		cells := cellRegex.FindAllStringSubmatch(row[1], -1)
		if len(cells) < 5 {
			continue
		}

		// セル内容をテキスト化
		extract := func(s string) string {
			s = tagStripRegex.ReplaceAllString(s, "")
			return strings.TrimSpace(strings.ReplaceAll(s, "&nbsp;", " "))
		}

		timeStr := extract(cells[0][1])
		if !timeRegex.MatchString(timeStr) {
			continue // 時刻でなければデータ行ではない
		}

		codeRaw := extract(cells[1][1])
		// 5桁の場合は末尾0を落として4桁にする (JPX旧仕様)
		code := codeRaw
		if len(code) == 5 {
			code = code[:4]
		}

		name := extract(cells[2][1])
		title := extract(cells[3][1])

		var docCat string
		if len(cells) >= 5 {
			docCat = extract(cells[4][1])
		}

		// PDF URL は cells[3] か cells[5] に含まれることが多い
		var pdfURL string
		for _, c := range cells {
			m := pdfRegex.FindStringSubmatch(c[1])
			if len(m) >= 2 {
				pdfURL = m[1]
				if !strings.HasPrefix(pdfURL, "http") {
					pdfURL = "https://www.release.tdnet.info/inbs/" + pdfURL
				}
				break
			}
		}

		results = append(results, TdnetDisclosure{
			Code:               code,
			DisclosureDateTime: dateStr + " " + timeStr,
			Name:               name,
			Title:              title,
			DocCategory:        docCat,
			PdfURL:             pdfURL,
		})
	}
	return results
}

// saveTdnetDisclosure は開示情報をDBに保存する。新規ならtrueを返す
func saveTdnetDisclosure(db *sql.DB, d TdnetDisclosure) (bool, error) {
	res, err := db.Exec(`
		INSERT OR IGNORE INTO tdnet_disclosures (
			code, disclosure_datetime, name, title, doc_category, pdf_url
		) VALUES (?, ?, ?, ?, ?, ?)`,
		d.Code, d.DisclosureDateTime, d.Name, d.Title, d.DocCategory, d.PdfURL)
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}
