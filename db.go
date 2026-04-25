package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "modernc.org/sqlite"
)

// --- DB初期化（3ファイル構成） ---

// initXbrlDB は財務データ用DB（xbrl.db）を初期化する
func initXbrlDB() (*sql.DB, error) {
	ensureDir()
	db, err := sql.Open("sqlite", "./data/xbrl.db")
	if err != nil {
		return nil, err
	}

	sqlStmt := `
	CREATE TABLE IF NOT EXISTS stocks (
		code TEXT PRIMARY KEY,
		name TEXT,
		updated_at DATETIME,
		-- 売上・利益
		net_sales INTEGER,
		operating_income INTEGER,
		net_income INTEGER,
		-- 資産・負債
		total_assets INTEGER,
		net_assets INTEGER,
		current_assets INTEGER,
		liabilities INTEGER,
		current_liabilities INTEGER,
		-- その他
		cash_and_deposits INTEGER,
		shares_issued INTEGER
	);`

	if _, err = db.Exec(sqlStmt); err != nil {
		return nil, fmt.Errorf("テーブル作成失敗: %w", err)
	}

	// マイグレーション（既存カラム追加）
	alterStatements := []string{
		"ALTER TABLE stocks ADD COLUMN operating_income INTEGER",
		"ALTER TABLE stocks ADD COLUMN net_income INTEGER",
		"ALTER TABLE stocks ADD COLUMN total_assets INTEGER",
		"ALTER TABLE stocks ADD COLUMN net_assets INTEGER",
		"ALTER TABLE stocks ADD COLUMN current_assets INTEGER",
		"ALTER TABLE stocks ADD COLUMN liabilities INTEGER",
		"ALTER TABLE stocks ADD COLUMN current_liabilities INTEGER",
		"ALTER TABLE stocks ADD COLUMN cash_and_deposits INTEGER",
		"ALTER TABLE stocks ADD COLUMN shares_issued INTEGER",
		"ALTER TABLE stocks ADD COLUMN investment_securities INTEGER",
		"ALTER TABLE stocks ADD COLUMN securities INTEGER",
		"ALTER TABLE stocks ADD COLUMN accounts_receivable INTEGER",
		"ALTER TABLE stocks ADD COLUMN inventories INTEGER",
		"ALTER TABLE stocks ADD COLUMN non_current_liabilities INTEGER",
		"ALTER TABLE stocks ADD COLUMN shareholders_equity INTEGER",
	}
	for _, stmt := range alterStatements {
		db.Exec(stmt)
	}

	// 四半期・通期の財務データ時系列テーブル
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS stock_financials (
		code TEXT NOT NULL,
		doc_type TEXT NOT NULL,
		submission_date TEXT NOT NULL,
		doc_description TEXT,
		net_sales INTEGER,
		operating_income INTEGER,
		net_income INTEGER,
		total_assets INTEGER,
		net_assets INTEGER,
		current_assets INTEGER,
		liabilities INTEGER,
		current_liabilities INTEGER,
		cash_and_deposits INTEGER,
		shares_issued INTEGER,
		investment_securities INTEGER,
		securities INTEGER,
		accounts_receivable INTEGER,
		inventories INTEGER,
		non_current_liabilities INTEGER,
		shareholders_equity INTEGER,
		PRIMARY KEY (code, submission_date)
	);`)
	if err != nil {
		log.Printf("⚠️ stock_financials table: %v", err)
	}

	// TDNET 適時開示情報テーブル
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS tdnet_disclosures (
		code TEXT NOT NULL,
		disclosure_datetime TEXT NOT NULL,
		name TEXT,
		title TEXT,
		doc_category TEXT,
		pdf_url TEXT,
		PRIMARY KEY (code, disclosure_datetime, title)
	);
	CREATE INDEX IF NOT EXISTS idx_tdnet_code ON tdnet_disclosures(code);
	CREATE INDEX IF NOT EXISTS idx_tdnet_datetime ON tdnet_disclosures(disclosure_datetime);
	`)
	if err != nil {
		log.Printf("⚠️ tdnet_disclosures table: %v", err)
	}

	return db, nil
}

