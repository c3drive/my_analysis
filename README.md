# EDINET XBRL Analyzer

EDINET APIから上場企業の決算データを取得し、XBRLファイルを解析して財務分析を行うシステム。
https://c3drive.github.io/my_analysis/

## 🚀 機能

- **自動データ収集**: GitHub Actionsで毎日18時（JST）に自動実行
- **XBRL解析**: 売上高・EPS・純資産などの財務データを抽出
- **静的Web分析**: ブラウザ上でSQLiteを直接解析（サーバーレス）
- **複数の分析手法**: ネットネットバリュー、オニールスクリーニング、マーケット天井検出

## 📊 アーキテクチャ

```
┌─────────────────────────────────────────────────┐
│          GitHub Actions (日次バッチ)              │
│  ┌─────────────────────────────────────────┐   │
│  │ 1. EDINET APIからデータ取得              │   │
│  │ 2. XBRLファイルをパース                  │   │
│  │ 3. SQLiteに保存                          │   │
│  │ 4. gzip圧縮                              │   │
│  │ 5. GitHub Releasesにアップロード         │   │
│  └─────────────────────────────────────────┘   │
└─────────────────────────────────────────────────┘
                        │
                        ▼
         ┌──────────────────────────┐
         │   GitHub Releases         │
         │  stock_data.db.gz         │
         │  (永続保存)               │
         └──────────────────────────┘
                        │
                        ▼
         ┌──────────────────────────┐
         │    GitHub Pages           │
         │  静的HTML + sqlite-wasm   │
         │  ブラウザでDB解析         │
         └──────────────────────────┘
```

## 🛠 技術スタック

| カテゴリ | 技術 | 備考 |
|---------|------|------|
| Runtime | Go 1.23 | 高速パース・低メモリ実行 |
| Database | SQLite 3 | Pure Go実装（CGO不要） |
| Task Runner | go-task | Taskfile.yml |
| CI/CD | GitHub Actions | 日次バッチ + Pages デプロイ |
| Storage | GitHub Releases | DBファイルの永続保存 |
| Frontend | sqlite-wasm | ブラウザ内でDB解析 |

## 📂 ディレクトリ構造

```
.
├── .github/
│   └── workflows/
│       ├── daily-update.yml      # 日次データ更新
│       └── deploy-pages.yml      # GitHub Pages デプロイ
├── data/
│   ├── raw/                      # 生XBRLファイル
│   ├── stock_data.db             # SQLiteデータベース
│   └── stock_data.db.gz          # 圧縮版（配信用）
├── web/
│   ├── index.html                # メインダッシュボード
│   ├── net-net-value.html        # ネットネット分析
│   ├── oneil-screen.html         # オニールスクリーニング
│   └── market-top.html           # マーケット天井検出
├── scripts/
│   └── compress_db.sh            # DB圧縮スクリプト
├── main.go                       # メインロジック
├── Taskfile.yml                  # タスク定義
└── README.md
```

## 🛠 実行手順

### 1. 初期セットアップ

```bash
# イメージのビルド
docker compose build

# 依存関係のインストール
docker compose run --rm app task init
```

### 2. ローカルでのデータ収集

```bash
# 特定日のデータを取得
docker compose run --rm app task run -- -mode=run -date=2025-11-13

# または日次更新シミュレーション
docker compose run --rm app task daily-update
```

### 3. ダッシュボードの起動

```bash
docker compose run --rm -p 8080:8080 app task serve
```

アクセス: `http://localhost:8080`

### 4. テスト解析

```bash
docker compose run --rm app task test-parse
```

## 🌐 GitHub Pages での公開

### 設定手順

1. **GitHub Secretsの設定**
   - リポジトリの Settings → Secrets and variables → Actions
   - `EDINET_API_KEY` を追加

2. **GitHub Pages の有効化**
   - Settings → Pages
   - Source: "GitHub Actions" を選択

3. **自動デプロイの確認**
   - `web/` ディレクトリを更新してpush
   - Actions タブで進行状況を確認

### アクセスURL

```
https://<username>.github.io/<repository-name>/
```

## 📝 利用可能なタスク

```bash
# 基本操作
task hello          # 疎通確認
task init           # 初期セットアップ
task run            # データ収集
task serve          # Webサーバー起動

# 高度な操作
task daily-update   # 日次更新シミュレーション
task compress       # DBの圧縮
task build-web      # Web資産のビルド
task clean          # データベースのクリーンアップ
```

## 🔧 開発環境

### 必要なツール

- Docker Desktop
- Git

### 環境変数

```bash
# .envファイルを作成
EDINET_API_KEY=your_api_key_here
```

## 📈 今後の実装予定

### Phase 1: データ層の強化
- [ ] 株価データの取得と保存
- [ ] EPS・純資産の抽出
- [ ] 決算発表日の記録

### Phase 2: 分析機能
- [ ] ネットネットバリュー計算
- [ ] オニール成長株スクリーニング
- [ ] マーケット天井検出ロジック

### Phase 3: UI改善
- [ ] インタラクティブなチャート
- [ ] フィルタリング機能
- [ ] レスポンシブデザイン

### Phase 4: 通知機能
- [ ] 新規銘柄の自動検出
- [ ] GitHub Issue での通知
- [ ] 条件達成時のアラート

## ⚡ トラブルシューティング

### ポート競合

```bash
# 8080ポートを使用中のプロセスを確認
lsof -i :8080

# プロセスを停止
kill -9 <PID>
```

### GitHub Actions の失敗

1. Actions タブで詳細ログを確認
2. Secretsが正しく設定されているか確認
3. 自動作成されたIssueを確認

### データベースが壊れた場合

```bash
# データベースを削除して再構築
task clean
task daily-update
```

## 📚 参考資料

- [EDINET API仕様書](https://disclosure2.edinet-fsa.go.jp/)
- [SQLite WASM Documentation](https://sqlite.org/wasm/)
- [GitHub Actions Documentation](https://docs.github.com/actions)

## 📄 ライセンス

MIT License

## 🙏 謝辞

本プロジェクトは [kawasin73氏のブログ記事](https://kawasin73.hatenablog.com/entry/2025/11/20/224346) を参考にして開発された。
