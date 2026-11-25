# host-status-mcp

Linux PoC。MCP からローカルまたは別ホストの OS、稼働時間、CPU、メモリ、ファイルシステム容量を取得する。ノードは台帳を交換し、到達できるペアを自動で直結する。共通の中継を許可すると、直結できないペアも照会できる。

## ビルドと単体起動

```sh
go build -o host-status-mcp ./cmd/host-status-mcp
./host-status-mcp version
./host-status-mcp stdio
```

MCP クライアントのコマンド設定では、実行ファイルへの絶対パスと引数 `stdio` を指定する。既定の状態ディレクトリは `os.UserConfigDir()` 配下の `host-status-mcp/state`。同じマシンで複数ノードを動かすときは、ノードごとに `-state-dir` または設定の `state_dir` を指定する。

## ピア設定のひな型を出力する

`config example` は編集用の JSON を標準出力に表示する。

```sh
./host-status-mcp config example                     # 待ち受けるピア
./host-status-mcp config example -join               # 既存のピアに発信するノード
./host-status-mcp config example -mtls               # mTLS で待ち受けるピア
./host-status-mcp config example -join -mtls > b.json
```

例のアドレスは `127.0.0.1:7443`。別ホスト間で使うときは、待ち受け側の `listen` に自分の閉域アドレス、`advertise` と参加側の `seeds` に参加側から到達できる待ち受け先を指定する。`-mtls` は CA 証明書・ノード証明書・秘密鍵のパスを追加する。各ノードのファイルに合わせて編集し、`./host-status-mcp serve -config b.json` で起動する。各項目の意味は `./host-status-mcp config example -help` でも確認できる。

同じホスト上で両方のひな型を使う場合は、次の例のようにノードごとに異なる `state_dir` を指定し、参加側で `"mcp_listen": ""` または別のループバックポートを設定する。

## 設定ファイルを検査する

```sh
./host-status-mcp config check -config peer.json
```

`serve` と共通の規則で JSON、設定項目、待受先の形式、ピア設定の整合性を検査する。mTLS の場合は、指定ファイルを読み、CA によるノード証明書の検証と秘密鍵との照合も行う。成功時は標準出力に `Configuration OK` とピアの通信方式、実効シードの件数と取得元を表示して終了コード 0 を返す。設定エラー時は標準エラー出力に理由を表示して 1 を返す。

稼働中のノードと同じ設定にも使える。状態ディレクトリの作成・ロック・更新、待ち受け、ピアへの接続は行わない。ポートの空き状況、永続化済みノード ID との一致、ピアへの到達性は実際の起動・接続時に確認される。

## 2 台を接続する

A の設定 `a.json` の例。

```json
{
  "mcp_listen": "127.0.0.1:8787",
  "state_dir": "/tmp/host-status-demo-a",
  "filesystems": ["/"],
  "peer": {
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
    "seeds": ["127.0.0.1:7443"]
  }
}
```

別々の端末で `./host-status-mcp serve -config a.json` と `./host-status-mcp serve -config b.json` を実行する。上の設定は同じ Linux ホストで試すためのもの。別ホストで使う場合は `peer.listen` のアドレス、`peer.advertise`、`peer.seeds` に閉域で到達できるアドレスを使う。平文モードでは `node_id` と台帳を参加ピアの申告として扱う。

MCP クライアントから A の `http://127.0.0.1:8787/mcp` に接続する。`host_list` で B の ID を確認し、`host_status` に `{"target":{"node_id":"B の ID"},"sections":["memory"]}` を渡す。接続は B が始めるが、A がその双方向リンクで B に採取を依頼できる。`target` を省略するとローカルを採取する。 `host_list` の `is_local: true` は、MCP クライアントが接続しているノードを示す。同じホスト名を持つピアは `node_id` で指定できる。

A と B が C を共通中継にする場合は、C で `peer.relay: true` と `peer.listen` を設定する。A と B のシードに C のアドレスを設定し、両方の `peer.trusted_relays` に C の起動ログに出る `node_id` を追加する。`peer.deny_nodes` に設定した ID の接続と照会を拒否する。設定変更はプロセスの起動時に適用する。

## ピア間の mTLS

