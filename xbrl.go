package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func fetchFromAPI(url, apiKey string) ([]byte, error) {
	client := &http.Client{Timeout: 3 * time.Minute}
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

// XBRLタグと対応するフィールドのマッピング
// EDINETのXBRL形式:
//   - 経営指標サマリー: jpcrp_cor:XXXSummaryOfBusinessResults (contextRef="CurrentYearDuration/Instant")
//   - 財務諸表本体: jppfs_cor:XXX (contextRef="CurrentYearDuration/Instant")
//   - 四半期: contextRef="CurrentQuarterDuration" or "CurrentYTDDuration"
//   - 非連結: contextRefに "_NonConsolidatedMember" サフィックス
var xbrlTagPatterns = map[string]*regexp.Regexp{
	// ====== 売上高 ======
	// サマリー（連結・年度）
	"NetSales": regexp.MustCompile(`<jpcrp_cor:NetSalesSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration"[^>]*>(\d+)</`),
	// サマリー（非連結含む）
	"NetSalesFallback": regexp.MustCompile(`<jpcrp_cor:NetSalesSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(\d+)</`),
	// 財務諸表本体
	"NetSalesFallback2": regexp.MustCompile(`<jppfs_cor:NetSales[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(\d+)</`),
	// 四半期累計
	"NetSalesFallback3": regexp.MustCompile(`<jpcrp_cor:NetSalesSummaryOfBusinessResults[^>]*contextRef="CurrentYTDDuration[^"]*"[^>]*>(\d+)</`),
	// IFRS適用企業の売上収益
	"NetSalesFallback4": regexp.MustCompile(`<jpcrp_cor:RevenueIFRSSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(\d+)</`),
	// 営業収益（銀行・保険など）
	"OperatingRevenues": regexp.MustCompile(`<jpcrp_cor:OperatingRevenue[12]SummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(\d+)</`),
	// 四半期営業収益
	"OperatingRevenuesFallback": regexp.MustCompile(`<jpcrp_cor:OperatingRevenue[12]SummaryOfBusinessResults[^>]*contextRef="CurrentYTDDuration[^"]*"[^>]*>(\d+)</`),

	// ====== 営業利益 ======
	// サマリー（連結）
	"OperatingIncome": regexp.MustCompile(`<jpcrp_cor:OperatingIncomeLossSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration"[^>]*>(-?\d+)</`),
	// サマリー（非連結含む）
	"OperatingIncomeFallback": regexp.MustCompile(`<jpcrp_cor:OperatingIncomeLossSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	// 財務諸表本体
	"OperatingIncomeFallback2": regexp.MustCompile(`<jppfs_cor:OperatingIncome[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	// 四半期累計
	"OperatingIncomeFallback3": regexp.MustCompile(`<jpcrp_cor:OperatingIncomeLossSummaryOfBusinessResults[^>]*contextRef="CurrentYTDDuration[^"]*"[^>]*>(-?\d+)</`),

	// ====== 経常利益 ======
	"OrdinaryIncome":         regexp.MustCompile(`<jpcrp_cor:OrdinaryIncomeLossSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	"OrdinaryIncomeFallback": regexp.MustCompile(`<jpcrp_cor:OrdinaryIncomeLossSummaryOfBusinessResults[^>]*contextRef="CurrentYTDDuration[^"]*"[^>]*>(-?\d+)</`),

	// ====== 純利益 ======
	// 親会社株主帰属 サマリー（連結）
	"NetIncome": regexp.MustCompile(`<jpcrp_cor:ProfitLossAttributableToOwnersOfParentSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration"[^>]*>(-?\d+)</`),
	// 親会社株主帰属 サマリー（非連結含む）
	"NetIncomeFallback": regexp.MustCompile(`<jpcrp_cor:ProfitLossAttributableToOwnersOfParentSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	// 財務諸表本体 当期純利益
	"NetIncomeFallback2": regexp.MustCompile(`<jppfs_cor:ProfitLoss[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	// 非連結 NetIncomeLoss
	"NetIncomeFallback3": regexp.MustCompile(`<jpcrp_cor:NetIncomeLossSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	// 四半期累計 純利益
	"NetIncomeFallback4": regexp.MustCompile(`<jpcrp_cor:ProfitLossAttributableToOwnersOfParentSummaryOfBusinessResults[^>]*contextRef="CurrentYTDDuration[^"]*"[^>]*>(-?\d+)</`),
	// IFRS 親会社帰属利益
	"NetIncomeFallback5": regexp.MustCompile(`<jpcrp_cor:ProfitLossAttributableToOwnersOfParentIFRSSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),

	// ====== 総資産 ======
	// サマリー（連結）
	"TotalAssets": regexp.MustCompile(`<jpcrp_cor:TotalAssetsSummaryOfBusinessResults[^>]*contextRef="CurrentYearInstant"[^>]*>(\d+)</`),
	// サマリー（非連結含む）
	"TotalAssetsFallback": regexp.MustCompile(`<jpcrp_cor:TotalAssetsSummaryOfBusinessResults[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	// 財務諸表本体
	"TotalAssetsFallback2": regexp.MustCompile(`<jppfs_cor:Assets[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	// 四半期末時点
	"TotalAssetsFallback3": regexp.MustCompile(`<jpcrp_cor:TotalAssetsSummaryOfBusinessResults[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 純資産 ======
	// サマリー（連結）
	"NetAssets": regexp.MustCompile(`<jpcrp_cor:NetAssetsSummaryOfBusinessResults[^>]*contextRef="CurrentYearInstant"[^>]*>(\d+)</`),
	// サマリー（非連結含む）
	"NetAssetsFallback": regexp.MustCompile(`<jpcrp_cor:NetAssetsSummaryOfBusinessResults[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	// 財務諸表本体
	"NetAssetsFallback2": regexp.MustCompile(`<jppfs_cor:NetAssets[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	// 四半期末時点
	"NetAssetsFallback3": regexp.MustCompile(`<jpcrp_cor:NetAssetsSummaryOfBusinessResults[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),
	// 株主資本（EquityAttributableToOwnersOfParent - IFRS用）
	"NetAssetsFallback4": regexp.MustCompile(`<jpcrp_cor:EquityAttributableToOwnersOfParentIFRSSummaryOfBusinessResults[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 流動資産 ======
	"CurrentAssets":         regexp.MustCompile(`<jppfs_cor:CurrentAssets[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"CurrentAssetsFallback": regexp.MustCompile(`<jppfs_cor:CurrentAssets[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 負債合計 ======
	"Liabilities":         regexp.MustCompile(`<jppfs_cor:Liabilities[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"LiabilitiesFallback": regexp.MustCompile(`<jppfs_cor:Liabilities[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 流動負債 ======
	"CurrentLiabilities":         regexp.MustCompile(`<jppfs_cor:CurrentLiabilities[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"CurrentLiabilitiesFallback": regexp.MustCompile(`<jppfs_cor:CurrentLiabilities[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 現金預金 ======
	"CashAndDeposits":         regexp.MustCompile(`<jppfs_cor:CashAndDeposits[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"CashAndDepositsFallback": regexp.MustCompile(`<jppfs_cor:CashAndDeposits[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 発行済株式数 ======
	// サマリー（contextRefにNonConsolidatedMember等が付く場合あり）
	"SharesIssued": regexp.MustCompile(`<jpcrp_cor:TotalNumberOfIssuedSharesSummaryOfBusinessResults[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	// 四半期末時点
	"SharesIssuedFallback": regexp.MustCompile(`<jpcrp_cor:TotalNumberOfIssuedSharesSummaryOfBusinessResults[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),
	// 提出日時点の発行済株式数
	"SharesIssuedFallback2": regexp.MustCompile(`<jpcrp_cor:NumberOfIssuedSharesAsOfFilingDateEtcTotalNumberOfSharesEtc[^>]*>(\d+)</`),

	// ====== 投資有価証券 ======
	"InvestmentSecurities":         regexp.MustCompile(`<jppfs_cor:InvestmentSecurities[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"InvestmentSecuritiesFallback": regexp.MustCompile(`<jppfs_cor:InvestmentSecurities[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 有価証券（短期） ======
	"Securities":         regexp.MustCompile(`<jppfs_cor:Securities[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"SecuritiesFallback": regexp.MustCompile(`<jppfs_cor:Securities[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 売掛金 ======
	// 受取手形及び売掛金
	"AccountsReceivable":         regexp.MustCompile(`<jppfs_cor:NotesAndAccountsReceivableTrade[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"AccountsReceivableFallback": regexp.MustCompile(`<jppfs_cor:NotesAndAccountsReceivableTrade[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),
	// 売掛金単独
	"AccountsReceivableFallback2": regexp.MustCompile(`<jppfs_cor:AccountsReceivableTrade[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	// 売掛金及び契約資産 (IFRS/収益認識基準)
	"AccountsReceivableFallback3": regexp.MustCompile(`<jppfs_cor:NotesAndAccountsReceivableTradeAndContractAssets[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 棚卸資産 ======
	"Inventories":         regexp.MustCompile(`<jppfs_cor:Inventories[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"InventoriesFallback": regexp.MustCompile(`<jppfs_cor:Inventories[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),
	// 商品及び製品
	"InventoriesFallback2": regexp.MustCompile(`<jppfs_cor:MerchandiseAndFinishedGoods[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 固定負債 ======
	"NonCurrentLiabilities":         regexp.MustCompile(`<jppfs_cor:NoncurrentLiabilities[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"NonCurrentLiabilitiesFallback": regexp.MustCompile(`<jppfs_cor:NoncurrentLiabilities[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),
	// 固定負債合計 別タグ
	"NonCurrentLiabilitiesFallback2": regexp.MustCompile(`<jppfs_cor:FixedLiabilities[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 株主資本 ======
	"ShareholdersEquity":         regexp.MustCompile(`<jppfs_cor:ShareholdersEquity[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),
	"ShareholdersEquityFallback": regexp.MustCompile(`<jppfs_cor:ShareholdersEquity[^>]*contextRef="CurrentQuarterInstant[^"]*"[^>]*>(\d+)</`),
	// 株主資本合計 (別名)
	"ShareholdersEquityFallback2": regexp.MustCompile(`<jppfs_cor:StockholdersEquity[^>]*contextRef="CurrentYearInstant[^"]*"[^>]*>(\d+)</`),

	// ====== 営業活動によるキャッシュフロー (CFO) ======
	// Phase 1b (バリュー F9) で使用
	"OperatingCashFlow":          regexp.MustCompile(`<jppfs_cor:CashFlowsFromUsedInOperatingActivities[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	"OperatingCashFlowFallback":  regexp.MustCompile(`<jppfs_cor:NetCashProvidedByUsedInOperatingActivities[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	"OperatingCashFlowFallback2": regexp.MustCompile(`<jpcrp_cor:CashFlowsFromOperatingActivitiesSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),

	// ====== 売上総利益 (粗利) ======
	// Phase 1b (バリュー F9) で使用
	"GrossProfit":         regexp.MustCompile(`<jppfs_cor:GrossProfit[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),
	"GrossProfitFallback": regexp.MustCompile(`<jppfs_cor:GrossProfitsLosses[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>(-?\d+)</`),

	// ====== 1株配当 (DPS) ======
	// Phase 2 (高配当) で使用。注: 小数 ([\d.]+) なので他タグの (\d+) と異なる
	"DividendPerShare":          regexp.MustCompile(`<jpcrp_cor:DividendPaidPerShareSummaryOfBusinessResults[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>([\d.]+)</`),
	"DividendPerShareFallback":  regexp.MustCompile(`<jpcrp_cor:DividendPerShare[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>([\d.]+)</`),
	"DividendPerShareFallback2": regexp.MustCompile(`<jppfs_cor:DividendPerShare[^>]*contextRef="CurrentYearDuration[^"]*"[^>]*>([\d.]+)</`),
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

	client := &http.Client{Timeout: 3 * time.Minute}
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

// getBaseTagName はフォールバックタグ名からベースタグ名を取得する
func getBaseTagName(tagName string) string {
	// Fallback5 → Fallback4 → ... → Fallback → base の順で除去
	for _, suffix := range []string{"Fallback5", "Fallback4", "Fallback3", "Fallback2", "Fallback"} {
		if strings.HasSuffix(tagName, suffix) {
			return strings.TrimSuffix(tagName, suffix)
		}
	}
	return tagName
}

// applyXBRLValue は抽出した値をFinancialDataに設定する
func applyXBRLValue(data *FinancialData, found map[string]bool, baseName string, value int64) {
	switch baseName {
	case "NetSales", "OperatingRevenues":
		if !found["NetSales"] {
			data.NetSales = value
			found["NetSales"] = true
		}
	case "OperatingIncome":
		if !found["OperatingIncome"] {
			data.OperatingIncome = value
			found["OperatingIncome"] = true
		}
	case "OrdinaryIncome":
		// 経常利益 → OperatingIncomeが0なら代用
		if !found["OperatingIncome"] {
			data.OperatingIncome = value
		}
	case "NetIncome":
		if !found["NetIncome"] {
			data.NetIncome = value
			found["NetIncome"] = true
		}
	case "TotalAssets":
		if !found["TotalAssets"] {
			data.TotalAssets = value
			found["TotalAssets"] = true
		}
	case "NetAssets":
		if !found["NetAssets"] {
			data.NetAssets = value
			found["NetAssets"] = true
		}
	case "CurrentAssets":
		if !found["CurrentAssets"] {
			data.CurrentAssets = value
			found["CurrentAssets"] = true
		}
	case "Liabilities":
		if !found["Liabilities"] {
			data.Liabilities = value
			found["Liabilities"] = true
		}
	case "CurrentLiabilities":
		if !found["CurrentLiabilities"] {
			data.CurrentLiabilities = value
			found["CurrentLiabilities"] = true
		}
	case "CashAndDeposits":
		if !found["CashAndDeposits"] {
			data.CashAndDeposits = value
			found["CashAndDeposits"] = true
		}
	case "SharesIssued":
		if !found["SharesIssued"] {
			data.SharesIssued = value
			found["SharesIssued"] = true
		}
	case "InvestmentSecurities":
		if !found["InvestmentSecurities"] {
			data.InvestmentSecurities = value
			found["InvestmentSecurities"] = true
		}
	case "Securities":
		if !found["Securities"] {
			data.Securities = value
			found["Securities"] = true
		}
	case "AccountsReceivable":
		if !found["AccountsReceivable"] {
			data.AccountsReceivable = value
			found["AccountsReceivable"] = true
		}
	case "Inventories":
		if !found["Inventories"] {
			data.Inventories = value
			found["Inventories"] = true
		}
	case "NonCurrentLiabilities":
		if !found["NonCurrentLiabilities"] {
			data.NonCurrentLiabilities = value
			found["NonCurrentLiabilities"] = true
		}
	case "ShareholdersEquity":
		if !found["ShareholdersEquity"] {
			data.ShareholdersEquity = value
			found["ShareholdersEquity"] = true
		}
	case "OperatingCashFlow":
		if !found["OperatingCashFlow"] {
			data.OperatingCashFlow = value
			found["OperatingCashFlow"] = true
		}
	case "GrossProfit":
		if !found["GrossProfit"] {
			data.GrossProfit = value
			found["GrossProfit"] = true
		}
	}
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
			baseName := getBaseTagName(tagName)

			// 既にベースタグで取得済みならスキップ
			if found[baseName] {
				continue
			}

			matches := pattern.FindStringSubmatch(contentStr)
			if len(matches) >= 2 {
				// DividendPerShare は小数 (例: 25.5円) なので別処理
				if baseName == "DividendPerShare" {
					if !found["DividendPerShare"] {
						if dps, err := strconv.ParseFloat(matches[1], 64); err == nil && dps > 0 {
							data.DividendPerShare = dps
							found["DividendPerShare"] = true
						}
					}
					continue
				}
				value, _ := strconv.ParseInt(matches[1], 10, 64)
				// 売上・資産系はプラスのみ、利益系・CFOはマイナスも許容
				isProfit := baseName == "OperatingIncome" || baseName == "OrdinaryIncome" || baseName == "NetIncome" || baseName == "OperatingCashFlow"
				if value > 0 || (isProfit && value != 0) {
					applyXBRLValue(&data, found, baseName, value)
				}
			}
		}
	}

	// 何かデータが取れたかチェック（1つでもあればOK）
	if data.NetSales == 0 && data.TotalAssets == 0 && data.NetAssets == 0 &&
		data.NetIncome == 0 && data.OperatingIncome == 0 && data.SharesIssued == 0 {
		return data, fmt.Errorf("no financial data found in XBRL")
	}

	fmt.Printf("    📊 抽出: 売上=%d, 営業利益=%d, 純利益=%d, 総資産=%d, 純資産=%d, 株式数=%d\n",
		data.NetSales, data.OperatingIncome, data.NetIncome, data.TotalAssets, data.NetAssets, data.SharesIssued)

	return data, nil
}

// テスト用関数
func testLocalParse() {
	db, err := initXbrlDB()
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
	found := make(map[string]bool)

	for tagName, pattern := range xbrlTagPatterns {
		baseName := getBaseTagName(tagName)

		// 既にベースタグで取得済みならスキップ
		if found[baseName] {
			continue
		}

		matches := pattern.FindStringSubmatch(contentStr)
		if len(matches) >= 2 {
			if baseName == "DividendPerShare" {
				if dps, err := strconv.ParseFloat(matches[1], 64); err == nil && dps > 0 {
					data.DividendPerShare = dps
					found["DividendPerShare"] = true
				}
				continue
			}
			value, _ := strconv.ParseInt(matches[1], 10, 64)
			isProfit := baseName == "OperatingIncome" || baseName == "OrdinaryIncome" || baseName == "NetIncome" || baseName == "OperatingCashFlow"
			if value > 0 || (isProfit && value != 0) {
				applyXBRLValue(&data, found, baseName, value)
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
