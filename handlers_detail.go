package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

func registerDetailHandlers() {
	// 個別銘柄の財務時系列API（四半期・通期）
	http.HandleFunc("/api/financials/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		code := strings.TrimPrefix(r.URL.Path, "/api/financials/")
		if code == "" {
			http.Error(w, "code required", http.StatusBadRequest)
			return
		}

		db, err := openServerDB()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		type FinancialPoint struct {
			DocType         string `json:"doc_type"`
			SubmissionDate  string `json:"submission_date"`
			DocDescription  string `json:"doc_description"`
			NetSales        int64  `json:"net_sales"`
			OperatingIncome int64  `json:"operating_income"`
			NetIncome       int64  `json:"net_income"`
			TotalAssets     int64  `json:"total_assets"`
			NetAssets       int64  `json:"net_assets"`
			SharesIssued    int64  `json:"shares_issued"`
		}

		rows, err := db.Query(`
			SELECT doc_type, submission_date, COALESCE(doc_description, ''),
			       COALESCE(net_sales, 0), COALESCE(operating_income, 0), COALESCE(net_income, 0),
			       COALESCE(total_assets, 0), COALESCE(net_assets, 0), COALESCE(shares_issued, 0)
			FROM stock_financials
			WHERE code = ?
			ORDER BY submission_date ASC`, code)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]FinancialPoint{})
			return
		}
		defer rows.Close()

		var points []FinancialPoint
		for rows.Next() {
			var p FinancialPoint
			if err := rows.Scan(&p.DocType, &p.SubmissionDate, &p.DocDescription,
				&p.NetSales, &p.OperatingIncome, &p.NetIncome,
				&p.TotalAssets, &p.NetAssets, &p.SharesIssued); err != nil {
				continue
			}
			points = append(points, p)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(points)
	})

	// 個別銘柄の TDNET 適時開示一覧API
	http.HandleFunc("/api/disclosures/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		code := strings.TrimPrefix(r.URL.Path, "/api/disclosures/")
		if code == "" {
			http.Error(w, "code required", http.StatusBadRequest)
			return
		}

		db, err := openServerDB()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		type Disclosure struct {
			DisclosureDateTime string `json:"disclosure_datetime"`
			Title              string `json:"title"`
			DocCategory        string `json:"doc_category"`
			PdfURL             string `json:"pdf_url"`
		}

		rows, err := db.Query(`
			SELECT disclosure_datetime, COALESCE(title,''), COALESCE(doc_category,''), COALESCE(pdf_url,'')
			FROM tdnet_disclosures
			WHERE code = ?
			ORDER BY disclosure_datetime DESC
			LIMIT 50`, code)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]Disclosure{})
			return
		}
		defer rows.Close()

		var items []Disclosure
		for rows.Next() {
			var d Disclosure
			if err := rows.Scan(&d.DisclosureDateTime, &d.Title, &d.DocCategory, &d.PdfURL); err != nil {
				continue
			}
			items = append(items, d)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(items)
	})

	// 市場指数データAPI（市場天井検出用）
	http.HandleFunc("/api/market-index/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		code := strings.TrimPrefix(r.URL.Path, "/api/market-index/")
		if code == "" {
			code = "^NKX" // デフォルトは日経225
		}

		db, err := openServerDB()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		// 直近2年分に制限 (市場天井検出には十分。全期間は数十万行で重い)
		rows, err := db.Query(`
			SELECT code, date, open, high, low, close, volume
			FROM price_db.stock_prices
			WHERE code = ?
			  AND date >= date('now', '-730 days')
			ORDER BY date ASC`, code)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var prices []StockPrice
		for rows.Next() {
			var p StockPrice
			if err := rows.Scan(&p.Code, &p.Date, &p.Open, &p.High, &p.Low, &p.Close, &p.Volume); err != nil {
				continue
			}
			prices = append(prices, p)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(prices)
	})

}
