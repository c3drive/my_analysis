package main

import (
	"flag"
	"log"
	"os"
	"time"
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
		SubmissionDate string `json:"submitDateTime"`
		DocTypeCode    string `json:"docTypeCode"`
		DocDescription string `json:"docDescription"`
	} `json:"results"`
}

// Stock は銘柄の財務データを保持する構造体
type Stock struct {
	Code      string `json:"Code"`
	Name      string `json:"Name"`
	UpdatedAt string `json:"UpdatedAt"`
	// 売上・利益
	NetSales        int64 `json:"NetSales"`        // 売上高
	OperatingIncome int64 `json:"OperatingIncome"` // 営業利益
	NetIncome       int64 `json:"NetIncome"`       // 純利益
	// 資産・負債
	TotalAssets        int64 `json:"TotalAssets"`        // 総資産
	NetAssets          int64 `json:"NetAssets"`          // 純資産
	CurrentAssets      int64 `json:"CurrentAssets"`      // 流動資産
	Liabilities        int64 `json:"Liabilities"`        // 負債合計
	CurrentLiabilities int64 `json:"CurrentLiabilities"` // 流動負債
	// その他
	CashAndDeposits       int64 `json:"CashAndDeposits"`       // 現金及び預金
	SharesIssued          int64 `json:"SharesIssued"`          // 発行済株式数
	InvestmentSecurities  int64 `json:"InvestmentSecurities"`  // 投資有価証券
	Securities            int64 `json:"Securities"`            // 有価証券（短期）
	AccountsReceivable    int64 `json:"AccountsReceivable"`    // 売掛金
	Inventories           int64 `json:"Inventories"`           // 棚卸資産
	NonCurrentLiabilities int64 `json:"NonCurrentLiabilities"` // 固定負債
	ShareholdersEquity    int64 `json:"ShareholdersEquity"`    // 株主資本
	// JPX メタデータ
	MarketSegment string `json:"MarketSegment,omitempty"` // プライム/スタンダード/グロース
	Sector33      string `json:"Sector33,omitempty"`      // 33業種
	Sector17      string `json:"Sector17,omitempty"`      // 17業種
}

// FinancialData はXBRLから抽出した財務データ
type FinancialData struct {
	NetSales              int64
	OperatingIncome       int64
	NetIncome             int64
	TotalAssets           int64
	NetAssets             int64
	CurrentAssets         int64
	Liabilities           int64
	CurrentLiabilities    int64
	CashAndDeposits       int64
	SharesIssued          int64
	InvestmentSecurities  int64
	Securities            int64
	AccountsReceivable    int64
	Inventories           int64
	NonCurrentLiabilities int64
	ShareholdersEquity    int64
}

// StockPrice は株価データを保持する構造体
type StockPrice struct {
	Code   string  `json:"Code"`
	Date   string  `json:"Date"`
	Open   float64 `json:"Open"`
	High   float64 `json:"High"`
	Low    float64 `json:"Low"`
	Close  float64 `json:"Close"`
	Volume int64   `json:"Volume"`
}

// Metrics は投資指標計算の結果を保持する
type Metrics struct {
	MarketCap   int64
	PER         *float64
	PBR         *float64
	EPS         *float64
	ROE         *float64
	EquityRatio *float64
	NetNetRatio *float64
}

// calcMetrics は共通の投資指標を計算する
func calcMetrics(lastPrice float64, sharesIssued, netIncome, netAssets, totalAssets, currentAssets, liabilities int64) Metrics {
	var m Metrics
	if lastPrice > 0 && sharesIssued > 0 {
		m.MarketCap = int64(lastPrice * float64(sharesIssued))
	}
	if m.MarketCap > 0 && netIncome > 0 {
		v := float64(m.MarketCap) / float64(netIncome)
		m.PER = &v
	}
	if m.MarketCap > 0 && netAssets > 0 {
		v := float64(m.MarketCap) / float64(netAssets)
		m.PBR = &v
	}
	if netIncome > 0 && sharesIssued > 0 {
		v := float64(netIncome) / float64(sharesIssued)
		m.EPS = &v
	}
	if netIncome > 0 && netAssets > 0 {
		v := float64(netIncome) / float64(netAssets) * 100
		m.ROE = &v
	}
	if totalAssets > 0 && netAssets > 0 {
		v := float64(netAssets) / float64(totalAssets) * 100
		m.EquityRatio = &v
	}
	if m.MarketCap > 0 && currentAssets > 0 {
		v := float64(currentAssets-liabilities) / float64(m.MarketCap)
		m.NetNetRatio = &v
	}
	return m
}

func main() {
	mode := flag.String("mode", "run", "execution mode: run, batch, serve, fetch-prices, calc-rs, export-json, fetch-tdnet, parse-tanshin, debug-tanshin, import-jpx, or test-parse")
	dateFlag := flag.String("date", time.Now().Format("2006-01-02"), "target date for run mode (YYYY-MM-DD)")
	fromFlag := flag.String("from", "", "start date for batch mode (YYYY-MM-DD)")
	toFlag := flag.String("to", "", "end date for batch mode (YYYY-MM-DD)")
	fileFlag := flag.String("file", "", "input file path (for import-jpx mode)")
	codeFlag := flag.String("code", "", "stock code (for debug-tanshin mode)")
	flag.Parse()

	switch *mode {
	case "test-parse":
		testLocalParse()
	case "run":
		runCollector(*dateFlag)
	case "batch":
		runBatch(*fromFlag, *toFlag)
	case "serve":
		startServer()
	case "fetch-prices":
		fetchStockPrices()
	case "calc-rs":
		calculateRS()
	case "export-json":
		exportJSON()
	case "fetch-tdnet":
		fetchTdnet(*dateFlag)
	case "parse-tanshin":
		parseTanshinForDate(*dateFlag)
	case "debug-tanshin":
		debugTanshin(*codeFlag, *dateFlag)
	case "import-jpx":
		importJPX(*fileFlag)
	default:
		log.Fatalf("Unknown mode: %s", *mode)
	}
}
