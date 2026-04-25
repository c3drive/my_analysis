package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

// importJPX は JPX 上場銘柄一覧 CSV を取り込む
// JPX 公式 (https://www.jpx.co.jp/markets/statistics-equities/misc/01.html) から
// data_j.xls をダウンロードし、CSV (UTF-8) に変換したファイルを指定する
//
// 期待する CSV フォーマット (1行目はヘッダ):
//   日付, コード, 銘柄名, 市場・商品区分, 33業種コード, 33業種区分, 17業種コード, 17業種区分, 規模コード, 規模区分
func importJPX(filePath string) {
	if filePath == "" {
		log.Fatalf("-file=path/to/jpx.csv が必要 (JPX data_j.xls を CSV 変換したもの)")
	}

	f, err := os.Open(filePath)
	if err != nil {
		log.Fatalf("ファイルが開けません: %v", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // 列数の不一致を許容

	db, err := initXbrlDB()
	if err != nil {
		log.Fatalf("DB init failed: %v", err)
	}
	defer db.Close()

	fmt.Println("📥 Importing JPX listed stocks...")

	// インデックスを動的に決定 (列順は変動する可能性があるためヘッダ名で識別)
	header, err := r.Read()
	if err != nil {
		log.Fatalf("ヘッダ読み込み失敗: %v", err)
	}

	idx := func(needle string) int {
		for i, h := range header {
			if strings.Contains(h, needle) {
				return i
			}
		}
		return -1
	}

	codeIdx := idx("コード")
	nameIdx := idx("銘柄名")
	segIdx := idx("市場")
	s33Idx := idx("33業種区分")
	s17Idx := idx("17業種区分")

	if codeIdx < 0 || nameIdx < 0 {
		log.Fatalf("必須列(コード/銘柄名)が見つかりません。ヘッダ: %v", header)
	}

	insertCount := 0
	updateCount := 0
	skippedCount := 0

	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("⚠️ 行読み込みエラー: %v", err)
			continue
		}
		if len(row) <= codeIdx || len(row) <= nameIdx {
			continue
		}

		code := strings.TrimSpace(row[codeIdx])
		// 5桁(末尾0付き)を4桁に正規化
		if len(code) == 5 && strings.HasSuffix(code, "0") {
			code = code[:4]
		}
		if len(code) != 4 {
			skippedCount++
			continue
		}

		name := strings.TrimSpace(row[nameIdx])
		segment := safeIdx(row, segIdx)
		s33 := safeIdx(row, s33Idx)
		s17 := safeIdx(row, s17Idx)

		// ETF/REIT/インデックス系は除外（純粋な事業会社のみ）
		if strings.Contains(segment, "ETF") || strings.Contains(segment, "ETN") ||
			strings.Contains(segment, "REIT") || strings.Contains(segment, "出資") ||
			strings.Contains(segment, "PRO") {
			skippedCount++
			continue
		}

		// 既存レコードの有無確認
		var existing int
		db.QueryRow("SELECT COUNT(*) FROM stocks WHERE code = ?", code).Scan(&existing)

		if existing == 0 {
			// 新規挿入 (財務データはなし、JPX由来のメタデータのみ)
			_, err := db.Exec(`
				INSERT INTO stocks (code, name, market_segment, sector_33, sector_17)
				VALUES (?, ?, ?, ?, ?)`,
				code, name, segment, s33, s17)
			if err != nil {
				log.Printf("⚠️ Insert failed for %s: %v", code, err)
				continue
			}
			insertCount++
		} else {
			// 既存レコードに JPX メタデータを上書き (財務データは保持)
			_, err := db.Exec(`
				UPDATE stocks SET
					name = CASE WHEN ? != '' THEN ? ELSE name END,
					market_segment = ?,
					sector_33 = ?,
					sector_17 = ?
				WHERE code = ?`,
				name, name, segment, s33, s17, code)
			if err != nil {
				log.Printf("⚠️ Update failed for %s: %v", code, err)
				continue
			}
			updateCount++
		}
	}

	fmt.Printf("\n✅ JPX 取込完了: 新規=%d件, 更新=%d件, スキップ=%d件\n", insertCount, updateCount, skippedCount)

	// 業種別カバレッジを表示
	rows, err := db.Query(`SELECT sector_17, COUNT(*) FROM stocks WHERE sector_17 != '' GROUP BY sector_17 ORDER BY COUNT(*) DESC LIMIT 10`)
	if err == nil {
		defer rows.Close()
		fmt.Println("\n📊 17業種別 上位:")
		for rows.Next() {
			var sector string
			var count int
			rows.Scan(&sector, &count)
			fmt.Printf("  %s: %d社\n", sector, count)
		}
	}
}

func safeIdx(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}
