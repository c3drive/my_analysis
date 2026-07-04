package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

func registerYutaiRanking() {
	// 株主優待API: 実質利回り (配当 + 優待換算) / 投資額 (100点満点)
	// 配点: 実質利回り(50) + 必要投資額(20, 30万円以下優先) + 長期保有特典(15) + カテゴリ実用性(15)
	http.HandleFunc("/api/yutai-ranking", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if cached, ok := cacheGet("api:yutai-ranking"); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Write(cached)
			return
		}

		yutaiMap, err := loadYutaiCSV()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(yutaiMap) == 0 {
			body := []byte("[]")
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
			return
		}

		db, err := openServerDB()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		type YutaiStock struct {
			Code             string   `json:"Code"`
			Name             string   `json:"Name"`
			Score            float64  `json:"Score"`
			LastPrice        float64  `json:"LastPrice"`
			MinInvestment    int64    `json:"MinInvestment"` // 株価 × 必要株数
			MinShares        int64    `json:"MinShares"`
			DividendPerShare float64  `json:"DividendPerShare"`
			YutaiValueYen    int64    `json:"YutaiValueYen"`
			HoldMonths       int      `json:"HoldMonths"`
			Category         string   `json:"Category"`
			Note             string   `json:"Note"`
			DividendYield    *float64 `json:"DividendYield"` // 配当利回り (DPS / 株価 * 100)
			YutaiYield       *float64 `json:"YutaiYield"`    // 優待利回り (yutai / 投資額 * 100)
			TotalYield       *float64 `json:"TotalYield"`    // 配当+優待 利回り (%)
			Sector33         string   `json:"Sector33,omitempty"`
			MarketSegment    string   `json:"MarketSegment,omitempty"`
		}

		// yutai.csv にあるコードのみ対象
		codes := make([]string, 0, len(yutaiMap))
		for c := range yutaiMap {
			codes = append(codes, c)
		}
		// IN 句生成 (SQLite は ? の連続でOK)
		placeholders := strings.Repeat("?,", len(codes))
		placeholders = strings.TrimSuffix(placeholders, ",")
		args := make([]interface{}, len(codes))
		for i, c := range codes {
			args[i] = c
		}

		query := `
			SELECT s.code, s.name,
			       COALESCE(s.dividend_per_share, 0.0),
			       COALESCE(s.market_segment, ''), COALESCE(s.sector_33, ''),
			       COALESCE(p.close, 0) as last_price
			FROM stocks s
			LEFT JOIN (
				SELECT code, close FROM price_db.stock_prices sp1
				WHERE date = (SELECT MAX(date) FROM price_db.stock_prices sp2 WHERE sp2.code = sp1.code)
			) p ON s.code = p.code
			WHERE s.code IN (` + placeholders + `)`
		rows, err := db.Query(query, args...)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var stocks []YutaiStock
		for rows.Next() {
			var code, name, marketSegment, sector33 string
			var dps, lastPrice float64
			if err := rows.Scan(&code, &name, &dps, &marketSegment, &sector33, &lastPrice); err != nil {
				continue
			}
			y, ok := yutaiMap[code]
			if !ok {
				continue
			}
			ys := YutaiStock{
				Code:             code,
				Name:             name,
				LastPrice:        lastPrice,
				MinShares:        y.MinShares,
				DividendPerShare: dps,
				YutaiValueYen:    y.YutaiValueYen,
				HoldMonths:       y.HoldMonths,
				Category:         y.Category,
				Note:             y.Note,
				MarketSegment:    marketSegment,
				Sector33:         sector33,
			}

			ys.MinInvestment = int64(lastPrice) * y.MinShares

			if lastPrice > 0 && dps > 0 {
				v := dps / lastPrice * 100
				ys.DividendYield = &v
			}
			if ys.MinInvestment > 0 && y.YutaiValueYen > 0 {
				v := float64(y.YutaiValueYen) / float64(ys.MinInvestment) * 100
				ys.YutaiYield = &v
			}
			if ys.MinInvestment > 0 {
				div := dps * float64(y.MinShares) // 配当額 (年間)
				total := (div + float64(y.YutaiValueYen)) / float64(ys.MinInvestment) * 100
				ys.TotalYield = &total
			}

			// スコア計算 (100点満点)
			score := 0.0

			// 実質利回り 50点 (≥6%で満点)
			if ys.TotalYield != nil {
				switch {
				case *ys.TotalYield >= 6:
					score += 50
				case *ys.TotalYield >= 5:
					score += 42
				case *ys.TotalYield >= 4:
					score += 32
				case *ys.TotalYield >= 3:
					score += 22
				case *ys.TotalYield >= 2:
					score += 10
				}
			}

			// 必要投資額 20点 (10万以下で満点、30万までは段階的)
			switch {
			case ys.MinInvestment > 0 && ys.MinInvestment <= 100_000:
				score += 20
			case ys.MinInvestment <= 200_000:
				score += 14
			case ys.MinInvestment <= 300_000:
				score += 9
			case ys.MinInvestment <= 500_000:
				score += 4
			}

			// 長期保有特典 15点
			switch {
			case y.HoldMonths >= 36:
				score += 15
			case y.HoldMonths >= 12:
				score += 10
			case y.HoldMonths > 0:
				score += 5
			}

			// カテゴリ実用性 15点 (主観だが、現金等価物が高評価)
			switch y.Category {
			case "QUOカード", "QUO", "クオカード", "ギフトカード", "カタログ":
				score += 15
			case "食品", "外食":
				score += 12
			case "自社製品":
				score += 8
			default:
				score += 5
			}

			ys.Score = score
			stocks = append(stocks, ys)
		}

		sort.Slice(stocks, func(i, j int) bool { return stocks[i].Score > stocks[j].Score })

		body, err := json.Marshal(stocks)
		if err != nil {
			http.Error(w, "json marshal: "+err.Error(), http.StatusInternalServerError)
			return
		}
		cacheSet("api:yutai-ranking", body, 60*time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "MISS")
		w.Write(body)
	})

}
