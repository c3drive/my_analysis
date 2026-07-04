# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this project is

EDINET XBRL Analyzer — EDINET APIから日本の上場企業の有価証券報告書（XBRL）を取得・解析し、財務分析を行うシステム。GitHub Actionsで毎日18時JSTに自動実行され、結果をGitHub Releases（SQLite DB）とGitHub Pages（静的HTML）で公開する。

公開URL: https://c3drive.github.io/my_analysis/

## Build & run commands

全てDockerコンテナ内で `go-task` 経由で実行する。ホスト側に Go は不要。

```bash
docker compose build                                          # イメージビルド
docker compose run --rm app task run -- -mode=run -date=2025-11-13  # 特定日のデータ取得
docker compose run --rm app task daily-update                  # 日次更新シミュレーション
docker compose run --rm app task test-parse                    # テストパース（ローカルXBRL）
docker compose run --rm -p 8080:8080 app task serve            # Webサーバー起動 → localhost:8080
docker compose run --rm app task bulk-load FROM=2025-04-01 TO=2026-03-30  # 一括取得
docker compose run --rm app task compress                      # DB圧縮（gzip）
```

ホスト側で直接 Go を使う場合（mise で go 1.24 をインストール済みなら）:
```bash
go run . -mode=run -date=2025-11-13
go run . -mode=serve
go run . -mode=batch -from=2025-04-01 -to=2026-02-22
```

テスト: `go test ./...`（`metrics_test.go` = 成長指標計算、`tanshin_test.go` = 決算短信PDFパース）。
`task test-parse` はユニットテストではなく、ローカルXBRLファイルの手動パース確認用。

## Architecture

### ファイル構成（単一 main パッケージ、計約6,000行。1ファイル=1ドメインを維持すること）

| ファイル | 役割 |
|---------|------|
| `main.go` (173) | 型定義・エントリポイント・モード分岐 |
| `server.go` (73) | サーバー起動・静的配信・DB配布ルート・ハンドラ登録 |
| `handlers_stocks.go` (383) | 基本データAPI（stocks / prices / rs / stocks-as-of / available-codes） |
| `handlers_detail.go` (157) | 銘柄詳細API（financials / disclosures / market-index） |
| `handlers_oneil.go` (238) | オニール成長株ランキング |
| `handlers_cycle.go` (199) | サイクル投資（業種別RS）ランキング |
| `handlers_value.go` (225) | バリュー（グレアム + Piotroski F9）ランキング |
| `handlers_dividend.go` (243) | 高配当ランキング |
| `handlers_yutai.go` (204) | 株主優待実質利回りランキング |
| `export.go` (156) | GitHub Pages用 stocks.json 出力（export-json モード） |
| `prices.go` (502) | 株価取得（Stooq → Yahoo Finance フォールバック）・RS計算 |
| `xbrl.go` (493) | XBRLパース（正規表現ベース）・タグフォールバック |
| `tanshin.go` (491) | 決算短信PDFパース（pdftotext → 正規表現） |
| `db.go` (408) | DB初期化・レガシー移行・CRUD |
| `metrics.go` (307) | EPS/売上の成長指標計算（QoQ/YoY/CAGR） |
| `alerts.go` (291) | 日次アラート検出（RS急上昇・業績修正・出来高急増） |
| `collector.go` (213) | EDINET書類収集 |
| `tdnet.go` (193) | TDNET適時開示メタデータ取得 |
| `jpx.go` (159) | JPX銘柄リストCSV取込 |
| `yutai.go` (93) | 株主優待CSV（`data/yutai.csv`、手動メンテ）読込 |
| `cache.go` (61) | APIレスポンスのインメモリTTLキャッシュ |
| `debug_xbrl.go` / `debug_shares.go` | デバッグ用補助 |

### 12の実行モード (`-mode` フラグ)

| Mode | 概要 |
|------|------|
| `run` | 指定日のEDINET書類を取得→XBRLパース→xbrl.dbへ保存 |
| `batch` | `-from`/`-to` で日付範囲を指定し `run` をループ実行（土日スキップ） |
| `serve` | `:8080` でWebサーバー起動。静的ファイル + REST API |
| `fetch-prices` | xbrl.dbの全銘柄の株価をStooq（失敗時Yahoo）から取得→stock_price.db |
| `calc-rs` | 3/6/9/12ヶ月リターンからRS（リラティブストレングス）を計算→rs.db |
| `export-json` | GitHub Pages用 `stocks.json` を出力（deploy-pages.ymlが使用） |
| `fetch-tdnet` | TDNET適時開示メタデータ取得（過去31日分のみ取得可能） |
| `parse-tanshin` | 決算短信PDFから財務5項目を抽出→stock_financials |
| `debug-tanshin` | 決算短信パースのデバッグ |
| `import-jpx` | JPX銘柄リスト（data_j.xls由来CSV）取込。市場区分・業種を補完 |
| `detect-alerts` | 日次アラート検出→Markdown出力（CIがIssue投稿） |
| `test-parse` | ローカルXBRLファイルの解析テスト |

