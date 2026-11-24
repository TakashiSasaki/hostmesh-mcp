# host-status-mcp

Linux PoC。MCP からローカルまたは別ホストの OS、稼働時間、CPU、メモリ、ファイルシステム容量を取得する。ノードは台帳を交換し、到達できるペアを自動で直結する。共通の中継を許可すると、直結できないペアも照会できる。

## ビルドと単体起動

```sh
go build -o host-status-mcp ./cmd/host-status-mcp
./host-status-mcp version
./host-status-mcp stdio
```

MCP クライアントのコマンド設定では、実行ファイルへの絶対パスと引数 `stdio` を指定する。既定の状態ディレクトリは `os.UserConfigDir()` 配下の `host-status-mcp/state`。同じマシンで複数ノードを動かすときは、ノードごとに `-state-dir` または設定の `state_dir` を指定する。

## 2 台を接続する

A の設定 `a.json` の例。

```json
{
  "mcp_listen": "127.0.0.1:8787",
  "state_dir": "/tmp/host-status-demo-a",
  "filesystems": ["/"],
  "peer": {
    "security": "plaintext",
    "listen": "127.0.0.1:7443",
    "advertise": ["127.0.0.1:7443"]
  }
}
```

B の設定 `b.json` の例。

```json
{
  "mcp_listen": "",
  "state_dir": "/tmp/host-status-demo-b",
  "peer": {
    "security": "plaintext",
    "seeds": ["127.0.0.1:7443"]
  }
}
```

別々の端末で `./host-status-mcp serve -config a.json` と `./host-status-mcp serve -config b.json` を実行する。上の設定は同じ Linux ホストで試すためのもの。別ホストで使う場合は `peer.listen` のアドレス、`peer.advertise`、`peer.seeds` に閉域で到達できるアドレスを使う。平文モードでは `node_id` と台帳を参加ピアの申告として扱う。

MCP クライアントから A の `http://127.0.0.1:8787/mcp` に接続する。`host_list` で B の ID を確認し、`host_status` に `{"target":{"node_id":"B の ID"},"sections":["memory"]}` を渡す。接続は B が始めるが、A がその双方向リンクで B に採取を依頼できる。`target` を省略するとローカルを採取する。

A と B が C を共通中継にする場合は、C で `peer.relay: true` と `peer.listen` を設定する。A と B のシードに C のアドレスを設定し、両方の `peer.trusted_relays` に C の起動ログに出る `node_id` を追加する。`peer.deny_nodes` に設定した ID の接続と照会を拒否する。設定変更はプロセスの起動時に適用する。

## 状態照会

`server_version` は、MCP クライアントが接続しているサーバーの名前とバージョンを返す。指定したピアのバージョンは `host_status` の `system.server_version` に含まれる。

`host_status` で `sections` を省略すると `system`、`cpu`、`memory`、`filesystems` を取得する。追加項目は `sections` で指定する。

```json
{
  "target": {"node_id": "対象ノードの ID"},
  "sections": ["swap", "network", "processes"]
}
```

| 項目 | 内容 |
| --- | --- |
| `swap` | Linux のスワップ有効状態と総量、使用量、空き容量（バイト）。 |
| `network` | 対象ノードから見たインターフェース名、稼働フラグ、MTU、IPv4・IPv6 のプレフィックス付きアドレス。`peer.share_addresses: false` のノードはピア向け応答でアドレスを空にする。 |
| `processes` | 対象から見えるプロセスの PID、名前、CPU 使用率、常駐メモリ。CPU 使用率が高い順に最大 100 件を表示し、採取件数、読み取れなかった件数、打ち切りの有無を添える。 |
| `storage` | マウント先、マウント元、ファイルシステムの種類、読み取り専用状態と、Linux から見えるディスク、パーティション、論理ボリューム。 |
| `clock` | 照会先の現在時刻、タイムゾーンの略称、UTC からのオフセット（秒）。 |

