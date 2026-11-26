# インストール

コマンド名は Linux・macOS では `hostmesh-mcp`、Windows では `hostmesh-mcp.exe` です。Windows x64 は CI でテスト済みの実行ファイルを取得できます。正式版を公開すると、GitHub Releases にも Linux 用バイナリと Windows 用 ZIP が掲載されます。[リリース一覧](https://github.com/TakashiSasaki/hostmesh-mcp/releases)

| 使う環境 | 導入方法 |
| --- | --- |
| Linux（ARM64、x86-64） | 対象ホストでビルド。小型機向けには別のホストからもビルド可能 |
| macOS（Apple Silicon、Intel） | Mac のターミナルでビルド。どちらの CPU でも同じ手順 |
| Windows x64 | [Windows ホストにネイティブ版を導入](#windows) |

## 既存環境からの更新

0.0.35 の新規インストールは OS のユーザー設定ディレクトリ配下の `hostmesh-mcp/state` に台帳を作成します。以前の既定の場所に台帳があり、新しい場所に台帳がない場合は、同じノード ID で運転を続けるため以前の場所を使います。保存先も統一するには、台帳を使うプロセスを停止し、新しい保存先に既存台帳がないことを確認してから、使用中の `state` ディレクトリ全体を移動します。実際の保存先は監視 MCP の `server_config` が返す `state_dir` で確認できます。明示的な `state_dir` は引き続き有効です。

mTLS メッシュは既存証明書のままピアを先に更新し、最後に発行局を更新してください。0.0.35 の発行局は新規登録・更新とも `urn:hostmesh-mcp:node:<UUID>` を発行します。0.0.34 以前のピアはこの識別子を認識できません。配備済みサービスや MCP クライアントで実行ファイルを指定している場合は、その起動パスも確認してください。

## Linux と macOS

Git と Go を用意します。この版が指定する Go ツールチェーンは 1.27.3 です。Go のインストーラーは、Linux では ARM64 または AMD64、macOS では Apple Silicon 用の ARM64 または Intel 用の AMD64 を選びます。[Go の導入手順](https://go.dev/doc/install)

ターミナルで、次のコマンドを実行します。main ブランチを取得し、実行ファイルを自分のホームディレクトリに配置します。

```sh
git clone https://github.com/TakashiSasaki/hostmesh-mcp.git
cd hostmesh-mcp
mkdir -p "$HOME/.local/bin"
CGO_ENABLED=0 go build -trimpath -o "$HOME/.local/bin/hostmesh-mcp" ./cmd/hostmesh-mcp
"$HOME/.local/bin/hostmesh-mcp" version
```

最後にビルドしたバージョンが表示されれば導入できています。MCP クライアントには次のコマンドが表示する絶対パスを登録し、引数に `stdio` を1つ指定します。接続後に `server_version` を呼び出し、同じ版が返ることを確認してください。

```sh
printf '%s\n' "$HOME/.local/bin/hostmesh-mcp"
```

メッシュのピアとして起動する場合は、設定ファイルを新規作成するディレクトリで次を実行します。ひな型はローカルで試すためのループバック設定です。別ホストから参加する場合は待受・広告アドレスを先に編集してください。[2 台を接続する手順](README.md#2-台を接続する)

```sh
"$HOME/.local/bin/hostmesh-mcp" config example > peer.json
"$HOME/.local/bin/hostmesh-mcp" config check -config peer.json
"$HOME/.local/bin/hostmesh-mcp" serve -config peer.json
```

既定の MCP 接続先は `http://127.0.0.1:8787/mcp` です。macOS ではプロセス一覧と個別照会を利用でき、プロセス終了ツールは対象外です。Linux での常駐・バイナリ自動更新の設定は[更新ガイド](UPDATING.md)にあります。

### 小型 Linux 機器への転送

対象機器に Go を入れる余裕がない場合は、上で取得したソースディレクトリでクロスビルドし、生成した実行ファイルを機器にコピーできます。以下は **64 ビット ARM の Linux** 向けです。コピー先で実行権限を付け、`./hostmesh-mcp version` で確認します。

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOARM64=v8.0 go build -trimpath -o hostmesh-mcp ./cmd/hostmesh-mcp
```

32 ビット ARM は CI の検証対象外です。32 ビット OS を搭載した機器では、CPU と OS に合う `GOARCH=arm`・`GOARM` を選び、対象機器上で動作を確認してください。

## Windows

監視したい Windows x64 マシン上で `hostmesh-mcp.exe` を実行します。Windows の OS・カーネル、CPU、物理メモリ、ページファイル、プロセス、ネットワークインターフェース、ドライブ文字・ボリュームを一般ユーザーの権限で照会できます。

### テスト済みの exe を使う

1. GitHub にサインインして [main の CI 一覧](https://github.com/TakashiSasaki/hostmesh-mcp/actions/workflows/ci.yml?query=branch%3Amain) を開き、Windows ジョブが成功した実行を選びます。実行概要の **Artifacts** から `hostmesh-mcp-windows-amd64` をダウンロードします。
2. ZIP 内の `hostmesh-mcp.exe` を、自分のユーザーディレクトリの `AppData\Local\Programs\hostmesh-mcp` に配置します。PowerShell で次を実行すると、ダウンロード・フォルダーの既定のファイル名から展開し、インストールを確認できます。

   ```powershell
   $installDir = Join-Path $env:LOCALAPPDATA 'Programs\hostmesh-mcp'
   Expand-Archive -LiteralPath "$HOME\Downloads\hostmesh-mcp-windows-amd64.zip" -DestinationPath $installDir -Force
   $exe = Join-Path $installDir 'hostmesh-mcp.exe'
   & $exe version
   $exe
   ```

3. Windows の MCP クライアントに、最後に表示された **exe の絶対パス**をコマンド、`["stdio"]` を引数として登録します。接続後、`host_status` に次の引数を渡し、`node.os` が `windows` であること、`storage.mounts` にシステムドライブがあることを確認します。

   ```json
   {"sections":["system","memory","swap","processes","storage"]}
   ```

ピアとして HTTP で起動する場合は、新しい設定ファイルの保存先で次を実行します。PowerShell の世代による既定の文字コードの違いを避けるため JSON を UTF-8 で保存します。別マシンからメッシュに参加する方法は[ピア接続の手順](README.md#2-台を接続する)を参照してください。

```powershell
[IO.File]::WriteAllText((Join-Path $PWD 'peer.json'), ((& $exe config example) -join [Environment]::NewLine))
& $exe config check -config peer.json
& $exe serve -config peer.json
```

### PowerShell でソースからビルドする

CI の成果物が保存期間を過ぎている場合も、Windows 用の Git と Go を導入し、次のコマンドで同じ場所にネイティブ版をインストールできます。[Go の導入手順](https://go.dev/doc/install)

```powershell
git clone https://github.com/TakashiSasaki/hostmesh-mcp.git
cd hostmesh-mcp
$installDir = Join-Path $env:LOCALAPPDATA 'Programs\hostmesh-mcp'
New-Item -ItemType Directory -Force $installDir | Out-Null
$exe = Join-Path $installDir 'hostmesh-mcp.exe'
$env:CGO_ENABLED = '0'
go build -trimpath -o $exe ./cmd/hostmesh-mcp
& $exe version
```
