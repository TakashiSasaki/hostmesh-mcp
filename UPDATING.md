# ピアの自動更新

Linux 上の単体バイナリを対象とする。ピアの起動と更新は、同じバイナリに含まれる `supervise`、または既存の systemd サービスで管理できる。更新コマンドは MCP ツールと独立したローカル CLI。平文・mTLS のどちらでも同じ MCP URL、ピアポート、設定、証明書、状態ディレクトリ、ノード ID を引き継ぐ。再起動中のクライアントは同じ URL で再接続する。

## systemd を使わない機器での起動

初回は、信頼済みの 0.0.21 以降の配布バイナリを監督用とピア用にコピーする。例えば `/opt/host-status` を用意し、次の 2 ファイルを配置する。

```sh
cp ./host-status-mcp /opt/host-status/host-status-mcp-supervisor
cp ./host-status-mcp /opt/host-status/host-status-mcp
chmod 755 /opt/host-status/host-status-mcp-supervisor /opt/host-status/host-status-mcp
```

機器の init から次のコマンドをフォアグラウンドで起動する。監督プロセス自身が終了した場合と機器の再起動時の起動は init が担当する。

```sh
/opt/host-status/host-status-mcp-supervisor supervise -config /etc/host-status/updater.json
```

`updater.json` の例。配布元の URL と署名検証用の公開鍵は実際のものに置き換える。

```json
{
  "binary": "/opt/host-status/host-status-mcp",
  "peer_config": "/etc/host-status/peer.json",
  "manifest_url": "https://updates.example.org/host-status-mcp/release.json",
  "public_key_file": "/etc/host-status/release.pub",
  "check_interval": "24h"
}
```

- `peer_config` は通常の `serve -config` に渡すピア設定の絶対パス。ピアはその設定ファイルの親ディレクトリを作業ディレクトリとして起動する。既存ピアの移行では、相対指定の台帳や証明書が元と同じ場所を指すことを確認する。MCP とピアの待受には固定ポートを使う。送信専用ピアでは更新設定に `observer_mcp_url` を指定する。
- `supervise` は配布元への接続を待たずに手元のピアを起動する。最初の更新確認は `check_interval` 経過後。省略時は 24 時間で、すぐに確認・適用するには監督プロセスの PID に `kill -USR1 <PID>` を送る。ピアが異常終了すると再起動し、起動直後の終了が続く場合は待ち時間を 1 秒から最大 30 秒に延ばす。監督プロセスへの `SIGTERM` または `SIGINT` で子も停止する。
- 新版に切り替える際は旧版のバイナリと台帳を保存し、起動後に MCP で版とノード ID を照合する。新版が起動できない場合は旧版に戻る。未確定の更新があれば、次の `supervise` 起動時にリリースの照会に先立って旧版を復旧する。更新対象は `binary` に指定した子だけ。監督用の実行ファイルを更新するときは管理者が別途入れ替えて再起動する。
- 同一アカウントで親子を実行し、Linux の `/proc` をマウントする。バイナリ配置先にはハードリンク、アトミックな rename、ファイルロックとディレクトリの同期が可能な永続ストレージを使い、候補版と台帳のコピーを保存できる空き容量を確保する。`binary` の隣の `.update-lock`、`.update-pending`、`.update-pending.stage`、`.update-pending.finished` は更新処理専用。複数ピアにはそれぞれ専用のバイナリパス、設定、台帳、ポートを割り当てる。

## systemd サービスの初回準備

以下はユーザーサービス `host-status-mcp.service` の例。対象サービスの `ExecStart` には更新対象のバイナリの絶対パスと `serve -config <ピア設定>` を指定する。MCP とピアの待受先には固定ポートを使う。ホスト上で複数のサービスを個別に更新する場合は、各サービスに専用のバイナリパスを割り当てる。

0.0.18 以降の信頼済みバイナリのコピーを更新用にも配置する。これで更新中に候補版が起動できなくなっても、同じ配布バイナリから用意した更新コマンドを実行できる。

```sh
install -Dm755 ./host-status-mcp ~/.local/libexec/host-status-mcp-updater
```

