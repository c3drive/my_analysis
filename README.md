# イメージのビルド
docker compose build

# 動作確認（ツールバージョンの表示）
docker compose run --rm app task hello

# 🛠 使用ツールと構成カテゴリツール / 構成備考環境管理miseDocker内でPython/Node.jsのバージョンを管理言語Python 3.12 / Node.js 22分析スクリプトおよびフロントエンド用自動化Taskfile (go-task)task コマンドによる作業の共通化インフラGitHub Actions日次バッチ実行とSQLiteファイルの自動PushデータベースSQLitedata/stock_data.db としてリポジトリ内で管理

# 📂 ディレクトリ構造
`backend/`: データ収集・加工スクリプト (Python)

`frontend/`: 解析用SPA (React/Vite + sqlite-wasm)

`data/`: 更新されたSQLiteファイルが格納される場所

# 📝 開発時の注意点（Tips）
`mise` はコンテナ内で `/root/.local/bin/mise` に配置されている。

プロジェクトルートの `.mise.toml` は `MISE_TRUSTED_CONFIG_PATHS` によって自動的に信頼される設定となっている。