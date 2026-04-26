package main

import (
	"encoding/csv"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
)

// YutaiRecord は data/yutai.csv の1行を表す
type YutaiRecord struct {
	Code           string `json:"Code"`
	YutaiValueYen  int64  `json:"YutaiValueYen"`  // 年間優待換算額 (円)
	MinShares      int64  `json:"MinShares"`      // 必要最低株数 (通常 100)
	HoldMonths     int    `json:"HoldMonths"`     // 長期保有特典の発動月数 (なければ 0)
	Category       string `json:"Category"`       // 食品/外食/QUO/自社製品/カタログ/その他
	Note           string `json:"Note"`
}

// loadYutaiCSV は data/yutai.csv を読み込んで code → YutaiRecord のマップを返す
// 先頭が # の行とヘッダ行はスキップ。ファイルが無ければ空マップ + nil
func loadYutaiCSV() (map[string]YutaiRecord, error) {
	path := "./data/yutai.csv"
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]YutaiRecord{}, nil
		}
		return nil, err
	}
	defer f.Close()

	result := make(map[string]YutaiRecord)
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1 // 列数の揺れを許容
	r.Comment = '#'

	headerSeen := false
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("⚠️ yutai.csv parse: %v", err)
			continue
		}
		if len(row) == 0 {
			continue
		}
		// 1行目はヘッダ
		if !headerSeen {
			headerSeen = true
			if strings.EqualFold(strings.TrimSpace(row[0]), "code") {
				continue
			}
			// ヘッダなしで始まる場合は処理続行
		}
		if len(row) < 2 {
			continue
		}

		code := strings.TrimSpace(row[0])
		if code == "" || len(code) != 4 {
			continue
		}

		rec := YutaiRecord{Code: code}
		if len(row) > 1 {
			rec.YutaiValueYen, _ = strconv.ParseInt(strings.TrimSpace(row[1]), 10, 64)
		}
		if len(row) > 2 {
			rec.MinShares, _ = strconv.ParseInt(strings.TrimSpace(row[2]), 10, 64)
		}
		if rec.MinShares == 0 {
			rec.MinShares = 100 // デフォルト
		}
		if len(row) > 3 {
			n, _ := strconv.Atoi(strings.TrimSpace(row[3]))
			rec.HoldMonths = n
		}
		if len(row) > 4 {
			rec.Category = strings.TrimSpace(row[4])
		}
		if len(row) > 5 {
			rec.Note = strings.TrimSpace(row[5])
		}
		result[code] = rec
	}
	return result, nil
}
