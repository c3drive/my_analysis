FROM ubuntu:24.04

# 必要なパッケージのインストール
RUN apt-get update && apt-get install -y \
    curl \
    git \
    unzip \
    build-essential \
    sqlite3 \
    bc \
    && rm -rf /var/lib/apt/lists/*

# miseのインストール
RUN curl https://mise.jdx.dev/install.sh | sh

# 環境変数の設定
ENV PATH="/root/.local/bin:$PATH"
# 【追加】プロジェクトディレクトリの設定ファイルを信頼させる
ENV MISE_TRUSTED_CONFIG_PATHS="/app"

# ワークディレクトリの設定
WORKDIR /app

# ツールのインストール準備
COPY .mise.toml ./

# インストール実行
RUN mise install -y && mise reshim
ENV PATH="/root/.local/share/mise/shims:$PATH"

# Goのパス設定
ENV GOPATH="/go"
ENV PATH="/go/bin:$PATH"

# デフォルトのコマンド
CMD ["bash"]