プロセスの CPU 使用率は約 1 秒間隔の 2 サンプルから計算し、1 コアを使い切ると 100% になる。計測中に起動したプロセスなど、差分を取れない場合は `cpu_percent` が `null` になる。Docker では採取元コンテナから見えるプロセスとインターフェースが返る。

## ホストの時計

`host_status` に `{"sections":["clock"]}` を渡す。別ホストの時計を読む場合は `target` を指定する。`utc_time` と `local_time` は同じ瞬間を UTC と現地時刻で表した値。`timezone_abbreviation` と `utc_offset_seconds` は、採取元の MCP サーバープロセスで有効なタイムゾーンに従う。

## ストレージ一覧

`host_status` に `{"sections":["storage"]}` を渡す。別ホストを調べる場合は `target` にそのノードを指定する。

- `mounts` はマウント先とマウント元、ファイルシステムの種類、読み取り専用かどうかを返す。ネットワーク共有や `tmpfs` も含み、対応するブロックデバイスを確認できたマウントには `block_device_id` が付く。
- `block_devices` はディスク、パーティション、device-mapper（LVM や暗号化デバイスなど）、ソフトウェア RAID、ループデバイスを返す。取得できた容量、読み取り専用状態、パーティションの親やボリュームの下位デバイスも含む。未マウントのデバイスも対象になる。

空き容量を調べるには、既存の `filesystems` で設定済みパスを照会する。Linux コンテナではマウント一覧はコンテナから見える範囲になり、ブロックデバイス一覧はそのコンテナに公開された sysfs に従う。Windows のドライブ文字は Windows 対応時に扱う。

## ローカルプロセスの終了

`server_version`、`host_list`、`host_status` は読み取り専用として通知する。`process_kill` は破壊的な書き込みツールとして通知し、接続先 MCP サーバーの Linux プロセスにシグナルを送る。stdio では利用できる。ループバックの HTTP で有効にするには、設定のトップレベルに `"allow_process_kill": true` を追加する。

`host_status` に `{"sections":["processes"]}` を渡し、ローカルの一覧から対象の `pid` と `process_identity` をコピーする。次の例の値を対象のプロセスに合わせて置き換える。

```json
{"pid": 1234, "process_identity": "一覧で取得した識別子", "signal": "TERM"}
```

`signal` の既定値は `TERM`。強制終了するには `KILL` を明示する。`signal_sent: true` は OS がシグナル送信を受け付けたことを示す。終了したかはプロセス一覧で確認する。PID 1 と MCP サーバー自身への送信は拒否する。送信には実行ユーザーの OS 権限と Linux の pidfd 対応が必要。

## テスト

```sh
go test ./...
go test -race ./internal/peer ./internal/state
HOST_STATUS_DOCKER_TEST=1 go test -v ./cmd/host-status-mcp -run '^TestDockerRelay$' -count=1
HOST_STATUS_SCALE_TEST=1 go test -v ./internal/peer -run '^TestMesh100$' -count=1
```

通常のテストはループバック TCP と一時ディレクトリを使う。Docker 試験はローカルの Docker Engine を使い、A と B の TCP を 2 つの独立したブリッジで分離して C 経由の照会と再起動後の復旧を確認する。100 台の試験はメモリ内 gRPC で、フルメッシュと単一中継の両方を確認する。実バイナリでプロセス試験を繰り返すには、絶対パスを `HOST_STATUS_TEST_BINARY` に設定して `go test -count=1 ./cmd/host-status-mcp` を実行する。

ピアの型を変更した場合は、`protoc` 36.2、`protoc-gen-go` v1.36.12、`protoc-gen-go-grpc` v1.6.0 を使う。

```sh
protoc -I . --go_out=paths=source_relative:. --go-grpc_out=paths=source_relative:. proto/hoststatus/peer/v1/peer.proto
```

[設計](DESIGN.md) と [テスト設計](TESTING.md) に Linux の ARM64・x86_64 配布、curl による導入、後続の OS 対応と任意の mTLS の条件を記載している。
