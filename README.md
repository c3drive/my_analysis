# EDINET XBRL Analyzer

EDINET APIから上場企業の決算データを取得し、XBRLファイルを解析して売上高を抽出・可視化するシステム。

## 🚀 機能
- **Data Collection**: EDINET APIから書類一覧を取得し、XBRLをメモリ上で解析。
- **Automated Parsing**: 正規表現を用いた売上高（NetSales/OperatingRevenues）の自動抽出。
- **Dashboard**: 抽出したデータをSQLiteに保存し、Webブラウザで可視化。
- **CI/CD**: GitHub Actionsによる自動ビルドとモックデータによる動作検証。


## 🛠 使用ツールと構成カテゴリツール / 構成備考環境管理miseDocker内でPython/カテゴリ,技術,備考
Runtime,Go 1.23,財務データの高速パースと低メモリ実行のため採用
Database,SQLite 3,modernc.org/sqlite (Pure Go) を使用しCGOを排除
Tool Manager,mise,"コンテナ内のGo, Node.js, Taskのバージョンを厳密に管理"
Task Runner,go-task,Taskfile.yml による共通コマンドの定義
Infrastructure,GitHub Actions,日次バッチ実行と成果物(DB)の自動Push
Environment,Docker,ubuntu:24.04 ベースのクリーンな開発環境

## 📂 ディレクトリ構造
.
├── .mise.toml         # ツールのバージョン定義
├── Dockerfile         # mise/Go環境を内蔵したビルド定義
├── docker-compose.yml # 開発コンテナの起動定義
├── Taskfile.yml       # プロジェクト用コマンド集
├── main.go            # データ収集エンジンのメインロジック
├── data/              # SQLiteデータベース保存先
│   └── stock_data.db  # 生成されたデータベースファイル
└── README.md          # 本ドキュメント

## 🛠 実行手順

### 1. イメージのビルド
```bash
docker compose build
```

### 2. 疎通確認（Go/Node/Taskのバージョン確認）
```bash
docker compose run --rm app task hello
```

### 3. Goの依存関係とSQLiteドライバーの初期化（初回のみ）
```bash
docker compose run --rm app task init
```

### 4. データベースの初期化・実行
```bash
docker compose run --rm app task run
```

### 5. データの収集 (Run Mode)
```bash
docker compose run --rm app go run main.go -mode=run -date=2025-11-13
```
### 6. ダッシュボードの起動 (Serve Mode)
```bash
docker compose run --rm -p 8080:8080 app go run main.go -mode=serve
```
アクセス: `http://localhost:8080`

### 7. ローカルファイルのテスト解析 (Test-Parse Mode)
```bash
docker compose run --rm app go run main.go -mode=test-parse
```

## 📝 開発時の注意点（Tips）
`mise` はコンテナ内で `/root/.local/bin/mise` に配置されている。

プロジェクトルートの `.mise.toml` は `MISE_TRUSTED_CONFIG_PATHS` によって自動的に信頼される設定となっている。

##⚡ トラブルシューティング
ポート 8080 が塞がっている場合
`Bind for 0.0.0.0:8080 failed: port is already allocated` というエラーが出た際の対処法。

占拠しているプロセスを特定して排除する
```bash
# 8080ポートを使用中のプロセスを確認
lsof -i :8080

# 特定したPID（プロセスID）を強制終了
kill -9 <PID>

# または、Dockerの全コンテナを停止させる
docker stop $(docker ps -q)
```

## 📈 今後の強化予定 (Roadmap)
- [ ] 指標の追加: 純利益（NetIncome）や純資産（NetAssets）の抽出対応。
- [ ] 一括処理: 複数日のデータを一括で取得するスクリプトの実装。
- [ ] 可視化強化: グラフライブラリを用いた売上高ランキングの表示。