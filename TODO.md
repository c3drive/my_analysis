# EDINET XBRL Analyzer - TODO リスト

**参考**: [kawasin73さんの記事](https://kawasin73.hatenablog.com/entry/2025/11/20/224346)

**最終更新**: 2026-04-25（株価カバレッジ拡大・接続プール修正完了）

---

## 🏗️ アーキテクチャ・インフラ

> **方針**: S3は使わず、GitHub Releases + GitHub Pages でシンプルに構成

### 0. データソース拡張
- [x] **TDNET対応 (段階1: メタデータ)** ✅
  - [x] 適時開示メタデータの取得（日付・コード・会社名・表題・PDF URL）
  - [x] tdnet_disclosures テーブル
  - [x] -mode=fetch-tdnet -date=YYYY-MM-DD
  - [x] /api/disclosures/{code} エンドポイント
  - [x] 銘柄詳細に「最近の適時開示」セクション
  - 制約: TDNET 公式サイトは過去31日分のみ。古いデータはバックフィル不可
- [x] **TDNET対応 (段階2: PDF パース)** ✅ 決算短信PDFから財務データ抽出
  - [x] poppler-utils (pdftotext) を Dockerfile に追加
  - [x] tanshin.go: PDF DL → pdftotext → 正規表現抽出
  - [x] -mode=parse-tanshin -date=YYYY-MM-DD モード
  - [x] 主要5項目 (売上高/営業利益/純利益/総資産/純資産) を抽出
  - [x] stock_financials に doc_type='SHORT_REPORT' で保存
  - 注意: 決算短信フォーマット差異により抽出精度はベストエフォート
  - 注意: PDF は一時ファイル処理 (保存しない)
- [x] **決算発表日の特定** ✅ tdnet_disclosures + 決算キーワード判定
- [x] **chart 上の決算発表期間ハイライト** ✅ 銘柄詳細チャートにマーカー表示

### 1. ストレージ構成（GitHub Releases）
- [x] SQLiteファイルをGitHub Releasesにアップロード（daily-update.yml）
- [ ] **Raw XBRL files** をtar.gz化してReleasesに保存（オプション）
- [x] gzip圧縮版DBの配置（daily-update.ymlで実装済み）

### 2. GitHub Actions 拡張
- [x] 最新ReleasesからSQLiteダウンロード
- [x] 更新済みSQLiteをReleasesにアップロード
- [x] **GitHub Actions Summary** にDB downloadリンク表示
- [x] 解析ページリンクをSummaryに出力
- [x] **新規銘柄発見時のIssue作成**（Daily Notification）

### 3. GitHub Pages ページ構成
- [x] `oneil-screen.html` - オニール成長株アナライザー（プレースホルダー）
- [x] `net-net-value.html` - ネットネットバリュー（プレースホルダー）
- [x] `market-top.html` - マーケット天井検出（プレースホルダー）
- [x] sqlite-wasm または 静的JSON でDB読み込み（Go API + 静的JSONハイブリッド対応済み）

### 4. Use Cases 対応
- [ ] **Local Development**: GitHub Releases からDB取得
- [ ] **Local Evaluation**: ブラウザからSQLite/JSON読み込み
- [ ] **Quick Evaluation**: GitHub Pages で直接解析
- [ ] **Daily Notification**: GitHub Issue → メール通知

---

## 🔴 高優先度（記事の主要機能を再現するために必須）

### 🚨 最重要課題：データが古い＆少ない問題（2026-02-22 詳細調査）

#### 📊 現状データ（ローカルDB `data/stock_data.db`）
| 指標 | 値 | 問題 |
|------|------|------|
| 全銘柄数 | 640社 | 東証上場 ~3,900社に対し16%しかカバーしていない |
| 売上データあり | 37社 (5.8%) | 大半が売上0 → 財務データが取れていない |
| 純利益データあり | 19社 (3.0%) | ほぼ分析不能 |
| 総資産データあり | 20社 (3.1%) | |
| 株価データあり | 490社 (76.5%) | Stooqで取得 → 比較的OK |
| DB内の最古日付 | (空) | 638件は `updated_at` が空 |
| DB内の最新日付 | 2025-12-25 | **2ヶ月前のデータ** |

#### 🔍 根本原因分析

**原因1: daily-update.yml が新規データを蓄積していない**
- GitHub Actionsは毎日 `success` で完了している（run 36〜40: 2/17〜2/21全成功）
- しかし、各run は30秒以内で終了 → **XBRLダウンロード0件の可能性大**
- ワークフローは最新リリースDBを復元 → 当日1日分だけ追加 → 新リリース作成
- **当日にEDINET提出がない日**（休日、祝日）は何も追加されずにDBがそのまま再アップロードされる
- タグ `data-2026-01-24` 以降にDBにデータが蓄積されていない（`latest`タグもここで止まっている）

**原因2: 初期データが2025-12-25の1日分のみ**
- `runCollector` は `-date` で指定した1日分のみ取得する設計
- DB内の640件は初回（2025-12-25）のみで取得されたもの
- その後の daily-update で追加された件数が極めて少ない（各日10〜30件程度）
- **過去の有価証券報告書を遡って取得する仕組みがない**

**原因3: deploy-pages.yml がハードコードされた古い日付を使用**
- `deploy-pages.yml` 28行目: `go run main.go -mode=run -date=2025-11-13` → **固定日付**
- GitHub Pages のデプロイは `2025-11-13` のデータのみで静的JSON(`stocks.json`)を生成
- デプロイ環境のダッシュボードも古いデータ

**原因4: XBRLパース成功率が低い**
- 640件中37件（5.8%）しか売上データを取得できていない
- XBRLの正規表現パターンがEDINETの実際のフォーマットと合っていない可能性
- 書類をダウンロードしても財務データの抽出に失敗 → 空データとして保存される

#### ✅ 対策TODO（優先度順）

**Phase 1: 即効性のある修正** ✅ 完了
- [x] **deploy-pages.yml の日付をハードコードから動的に変更**
  - 固定日付を廃止 → 最新リリースDBをダウンロードする方式に変更
  - daily-update完了後に `workflow_run` で自動デプロイをトリガー
- [x] **daily-update.yml にデータ蓄積ログを追加**
  - XBRLダウンロード・パース結果の件数をログに出力
  - 0件の場合にIssue通知（`data-quality`ラベル付き）
- [x] **daily-update.yml を複数日遡って取得する形に改善**
  - 当日だけでなく過去7日分を順番に取得（手動実行時に日数変更可能）
  - `INSERT OR REPLACE` なので重複問題なし
- [x] **deploy-pages.yml の gen_json.go を全カラム対応**
  - `Code, Name, NetSales, UpdatedAt` → 全財務データ + 株価 + 指標

**Phase 2: データ量の大幅改善（バッチ取得）**
- [x] **過去1年分のEDINETデータを一括取得するバッチモード実装**
  - `main.go` に `-mode=batch -from=2025-04-01 -to=2026-02-22` モード追加
  - 日付を1日ずつ走査して `runCollector` を繰り返し呼び出す
  - EDINET API のレートリミットに注意（1秒間隔のスリープ等）
  - 土日は自動スキップ

**Phase 3: XBRLパース改善**
- [x] **XBRLパースの正規表現を改善**（EDINETの実際のタグ形式を調査）
- [ ] **テスト用にXBRLファイルをダウンロードして解析ログを出力**
- [x] **有価証券報告書（docTypeCode=120）を優先的にパース**
- [x] **四半期報告書（140）・半期報告書（160）のXBRL形式にも対応**

**Phase 4: データカバレッジ拡大**
- [x] **日本株全銘柄リスト**を外部から取得（JPX証券コード一覧CSV）✅ -mode=import-jpx
  - JPX data_j.xls を CSV 化して取込
  - market_segment / sector_33 / sector_17 を stocks テーブルに追加
- [x] **EDINET提出書類のない企業の基本情報を補完** ✅ JPX 取込で新規銘柄が追加される
- [x] **deploy-pages.yml の gen_json.go を最新の全カラムに対応** ✅ `-mode=export-json` に統合
  - インラインGoコード(180行)を廃止 → `go run main.go -mode=export-json` に置換
  - main.goの`calcMetrics()`を直接利用するため、列追加時の二重メンテが不要に


### 5. データベース構成の変更（3ファイル構成） ✅ 完了
現在: 3つのDBファイル構成（旧stock_data.dbから自動マイグレーション対応）

- [x] `xbrl.db` - 財務データ用DB（56KB, 640銘柄）
- [x] `stock_price.db` - 株価データ用DB（7.7MB, 119,922レコード）
- [x] `rs.db` - リラティブストレングス用DB（テーブル構造作成済み）
- [x] サーバーで `ATTACH DATABASE` によるクロスDB JOIN
- [x] `migrateFromLegacyDB()` による自動移行

### 6. XBRLパース項目の拡張

#### 資産項目（ネットネット計算用）
- [x] `cash_and_deposits` - 現金及び預金（main.go実装済み）
- [x] ~~`deposits` - 預金~~ → `cash_and_deposits` で合算取得済み (単独開示は稀)
- [x] `investment_securities` - 投資有価証券 ✅ XBRLパース・DB・UI対応
- [x] `accounts_receivable` - 売掛金 ✅ XBRLパース・DB・UI対応 (NotesAndAccountsReceivableTrade含む)
- [x] `marketable_securities` - 有価証券 ✅ `securities`として実装
- [x] ~~`notes_receivable` - 受取手形~~ → `accounts_receivable` 内で `NotesAndAccountsReceivableTrade` をフォールバック取得済み
- [x] `inventories` - 棚卸資産 ✅ XBRLパース・DB・UI対応

#### 負債項目
- [x] `liabilities` - 負債合計（main.go実装済み）
- [x] `current_liabilities` - 流動負債（main.go実装済み）
- [x] `non_current_liabilities` - 固定負債 ✅ XBRLパース・DB・UI対応

#### 利益・資本項目
- [x] `net_income` - 純利益（main.go実装済み）
- [x] `operating_income` - 営業利益（main.go実装済み）
- [x] `net_assets` - 純資産（main.go実装済み）
- [x] `total_assets` - 総資産（main.go実装済み）
- [x] `current_assets` - 流動資産（main.go実装済み）
- [x] `shareholders_equity` - 株主資本 ✅ XBRLパース・DB・UI対応
- [x] `eps` - 1株当たり利益（純利益÷発行済株式数で計算）
- [x] `number_of_shares` - 発行済株式数（main.go実装済み）
- [x] `roe` - 自己資本利益率（純利益÷純資産×100で計算）

### 7. 株価データの取得
- [x] 外部APIから日次株価を取得（Stooq API使用）
- [x] 株価テーブル（code, date, open, high, low, close, volume）
- [x] 時価総額の計算（株価 × 発行済株式数）
- [x] 自己資本比率の計算・表示
- [x] PER（株価収益率）の計算・表示
- [x] PBR（株価純資産倍率）の計算・表示
- [x] ネットネット値の計算・表示

---

## 📊 オニール成長株アナライザー機能

### 4. パラメータ設定UI ✅ 完了

#### 基本設定
- [ ] 基準日: yyyy/mm/dd（日付ピッカー）
- [ ] 利益タイプ選択: 純利益 (Net Income) / 営業利益

#### スクリーニング閾値（折りたたみパネル「⚙️ スコアパラメータをカスタマイズ」内）
- [x] ROE 閾値スライダー（デフォルト 15%）
- [x] PER 上限スライダー（デフォルト 15倍）
- [x] PBR 上限スライダー（デフォルト 1.5倍）
- [x] 自己資本率 閾値スライダー（デフォルト 40%）
- [x] RS 閾値スライダー（デフォルト 80）
- [x] Q0 EPS YoY 閾値スライダー（デフォルト 25%）
- [x] 3Y EPS CAGR 閾値スライダー（デフォルト 15%）

#### ランキングウェイト ✅ 完了
- [x] 各指標のウェイトスライダー（0〜40点、即座にスコア再計算）
- [x] デフォルトに戻すボタン

### 5. 成長株ランキングテーブル（✅ 基本実装完了）
| カラム | 説明 | 実装状態 |
|--------|------|---------|
| コード | 証券コード | ✅ |
| 企業名 | 会社名 | ✅ |
| SCORE | 加重スコア | ✅ |
| Q0 EPS% | 直近四半期EPS成長率 | 🟡 stock_financials蓄積中 |
| Q1 EPS% | 1四半期前EPS成長率 | 🟡 stock_financials蓄積中 |
| 2Y CAGR% | 2年間年平均成長率 | 🟡 stock_financials蓄積中 |
| Y0 EPS% | 今期EPS成長率 | 🟡 stock_financials蓄積中 |
| ROE% | 自己資本利益率 | ✅ |
| RS | リラティブストレングス | ✅ RS列追加済み |
| 自己資本率% | 自己資本比率 | ✅ |
| 時価総額(億) | 時価総額 | ✅ |


### 6. 銘柄詳細ページ 🟡 改善中

#### ヘッダー
- [x] ←ランキングに戻る ナビゲーション
- [x] 会社名 + 証券コード表示
- [x] 日足/週足 切り替えボタン ✅ 実装済み
- [x] 市場区分・業種バッジ表示 ✅ 追加

#### 株価チャート
- [x] ローソク足チャート
- [x] 出来高バー
- [x] 移動平均線
- [x] **決算発表マーカー** ✅ TDNET開示データから📊/⚠/💰マーカーを自動表示

#### 主要指標カード
- [x] 最新ROE (通期): XX.X%
- [x] 最新EPS YoY (四半期): XXXX.X% ✅ Q0EPSYoY指標カード追加
- [x] 最新売上 YoY (四半期): XX.X% ✅ Q0SalesYoY指標カード追加
- [x] 最新EPS YoY (通期): XX.X% ✅ Y0EPSYoY指標カード追加
- [x] 3年EPS CAGR: XX.X% ✅ EPS3YCAGR指標カード追加
- [x] RS: XX ✅ 追加済み

### 7. 財務データ詳細ページ（複数チャートグリッド） ✅ 完了
- [x] EPS 推移 (Log) 折れ線チャート
- [x] EPS YoY 成長率 棒グラフ
- [x] 売上 推移 (Log) 折れ線チャート
- [x] 売上 YoY 成長率 棒グラフ
- [x] ROE (通期) 折れ線チャート
- [x] 純資産・総資産 折れ線チャート（複数系列）

---

## 📈 市場天井検出ツール機能 ✅ v1実装完了

### 8. 基本設定
- [x] ~~DBファイル選択（Choose File）~~ → 銘柄コード選択プルダウンに変更
- [x] 銘柄選択プルダウン（株価データのある全銘柄を表示）

### 9. マーカー表示トグル
- [x] 分配日 (D) ON/OFF スイッチ
- [x] ストーリング (S) ON/OFF スイッチ

### 10. 分配日 (Distribution) パラメータ
- [x] 下落率 (以上): スライダー（デフォルト -0.2%）
- [x] 条件表示: 終値変化率 <= -0.20% AND 出来高 >= 前日

### 11. ストーリング (Stalling) パラメータ
- [x] 変化率 (下限): スライダー（デフォルト -0.1%）
- [x] 変化率 (上限): スライダー（デフォルト +0.3%）
- [x] 条件表示

### 12. 共通条件: 出来高
- [x] 平均日数 (Y): スライダー（デフォルト 25日）
- [x] 増加率 (X): スライダー（デフォルト 5.0%）
- [x] AND条件チェックボックス ✅ 分配日に平均出来高超え条件を追加可能

### 13. 注意期間 (背景色) パラメータ
- [x] 判定日数 (N): スライダー（デフォルト 25日）
- [x] マーカー個数 合計 (M): スライダー（デフォルト 6個）
- [x] 分配日 閾値 (M_d): スライダー（デフォルト 4個）

### 14. 市場天井チャート
- [x] ローソク足チャート（lightweight-charts v4）
- [x] 出来高バー（分配日・ストーリング日はカラーコーディング）
- [x] **注意期間のオレンジ色キャンドルハイライト**
- [x] 分配日マーカー（赤 D 矢印）
- [x] ストーリングマーカー（緑 S 矢印）
- [x] 水平ライン（価格レベル） ✅ ダブルクリックで追加、右クリックで最寄りライン削除
- [x] シグナルサマリーカード（分配日数・ストーリング数・注意期間数・現在の状態）
- [x] 検出イベントログ（日付・種別・変化率・出来高の一覧）
- [x] リアルタイムパラメータ調整（スライダー操作で即座に再分析）

---

## 💎 ネットネットバリュー機能

### 15. PBR計算式のカスタマイズ
- [x] 資産項目の選択UI（係数付き）
- [x] 負債項目の選択UI（係数付き）
- [x] 項目の追加・削除機能
- [x] スペシャルPBR計算

### 16. 計算オプション
- [x] 上位N社を計算（全件表示）
- [x] 特定日で計算（日付ピッカー）✅ /api/stocks-as-of/{date} で過去株価による再計算
- [x] 証券コードで追加

### 17. タブ切り替え
- [x] ランキングタブ
- [x] PBRチャートタブ ✅ Canvas散布図実装
- [x] カスタムチャートタブ ✅ 業種別集計棒グラフ（17/33業種 × 6種指標）

### 18. ランキングテーブル
- [x] 順位
- [x] 証券コード
- [x] 会社名
- [x] スペシャルPBR（クリックで詳細）
- [x] PBR
- [x] 自己資本比率 (%)
- [x] 時価総額 (億円)

---

## 🟡 中優先度

### 19. チャートライブラリ ✅ 導入完了
- [x] **lightweight-charts (TradingView)** の導入
- [x] ローソク足チャート（stock-detail.html, market-top.html）
- [x] 出来高チャート
- [x] 折れ線チャート（stock-detail.html: ライン/エリア切替）
- [x] 棒グラフ ✅ stock-detail の財務時系列 EPS YoY/売上 YoY で HistogramSeries 使用
- [x] マーカー表示（market-top.html: D/Sマーカー）
- [x] 背景色ハイライト（market-top.html: 注意期間）

### 20. リラティブストレングス (RS) 計算 ✅ 完了
- [x] RS値の計算ロジック（3ヶ月×40% + 6ヶ月×20% + 9ヶ月×20% + 12ヶ月×20%）
- [x] RS用DBへの保存（rs.db / rs_scoresテーブル）
- [x] RS推移チャート ✅ lightweight-chartsエリアチャート実装

### 21. 新規銘柄発見時の通知 ✅ 完了
- [x] 条件を満たす銘柄の検出（過去7日以内の更新）
- [x] GitHub Issue自動作成（new-stockラベル）
- [x] 銘柄詳細情報をIssue本文に

### 22. ローカルDBファイルアップロード ✅ 完了 (query.html)
- [x] Choose File ボタン (.db / .db.gz 対応、pako で gunzip)
- [x] GitHub Release から自動取得ボタン
- [x] DB読込状態表示

---

## 🟠 コードレビュー指摘事項（2026-03-23） ✅ 全完了

### 26. gen_json.go: stocks.jsonにRS値・投資指標が未含有 ✅ 完了
- [x] `deploy-pages.yml`内の`gen_json.go`でrs.dbをATTACHし、RSランクをstocks.jsonに含める
- [x] EquityRatio, PER, PBR等の投資指標もstocks.jsonに含める（GitHub Pagesでオニールスクリーニングが動くように）
- ~~現状: GitHub Pages版はRS無しで動作~~ → MarketCap, PER, PBR, EPS, ROE, EquityRatio, NetNetRatio, RS を出力

### 27. /api/prices/ のDB接続方式 ✅ 完了
- [x] `/api/prices/`, `/api/market-index/`, `/api/available-codes` が`sql.Open`で毎回直接stock_price.dbを開いていた
- [x] `openServerDB()`のATTACH DB方式に統一して`price_db.stock_prices`でクエリ

### 28. net-net-value.html: カスタム項目のロジック改善 ✅ 完了
- [x] カスタム資産項目の計算が`fixedAssets * coeff * 0.1`で固定的だった
- [x] DBカラム（現金、流動資産、固定資産、総資産、純資産、負債、流動負債）から選択 + 係数 + 加算/減算切替の方式に改善

## 🟠 コードレビュー指摘事項（2026-04-18） ✅ 全完了

### 29. /api/stocks の N+1 クエリ ✅ 完了
- [x] RS値をループ内で毎回`QueryRow`していた → 事前に全RS値を`map[string]float64`に一括読み込みに変更

### 30. 散布図の軸レンジ計算バグ ✅ 完了
- [x] `xMin * 0.9` が負の値で逆転する問題 → データレンジに対する±5%マージン方式に修正

### 31. /api/rs/ の CORS ヘッダー欠落 ✅ 完了
- [x] `Access-Control-Allow-Origin: *` を追加（他エンドポイントと統一）

### 32. rows.Scan エラーハンドリング ✅ 完了
- [x] 全8箇所の `rows.Scan` にエラーチェック追加（無言の不正値混入を防止）

### 33. 水平ライン削除UX改善 ✅ 完了
- [x] 右クリックで「最後に追加したライン」→「クリック位置に最も近いライン」を削除する方式に変更

### 34. 散布図リサイズ対応 ✅ 完了
- [x] `ResizeObserver` を追加（ウィンドウリサイズ時に散布図が再描画される）

### 35. deploy-pages.yml インラインGoコード廃止 ✅ 完了
- [x] 180行のインライン`gen_json.go`を削除 → `main.go -mode=export-json` に統合
- [x] 列追加時の二重メンテナンスが不要に

### 36. 四半期データ蓄積基盤 ✅ 完了
- [x] `stock_financials` テーブル追加（code, doc_type, submission_date, doc_description + 全財務カラム）
- [x] `runCollector` で `stocks`（最新サマリ）と `stock_financials`（時系列）の両方に保存
- [x] 四半期EPS成長率計算の前提条件が整った（データ蓄積開始）

### 37. main.go ファイル分割 ✅ 完了
- [x] `main.go`(2073行) → 6ファイルに分割
  - `main.go`(153) - 型定義・エントリーポイント
  - `collector.go`(210) - EDINET データ収集
  - `db.go`(356) - DB初期化・マイグレーション・CRUD
  - `server.go`(609) - HTTPサーバー・export-json
  - `xbrl.go`(448) - XBRLパース・タグパターン
  - `prices.go`(341) - 株価取得・RS計算

## 🟠 UI強化（2026-04-25） ✅ 完了

### 41. ダッシュボード 市場区分・業種列 ✅ 完了
- [x] index.html テーブルに「市場」「業種」列を追加（JPX MarketSegment / Sector33）
- [x] 市場区分を色分け（プライム=緑、スタンダード=黄、グロース=ピンク）
- [x] 業種ドロップダウンフィルター追加

### 42. 銘柄詳細 決算マーカー ✅ 完了
- [x] TDNET開示データから「決算」「業績」「配当」「修正」キーワードを抽出
- [x] 📊（決算短信）⚠（業績修正）💰（配当）マーカーをチャート上に表示

### 43. 銘柄詳細 市場区分・業種バッジ ✅ 完了
- [x] ヘッダーに市場区分（色分けバッジ）と業種バッジを表示

### 44. オニールスクリーニング 業種・市場フィルタ ✅ 完了
- [x] 市場区分プルダウン（プライム/スタンダード/グロース）
- [x] 業種プルダウン（Sector33 から動的生成）
- [x] 業種列をテーブル末尾に追加

## 🟠 株価データソース拡張（2026-04-25） ✅ 完了

### 45. Stooq + Yahoo Finance フォールバック ✅ 完了
- [x] Stooq に User-Agent / Referer / Accept ヘッダを追加（ブロック軽減）
- [x] fetchPricesFromYahoo: query1.finance.yahoo.com/v8/finance/chart 実装
- [x] Stooq 失敗時に Yahoo Finance へ自動フォールバック
- [x] 株価カバレッジ: 490銘柄 → 3777銘柄 (90.0%) に大幅拡大
- [x] RS 再計算で 3742 銘柄カバー

## 🟠 接続プール障害修正（2026-04-25） ✅ 解決

### 46. ATTACH DATABASE が API で見えない問題 ✅ 完了
- [x] 症状: 銘柄詳細「データ取得エラー」、全銘柄 RS=null
- [x] 原因: database/sql の接続プールが複数接続を持ち、ATTACH した接続と
  SELECT する接続が別だったため `no such table: rs_db.rs_scores` 発生
- [x] 修正: openServerDB で SetMaxOpenConns(1) + SetMaxIdleConns(1)
- [x] 副作用のデッドロック修正: メインクエリ前に補助マップ（rsMap, financialsMap）
  を完全ロードする順序に変更（/api/stocks, /api/oneil-ranking, exportJSON）

---

## 🟢 低優先度

### 38. sqlite-wasm の対応 ✅ 完了 (query.html)
- [x] ブラウザ内でDB読み込み (sql.js v1.10.3 + pako で gunzip)
- [x] クエリ実行 (textarea + Cmd+Enter ショートカット)
- [x] プリセットクエリ8種 (銘柄数/売上トップ/ネットネット候補/業種別/etc)
- [x] 結果テーブル表示 (NULL/数値/文字列の型別書式)

### 39. S3への永続化（オプション）
- [ ] 生XBRLファイルのS3アップロード
- [ ] SQLiteファイルのS3保存

### 40. UI/UXの向上
- [ ] SPA化（1ファイルHTML）
- [x] レスポンシブデザイン ✅ 全6ページに 768px / 480px ブレークポイントで対応
- [x] 値クリックで詳細ポップアップ ✅ ダッシュボード/オニールで指標セルクリック→解説表示

---

## ✅ 完了済み

### インフラ・基盤
- [x] Docker環境構築
- [x] EDINET APIからの書類取得
- [x] SQLite保存（拡張スキーマ対応）
- [x] GitHub Actions日次バッチ（daily-update.yml）
- [x] GitHub Pagesデプロイ（deploy-pages.yml）
- [x] gzip圧縮版DBの配置

### XBRLパース
- [x] 売上高（NetSales）
- [x] 営業利益（OperatingIncome）
- [x] 純利益（NetIncome / ProfitLoss）
- [x] 総資産（TotalAssets / Assets）
- [x] 純資産（NetAssets）
- [x] 流動資産（CurrentAssets）
- [x] 負債合計（Liabilities）
- [x] 流動負債（CurrentLiabilities）
- [x] 現金及び預金（CashAndDeposits）
- [x] 発行済株式数（SharesIssued）

### フロントエンド
- [x] 基本ダッシュボード（ダークモードUI）
- [x] 分析ページのプレースホルダー作成（net-net-value.html, oneil-screen.html, market-top.html）
- [x] Go API + 静的JSONのハイブリッド対応
- [x] テーブルに財務項目列追加（営業利益、純利益、総資産、純資産）
- [x] 金額を億円単位で表示
- [x] テーブルソート機能（ヘッダークリックで昇順/降順切り替え）
- [x] サーバー起動時のDBマイグレーション自動実行
- [x] 検索機能（証券コード・企業名でリアルタイム検索）
- [x] 「データありのみ」フィルター機能
- [x] CSVエクスポート機能（BOM付きでExcel対応）

### 株価データ
- [x] Stooq APIから日次株価を取得（fetch-pricesモード）
- [x] stock_pricesテーブル作成（code, date, open, high, low, close, volume）
- [x] 最新株価をダッシュボードに表示
- [x] 時価総額の計算・表示（株価 × 発行済株式数）
- [x] /api/prices/{code} エンドポイント追加

---

## 📊 機能比較サマリー

| 機能 | kawasin73さん | 現プロジェクト |
|------|--------------|----------------|
| DB構成 | 3ファイル (xbrl/stock_price/rs) | **3ファイル ✅** |
| XBRLパース | 多数の財務項目 | **主要16項目 ✅** |
| 株価データ | あり | **Stooq API ✅** |
| ネットネット計算 | カスタム係数対応 | **カスタム計算 ✅** |
| オニールスクリーニング | 完全実装 | **スコアリング+フィルター ✅** |
| RS計算 | 専用DB | **DB+計算+表示 ✅** |
| 市場天井検出 | パラメータ調整可能 | **パラメータ調整 ✅** |
| チャート | lightweight-charts | **lightweight-charts v4/v5 ✅** |
| 決算期間ハイライト | あり | なし ❌ |
| 銘柄詳細ページ | 複数チャート | **チャート+指標カード+財務データ ✅** |

---

## 🚨 GitHub Pages 銘柄数問題（64件問題）

### 調査結果（2026-03-08）

**現象**: GitHub Pagesで62件しか銘柄が表示されない（日本上場企業は約3,800社）

**GitHub Releases最新データ** (data-2026-03-07):
- `xbrl.db.gz`: 6,035 bytes（62銘柄のみ）
- `stock_price.db.gz`: 317,251 bytes（58銘柄の株価）
- `rs.db.gz`: 9 bytes（ほぼ空）

**ローカルDB状態**:
- `xbrl.db`: 640件中 `net_sales > 0` は37件のみ（5.8%）
- `xbrl.db`: 638件は `updated_at` が空（レガシーDB移行データ）
- `stock_price.db`: 490銘柄 119,922レコード

### 🔴 原因1: EDINET取得期間が短すぎる（最重要）
- `daily-update.yml` のデフォルトは **7日間** のみ
- 有報提出は年2回の決算期に集中（3月決算→6月提出、9月中間→11月提出）
- **7日間では、その週に有報を提出した数十社しか取得できない**
- 日本の上場企業3,800社をカバーするには**最低1年分（365日）の過去データ**が必要

### 🔴 原因2: CI環境でレガシーDBが復元されない
- 3DB移行後、CI環境では `stock_data.db` が存在せず `xbrl.db` のみ復元
- `xbrl.db` が62件のデータしか持っていない → 毎日62件+新規数件でしか更新されない
- **一度データが少なくなると、そこから積み上がるだけ**で3,800社には到達しない
- 解決策: 初回バルクロードまたは長期間のバッチ実行が必要

### 🔴 原因3: XBRLパース成功率が極めて低い
- 640件中、財務データ（net_sales等）が取得できているのは**37件のみ（5.8%）**
- XBRLタグのパターンが限定的で、多くの有報のフォーマットに対応できていない
- → 取得はできても解析結果が空の銘柄が大量に存在する

### 🟡 原因4: saveStock の INSERT OR REPLACE による上書き
- 同一銘柄が訂正報告書を提出すると、合には有効データが空データで上書きされる可能性
- 現在は「空でないデータのみ更新」のガードがない

### 📋 改善タスク（優先度順）

#### P0: 初回バルクロード（即効性あり・最重要） ✅ 完了
- [x] **daily-update.yml にバルクロードモードを追加**
  - `bulk-load.yml` ワークフローを新規作成（手動実行、日付範囲・チャンクサイズ指定可能）
  - 期間を分割実行（デフォルト30日×N回）
  - EDINET APIのレートリミット対策（チャンク間5秒休憩 + 各日1秒スリープ）
- [x] **GitHub Actions の実行時間制限対策**
  - `timeout-minutes: 300`（5時間）設定
  - チャンク分割でAPIレートリミットを回避
  - 完了後に自動で株価取得・RS計算・リリース作成
- [x] **ローカルバッチ実行 → DBアップロード手順の整備**
  - Taskfileに `task bulk-load FROM=2025-04-01 TO=2026-03-30` タスク追加
  - `task upload-db` でGitHub Releasesにアップロード
  - バッチ→株価取得→RS計算→圧縮→アップロードの一連の流れを自動化

#### P1: saveStock の空データ上書き防止 ✅ 完了
- [x] **saveStock で空データ保存をスキップするガード追加**
  - 全てのフィールドが0の場合は `INSERT OR REPLACE` をスキップ
  - 既存レコードがあり、新データが空の場合は更新しない
- [x] **INSERT OR REPLACE → UPSERT with COALESCE に変更**
  ```sql
  INSERT INTO stocks (...) VALUES (...)
  ON CONFLICT(code) DO UPDATE SET
    net_sales = CASE WHEN excluded.net_sales > 0 THEN excluded.net_sales ELSE stocks.net_sales END,
    ...
  ```

#### P2: XBRLパース成功率の向上 ✅ 主要改善完了
- [x] **現在のパース成功率を計測するログ追加**
  - 各フィールドごとに「取得できた件数 / 処理した件数」を出力
- [x] **XBRLタグパターンの拡充**
  - 複数のタグバリエーションに対応（連結/非連結/四半期/IFRS）
  - 各フィールドにFallback3〜5まで追加
  - マイナス値（赤字）の営業利益・純利益にも対応
- [x] **四半期報告書のパースロジック改善**
  - `CurrentYTDDuration` / `CurrentQuarterDuration` / `CurrentQuarterInstant` 対応
- [x] **コンテキストIDフォールバック**
  - 連結→非連結→個別の順でフォールバック
  - `getBaseTagName()` と `applyXBRLValue()` でDRY化

#### P3: 日次更新の改善
- [x] **daily-update.yml の days_back デフォルト値を 30 に増加**
  - 現在の7日では祝日や連休を挟むとデータ漏れが発生
  - 30日にすれば月に1回の有報提出をカバー可能
- [ ] **更新統計の改善**
  - EDINET日次で「有報あり/なし」を記録
  - 取得済み期間のギャップ検出
- [ ] **GitHub Pages用 stocks.json に株価データを含める**
  - 現在: `deploy-pages.yml` の `gen_json.go` で株価JOIN
  - 改善: stock_price.db のデータが正しくJOINされているか確認

#### P4: データ品質モニタリング
- [x] **GitHub Actions後の品質レポート拡充** ✅
  - 総銘柄数 / 売上 / 純利益 / 総資産 / 現金 / 株主資本 / 株価 / RS のサマリー
  - Job Summary に全指標を出力
- [ ] **データカバレッジダッシュボード**
  - GitHub Pagesにデータカバレッジページを追加
  - 証券コード範囲ごとのカバー率表示

---

## 🎯 推奨実装順序（更新版）

~~1. **XBRLパース拡張** → 全ての計算の基礎~~ ✅ 完了

~~1. **EPS計算ロジック** → 純利益÷発行済株式数で計算可能に~~ ✅ 完了（ROEも実装済み）
2. ~~**株価データ取得** → RS・時価総額に必須~~ ✅ Stooq実装済み
3. ~~**ネットネット計算UI実装** → 取得済みデータで計算可能~~ ✅ 完了
4. ~~**3DB構成に変更** → xbrl.db / stock_price.db / rs.db~~ ✅ 完了
5. ~~**lightweight-charts導入** → チャート表示の基盤~~ ✅ v4/v5導入済み
6. ~~**オニールランキング実装** → スコア計算・テーブル~~ ✅ 基本実装済み
7. ~~**銘柄詳細ページ** → チャート・主要指標~~ ✅ 完了
8. ~~**市場天井検出** → パラメータUI・チャート~~ ✅ v1完了
9. ~~**🚨 銘柄数問題修正（P0: バルクロード）** → bulk-load.yml + Taskfile~~ ✅ 完了
10. ~~**🚨 saveStock の空データ上書き防止（P1）** → UPSERT + COALESCE~~ ✅ 完了
11. ~~**🚨 XBRLパース成功率向上（P2）** → タグパターン拡充~~ ✅ 完了
12. ~~**日次更新改善（P3）** → days_back=30~~ ✅ 完了 + 品質レポート
13. ~~**RS計算実装** → リラティブストレングスの計算ロジック~~ ✅ 完了
14. ~~**ネットネット計算改善** → カスタム係数の追加・削除~~ ✅ 完了

## 🟢 アラート機能（2026-04-26） ✅ 完了

### 47. 日次アラート検出 + Issue 自動投稿
- [x] alerts.go: detect-alerts モード新設
- [x] RS急上昇 (+15以上)、業績修正開示、出来高急増 (5日平均×3超 + 株価上昇) を検出
- [x] Markdown 形式でテーブル出力 (上限30件、業績修正は全件)
- [x] daily-update.yml に組み込み、結果を GitHub Issue として自動投稿
- [x] アラートなしの日は Issue 作成スキップ