// initPriceDB は株価データ用DB（stock_price.db）を初期化する
func initPriceDB() (*sql.DB, error) {
	ensureDir()
	db, err := sql.Open("sqlite", "./data/stock_price.db")
	if err != nil {
		return nil, err
	}

	sqlStmt := `
	CREATE TABLE IF NOT EXISTS stock_prices (
		code TEXT,
		date TEXT,
		open REAL,
		high REAL,
		low REAL,
		close REAL,
		volume INTEGER,
		PRIMARY KEY (code, date)
	);`
	if _, err = db.Exec(sqlStmt); err != nil {
		return nil, fmt.Errorf("株価テーブル作成失敗: %w", err)
	}

	return db, nil
}

// initRsDB はリラティブストレングス用DB（rs.db）を初期化する
func initRsDB() (*sql.DB, error) {
	ensureDir()
	db, err := sql.Open("sqlite", "./data/rs.db")
	if err != nil {
		return nil, err
	}

	sqlStmt := `
	CREATE TABLE IF NOT EXISTS rs_scores (
		code TEXT,
		date TEXT,
		rs_score REAL,
		rs_rank INTEGER,
		PRIMARY KEY (code, date)
	);`
	if _, err = db.Exec(sqlStmt); err != nil {
		return nil, fmt.Errorf("RSテーブル作成失敗: %w", err)
	}

	return db, nil
}

// openServerDB はサーバー用に全DBをATTACHした接続を返す
func openServerDB() (*sql.DB, error) {
	ensureDir()
	// メインはxbrl.db
	db, err := sql.Open("sqlite", "./data/xbrl.db")
	if err != nil {
		return nil, err
	}

	// 株価DBをアタッチ
	_, err = db.Exec(`ATTACH DATABASE './data/stock_price.db' AS price_db`)
	if err != nil {
		// stock_price.db が存在しなければ無視
		log.Printf("⚠️ stock_price.db attach: %v", err)
	}

	// RS DBをアタッチ
	_, err = db.Exec(`ATTACH DATABASE './data/rs.db' AS rs_db`)
	if err != nil {
		log.Printf("⚠️ rs.db attach: %v", err)
	}

	return db, nil
}

// migrateFromLegacyDB は旧 stock_data.db からデータを移行する
func migrateFromLegacyDB() {
	legacyPath := "./data/stock_data.db"
	if _, err := os.Stat(legacyPath); os.IsNotExist(err) {
		return // 旧DBなし
	}

	// xbrl.db が既にあればスキップ
	if _, err := os.Stat("./data/xbrl.db"); err == nil {
		var count int
		xdb, err := sql.Open("sqlite", "./data/xbrl.db")
		if err == nil {
			defer xdb.Close()
			xdb.QueryRow("SELECT COUNT(*) FROM stocks").Scan(&count)
			if count > 0 {
				fmt.Printf("📋 xbrl.db already has %d records, skipping migration\n", count)
				return
			}
		}
	}

	fmt.Println("🔄 Migrating from legacy stock_data.db...")

	legacyDB, err := sql.Open("sqlite", legacyPath)
	if err != nil {
		log.Printf("⚠️ Legacy DB open failed: %v", err)
		return
	}
	defer legacyDB.Close()

	// 財務データを移行
	xbrlDB, err := initXbrlDB()
	if err != nil {
		log.Printf("⚠️ xbrl.db init failed: %v", err)
		return
	}
	defer xbrlDB.Close()

	rows, err := legacyDB.Query(`SELECT code, name, COALESCE(updated_at, ''),
		COALESCE(net_sales,0), COALESCE(operating_income,0), COALESCE(net_income,0),
		COALESCE(total_assets,0), COALESCE(net_assets,0), COALESCE(current_assets,0),
		COALESCE(liabilities,0), COALESCE(current_liabilities,0),
		COALESCE(cash_and_deposits,0), COALESCE(shares_issued,0)
		FROM stocks`)
	if err != nil {
		log.Printf("⚠️ Legacy stocks query failed: %v", err)
		return
	}
	defer rows.Close()

	stockCount := 0
	for rows.Next() {
		var d FinancialData
		var code, name, updatedAt string
		if err := rows.Scan(&code, &name, &updatedAt,
			&d.NetSales, &d.OperatingIncome, &d.NetIncome,
			&d.TotalAssets, &d.NetAssets, &d.CurrentAssets,
			&d.Liabilities, &d.CurrentLiabilities,
			&d.CashAndDeposits, &d.SharesIssued); err != nil {
			continue
		}
		saveStock(xbrlDB, code, name, updatedAt, d)
		stockCount++
	}
	fmt.Printf("  ✅ Migrated %d stocks to xbrl.db\n", stockCount)

	// 株価データを移行
	priceDB, err := initPriceDB()
	if err != nil {
		log.Printf("⚠️ stock_price.db init failed: %v", err)
		return
	}
	defer priceDB.Close()

	pRows, err := legacyDB.Query(`SELECT code, date, open, high, low, close, volume FROM stock_prices`)
	if err != nil {
		log.Printf("⚠️ Legacy prices query failed: %v", err)
		return
	}
	defer pRows.Close()

	priceCount := 0
	for pRows.Next() {
		var p StockPrice
		pRows.Scan(&p.Code, &p.Date, &p.Open, &p.High, &p.Low, &p.Close, &p.Volume)
		savePricesToDB(priceDB, p.Code, []StockPrice{p})
		priceCount++
	}
	fmt.Printf("  ✅ Migrated %d price records to stock_price.db\n", priceCount)

	// RS DB初期化
	rsDB, err := initRsDB()
	if err != nil {
		log.Printf("⚠️ rs.db init failed: %v", err)
	} else {
		rsDB.Close()
		fmt.Println("  ✅ Created rs.db")
	}

	fmt.Println("🔄 Migration complete!")
}

