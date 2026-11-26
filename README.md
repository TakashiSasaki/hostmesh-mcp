# hostmesh-mcp

Linux・macOS・Windows x64 対応。MCP からローカルまたは別ホストの OS、稼働時間、CPU、メモリ、ファイルシステム容量を取得する。ノードは台帳を交換し、到達できるペアを自動で直結する。共通の中継を許可すると、直結できないペアも照会できる。

macOS 版は Apple Silicon と Intel に対応する。`stdio` と `serve` の両方でプロセス情報を読み取れる。`process_kill` は登録せず、`server_config.allow_process_kill` は常に `false` を返す。ディスク一覧には APFS コンテナとボリュームを含む。コンテナ内の空き容量は共有される。バイナリの自動更新と `supervise` は Linux 用。mTLS の証明書更新は両 OS で利用できる。

Windows 版は Windows ホスト上で動く読み取り専用の `hostmesh-mcp.exe`。プロセスの CPU 使用率とワーキングセット、物理メモリ、実際のページファイル別使用量（`swap.page_files`）、システムドライブなどを取得する。

[インストールガイド（Linux・macOS・Windows）](INSTALL.md)

Linux ARM64 での常駐メモリの目安は、v0.0.35 の実測で mTLS ピア 1 プロセスが約 22 MiB、CA とピアを同じホストで別々に起動すると合計約 30 MiB。Linux の自動更新用に `supervise` でピアを起動すると親子合計で約 46 MiB。いずれも共有ページを按分した値（PSS）。[構成別の実測値と測定条件](TESTING.md#linux-arm64-の常駐メモリ)

## エージェントから管理する

管理用 STDIO MCP は `hostmesh-mcp mcp` で起動し、最初に `admin_help` を呼ぶ。CLI では `hostmesh-mcp help administration` と `hostmesh-mcp admin list` から同じ業務を探せる。ピアと CA の初回準備、証明書、設定編集、サービスの運転、Linux の更新に対応する。[管理クライアントの設定・操作例](ADMIN.md)

## CLI で使い方を調べる

`hostmesh-mcp --help` でコマンド一覧、`hostmesh-mcp help ca` で CA の操作一覧を表示する。初回登録を担当するエージェントは **`hostmesh-mcp help enrollment`** から始めると、バイナリ単体で CSR の確認、登録プロファイルの例、承認・見送り、証明書の返却までの手順を確認できる。[CA の運用手順](CA.md#ノードを登録する)

## ビルドと単体起動

```sh
go build -o hostmesh-mcp ./cmd/hostmesh-mcp
./hostmesh-mcp version
./hostmesh-mcp stdio
```

MCP クライアントのコマンド設定では、実行ファイルへの絶対パスと引数 `stdio` を指定する。既定の状態ディレクトリは `os.UserConfigDir()` 配下の `hostmesh-mcp/state`。同じマシンで複数ノードを動かすときは、ノードごとに `-state-dir` または設定の `state_dir` を指定する。

## ピア設定のひな型を出力する

`config example` は編集用の JSON を標準出力に表示する。

```sh
./hostmesh-mcp config example                     # 待ち受けるピア
./hostmesh-mcp config example -join               # 既存のピアに発信するノード
./hostmesh-mcp config example -mtls               # mTLS で待ち受けるピア
./hostmesh-mcp config example -join -mtls > b.json
```

例のアドレスは `127.0.0.1:7443`。別ホスト間で使うときは、待ち受け側の `listen` に自分の閉域アドレス、`advertise` と参加側の `seeds` に参加側から到達できる待ち受け先を指定する。`-mtls` は CA 証明書・ノード証明書・秘密鍵のパスを追加する。各ノードのファイルに合わせて編集し、`./hostmesh-mcp serve -config b.json` で起動する。各項目の意味は `./hostmesh-mcp config example -help` でも確認できる。

同じホスト上で両方のひな型を使う場合は、次の例のようにノードごとに異なる `state_dir` を指定し、参加側で `"mcp_listen": ""` または別のループバックポートを設定する。

## 設定ファイルを検査する

```sh
./hostmesh-mcp config check -config peer.json
```

`serve` と共通の規則で JSON、設定項目、待受先の形式、ピア設定の整合性を検査する。mTLS の場合は、指定ファイルを読み、CA によるノード証明書の検証と秘密鍵との照合も行う。成功時は標準出力に `Configuration OK` とピアの通信方式、実効シードの件数と取得元を表示して終了コード 0 を返す。設定エラー時は標準エラー出力に理由を表示して 1 を返す。

稼働中のノードと同じ設定にも使える。状態ディレクトリの作成・ロック・更新、待ち受け、ピアへの接続は行わない。ポートの空き状況、永続化済みノード ID との一致、ピアへの到達性は実際の起動・接続時に確認される。

## 2 台を接続する

A の設定 `a.json` の例。

```json
{
  "mcp_listen": "127.0.0.1:8787",
  "state_dir": "/tmp/hostmesh-mcp-demo-a",
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
  "state_dir": "/tmp/hostmesh-mcp-demo-b",
  "peer": {
    "seeds": ["127.0.0.1:7443"]
  }
}
```

別々の端末で `./hostmesh-mcp serve -config a.json` と `./hostmesh-mcp serve -config b.json` を実行する。上の設定は同じ Linux ホストで試すためのもの。別ホストで使う場合は `peer.listen` のアドレス、`peer.advertise`、`peer.seeds` に閉域で到達できるアドレスを使う。平文モードでは `node_id` と台帳を参加ピアの申告として扱う。

MCP クライアントから A の `http://127.0.0.1:8787/mcp` に接続する。`host_list` で B の ID を確認し、`host_status` に `{"target":{"node_id":"B の ID"},"sections":["memory"]}` を渡す。接続は B が始めるが、A がその双方向リンクで B に採取を依頼できる。`target` を省略するとローカルを採取する。 `host_list` の `is_local: true` は、MCP クライアントが接続しているノードを示す。同じホスト名を持つピアは `node_id` で指定できる。

A と B が C を共通中継にする場合は、C で `peer.relay: true` と `peer.listen` を設定する。A と B のシードに C のアドレスを設定し、両方の `peer.trusted_relays` に C の起動ログに出る `node_id` を追加する。`peer.deny_nodes` に設定した ID の接続と照会を拒否する。設定変更はプロセスの起動時に適用する。

## 同じ接続先でピアを更新する

Linux 版 0.0.30 以降は、更新設定の `"source": "github"` で公式 GitHub Releases の Latest を追跡する。各ピアは GitHub のリリース証明とバイナリの SHA-256 を検証する。Linux で `hostmesh-mcp update check -config updater.json` により署名付き公開版を確認し、`update apply` で適用できる。systemd を使わない機器では、監督用コピーを `hostmesh-mcp-supervisor supervise -config updater.json` で起動すると、ピアの再起動と署名付き更新を自動化できる。systemd での定期適用には同梱のユーザータイマーを使える。[更新設定・署名・復旧手順](UPDATING.md)

常駐ピアには固定の MCP ポートとピアポートを設定する。更新時には短い切断を許容し、MCP クライアントは登録済みの URL に再接続する。

1. 旧版の稼働中に新版バイナリを別ファイルに配置し、新版の `config check -config peer.json` で運用中の設定を検査する。
2. 旧版を正常終了させ、終了を待ってから、新版を同じ設定で `serve -config peer.json` として起動する。同じ `state_dir` と、mTLS では同じノード証明書・鍵を使い、`node_id` を引き継ぐ。
3. 従来の MCP URL に再接続し、`server_version` と `server_config` で新版のバージョンと従来のノード ID を確認する。メッシュ参加時は、別のピアから従来の ID で `host_status` を呼び、`sections.system.server_version` を照合できる。

## 同じホストで独立したピアを並行起動する

新旧の挙動を比較する場合などは、別々のノード ID と待受先を使って同時に稼働できる。以下は 2 組の設定例。ピア用の IP は、そのホストが閉域網で使用するアドレスに置き換える。

| 設定 | 旧プロセスの例 | 新プロセスの例 |
| --- | --- | --- |
| `state_dir` | `/var/lib/hostmesh-mcp/blue` | `/var/lib/hostmesh-mcp/green` |
| `mcp_listen` | `127.0.0.1:8787` | `127.0.0.1:8788` |
| `peer.listen` と `peer.advertise` の接続先 | `10.0.0.5:7443` | `10.0.0.5:7444` |

各組に固有の `node_id` を使う。平文では空の状態ディレクトリで初回起動すると ID が発行される。mTLS では共通の CA からそれぞれ別の UUID の証明書を発行し、組ごとに証明書と秘密鍵を設定する。次回以降は各組の状態ディレクトリと ID を再利用できる。

1. 新版を別の実行ファイルに配置する。新版の `config check -config green.json` を通し、旧版の稼働中に新版を `serve -config green.json` で起動する。新版のシードには更新中も稼働する別のピアを指定できる。
2. 新版の MCP 入口で `server_config` を呼び、`node_id` と待受先を確認する。別の稼働中ピアから `host_status` を `{"target":{"node_id":"新版の UUID"},"sections":["system"]}` で呼び、応答中の `node_id` と `sections.system.server_version` が新版に一致することを確認する。同一ホストで併存中はホスト名と IP が一致するので、照会先に UUID を使う。
3. 旧版の MCP 入口を使っているクライアントは新版の入口へ再接続する。旧版を固定シードまたは中継にしているピアでは、必要な接続先や `trusted_relays` を新版に切り替えて経路を確認する。
4. 確認が通ったら旧版のサービスだけを停止する。手動起動なら旧プロセスの PID に `SIGTERM` を送り、終了を待つ。別の稼働中ピアから新版の UUID を再度照会する。新版の起動や照会に失敗した場合は旧版を稼働させたまま、設定とログを確認する。

## ピア間の mTLS

ピアの通信方式は `serve` の起動時に決まる。`peer.ca_file`、`peer.cert_file`、`peer.key_file` をすべて省略すると平文、3 点をすべて指定すると mTLS で起動する。一部だけ指定した場合や、証明書・鍵の読み込みや検証に失敗した場合は起動エラーになる。既存の `peer.security` 設定は省略できる。指定する場合は証明書の構成と一致する値（`plaintext` または `mtls`）を使う。

待ち受けと発信には同じ方式を使う。異なる方式のシードを指定してもハンドシェイクで終了し、その相手とは台帳の交換も状態照会の中継も行わない。mTLS では TLS 1.3 で相互認証し、ノード ID と台帳レコードの署名を検証する。MCP クライアントは stdio またはループバック HTTP で接続する。

次は A のピア設定例。B には B 固有の証明書と鍵を配置し、`seeds` に `["a.example.internal:7443"]` を指定する。既存ノードは `state_dir` を引き継ぎ、`host_list` の `is_local: true` に表示された UUID を証明書に使う。新規ノードは UUID v4 を一つ発行し、その値を証明書と初回の永続 ID に使う。

```json
{
  "peer": {
    "listen": ":7443",
    "advertise": ["a.example.internal:7443"],
    "ca_file": "/etc/hostmesh-mcp/ca.pem",
    "cert_file": "/etc/hostmesh-mcp/node.pem",
    "key_file": "/etc/hostmesh-mcp/node.key"
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
subjectAltName=URI:urn:hostmesh-mcp:node:7043f65e-b1f6-489d-a0d2-5d402035735d,DNS:a.example.internal,IP:192.0.2.10
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

設定と証明書は `serve` の起動時に読み込む。手動更新時は同じ UUID の証明書を配置し、期限前にノードを再起動する。有効期限に達したリンクは切断される。特定ノードの参加を取り消す場合は各ノードの `peer.deny_nodes` に ID を追加して再起動する。同じ `state_dir` で平文と mTLS を切り替えると、自ノードの ID は引き継ぎ、それまでの他ノードの台帳を破棄する。新しい方式のシードからピアを再発見する。

### 既存のピア接続による証明書の自動更新

mTLS ピアに初回の証明書を登録した後、次の設定を各ノードの `peer` オブジェクトに追加する。A → B → C → CA の例では、B と C が更新経路を広告し、A が既存のピア接続から経路を発見する。A から CA への名前解決や TCP 接続は不要。機器上では Go バイナリ自身が処理する。

| ノード | `peer.renewal` の設定例 |
| --- | --- |
| A（更新元） | `{"url":"https://ca.example.internal/.well-known/est/simplereenroll"}` |
| B（中継） | `{"relay":true}` |
| C（CA に到達できる出口） | `{"gateway":"ca.example.internal:443"}` |

B や C 自身の証明書も更新する場合は同じオブジェクトに `url` を追加する。状態照会の `peer.relay` と証明書更新の中継は個別に設定する。経路上の各ピアには 0.0.19 以降が必要。更新設定を省略したピアは従来どおり動く。

新規の発行局は `hostmesh-mcp ca bootstrap -dir mesh-ca -server-name ca.example.internal` でルート CA・中間 CA・更新サーバー証明書・初期台帳を一括生成し、`hostmesh-mcp ca serve -config mesh-ca/online/ca.json` で起動できる。参加するピアでは `hostmesh-mcp peer csr -dir mesh-peer` で初回登録用の秘密鍵と CSR を生成し、`mesh-peer/node.csr` を管理者に渡す。発行用ホストでは別プロセスの通常ピアがメッシュからの更新要求を中継する。ひな型は `hostmesh-mcp ca config example` で取得できる。[中間 CA の準備・初回登録・失効手順](CA.md)

CA 側には、既存のクライアント証明書を mTLS で認証し、失効済みの資格での再発行を拒否する EST の `simplereenroll` 窓口を用意する。CA は CSR の署名、鍵の一致、ノードの参加許可を確認し、そのノードの URI SAN、必要な名前・用途、証明書内のシードを含む新しい証明書を返す。EST サーバー自体には URL のホスト名に有効な専用のサーバー証明書を発行し、更新元の `ca_file` から検証できるようにする。CA 署名鍵は発行側にだけ置く。[EST の再登録](https://www.rfc-editor.org/rfc/rfc7030#section-4.2)・[EST の転送形式](https://www.rfc-editor.org/rfc/rfc8951#section-3)

更新元は実効有効期間の約 3 分の 2 が過ぎると、現在の鍵で署名した CSR を送る。ネットワーク障害や CA の一時的な拒否時は再試行し、CA の `Retry-After` を尊重する。`cert_file` は公開証明書チェーン専用とし、秘密鍵と信頼する CA は別ファイルに置く。サービスアカウントに証明書配置ディレクトリの書き込みを許可する。新しい証明書の検証後、証明書ファイルをアトミックに置換して同じプロセス内で MCP とピア接続を起動し直す。`server_config` の証明書指紋と `not_after` で切り替えを確認できる。

鍵漏洩時は、その鍵で発行済みの証明書を CA で失効させ、同じ公開鍵での再発行を停止する。鍵交換と再登録は管理者が行う。この実装で CA の失効判断が効くのは再発行時。発行済みの証明書によるピア通信は、その有効期限または各ピアの `deny_nodes` の適用まで可能。期限切れでメッシュに接続できなくなった機器も手動で再登録する。

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

空き容量を調べるには、既存の `filesystems` で設定済みパスを照会する。Linux コンテナではマウント一覧はコンテナから見える範囲になり、ブロックデバイス一覧はそのコンテナに公開された sysfs に従う。Windows ではボリュームとそのドライブ文字・ディレクトリマウントを返す。

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
HOSTMESH_MCP_DOCKER_TEST=1 go test -v ./cmd/hostmesh-mcp -run '^TestDockerRelay$' -count=1
HOSTMESH_MCP_SCALE_TEST=1 go test -v ./internal/peer -run '^TestMesh100$' -count=1
```

通常のテストはループバック TCP と一時ディレクトリを使う。Docker 試験はローカルの Docker Engine を使い、A と B の TCP を 2 つの独立したブリッジで分離して C 経由の照会と再起動後の復旧を確認する。100 台の試験はメモリ内 gRPC で、フルメッシュと単一中継の両方を確認する。実バイナリでプロセス試験を繰り返すには、絶対パスを `HOSTMESH_MCP_TEST_BINARY` に設定して `go test -count=1 ./cmd/hostmesh-mcp` を実行する。

ピアの型を変更した場合は、`protoc` 36.2、`protoc-gen-go` v1.36.12、`protoc-gen-go-grpc` v1.6.0 を使う。

```sh
protoc -I . --go_out=paths=source_relative:. --go-grpc_out=paths=source_relative:. proto/*/peer/v1/peer.proto
```

[設計](DESIGN.md) と [テスト設計](TESTING.md) に Linux の ARM64・x86_64 配布、curl による導入、後続の OS 対応と任意の mTLS の条件を記載している。
