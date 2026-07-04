package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
)

// exportJSON はDBからデータを読み込み、web/stocks.json に出力する
// deploy-pages.yml のインラインGoコードの代替
func exportJSON() {
	ensureDir()
	migrateFromLegacyDB()

	// マイグレーション (ALTER TABLE ADD COLUMN) を実行して列の欠損を補う
	// 古い release から DB を取得した場合に必須
	migDB, err := initXbrlDB()
	if err != nil {
		log.Printf("⚠️ Migration warning: %v", err)
	} else {
		migDB.Close()
	}

	db, err := openServerDB()
	if err != nil {
		log.Fatalf("DB open error: %v", err)
	}
	defer db.Close()

	// SetMaxOpenConns(1) のためメインクエリ前に補助データを完全ロードする
	// RS値を一括取得
	rsMap := make(map[string]float64)
	rsRows, rsErr := db.Query(`
		SELECT code, rs_rank FROM rs_db.rs_scores rs1
		WHERE date = (SELECT MAX(date) FROM rs_db.rs_scores rs2 WHERE rs2.code = rs1.code)`)
	if rsErr == nil {
		for rsRows.Next() {
			var code string
			var rank float64
			if rsRows.Scan(&code, &rank) == nil {
				rsMap[code] = rank
			}
		}
		rsRows.Close()
	}

	// 成長指標用の時系列データを一括ロード
	financialsMap, _ := loadAllFinancials(db)

	// メインクエリ (補助データロード後)
	rows, err := db.Query(`
		SELECT s.code, s.name, COALESCE(s.updated_at, ''),
			   COALESCE(s.net_sales, 0), COALESCE(s.operating_income, 0), COALESCE(s.net_income, 0),
			   COALESCE(s.total_assets, 0), COALESCE(s.net_assets, 0), COALESCE(s.current_assets, 0),
			   COALESCE(s.liabilities, 0), COALESCE(s.current_liabilities, 0),
			   COALESCE(s.cash_and_deposits, 0), COALESCE(s.shares_issued, 0),
			   COALESCE(s.investment_securities, 0), COALESCE(s.securities, 0),
			   COALESCE(s.accounts_receivable, 0), COALESCE(s.inventories, 0),
			   COALESCE(s.non_current_liabilities, 0), COALESCE(s.shareholders_equity, 0),
			   COALESCE(s.market_segment, ''), COALESCE(s.sector_33, ''), COALESCE(s.sector_17, ''),
			   COALESCE(p.close, 0) as last_price,
			   p.date as price_date
		FROM stocks s
		LEFT JOIN (
			SELECT code, close, date FROM price_db.stock_prices sp1
			WHERE date = (SELECT MAX(date) FROM price_db.stock_prices sp2 WHERE sp2.code = sp1.code)
		) p ON s.code = p.code
		ORDER BY s.code ASC`)
	if err != nil {
		log.Fatalf("Query error: %v", err)
	}
	defer rows.Close()

	type StockJSON struct {
		Stock
		LastPrice   float64  `json:"LastPrice"`
		PriceDate   *string  `json:"PriceDate"`
		MarketCap   int64    `json:"MarketCap"`
		PER         *float64 `json:"PER"`
		PBR         *float64 `json:"PBR"`
		EPS         *float64 `json:"EPS"`
		ROE         *float64 `json:"ROE"`
		EquityRatio *float64 `json:"EquityRatio"`
		NetNetRatio *float64 `json:"NetNetRatio"`
		RS          *float64 `json:"RS"`
		GrowthMetrics
	}

	var stocks []StockJSON
	for rows.Next() {
		var s StockJSON
		var priceDate sql.NullString
		if err := rows.Scan(&s.Code, &s.Name, &s.UpdatedAt,
			&s.NetSales, &s.OperatingIncome, &s.NetIncome,
			&s.TotalAssets, &s.NetAssets, &s.CurrentAssets,
			&s.Liabilities, &s.CurrentLiabilities,
			&s.CashAndDeposits, &s.SharesIssued,
			&s.InvestmentSecurities, &s.Securities,
			&s.AccountsReceivable, &s.Inventories,
			&s.NonCurrentLiabilities, &s.ShareholdersEquity,
			&s.MarketSegment, &s.Sector33, &s.Sector17,
			&s.LastPrice, &priceDate); err != nil {
			log.Printf("⚠️ Scan error: %v", err)
			continue
		}

		if priceDate.Valid {
			s.PriceDate = &priceDate.String
		}

		m := calcMetrics(s.LastPrice, s.SharesIssued, s.NetIncome, s.NetAssets, s.TotalAssets, s.CurrentAssets, s.Liabilities)
		s.MarketCap = m.MarketCap
		s.PER = m.PER
		s.PBR = m.PBR
		s.EPS = m.EPS
		s.ROE = m.ROE
		s.EquityRatio = m.EquityRatio
		s.NetNetRatio = m.NetNetRatio

		if rank, ok := rsMap[s.Code]; ok {
			rs := rank
			s.RS = &rs
		}

		if records, ok := financialsMap[s.Code]; ok {
			s.GrowthMetrics = calcGrowthMetrics(records)
		}

		stocks = append(stocks, s)
	}

	jsonData, err := json.MarshalIndent(stocks, "", "  ")
	if err != nil {
		log.Fatalf("JSON marshal error: %v", err)
	}
	if err := os.WriteFile("./web/stocks.json", jsonData, 0644); err != nil {
		log.Fatalf("Write error: %v", err)
	}

	withSales, withPrice, withRS := 0, 0, 0
	for _, s := range stocks {
		if s.NetSales > 0 {
			withSales++
		}
		if s.LastPrice > 0 {
			withPrice++
		}
		if s.RS != nil {
			withRS++
		}
	}
	fmt.Printf("✅ Generated stocks.json with %d records\n", len(stocks))
	fmt.Printf("  With sales: %d, With price: %d, With RS: %d\n", withSales, withPrice, withRS)
}
