package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

func registerCycleRanking() {
	// サイクル投資API: 業種(33)別のRS中央値・モメンタム + 構成銘柄
	http.HandleFunc("/api/cycle-ranking", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if cached, ok := cacheGet("api:cycle-ranking"); ok {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Write(cached)
			return
		}

		db, err := openServerDB()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()

		type Member struct {
			Code   string  `json:"code"`
			Name   string  `json:"name"`
			RS     int     `json:"rs"`
			Price  float64 `json:"price"`
			Volume int64   `json:"volume"`
		}
		type SectorEntry struct {
			Sector      string   `json:"sector"`
			MemberCount int      `json:"memberCount"`
			RSMedian    float64  `json:"rsMedian"`
			RSMomentum  float64  `json:"rsMomentum"` // 30日前→現在の中央値変化
			Score       float64  `json:"score"`
			Members     []Member `json:"members"`
		}

		// 1. 当日の最新 RS を一括取得
		rsNow := make(map[string]int)
		rows, err := db.Query(`
			SELECT code, rs_rank FROM rs_db.rs_scores rs1
			WHERE date = (SELECT MAX(date) FROM rs_db.rs_scores rs2 WHERE rs2.code = rs1.code)`)
		if err == nil {
			for rows.Next() {
				var c string
				var r int
				if rows.Scan(&c, &r) == nil {
					rsNow[c] = r
				}
			}
			rows.Close()
		}

		// 2. 約30日前の RS (各 code の最新日 - 30日 以前で最新)
		// 簡易実装: 全 rs_scores から最大日付を取り、その30日前を target にして集計
		var maxDate string
		db.QueryRow("SELECT MAX(date) FROM rs_db.rs_scores").Scan(&maxDate)
		past := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
		if maxDate != "" {
			t, perr := time.Parse("2006-01-02", maxDate)
			if perr == nil {
				past = t.AddDate(0, 0, -30).Format("2006-01-02")
			}
		}
		rsPast := make(map[string]int)
		rows, err = db.Query(`
			SELECT code, rs_rank FROM rs_db.rs_scores rs1
			WHERE date = (SELECT MAX(date) FROM rs_db.rs_scores rs2 WHERE rs2.code = rs1.code AND rs2.date <= ?)`, past)
		if err == nil {
			for rows.Next() {
				var c string
				var r int
				if rows.Scan(&c, &r) == nil {
					rsPast[c] = r
				}
			}
			rows.Close()
		}

		// 3. 最新株価 (出来高込み) を一括取得
		type priceData struct {
			price  float64
			volume int64
		}
		priceMap := make(map[string]priceData)
		rows, err = db.Query(`
			SELECT code, close, volume FROM price_db.stock_prices sp1
			WHERE date = (SELECT MAX(date) FROM price_db.stock_prices sp2 WHERE sp2.code = sp1.code)`)
		if err == nil {
			for rows.Next() {
				var c string
				var p float64
				var v int64
				if rows.Scan(&c, &p, &v) == nil {
					priceMap[c] = priceData{price: p, volume: v}
				}
			}
			rows.Close()
		}

		// 4. stocks から code, name, sector_33 をロード、業種別にグルーピング
		sectorMap := make(map[string][]Member)
		rows, err = db.Query(`
			SELECT code, name, COALESCE(sector_33, '') FROM stocks
			WHERE COALESCE(sector_33, '') != ''`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var code, name, sector string
			if rows.Scan(&code, &name, &sector) != nil {
				continue
			}
			rs, ok := rsNow[code]
			if !ok {
				continue // RS のない銘柄はサイクル分析対象外
			}
			pd := priceMap[code]
			sectorMap[sector] = append(sectorMap[sector], Member{
				Code: code, Name: name, RS: rs, Price: pd.price, Volume: pd.volume,
			})
		}
		rows.Close()

		// 5. 業種ごとに集計してスコア計算
		var entries []SectorEntry
		for sector, members := range sectorMap {
			if len(members) < 3 {
				continue // 構成銘柄が極端に少ない業種は除外
			}
			// RS 中央値 (現在)
			rsValues := make([]int, len(members))
			for i, m := range members {
				rsValues[i] = m.RS
			}
			sort.Ints(rsValues)
			medianNow := float64(rsValues[len(rsValues)/2])

			// RS 中央値 (過去): 同じメンバーで集計
			pastValues := make([]int, 0, len(members))
			for _, m := range members {
				if v, ok := rsPast[m.Code]; ok {
					pastValues = append(pastValues, v)
				}
			}
			medianPast := medianNow
			if len(pastValues) > 0 {
				sort.Ints(pastValues)
				medianPast = float64(pastValues[len(pastValues)/2])
			}
			momentum := medianNow - medianPast

			// メンバーを RS 降順ソート
			sort.Slice(members, func(i, j int) bool { return members[i].RS > members[j].RS })

			// スコア (100点満点)
			// - 業種RS百分位: 40 (中央値0-99 を 0-40 に正規化)
			// - 業種RSモメンタム: 30 (-50〜+50 を 0-30 に正規化)
			// - 個別最高RS: 20 (上位銘柄RS)
			// - 売買代金合計: 10 (相対評価のため後で正規化)
			score := medianNow*0.4 + (momentum+50)/100*30
			if len(members) > 0 {
				score += float64(members[0].RS) * 0.2
			}
			// 売買代金は最大値で正規化するため一旦保留 (後段で更新)

			entries = append(entries, SectorEntry{
				Sector:      sector,
				MemberCount: len(members),
				RSMedian:    medianNow,
				RSMomentum:  momentum,
				Score:       score,
				Members:     members,
			})
		}

		// 6. スコア降順
		sort.Slice(entries, func(i, j int) bool { return entries[i].Score > entries[j].Score })

		body, err := json.Marshal(entries)
		if err != nil {
			http.Error(w, "json marshal: "+err.Error(), http.StatusInternalServerError)
			return
		}
		cacheSet("api:cycle-ranking", body, 60*time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "MISS")
		w.Write(body)
	})

}
