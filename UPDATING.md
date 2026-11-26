# ピアの自動更新

Linux 上の単体バイナリを対象とする。ピアの起動と更新は、同じバイナリに含まれる `supervise`、または既存の systemd サービスで管理できる。更新はローカル CLI または管理用 STDIO MCP の `update_check`・`update_apply` から実行できる。平文・mTLS のどちらでも同じ MCP URL、ピアポート、設定、証明書、状態ディレクトリ、ノード ID を引き継ぐ。再起動中のクライアントは同じ URL で再接続する。

## systemd を使わない機器での起動

初回は、GitHub 更新に対応した 0.0.30 以降の信頼済みバイナリを監督用とピア用にコピーする。例えば `/opt/hostmesh-mcp` を用意し、次の 2 ファイルを配置する。

```sh
cp ./hostmesh-mcp /opt/hostmesh-mcp/hostmesh-mcp-supervisor
cp ./hostmesh-mcp /opt/hostmesh-mcp/hostmesh-mcp
chmod 755 /opt/hostmesh-mcp/hostmesh-mcp-supervisor /opt/hostmesh-mcp/hostmesh-mcp
```

機器の init から次のコマンドをフォアグラウンドで起動する。監督プロセス自身が終了した場合と機器の再起動時の起動は init が担当する。

```sh
/opt/hostmesh-mcp/hostmesh-mcp-supervisor supervise -config /etc/hostmesh-mcp/updater.json
```

`updater.json` の例。`source` に `github` を指定し、パスは機器の配置に合わせる。

```json
{
  "binary": "/opt/hostmesh-mcp/hostmesh-mcp",
  "peer_config": "/etc/hostmesh-mcp/peer.json",
  "source": "github",
  "check_interval": "24h"
}
```

- `peer_config` は通常の `serve -config` に渡すピア設定の絶対パス。ピアはその設定ファイルの親ディレクトリを作業ディレクトリとして起動する。既存ピアの移行では、相対指定の台帳や証明書が元と同じ場所を指すことを確認する。MCP とピアの待受には固定ポートを使う。送信専用ピアでは更新設定に `observer_mcp_url` を指定する。
- `supervise` は配布元への接続を待たずに手元のピアを起動する。最初の更新確認は `check_interval` 経過後。省略時は 24 時間で、すぐに確認・適用するには監督プロセスの PID に `kill -USR1 <PID>` を送る。ピアが異常終了すると再起動し、起動直後の終了が続く場合は待ち時間を 1 秒から最大 30 秒に延ばす。監督プロセスへの `SIGTERM` または `SIGINT` で子も停止する。
- 新版に切り替える際は旧版のバイナリと台帳を保存し、起動後に MCP で版とノード ID を照合する。新版が起動できない場合は旧版に戻る。未確定の更新があれば、次の `supervise` 起動時にリリースの照会に先立って旧版を復旧する。更新対象は `binary` に指定した子だけ。監督用の実行ファイルを更新するときは管理者が別途入れ替えて再起動する。
- 同一アカウントで親子を実行し、Linux の `/proc` をマウントする。バイナリ配置先にはハードリンク、アトミックな rename、ファイルロックとディレクトリの同期が可能な永続ストレージを使い、候補版と台帳のコピーを保存できる空き容量を確保する。`binary` の隣の `.update-lock`、`.update-pending`、`.update-pending.stage`、`.update-pending.finished` は更新処理専用。複数ピアにはそれぞれ専用のバイナリパス、設定、台帳、ポートを割り当てる。

## systemd サービスの初回準備

以下はユーザーサービス `hostmesh-mcp.service` の例。対象サービスの `ExecStart` には更新対象のバイナリの絶対パスと `serve -config <ピア設定>` を指定する。MCP とピアの待受先には固定ポートを使う。ホスト上で複数のサービスを個別に更新する場合は、各サービスに専用のバイナリパスを割り当てる。

0.0.30 以降の信頼済みバイナリのコピーを更新用にも配置する。これで更新中に候補版が起動できなくなっても、同じ配布バイナリから用意した更新コマンドを実行できる。

```sh
install -Dm755 ./hostmesh-mcp ~/.local/libexec/hostmesh-mcp-updater
```

ピアのサービスアカウントで更新コマンドを実行する。Linux の `/proc` で実プロセスの設定、作業ディレクトリ、台帳を特定するため、対象プロセスの参照、バイナリ配置ディレクトリと台帳への書き込み、対象ユニットの操作が必要になる。偽の配布元への差し替えを防ぐため、更新設定とバイナリの配置ディレクトリは管理するアカウントだけが変更できる場所に置く。

`~/.config/hostmesh-mcp/updater.json` の例。実際の絶対パスとユニット名に置き換える。

```json
{
  "source": "github",
  "binary": "/home/alice/.local/bin/hostmesh-mcp",
  "systemd_unit": "hostmesh-mcp.service",
  "user_service": true
}
```

`user_service: true` でユーザーの systemd マネージャーを使用する。システムユニットを管理する場合は `false` とし、サービスアカウントに対象ユニットを操作できる権限を設定する。既存のパッケージやコンテナイメージは、それぞれの配布経路で更新する。

## systemd サービスの確認と適用

```sh
~/.local/libexec/hostmesh-mcp-updater update check -config ~/.config/hostmesh-mcp/updater.json
~/.local/libexec/hostmesh-mcp-updater update apply -config ~/.config/hostmesh-mcp/updater.json
```

`check` はインストール済みと署名を検証した公開版のバージョンを表示する。`apply` は `major.minor.patch` を数値比較し、新しい安定版だけを適用する。同版・旧版の公開情報では再起動しない。正常時の終了コードは 0。更新や復旧の失敗は標準エラーと終了コード 1 で通知する。