// saveStock は銘柄データをDBに保存する（UPSERT: 既存の有効データを空データで上書きしない）
func saveStock(db *sql.DB, code, name, updatedAt string, data FinancialData) error {
	_, err := db.Exec(`
		INSERT INTO stocks (
			code, name, updated_at,
			net_sales, operating_income, net_income,
			total_assets, net_assets, current_assets,
			liabilities, current_liabilities, cash_and_deposits, shares_issued,
			investment_securities, securities, accounts_receivable, inventories,
			non_current_liabilities, shareholders_equity
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(code) DO UPDATE SET
			name = excluded.name,
			updated_at = CASE WHEN excluded.updated_at != '' THEN excluded.updated_at ELSE stocks.updated_at END,
			net_sales = CASE WHEN excluded.net_sales > 0 THEN excluded.net_sales ELSE stocks.net_sales END,
			operating_income = CASE WHEN excluded.operating_income > 0 THEN excluded.operating_income ELSE stocks.operating_income END,
			net_income = CASE WHEN excluded.net_income > 0 THEN excluded.net_income ELSE stocks.net_income END,
			total_assets = CASE WHEN excluded.total_assets > 0 THEN excluded.total_assets ELSE stocks.total_assets END,
			net_assets = CASE WHEN excluded.net_assets > 0 THEN excluded.net_assets ELSE stocks.net_assets END,
			current_assets = CASE WHEN excluded.current_assets > 0 THEN excluded.current_assets ELSE stocks.current_assets END,
			liabilities = CASE WHEN excluded.liabilities > 0 THEN excluded.liabilities ELSE stocks.liabilities END,
			current_liabilities = CASE WHEN excluded.current_liabilities > 0 THEN excluded.current_liabilities ELSE stocks.current_liabilities END,
			cash_and_deposits = CASE WHEN excluded.cash_and_deposits > 0 THEN excluded.cash_and_deposits ELSE stocks.cash_and_deposits END,
			shares_issued = CASE WHEN excluded.shares_issued > 0 THEN excluded.shares_issued ELSE stocks.shares_issued END,
			investment_securities = CASE WHEN excluded.investment_securities > 0 THEN excluded.investment_securities ELSE stocks.investment_securities END,
			securities = CASE WHEN excluded.securities > 0 THEN excluded.securities ELSE stocks.securities END,
			accounts_receivable = CASE WHEN excluded.accounts_receivable > 0 THEN excluded.accounts_receivable ELSE stocks.accounts_receivable END,
			inventories = CASE WHEN excluded.inventories > 0 THEN excluded.inventories ELSE stocks.inventories END,
			non_current_liabilities = CASE WHEN excluded.non_current_liabilities > 0 THEN excluded.non_current_liabilities ELSE stocks.non_current_liabilities END,
			shareholders_equity = CASE WHEN excluded.shareholders_equity > 0 THEN excluded.shareholders_equity ELSE stocks.shareholders_equity END
	`,
		code, name, updatedAt,
		data.NetSales, data.OperatingIncome, data.NetIncome,
		data.TotalAssets, data.NetAssets, data.CurrentAssets,
		data.Liabilities, data.CurrentLiabilities, data.CashAndDeposits, data.SharesIssued,
		data.InvestmentSecurities, data.Securities, data.AccountsReceivable, data.Inventories,
		data.NonCurrentLiabilities, data.ShareholdersEquity,
	)
	return err
}

