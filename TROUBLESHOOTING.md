# GitHub Actions トラブルシューティングガイド

## よくあるエラーと解決方法

### エラー1: "EDINET_API_KEY not set"
**原因**: GitHub Secretsが設定されていない

**解決方法**:
1. Settings → Secrets and variables → Actions
2. New repository secret
3. Name: EDINET_API_KEY
4. Secret: (あなたのAPIキー)

---

### エラー2: "Permission denied" (GitHub Releases作成時)
**原因**: GITHUB_TOKENの権限不足

**解決方法**:
1. Settings → Actions → General
2. "Workflow permissions" セクション
3. "Read and write permissions" を選択
4. Save

---

### エラー3: "gh: command not found"
**原因**: GitHub CLIがインストールされていない

**解決方法**: 
ワークフローファイルを以下のように修正:

```yaml
- name: Download latest SQLite DB
  continue-on-error: true
  run: |
    # GitHub APIを直接使用
    LATEST_RELEASE=$(curl -s https://api.github.com/repos/${{ github.repository }}/releases/latest | jq -r '.tag_name')
    if [ "$LATEST_RELEASE" != "null" ]; then
      curl -L -o data/stock_data.db.gz \
        "https://github.com/${{ github.repository }}/releases/download/$LATEST_RELEASE/stock_data.db.gz"
      gunzip data/stock_data.db.gz
    fi
```

---

### エラー4: "No space left on device"
**原因**: Actionsランナーのディスク容量不足

**解決方法**:
不要なファイルを削除してから実行:

```yaml
- name: Free disk space
  run: |
    sudo rm -rf /usr/share/dotnet
    sudo rm -rf /opt/ghc
    sudo rm -rf "/usr/local/share/boost"
```

---

### エラー5: GitHub Pagesが404エラー
**原因**: デプロイが完了していない、またはパスが間違っている

**解決方法**:
1. Actions → Deploy to GitHub Pages → ログを確認
2. Settings → Pages → URLを確認
3. ブラウザのキャッシュをクリア (Cmd+Shift+R)

---

### エラー6: "Database download failed" (Webページ)
**原因**: DBのURLが正しく置換されていない

**解決方法**:
web/index.html の DB_URL_PLACEHOLDER が正しく置換されているか確認:

```bash
# ローカルで確認
grep "DB_URL_PLACEHOLDER" web/index.html

# 何も表示されなければOK（置換済み）
# 表示されたら、deploy-pages.yml のsedコマンドを確認
```

---

## デバッグ用コマンド

### ローカルでワークフローをシミュレート

```bash
# データ収集のテスト
docker compose run --rm app task daily-update

# DBの確認
docker compose run --rm app sqlite3 data/stock_data.db "SELECT COUNT(*) FROM stocks;"

# 圧縮の確認
ls -lh data/stock_data.db*
```

### GitHub CLIで手動実行

```bash
# ワークフローを手動トリガー
gh workflow run daily-update.yml

# 実行状況の確認
gh run list

# ログの確認
gh run view <run-id> --log
```

---

## 連絡先

問題が解決しない場合は、以下の情報とともにIssueを作成してね:

1. エラーメッセージの全文
2. GitHub Actionsのログ（該当部分）
3. 実行環境（OS、ブラウザなど）
