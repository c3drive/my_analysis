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

ホスト側で直接 Go を使う場合（mise で go 1.23 をインストール済みなら）:
```bash
go run . -mode=run -date=2025-11-13
go run . -mode=serve
go run . -mode=batch -from=2025-04-01 -to=2026-02-22
go run . -mode=fetch-prices
go run . -mode=calc-rs
```

テストスイートは存在しない。`task test-parse` はユニットテストではなく、ローカルXBRLファイルの手動パース確認用。

## Architecture

### Single-file Go app

ロジックは全て `main.go`（約60K）に集約されている。パッケージ分割なし。`debug_xbrl.go` と `debug_shares.go` はデバッグ用の補助ファイル。

### 6つの実行モード (`-mode` フラグ)

| Mode | 概要 |
|------|------|
| `run` | 指定日のEDINET書類を取得→XBRLパース→xbrl.dbへ保存 |
| `batch` | `-from`/`-to` で日付範囲を指定し `run` をループ実行（土日スキップ） |
| `serve` | `:8080` でWebサーバー起動。静的ファイル + REST API |
| `fetch-prices` | xbrl.dbの全銘柄の株価をStooqから取得→stock_price.db |
| `calc-rs` | 3/6/9/12ヶ月リターンからRS（リラティブストレングス）を計算→rs.db |
| `test-parse` | ローカルXBRLファイルの解析テスト |

### 3-DB構成（SQLite, Pure Go — CGO不要）

| DB | テーブル | 内容 |
|----|---------|------|
| `data/xbrl.db` | `stocks` | 財務データ（売上・利益・資産・負債・発行済株式数 etc） |
| `data/stock_price.db` | `stock_prices` | 日次株価（Stooq由来）PK: code+date |
| `data/rs.db` | `rs_scores` | RS値・RS順位 PK: code+date |

レガシーDB `data/stock_data.db` からの自動移行ロジック (`migrateFromLegacyDB()`) あり。

### 外部API

- **EDINET** (`api.edinet-fsa.go.jp`): 有価証券報告書ZIP取得。`EDINET_API_KEY` 環境変数が必要。無い場合は `test_data.json` のモックにフォールバック。
- **Stooq** (`stooq.com`): 日本株の日次株価CSV。証券コード4桁 + `.jp` サフィックス。
- レート制限: 両APIとも1秒間隔のスリープを挟む。

### Web API エンドポイント（serve モード）

- `/` — 静的ファイル（`./web/`）
- `/api/stocks` — 全銘柄 + 最新株価（stock_price.db をATTACH）
- `/api/prices/{code}` — 銘柄の株価履歴
- `/api/rs/{code}` — RS履歴
- `/api/oneil-ranking` — オニール成長株スクリーニング結果
- `/api/market-index/{code}` — マーケット天井検出データ（CORS: `*`）
- `/api/available-codes` — 有効な証券コード一覧（CORS: `*`）
- `/xbrl.db` — SQLiteファイル直接ダウンロード

### フロントエンド（`web/`）

サーバーレス構成。GitHub Pagesで配信される静的HTMLがブラウザ内で sqlite-wasm を使い、GitHub ReleasesからダウンロードしたDBを直接クエリする。

- `index.html` — メインダッシュボード
- `net-net-value.html` — ネットネットバリュー分析
- `oneil-screen.html` — オニール成長株スクリーニング
- `market-top.html` — マーケット天井検出
- `stock-detail.html` — 銘柄詳細（lightweight-charts v4/v5 でローソク足チャート）

### CI/CD（`.github/workflows/`）

- `daily-update.yml` — 毎日09:00 UTC（18:00 JST）に30日遡り取得→株価取得→RS計算→gzip→GitHub Release作成。失敗時/0件時はIssue自動作成。新規銘柄検出時もIssue通知。
- `deploy-pages.yml` — GitHub Pages デプロイ
- `ci.yaml` — CIビルド確認
- `bulk-load.yml` — バルクロード

## Key decisions & gotchas

- XBRLパースは正規表現ベース（XMLパーサー不使用）。タグのフォールバック（`OperatingIncome` → `OrdinaryIncome` 等）やサフィックス除去ロジックあり。
- ZIPはメモリ上で展開（ディスクに書かない）。`bytes.Reader` → `zip.Reader`。
- `stocks` テーブルはUPSERTで空データによる上書きを防止。
- DBファイルはgit管理外（`.gitignore`）。GitHub Releasesが永続ストレージ。CIはリリースから最新DBを復元してから更新する。
- `data/stock_data.db` だけが `.gitignore` で追跡対象だが、これはレガシー。