// saveStockFinancial は四半期・通期の財務データを時系列テーブルに保存する
func saveStockFinancial(db *sql.DB, code, docType, submissionDate, docDescription string, data FinancialData) error {
	_, err := db.Exec(`
		INSERT INTO stock_financials (
			code, doc_type, submission_date, doc_description,
			net_sales, operating_income, net_income,
			total_assets, net_assets, current_assets,
			liabilities, current_liabilities, cash_and_deposits, shares_issued,
			investment_securities, securities, accounts_receivable, inventories,
			non_current_liabilities, shareholders_equity
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(code, submission_date) DO UPDATE SET
			doc_type = excluded.doc_type,
			doc_description = excluded.doc_description,
			net_sales = CASE WHEN excluded.net_sales > 0 THEN excluded.net_sales ELSE stock_financials.net_sales END,
			operating_income = CASE WHEN excluded.operating_income != 0 THEN excluded.operating_income ELSE stock_financials.operating_income END,
			net_income = CASE WHEN excluded.net_income != 0 THEN excluded.net_income ELSE stock_financials.net_income END,
			total_assets = CASE WHEN excluded.total_assets > 0 THEN excluded.total_assets ELSE stock_financials.total_assets END,
			net_assets = CASE WHEN excluded.net_assets > 0 THEN excluded.net_assets ELSE stock_financials.net_assets END,
			current_assets = CASE WHEN excluded.current_assets > 0 THEN excluded.current_assets ELSE stock_financials.current_assets END,
			liabilities = CASE WHEN excluded.liabilities > 0 THEN excluded.liabilities ELSE stock_financials.liabilities END,
			current_liabilities = CASE WHEN excluded.current_liabilities > 0 THEN excluded.current_liabilities ELSE stock_financials.current_liabilities END,
			cash_and_deposits = CASE WHEN excluded.cash_and_deposits > 0 THEN excluded.cash_and_deposits ELSE stock_financials.cash_and_deposits END,
			shares_issued = CASE WHEN excluded.shares_issued > 0 THEN excluded.shares_issued ELSE stock_financials.shares_issued END,
			investment_securities = CASE WHEN excluded.investment_securities > 0 THEN excluded.investment_securities ELSE stock_financials.investment_securities END,
			securities = CASE WHEN excluded.securities > 0 THEN excluded.securities ELSE stock_financials.securities END,
			accounts_receivable = CASE WHEN excluded.accounts_receivable > 0 THEN excluded.accounts_receivable ELSE stock_financials.accounts_receivable END,
			inventories = CASE WHEN excluded.inventories > 0 THEN excluded.inventories ELSE stock_financials.inventories END,
			non_current_liabilities = CASE WHEN excluded.non_current_liabilities > 0 THEN excluded.non_current_liabilities ELSE stock_financials.non_current_liabilities END,
			shareholders_equity = CASE WHEN excluded.shareholders_equity > 0 THEN excluded.shareholders_equity ELSE stock_financials.shareholders_equity END
	`,
		code, docType, submissionDate, docDescription,
		data.NetSales, data.OperatingIncome, data.NetIncome,
		data.TotalAssets, data.NetAssets, data.CurrentAssets,
		data.Liabilities, data.CurrentLiabilities, data.CashAndDeposits, data.SharesIssued,
		data.InvestmentSecurities, data.Securities, data.AccountsReceivable, data.Inventories,
		data.NonCurrentLiabilities, data.ShareholdersEquity,
	)
	return err
}
