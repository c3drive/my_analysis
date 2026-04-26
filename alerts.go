package main

import (
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// detectAlerts は当日の注目銘柄を検出して Markdown 形式で stdout 出力する
// 用途: GitHub Actions で日次実行 → Issue body として gh issue create
//
// 検出パターン:
//  1. RS 急上昇: 直近の RS - 過去5営業日の RS が +15以上
//  2. 業績修正開示: TDNET 当日開示で「修正」「上方」「下方」を含むもの
//  3. 出来高急増: 当日出来高が直近5営業日平均の 3倍超 かつ 株価上昇
func detectAlerts(targetDate string) {
	db, err := openServerDB()
	if err != nil {
		log.Fatalf("DB open: %v", err)
	}
	defer db.Close()

	fmt.Printf("# 📢 アラート - %s\n\n", targetDate)

	// 1. RS 急上昇
	rsAlerts := detectRSSurge(db, targetDate, 15)
	if len(rsAlerts) > 0 {
		fmt.Printf("## 🚀 RS 急上昇 (+15以上)\n\n")
		fmt.Println("| コード | 銘柄名 | RS当日 | RS過去 | 上昇幅 | 業種 |")
		fmt.Println("|---|---|---|---|---|---|")
		for _, a := range rsAlerts {
			fmt.Printf("| %s | %s | %d | %d | +%d | %s |\n",
				a.Code, a.Name, a.RSNow, a.RSPast, a.Delta, a.Sector)
		}
		fmt.Println()
	}

	// 2. 業績修正開示
	revAlerts := detectRevisionDisclosures(db, targetDate)
	if len(revAlerts) > 0 {
		fmt.Printf("## ⚠️ 業績修正開示\n\n")
		fmt.Println("| コード | 銘柄名 | 開示時刻 | 表題 | PDF |")
		fmt.Println("|---|---|---|---|---|")
		for _, a := range revAlerts {
			pdfLink := "-"
			if a.PdfURL != "" {
				pdfLink = "[📄](" + a.PdfURL + ")"
			}
			fmt.Printf("| %s | %s | %s | %s | %s |\n",
				a.Code, a.Name, strings.TrimPrefix(a.DateTime, targetDate+" "),
				a.Title, pdfLink)
		}
		fmt.Println()
	}

	// 3. 出来高急増
	volAlerts := detectVolumeSurge(db, targetDate, 3.0)
	if len(volAlerts) > 0 {
		fmt.Printf("## 📊 出来高急増 (5日平均×3超 + 株価上昇)\n\n")
		fmt.Println("| コード | 銘柄名 | 出来高倍率 | 終値 | 騰落率 | 業種 |")
		fmt.Println("|---|---|---|---|---|---|")
		for _, a := range volAlerts {
			fmt.Printf("| %s | %s | ×%.1f | %.0f | %+.2f%% | %s |\n",
				a.Code, a.Name, a.VolumeMultiple, a.Close, a.PriceChange, a.Sector)
		}
		fmt.Println()
	}

	// サマリ
	total := len(rsAlerts) + len(revAlerts) + len(volAlerts)
	if total == 0 {
		fmt.Println("_本日のアラートはありません_")
	} else {
		fmt.Printf("---\n_合計 %d 件: RS急上昇 %d件 / 業績修正 %d件 / 出来高急増 %d件_\n",
			total, len(rsAlerts), len(revAlerts), len(volAlerts))
	}
}

type rsAlert struct {
	Code, Name, Sector string
	RSNow, RSPast      int
	Delta              int
}

func detectRSSurge(db *sql.DB, targetDate string, minDelta int) []rsAlert {
	// 当日の RS
	rsNow := make(map[string]int)
	if rows, err := db.Query(`SELECT code, rs_rank FROM rs_db.rs_scores WHERE date = ?`, targetDate); err == nil {
		for rows.Next() {
			var c string
			var r int
			if rows.Scan(&c, &r) == nil {
				rsNow[c] = r
			}
		}
		rows.Close()
	}
	if len(rsNow) == 0 {
		return nil
	}

	// 5営業日前あたりの RS (最も近い過去日)
	pastDate, _ := time.Parse("2006-01-02", targetDate)
	pastDateStr := pastDate.AddDate(0, 0, -7).Format("2006-01-02")
	rsPast := make(map[string]int)
	if rows, err := db.Query(`
		SELECT code, rs_rank FROM rs_db.rs_scores rs1
		WHERE date = (SELECT MAX(date) FROM rs_db.rs_scores rs2 WHERE rs2.code = rs1.code AND rs2.date <= ?)`,
		pastDateStr); err == nil {
		for rows.Next() {
			var c string
			var r int
			if rows.Scan(&c, &r) == nil {
				rsPast[c] = r
			}
		}
		rows.Close()
	}

	// 銘柄名・業種
	nameMap := loadStockNames(db)

	var alerts []rsAlert
	for code, now := range rsNow {
		past, ok := rsPast[code]
		if !ok {
			continue
		}
		delta := now - past
		if delta >= minDelta {
			n := nameMap[code]
			alerts = append(alerts, rsAlert{
				Code: code, Name: n.name, Sector: n.sector,
				RSNow: now, RSPast: past, Delta: delta,
			})
		}
	}
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].Delta > alerts[j].Delta })
	if len(alerts) > 30 {
		alerts = alerts[:30]
	}
	return alerts
}