ピアの通信方式は `serve` の起動時に決まる。`peer.ca_file`、`peer.cert_file`、`peer.key_file` をすべて省略すると平文、3 点をすべて指定すると mTLS で起動する。一部だけ指定した場合や、証明書・鍵の読み込みや検証に失敗した場合は起動エラーになる。既存の `peer.security` 設定は省略できる。指定する場合は証明書の構成と一致する値（`plaintext` または `mtls`）を使う。

待ち受けと発信には同じ方式を使う。異なる方式のシードを指定してもハンドシェイクで終了し、その相手とは台帳の交換も状態照会の中継も行わない。mTLS では TLS 1.3 で相互認証し、ノード ID と台帳レコードの署名を検証する。MCP クライアントは stdio またはループバック HTTP で接続する。

次は A のピア設定例。B には B 固有の証明書と鍵を配置し、`seeds` に `["a.example.internal:7443"]` を指定する。既存ノードは `state_dir` を引き継ぎ、`host_list` の `is_local: true` に表示された UUID を証明書に使う。新規ノードは UUID v4 を一つ発行し、その値を証明書と初回の永続 ID に使う。

```json
{
  "peer": {
    "listen": ":7443",
    "advertise": ["a.example.internal:7443"],
    "ca_file": "/etc/host-status-mcp/ca.pem",
    "cert_file": "/etc/host-status-mcp/node.pem",
    "key_file": "/etc/host-status-mcp/node.key"
  }
}
```

**証明書の発行例**。各ノードで新しい作業ディレクトリに秘密鍵と CSR を作成し、CSR を CA 管理者に渡す。

```sh
umask 077
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out node.key
openssl req -new -key node.key -subj '/CN=a.example.internal' -out node.csr
```

CA 管理者はノードの UUID と接続用の DNS 名・IP を確認し、以下の `node.ext` の例をそのノードの値に置き換える。発信にも待ち受けにも使う証明書には両方の拡張鍵用途を設定する。

```ini
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature
extendedKeyUsage=clientAuth,serverAuth
subjectAltName=URI:urn:host-status-mcp:node:7043f65e-b1f6-489d-a0d2-5d402035735d,DNS:a.example.internal,IP:192.0.2.10
```

CA 管理者は、参加ノード用の `node.ext` に初期接続先を設定する次の 1 行を追加できる。値は非クリティカル X.509 拡張の DER UTF8String で、カンマ区切りの `host:port` を 1〜16 件指定する。IPv6 の例は `[fd00::1]:7443`。

```ini
1.3.6.1.4.1.54392.3.36202.53280.64414.21169.48647.23055.51379.6491=ASN1:UTF8String:seed-a.example.internal:7443,seed-b.example.internal:7443
```

管理者の署名環境で既存の自己署名 CA を使って発行する。

```sh
openssl x509 -req -in node.csr -CA ca.pem -CAkey ca.key -CAcreateserial \
  -days 90 -sha256 -extfile node.ext -out node.pem
```

ノードに CA の公開証明書と発行済みのノード証明書を渡す。中間 CA がある場合、`cert_file` の PEM はノード証明書を先頭に中間証明書を続ける。CA の秘密鍵は署名環境で保管する。

設定と証明書は `serve` の起動時に読み込む。更新時は同じ UUID の証明書を配置し、期限前にノードを再起動する。有効期限に達したリンクは切断される。特定ノードの参加を取り消す場合は各ノードの `peer.deny_nodes` に ID を追加して再起動する。同じ `state_dir` で平文と mTLS を切り替えると、自ノードの ID は引き継ぎ、それまでの他ノードの台帳を破棄する。新しい方式のシードからピアを再発見する。

### 参加ノードの証明書にシードを含める

参加ノードでは `ca_file`、`cert_file`、`key_file` を設定し、`peer.seeds` を省略する。`config example -join -mtls` のひな型を使う場合は `seeds` 項目を削除する。初期接続先は次の優先順で決まる。

| 条件 | 実効シード | `peer.seed_source` |
| --- | --- | --- |
| 設定に `peer.seeds` がある | 指定した一覧。`[]` は初期シードへの発信を無効にする | `config` |
| 設定に一覧がなく、自ノードの検証済み証明書に拡張がある | 証明書内の一覧 | `certificate` |
| どちらもない | 空の一覧 | `none` |

接続先の名前とノード ID は従来どおり mTLS で検証する。シードの省略や空配列にかかわらず、既知のピアの再接続と台帳による発見は通常どおり動作する。

## 状態照会