ピアのサービスアカウントで更新コマンドを実行する。Linux の `/proc` で実プロセスの設定、作業ディレクトリ、台帳を特定するため、対象プロセスの参照、バイナリ配置ディレクトリと台帳への書き込み、対象ユニットの操作が必要になる。偽の配布元への差し替えを防ぐため、更新設定、公開鍵、バイナリの配置ディレクトリは管理するアカウントだけが変更できる場所に置く。

`~/.config/host-status-mcp/updater.json` の例。配布管理者が用意する URL と、実際の絶対パスに置き換える。

```json
{
  "manifest_url": "https://updates.example.org/host-status-mcp/release.json",
  "public_key_file": "/home/alice/.config/host-status-mcp/release.pub",
  "binary": "/home/alice/.local/bin/host-status-mcp",
  "systemd_unit": "host-status-mcp.service",
  "user_service": true
}
```

`user_service: true` でユーザーの systemd マネージャーを使用する。システムユニットを管理する場合は `false` とし、サービスアカウントに対象ユニットを操作できる権限を設定する。既存のパッケージやコンテナイメージは、それぞれの配布経路で更新する。

## systemd サービスの確認と適用

```sh
~/.local/libexec/host-status-mcp-updater update check -config ~/.config/host-status-mcp/updater.json
~/.local/libexec/host-status-mcp-updater update apply -config ~/.config/host-status-mcp/updater.json
```

`check` はインストール済みと署名を検証した公開版のバージョンを表示する。`apply` は `major.minor.patch` を数値比較し、新しい安定版だけを適用する。同版・旧版の公開情報では再起動しない。正常時の終了コードは 0。更新や復旧の失敗は標準エラーと終了コード 1 で通知する。

1. 専用の Ed25519 公開鍵でマニフェストの署名を検証する。自ホスト向けのバイナリを一時保存し、サイズと SHA-256、候補版自身が返すバージョンを照合する。
2. 旧版の稼働中に、候補版の `config check` を実際のサービスと同じ作業ディレクトリ・設定パスで実行する。旧版のノード ID とバージョンを MCP で確認する。
3. 旧版バイナリを保存してサービスを停止する。台帳の排他ロックを取って `state.json` を保存し、同じファイルシステム内でバイナリをアトミックに置換してユニットを起動する。
4. systemd の MainPID が新バイナリを実行していること、従来の MCP URL の `server_version` と `server_config` が期待した版・ID を返すことを確認する。起動後の応答は最大 20 秒待つ。

メッシュ経由の疎通も確認するには、更新設定に `"observer_mcp_url": "http://127.0.0.1:8788/mcp"` など、別の稼働中ピアの MCP 入口を追加する。そこから従来の ID で `host_status` を実行し、更新前後のバージョンを照合する。MCP 待受を無効にしたピアでは、この照会元を指定して使う。

起動や応答確認が失敗した場合は、旧バイナリと停止時の台帳を復元して起動・照会を確認する。同じバイナリへの並行適用はファイルロックで防ぐ。更新プロセスの強制終了などで `.update-pending` が残ったときは、同じ設定で `update apply` を実行する。この実行は配布サーバーへの接続に先立って旧版を復旧し、終了する。更新中はユニットや設定ファイルの編集を避ける。

## systemd タイマーでの自動実行

手動の `update check` と `update apply` で配布元とサービスの設定を確認してから、同梱のユーザータイマーを有効化できる。1 日に 1 回、ホストごとに最大 1 時間のずれを設けて同じ適用処理を呼び出す。

```sh
install -Dm644 deploy/systemd/host-status-mcp-update.service ~/.config/systemd/user/host-status-mcp-update.service
install -Dm644 deploy/systemd/host-status-mcp-update.timer ~/.config/systemd/user/host-status-mcp-update.timer
systemctl --user daemon-reload
systemctl --user enable --now host-status-mcp-update.timer
journalctl --user -u host-status-mcp-update.service
```

## 署名付きリリースの作成

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

スクリプトは Linux ARM64 と x86_64 向けの実行ファイル、`release.json`、そのバイト列に対する Ed25519 の署名（64 バイトの `release.json.sig`）を生成する。バイナリはバージョン別の名前で配布し、署名・マニフェストは組として公開する。不一致やダウンロードの途切れは適用前に検出される。公開 URL は HTTPS を使い、ローカル検証ではループバック IP への HTTP も利用できる。署名鍵は配布ホストにアップロードするファイルから除外する。
