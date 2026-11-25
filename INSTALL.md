# インストール

バージョン 0.0.23 の導入手順です。リポジトリ名は `hostmesh-mcp`、インストールするコマンド名は `host-status-mcp` です。この版のビルド済みバイナリは GitHub Releases で未配布のため、以下ではソースからインストールします。[リリース一覧](https://github.com/TakashiSasaki/hostmesh-mcp/releases)

| 使う環境 | 導入方法 |
| --- | --- |
| Linux（ARM64、x86-64） | 対象ホストでビルド。小型機向けには別のホストからもビルド可能 |
| macOS（Apple Silicon、Intel） | Mac のターミナルでビルド。どちらの CPU でも同じ手順 |
| Windows x64 | ネイティブ版は未対応。WSL 2 内の Linux を監視する場合は [Windows での手順](#windows) を使用 |

## Linux と macOS

Git と Go を用意します。この版が指定する Go ツールチェーンは 1.27.3 です。Go のインストーラーは、Linux では ARM64 または AMD64、macOS では Apple Silicon 用の ARM64 または Intel 用の AMD64 を選びます。[Go の導入手順](https://go.dev/doc/install)

ターミナルで、次のコマンドを実行します。CI で検証した 0.0.23 のコミットを取得し、実行ファイルを自分のホームディレクトリに配置します。

```sh
git clone https://github.com/TakashiSasaki/hostmesh-mcp.git
cd hostmesh-mcp
git switch --detach 0482a64e0e144e2dcc1b428180cb66aca046cba1
mkdir -p "$HOME/.local/bin"
CGO_ENABLED=0 go build -trimpath -o "$HOME/.local/bin/host-status-mcp" ./cmd/host-status-mcp
"$HOME/.local/bin/host-status-mcp" version
```

最後に `0.0.23` が表示されれば導入できています。MCP クライアントには次のコマンドが表示する絶対パスを登録し、引数に `stdio` を1つ指定します。接続後に `server_version` を呼び出し、同じ版が返ることを確認してください。

```sh
printf '%s\n' "$HOME/.local/bin/host-status-mcp"
```

メッシュのピアとして起動する場合は、設定ファイルを新規作成するディレクトリで次を実行します。ひな型はローカルで試すためのループバック設定です。別ホストから参加する場合は待受・広告アドレスを先に編集してください。[2 台を接続する手順](README.md#2-台を接続する)

```sh
"$HOME/.local/bin/host-status-mcp" config example > peer.json
"$HOME/.local/bin/host-status-mcp" config check -config peer.json
"$HOME/.local/bin/host-status-mcp" serve -config peer.json
```

既定の MCP 接続先は `http://127.0.0.1:8787/mcp` です。macOS ではプロセス一覧と個別照会を利用でき、プロセス終了ツールは対象外です。Linux での常駐・バイナリ自動更新の設定は[更新ガイド](UPDATING.md)にあります。

### 小型 Linux 機器への転送

対象機器に Go を入れる余裕がない場合は、上で取得したソースディレクトリでクロスビルドし、生成した実行ファイルを機器にコピーできます。以下は **64 ビット ARM の Linux** 向けです。コピー先で実行権限を付け、`./host-status-mcp version` で確認します。

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOARM64=v8.0 go build -trimpath -o host-status-mcp ./cmd/host-status-mcp
```

32 ビット ARM は CI の検証対象外です。32 ビット OS を搭載した機器では、CPU と OS に合う `GOARCH=arm`・`GOARM` を選び、対象機器上で動作を確認してください。

## Windows

0.0.23 には Windows ネイティブの実行ファイルがありません。Windows 自体のプロセス、ドライブ、ページファイルの監視は未対応です。**WSL 2 内の Linux の状態**を Windows の MCP クライアントから調べたい場合は、次の手順で Linux 版を起動できます。この構成はプロジェクトの CI 対象外です。

1. Ubuntu を導入済みの場合は、PowerShell で `wsl --list --verbose` を実行し、Ubuntu の `VERSION` が `2` であることを確認します。初めて使う場合は、管理者 PowerShell で `wsl --install -d Ubuntu` を実行し、求められたら Windows を再起動して Ubuntu のユーザーを作成します。[Microsoft の WSL 導入手順](https://learn.microsoft.com/ja-jp/windows/wsl/install)
2. PowerShell で `wsl -d Ubuntu` を実行して Ubuntu のシェルに入り、`cd ~` してから上の [Linux と macOS](#linux-と-macos) の手順を実行します。Go と Git は Ubuntu 側にインストールします。
3. Windows 側の PowerShell で、次のように起動を確認します。`/home/alice/.local/bin/host-status-mcp` は、Ubuntu 側の手順で表示した絶対パスに置き換えます。

   ```powershell
   wsl.exe -d Ubuntu --exec /home/alice/.local/bin/host-status-mcp version
   ```

Windows の MCP クライアントには、コマンドとして `wsl.exe`、引数として次の配列を指定します。接続確認には `server_version` を使用します。

```json
["-d", "Ubuntu", "--exec", "/home/alice/.local/bin/host-status-mcp", "stdio"]
```