`server_version` は、MCP クライアントが接続しているサーバーの名前とバージョンを返す。指定したピアのバージョンは `host_status` の `system.server_version` に含まれる。

読み取り専用の `server_config` に `{}` を渡すと、接続中のノードの `node_id`、MCP の方式、実際に割り当てられた待受先、ピア設定、シードの取得元、中継設定などが返る。`peer.security` は常駐モードで `plaintext` または `mtls`、stdio で `disabled`。mTLS の場合は `certificate` に発行者、有効期間、読み込み済みリーフ証明書の DER の SHA-256 指紋（16 進数）が入る。

```json
{"include_certificate": true}
```

この引数を `server_config` に渡すと `certificate.pem` に同じ公開リーフ証明書が入る。ファイル更新後、再起動で新しい証明書が読み込まれたかを指紋で確認できる。

`host_status` で `sections` を省略すると `system`、`cpu`、`memory`、`filesystems` を取得する。追加項目は `sections` で指定する。 要求した項目を一つも取得できなかった場合は、`COLLECTION_FAILED` の `section_errors` から失敗理由を確認できる。

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

プロセスの CPU 使用率は約 1 秒間隔の 2 サンプルから計算し、1 コアを使い切ると 100% になる。計測中に起動したプロセスなど、差分を取れない場合は `cpu_percent` が `null` になる。同じ照会で `cpu` と `processes` を指定すると、計測区間が重なる。Docker では採取元コンテナから見えるプロセスとインターフェースが返る。

## OS とカーネル

`host_status` に `{"sections":["system"]}` を渡す。別ホストを調べる場合は `target` を指定する。

| フィールド | 内容 |
| --- | --- |
| `node.os` | OS の種類。Linux では `linux`。 |
| `sections.system.os_id` / `os_name` | ディストリビューションの ID と表示名。 |
| `sections.system.os_version` / `os_version_id` | ディストリビューションの表示用バージョンとバージョン ID。取得できる値を返す。 |
| `sections.system.kernel_release` | 実行中のカーネルのリリース番号。`uname -r` と同じ値。 |

Linux コンテナではディストリビューションはコンテナ内の情報で、カーネルはコンテナが使用しているもの。ディストリビューション情報がない場合は、対応するフィールドを省略する。

## ホストの時計

`host_status` に `{"sections":["clock"]}` を渡す。別ホストの時計を読む場合は `target` を指定する。`utc_time` と `local_time` は同じ瞬間を UTC と現地時刻で表した値。`timezone_abbreviation` と `utc_offset_seconds` は、採取元の MCP サーバープロセスで有効なタイムゾーンに従う。

## ストレージ一覧

`host_status` に `{"sections":["storage"]}` を渡す。別ホストを調べる場合は `target` にそのノードを指定する。

- `mounts` はマウント先とマウント元、ファイルシステムの種類、読み取り専用かどうかを返す。ネットワーク共有や `tmpfs` も含み、対応するブロックデバイスを確認できたマウントには `block_device_id` が付く。
- `block_devices` はディスク、パーティション、device-mapper（LVM や暗号化デバイスなど）、ソフトウェア RAID、ループデバイスを返す。取得できた容量、読み取り専用状態、パーティションの親やボリュームの下位デバイスも含む。未マウントのデバイスも対象になる。

空き容量を調べるには、既存の `filesystems` で設定済みパスを照会する。Linux コンテナではマウント一覧はコンテナから見える範囲になり、ブロックデバイス一覧はそのコンテナに公開された sysfs に従う。Windows のドライブ文字は Windows 対応時に扱う。

## PID でプロセスを調べる

`process_status` に `{"pid":1234}` を渡すと、接続先 MCP サーバーのローカルプロセスの名前、CPU 使用率、常駐メモリ、取得できる場合は `process_identity` を返す。CPU 使用率順の上位 100 件に入らない PID も照会できる。

## ローカルプロセスの終了

`server_version`、`host_list`、`host_status`、`process_status` は読み取り専用として通知する。`process_kill` は破壊的な書き込みツールとして通知し、接続先 MCP サーバーの Linux プロセスにシグナルを送る。stdio では利用できる。ループバックの HTTP で有効にするには、設定のトップレベルに `"allow_process_kill": true` を追加する。

`process_status` またはローカルの `host_status` のプロセス一覧から対象の `pid` と `process_identity` をコピーする。次の例の値を対象のプロセスに合わせて置き換える。

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
