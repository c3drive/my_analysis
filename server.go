package main

import (
	"fmt"
	"log"
	"net/http"
)

// --- 閲覧ロジック ---
func startServer() {
	// 旧DBからの移行
	migrateFromLegacyDB()

	// DB初期化（3ファイル構成）
	xdb, err := initXbrlDB()
	if err != nil {
		log.Printf("⚠️ xbrl.db init warning: %v", err)
	} else {
		xdb.Close()
	}
	pdb, err := initPriceDB()
	if err != nil {
		log.Printf("⚠️ stock_price.db init warning: %v", err)
	} else {
		pdb.Close()
	}
	rdb, err := initRsDB()
	if err != nil {
		log.Printf("⚠️ rs.db init warning: %v", err)
	} else {
		rdb.Close()
	}
	log.Println("✅ Database schema migrated successfully (3-DB)")

	fs := http.FileServer(http.Dir("./web"))
	http.Handle("/", fs)

	http.HandleFunc("/xbrl.db", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-sqlite3")
		http.ServeFile(w, r, "./data/xbrl.db")
	})

	// query.html (sqlite-wasm) 用に圧縮版DBを同一オリジンで配信
	http.HandleFunc("/data/xbrl.db.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		http.ServeFile(w, r, "./data/xbrl.db.gz")
	})
	http.HandleFunc("/data/stock_price.db.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		http.ServeFile(w, r, "./data/stock_price.db.gz")
	})
	http.HandleFunc("/data/rs.db.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		http.ServeFile(w, r, "./data/rs.db.gz")
	})

	// APIハンドラ登録（ドメイン別ファイルに分割）
	registerStockHandlers()
	registerDetailHandlers()
	registerOneilRanking()
	registerCycleRanking()
	registerValueRanking()
	registerDividendRanking()
	registerYutaiRanking()

	fmt.Println("🌐 Dashboard starting at http://localhost:8080")
	fmt.Println("📂 Serving static files from ./web/")
	fmt.Println("📊 API endpoint: http://localhost:8080/api/stocks")
	fmt.Println("📈 Price API: http://localhost:8080/api/prices/{code}")
	fmt.Println("🚀 O'Neil Ranking API: http://localhost:8080/api/oneil-ranking")
	fmt.Println("📉 Market Index API: http://localhost:8080/api/market-index/{code}")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
