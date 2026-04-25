# EDINET XBRL Analyzer

EDINET APIから上場企業の決算データを取得し、XBRLファイルを解析して財務分析を行うシステム。

公開サイト: <a href="https://c3drive.github.io/my_analysis/" target="_blank" rel="noopener noreferrer">https://c3drive.github.io/my_analysis/</a>

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
│  │ 1. EDINET APIからデータ取得（30日遡り）  │   │
│  │ 2. XBRLファイルをパース                  │   │
│  │ 3. SQLite 3DB構成に保存                  │   │
│  │ 4. Stooq APIから株価取得                 │   │
│  │ 5. gzip圧縮 → GitHub Releasesへ         │   │
│  └─────────────────────────────────────────┘   │
└─────────────────────────────────────────────────┘
                        │
                        ▼
         ┌──────────────────────────┐
         │   GitHub Releases         │
         │  xbrl.db.gz (財務データ)  │
         │  stock_price.db.gz (株価) │
         │  rs.db.gz (RS指標)        │
         └──────────────────────────┘
                        │
                        ▼
         ┌──────────────────────────┐
         │    GitHub Pages           │
         │  静的HTML + JSON API      │
         │  ブラウザで分析           │
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
│       ├── daily-update.yml      # 日次データ更新（30日遡り取得）
│       ├── deploy-pages.yml      # GitHub Pages デプロイ
│       └── ci.yaml               # CI（ビルド確認）
├── data/
│   ├── xbrl.db                   # 財務データDB（EDINET XBRL）
│   ├── stock_price.db            # 株価データDB（Stooq）
│   ├── rs.db                     # リラティブストレングスDB
│   └── stock_data.db             # レガシーDB（自動移行対応）
├── web/
│   ├── index.html                # メインダッシュボード
│   ├── net-net-value.html        # ネットネットバリュー分析
│   ├── oneil-screen.html         # オニール成長株スクリーニング
│   ├── market-top.html           # マーケット天井検出
│   └── stock-detail.html         # 銘柄詳細（チャート・指標）
├── scripts/
│   └── compress_db.sh            # DB圧縮スクリプト
├── main.go                       # メインロジック
├── Taskfile.yml                  # タスク定義
├── TODO.md                       # 詳細TODO・進捗管理
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

## 📈 実装状況

### ✅ 完了済み
- [x] 株価データの取得と保存（Stooq API）
- [x] EPS・ROE・PER・PBR の計算
- [x] ネットネットバリュー計算（カスタム係数対応）
- [x] オニール成長株スクリーニング（基本スコアリング）
- [x] マーケット天井検出（パラメータ調整可能）
- [x] 銘柄詳細ページ（ローソク足チャート＋指標カード）
- [x] lightweight-charts v4/v5 導入
- [x] 3DB構成（xbrl.db / stock_price.db / rs.db）
- [x] UPSERT による空データ上書き防止
- [x] XBRLパース改善（四半期/IFRS/非連結/赤字対応）

### 🔜 今後の予定
- [ ] 銘柄数拡大（バルクロード実施）
- [ ] RS（リラティブストレングス）計算実装
- [ ] 新規銘柄発見時のGitHub Issue通知
- [ ] データカバレッジダッシュボード

> 詳細は [TODO.md](./TODO.md) を参照

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

- <a href="https://disclosure2.edinet-fsa.go.jp/" target="_blank" rel="noopener noreferrer">EDINET API仕様書</a>
- <a href="https://sqlite.org/wasm/" target="_blank" rel="noopener noreferrer">SQLite WASM Documentation</a>
- <a href="https://docs.github.com/actions" target="_blank" rel="noopener noreferrer">GitHub Actions Documentation</a>

## 📄 ライセンス

MIT License

## 🙏 謝辞

本プロジェクトは <a href="https://kawasin73.hatenablog.com/entry/2025/11/20/224346" target="_blank" rel="noopener noreferrer">kawasin73氏のブログ記事</a> を参考にして開発された。