### 3-DB構成（SQLite, Pure Go — CGO不要）

| DB | テーブル | 内容 |
|----|---------|------|
| `data/xbrl.db` | `stocks` / `stock_financials` / `tdnet_disclosures` | 最新財務サマリ / 報告書ごとの財務時系列 / 適時開示メタデータ |
| `data/stock_price.db` | `stock_prices` | 日次株価 PK: code+date |
| `data/rs.db` | `rs_scores` | RS値・RS順位 PK: code+date |

- serve時は `openServerDB()` が xbrl.db に price_db / rs_db を `ATTACH` してクロスDB JOIN する。
- **gotcha**: 接続プールが複数接続を持つと ATTACH が見えない接続で `no such table` になるため、`SetMaxOpenConns(1)` に固定している。補助マップ（rsMap等）はメインクエリの前に完全ロードすること（デッドロック防止）。
- レガシーDB `data/stock_data.db` からの移行ロジック（`migrateFromLegacyDB()`）は残っているが、xbrl.dbにデータがあればスキップされる。レガシーDBはgit管理外。

### 外部API

- **EDINET** (`api.edinet-fsa.go.jp`): 有価証券報告書ZIP取得。`EDINET_API_KEY` 環境変数が必要。無い場合は `test_data.json` のモックにフォールバック。
- **Stooq** (`stooq.com`): 日本株の日次株価CSV。失敗時は **Yahoo Finance** (`query1.finance.yahoo.com`) にフォールバック。
- **TDNET**: 適時開示メタデータ。公式サイトは過去31日分のみ。
- レート制限: 各APIとも1秒間隔のスリープを挟む。

### Web API エンドポイント（serve モード）

- `/` — 静的ファイル（`./web/`）
- `/api/stocks` — 全銘柄 + 最新株価 + RS
- `/api/stocks-as-of/{date}` — 指定日の株価による再計算
- `/api/prices/{code}` — 銘柄の株価履歴
- `/api/rs/{code}` — RS履歴
- `/api/financials/{code}` — 報告書ごとの財務時系列
- `/api/disclosures/{code}` — TDNET適時開示一覧
- `/api/oneil-ranking` — オニール成長株スクリーニング
- `/api/cycle-ranking` / `/api/dividend-ranking` / `/api/yutai-ranking` — シクリカル / 高配当 / 株主優待ランキング
- `/api/market-index/{code}` — マーケット天井検出データ
- `/api/available-codes` — 有効な証券コード一覧
- `/xbrl.db` — SQLiteファイル直接ダウンロード

### フロントエンド（`web/`、11ページ）

サーバーレス構成。GitHub Pagesで配信される静的HTMLがブラウザ内で sql.js を使い、GitHub ReleasesからダウンロードしたDBを直接クエリする。ローカルでは Go API を叩くハイブリッド構成。

- `index.html` — メインダッシュボード
- `stock-detail.html` — 銘柄詳細（lightweight-charts ローソク足 + 決算マーカー + 財務チャート）
- `oneil-screen.html` — オニール成長株スクリーニング
- `net-net-value.html` — ネットネットバリュー分析
- `market-top.html` — マーケット天井検出
- `value.html` — バリュー（グレアム + Piotroski F9）
- `dividend.html` — 高配当スクリーニング
- `yutai.html` — 株主優待実質利回り
- `cycle.html` — シクリカルランキング
- `watchlist.html` — ウォッチリスト（localStorage）
- `query.html` — ブラウザ内SQL実行（sql.js + pako）

### CI/CD（`.github/workflows/`）

- `daily-update.yml` — 毎日09:00 UTC（18:00 JST）に30日遡り取得→株価取得→RS計算→アラート検出→gzip→GitHub Release作成。失敗時/0件時はIssue自動作成。
- `deploy-pages.yml` — daily-update 完了後に `workflow_run` でGitHub Pagesへ自動デプロイ。
- `ci.yaml` — CIビルド確認（EDINET_API_KEYは意図的に空 = モックモード）。
- `bulk-load.yml` — 過去データの一括取得（手動実行、チャンク分割）。

## Key decisions & gotchas

- XBRLパースは正規表現ベース（XMLパーサー不使用）。タグのフォールバック（`OperatingIncome` → `OrdinaryIncome` 等）やサフィックス除去、連結→非連結のコンテキストフォールバックあり。
- ZIPはメモリ上で展開（ディスクに書かない）。`bytes.Reader` → `zip.Reader`。
- `stocks` テーブルはUPSERT + COALESCE で空データによる上書きを防止。
- DBファイルはgit管理外（`.gitignore`）。GitHub Releasesが永続ストレージ。CIはリリースから最新DBを復元してから更新する。
- `data/yutai.csv` のみgit追跡（手動メンテの株主優待データ）。