1. GitHub が署名したリリース証明と、タグに紐付くファイル名・SHA-256 を照合する。自ホスト向けのバイナリを一時保存し、サイズと SHA-256、候補版自身が返すバージョンを照合する。
2. 旧版の稼働中に、候補版の `config check` を実際のサービスと同じ作業ディレクトリ・設定パスで実行する。旧版のノード ID とバージョンを MCP で確認する。
3. 旧版バイナリを保存してサービスを停止する。台帳の排他ロックを取って `state.json` を保存し、同じファイルシステム内でバイナリをアトミックに置換してユニットを起動する。
4. systemd の MainPID が新バイナリを実行していること、従来の MCP URL の `server_version` と `server_config` が期待した版・ID を返すことを確認する。起動後の応答は最大 20 秒待つ。

メッシュ経由の疎通も確認するには、更新設定に `"observer_mcp_url": "http://127.0.0.1:8788/mcp"` など、別の稼働中ピアの MCP 入口を追加する。そこから従来の ID で `host_status` を実行し、更新前後のバージョンを照合する。MCP 待受を無効にしたピアでは、この照会元を指定して使う。

起動や応答確認が失敗した場合は、旧バイナリと停止時の台帳を復元して起動・照会を確認する。同じバイナリへの並行適用はファイルロックで防ぐ。更新プロセスの強制終了などで `.update-pending` が残ったときは、同じ設定で `update apply` を実行する。この実行は配布サーバーへの接続に先立って旧版を復旧し、終了する。更新中はユニットや設定ファイルの編集を避ける。

## systemd タイマーでの自動実行

手動の `update check` と `update apply` で配布元とサービスの設定を確認してから、同梱のユーザータイマーを有効化できる。1 日に 1 回、ホストごとに最大 1 時間のずれを設けて同じ適用処理を呼び出す。

```sh
install -Dm644 deploy/systemd/hostmesh-mcp-update.service ~/.config/systemd/user/hostmesh-mcp-update.service
install -Dm644 deploy/systemd/hostmesh-mcp-update.timer ~/.config/systemd/user/hostmesh-mcp-update.timer
systemctl --user daemon-reload
systemctl --user enable --now hostmesh-mcp-update.timer
journalctl --user -u hostmesh-mcp-update.service
```

以前の名前でタイマーを登録済みの場合は、切り替え前に旧タイマーを停止・無効化する。既存の `updater.json` を新しいユニットの `-config` が参照する場所に配置し、`systemd_unit` は実際に運転しているサービス名に合わせる。

## GitHub Releases で公開する

CI が通った `main` の配布対象コミットで、次を実行する。通常の push はテストと配布物の組み立てまでを行う。

```sh
tag="v$(cat cmd/hostmesh-mcp/VERSION)"
git tag -a "$tag" -m "Release $tag"
git push github "refs/tags/$tag"
```

タグ名と `VERSION` が一致し、Linux・macOS・Windows のテストが通ると、Actions が Linux の amd64 / arm64 の実行ファイルと Windows x64 の ZIP をドラフトに添付して公開する。GitHub の immutable releases を有効にしておく。Actions は公開された各ファイルを `gh release verify-asset` で検証してから安定版を Latest に指定する。失敗時は Actions のログを確認する。公開後の内容を修正するときは、新しいパッチ番号でリリースする。

ピアはトークンなしで公式リポジトリの Latest を取得し、固定のリポジトリ ID、リリース ID、タグ、資産名、SHA-256 を GitHub の署名済み証明と照合する。署名者と署名時刻の検証には GitHub の TUF 信頼ルートと Sigstore を使う。ピアから GitHub API、リリース配布先、証明の配布先、`tuf-repo.github.com` に HTTPS で到達できることが必要。取得・検証に失敗した場合は稼働中の版を継続する。新方式の初回導入では、ピア本体と更新を実行する監督用または更新用コピーの双方を手動で入れ替える。

## 独自の署名付きマニフェストで配布する場合

既存の配布元を使う場合は、更新設定で `source` を省略するか `manifest` を指定し、`manifest_url` と `public_key_file` を設定する。以下はこの方式専用の手順。

配布管理者は最初にリリース専用の署名鍵を用意し、公開鍵を初回導入時に各ホストに渡す。ピア mTLS 用 CA の運用とは独立している。

```sh
umask 077
openssl genpkey -algorithm Ed25519 -out release.key
openssl pkey -in release.key -pubout -out release.pub
```

配布するリビジョンのリポジトリ直下で、新規ディレクトリと署名鍵を指定する。

```sh
scripts/build-release.sh ./dist-release /absolute/path/to/release.key
```

0.0.26 から配布ファイル名は `hostmesh-mcp-<version>-linux-<arch>`。0.0.35 から署名付きマニフェストの製品 ID は `hostmesh-mcp`。独自配布で切り替える際は、先に各ホストの監督用・更新用プログラムを 0.0.35 以降に更新する。新版は従来の署名付きマニフェストも検証できる。GitHub Releases 方式はこの製品 ID を使用しない。更新先は設定の `binary` で指定する。

スクリプトは Linux ARM64 と x86_64 向けの実行ファイル、`release.json`、そのバイト列に対する Ed25519 の署名（64 バイトの `release.json.sig`）を生成する。バイナリはバージョン別の名前で配布し、署名・マニフェストは組として公開する。不一致やダウンロードの途切れは適用前に検出される。公開 URL は HTTPS を使い、ローカル検証ではループバック IP への HTTP も利用できる。署名鍵は配布ホストにアップロードするファイルから除外する。