type revisionAlert struct {
	Code, Name, DateTime, Title, PdfURL string
}

func detectRevisionDisclosures(db *sql.DB, targetDate string) []revisionAlert {
	rows, err := db.Query(`
		SELECT code, name, disclosure_datetime, title, COALESCE(pdf_url,'')
		FROM tdnet_disclosures
		WHERE disclosure_datetime LIKE ? || '%'
		  AND (title LIKE '%修正%' OR title LIKE '%上方%' OR title LIKE '%下方%')
		ORDER BY disclosure_datetime DESC`, targetDate)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var alerts []revisionAlert
	for rows.Next() {
		var a revisionAlert
		if err := rows.Scan(&a.Code, &a.Name, &a.DateTime, &a.Title, &a.PdfURL); err == nil {
			alerts = append(alerts, a)
		}
	}
	return alerts
}

type volAlert struct {
	Code, Name, Sector string
	VolumeMultiple     float64
	Close              float64
	PriceChange        float64
}

func detectVolumeSurge(db *sql.DB, targetDate string, minMultiple float64) []volAlert {
	// 当日の全銘柄: 当日出来高 + 直近5日平均出来高 + 終値変化率
	rows, err := db.Query(`
		WITH today AS (
			SELECT code, close, volume FROM price_db.stock_prices WHERE date = ?
		),
		yesterday AS (
			SELECT code, close FROM price_db.stock_prices sp1
			WHERE date = (SELECT MAX(date) FROM price_db.stock_prices sp2 WHERE sp2.code = sp1.code AND sp2.date < ?)
		),
		recent AS (
			SELECT code, AVG(volume) AS avg_vol, COUNT(*) AS cnt
			FROM (
				SELECT code, volume FROM price_db.stock_prices
				WHERE date >= date(?, '-7 days') AND date < ?
			) GROUP BY code
		)
		SELECT t.code, t.close, t.volume, r.avg_vol, y.close
		FROM today t
		JOIN recent r ON t.code = r.code
		JOIN yesterday y ON t.code = y.code
		WHERE r.cnt >= 3 AND r.avg_vol > 0
		  AND t.volume > r.avg_vol * ? AND t.close > y.close`,
		targetDate, targetDate, targetDate, targetDate, minMultiple)
	if err != nil {
		return nil
	}
	defer rows.Close()

	nameMap := loadStockNames(db)

	var alerts []volAlert
	for rows.Next() {
		var code string
		var todayClose, todayVol, avgVol, yClose float64
		if err := rows.Scan(&code, &todayClose, &todayVol, &avgVol, &yClose); err != nil {
			continue
		}
		if yClose <= 0 {
			continue
		}
		n := nameMap[code]
		alerts = append(alerts, volAlert{
			Code: code, Name: n.name, Sector: n.sector,
			VolumeMultiple: todayVol / avgVol,
			Close:          todayClose,
			PriceChange:    (todayClose - yClose) / yClose * 100,
		})
	}
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].VolumeMultiple > alerts[j].VolumeMultiple })
	if len(alerts) > 30 {
		alerts = alerts[:30]
	}
	return alerts
}

type stockMeta struct {
	name, sector string
}

func loadStockNames(db *sql.DB) map[string]stockMeta {
	m := make(map[string]stockMeta)
	rows, err := db.Query(`SELECT code, name, COALESCE(sector_17, '') FROM stocks`)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var c, n, s string
		if rows.Scan(&c, &n, &s) == nil {
			m[c] = stockMeta{name: n, sector: s}
		}
	}
	return m
}
