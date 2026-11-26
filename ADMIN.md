# エージェントから hostmesh-mcp を管理する

シェルを使うエージェントは `hostmesh-mcp help administration`、MCP クライアントを使うエージェントは `hostmesh-mcp mcp` を起動して `admin_help` から始める。どちらも同じ操作名・入力スキーマで初回登録や設定編集ができる。

## エージェントへの依頼例

**既存ピアの設定を検査したいとき**は、対象ホストと設定ファイルを指定する。管理 MCP を使えるエージェントには、たとえば次のように依頼できる。

> このホストの `/etc/hostmesh-mcp/peer.json` を検査してください。`admin_info` で対象パスを確認し、一致したら `config_check` に `{"kind":"peer"}` を渡してください。設定ファイルのパスと検査結果、失敗した場合はエラーを報告してください。今回は検査だけです。

シェルを使うエージェントには、同じホスト上で実行するコマンドを渡せる。

```sh
hostmesh-mcp config check -config /etc/hostmesh-mcp/peer.json
```

**新しいピアを試運転するとき**は、空の作業ディレクトリを用意し、管理 MCP の起動引数 `-peer-config` にその中の `peer.json` の絶対パスを渡す。接続後は同じ管理セッションで次のように進める。

1. `admin_info({})` の `peer_config` で保存先を確認し、`peer_config_example({})` でひな型を取得する。
2. ひな型の `config` に試験専用の絶対パスを `state_dir` として設定し、`mcp_listen` と `peer.listen` を `127.0.0.1:0` にする。固定の `peer.advertise` を外し、`config_save` に `kind: "peer"` と編集した `config` 全体を渡す。
3. 前回と同じ設定で再試行するときは、下の表の検査から再開する。内容を変える場合は `config_read({"kind":"peer"})` で現在の設定と `sha256` を取得し、編集した設定全体を `config` に、取得したハッシュを `expected_sha256` に指定して保存する。`configuration revision mismatch` が返った場合も再読込する。

設定済みのピアなら「この管理 MCP セッションで試運転し、ノード ID とメモリを確認してから停止してください」と依頼できる。

| 呼び出すツール（監視 HTTP 以外は管理 MCP） | 引数 | 確認する結果 |
| --- | --- | --- |
| `admin_info` → `config_check` | `{}` → `{"kind":"peer"}` | `peer_config` が対象の設定パスで、検査結果が `valid: true` |
| `peer_start` | `{}` | `details.node_id` と `details.mcp_url` |
| `service_status` | `{}` | `services.peer.status` が `running` |
| 監視 HTTP の `host_status` | `{"sections":["memory"]}` | 管理セッションを開いたまま別クライアントで `details.mcp_url` に接続し、応答の `node.node_id` と `sections.memory` を確認 |
| `service_stop` | `{"service":"peer"}` | `status` が `stopped` |
| `service_status` | `{}` | `services.peer.status` が `stopped` |

`details.mcp_url` は `/mcp` まで含む完全な URL。そのまま監視クライアントに渡す。シェルからの照会には初期化を含む実行例がある。[curl で監視用 MCP を照会する](OPERATIONS.md#curl-で監視用-mcp-を照会する)

検査に失敗したときは、そのエラーを報告して起動手順を終了する。試運転には専用の台帳と空いているポートを用意する。運転中の Linux サービスを管理する依頼では、対象の systemd ユニット名を指定する。[既存ユニットの操作とセッションの寿命](#起動停止と更新)

## 接続する

管理用 STDIO サーバーは起動した OS アカウントの権限でローカルファイル、サービス、プロセスを操作する。管理を任せるクライアントにこの起動設定を登録する。設定例の絶対パスは配置先に合わせる。

```json
{
  "mcpServers": {
    "hostmesh-admin": {
      "command": "/usr/local/bin/hostmesh-mcp",
      "args": ["mcp", "-peer-config", "/etc/hostmesh-mcp/peer.json",
               "-ca-config", "/etc/hostmesh-mcp/mesh-ca/online/ca.json",
               "-update-config", "/etc/hostmesh-mcp/updater.json"]
    }
  }
}
```

Windows は `command` に `hostmesh-mcp.exe` のパスを指定する。引数を省くと、作業ディレクトリの `peer.json`、`mesh-ca/online/ca.json`、`updater.json` を使う。ファイルがない状態でも起動できる。`admin_info` は解決済みのパスと OS を返す。設定を変更できるエージェントは、更新時に実行するバイナリや書き込み先も指定できるため、信頼する管理者に付与する。

監視用の `hostmesh-mcp stdio` と `serve` の HTTP エンドポイントは従来どおり使える。管理ツールを登録するのは `mcp`。別ホストの管理には、そのホストで管理用プロセスを起動する。

## 必要な操作を探す

| 業務 | 管理ツール | CLI での操作 |
| --- | --- | --- |
| 入力の確認 | `tools/list`、`admin_help` | `admin list`、`admin describe <tool>`、`help administration` |
| ひな型、設定、検査 | `peer_config_example`、`ca_config_example`、`update_config_example`（Linux）、`config_read`、`config_save`、`config_check` | `config example/check`、`ca config example/check`、または同名ツールを `admin call` |
| CA の新設、既存 CA の台帳初期化 | `ca_bootstrap`、`ca_init` | `ca bootstrap`、`ca init` |
| ピアの鍵作成、登録、証明書の導入、失効 | `peer_csr`、`ca_inspect_csr`、`ca_enroll`、`peer_install_certificate`、`ca_revoke` | `help enrollment`、または `admin call <tool>` |
| セッション中のピア・CA の運転 | `peer_start`、`ca_start`、`service_status`、`service_stop` | `serve -config ...`、`ca serve -config ...` をフォアグラウンドで起動し OS から停止 |
| バイナリ更新・監督（Linux） | `update_check`、`update_apply`、`supervisor_start`、`supervisor_update` | `update check/apply`、`supervise`、監督プロセスへの SIGUSR1 |
| 既存 systemd サービスの運転（Linux） | `unit_status`、`unit_start`、`unit_stop`、`unit_restart` | 起動引数でユニットを指定して `admin call unit_...` |
| ローカルプロセス | `process_status`、Linux の `process_kill` | `admin call process_status` で対象を特定し、必要時に `process_kill` にその識別子を渡す |

シェルでの短い呼び出し例。引数の JSON が長いときは `-` で標準入力から読み込む。CLI オプションは `admin` の直後に置く。

```sh
hostmesh-mcp admin list
hostmesh-mcp admin describe ca_enroll
hostmesh-mcp admin -peer-config /etc/hostmesh-mcp/peer.json call config_check '{"kind":"peer"}'
```

## 登録と設定の受け渡し

1. **参加ピア**で `peer_csr` に新規ディレクトリを指定する。管理者に渡すのは `csr_pem`。所有者が `csr_sha256` を別経路で伝え、秘密鍵は参加ピアに保管する。
2. **CA ホスト**で `ca_inspect_csr` に CSR と `lookup_registry: true` を渡す。未登録なら `node_id` は未割り当て。見送る場合はここで終了する。
3. 承認する場合は `ca_enroll` に同じ CSR、所有者確認済みの `expected_csr_sha256`、管理者が承認した `profile` を渡す。`profile.node_id` は小文字の UUID v4。`dns_names`・`ip_addresses`・`seeds`・`client_only` は任意。公開証明書チェーンと信頼ルートを参加ピアに返す。
4. **参加ピア**で `peer_install_certificate` に元の鍵のパス、公開証明書用の新規ディレクトリ、受け取った2つの PEM を渡す。鍵との対応・チェーンを検証して `ca_file`・`cert_file`・`key_file` の絶対パスを返す。これを `peer_config_example` の `config.peer` に設定する。
5. `config_save` に `kind: "peer"` と編集した設定オブジェクトを渡し、`config_check` する。既存設定の変更には `config_read` の `sha256` を `expected_sha256` に指定する。競合したら再読込して、現在の設定に意図した変更を適用する。

証明書から初期接続先を取得する場合、CA 側では `ca_enroll` の `profile.seeds` に待受ピアのアドレスを設定する。参加側では `peer_config_example({"join":true,"mtls":true})` の結果から `config.peer.seeds` **項目そのものを削除**して保存する。ひな型の値を残すと設定が優先され、空配列を指定するとシードへの発信が無効になる。`peer_start` の後、`details.mcp_url` の監視用 `server_config` で `peer.seed_source: "certificate"` を確認する。[相手のノード ID を指定した状態照会](README.md#ピア間の-mtls)

CLI では公開 PEM やハッシュを手で転記せず、JSON のフィールドを渡せる。以下は `jq` を使う例。CA 側で承認済み入力を `approved.json` として用意する。参加ピアへは公開結果の `issued.json` を渡す。

```sh
# CA ホスト
hostmesh-mcp admin call ca_enroll - < approved.json > issued.json
# 参加ピア
jq --arg key "/path/to/peer/node.key" --arg dir "/path/to/new-public-dir" \
  '{certificate_pem, ca_pem, key_file:$key, directory:$dir}' issued.json |
  hostmesh-mcp admin call peer_install_certificate -
```

設定編集でも、取得した版のハッシュと編集した内容を同じ JSON から組み立てると、長いハッシュの転記を省ける。

```sh
hostmesh-mcp admin call config_read '{"kind":"peer"}' |
  jq '{kind:"peer", expected_sha256:.sha256, config:(.config | .mcp_listen="127.0.0.1:8788")}' |
  hostmesh-mcp admin call config_save -
```

## 起動・停止と更新

`mcp_listen` はトップレベルの監視 HTTP 設定で、ループバックに対して待ち受ける。`peer.listen` がピア間通信の設定であり、mTLS はこの通信に適用する。テストで空きポートを使うには `mcp_listen: "127.0.0.1:0"` と `peer.listen: "127.0.0.1:0"` を指定し、`peer.advertise` の固定値を外す。`state_dir` をピアごとに分ける。`peer_start` の `details.mcp_url` と `details.peer_listen` に実際の値が入る。ピア設定内の相対パスはプロセスの作業ディレクトリ、CA 設定内の相対パスは CA 設定ファイルのディレクトリが基準。

`peer_start`・`ca_start`・`supervisor_start` の寿命は管理 MCP セッションに紐づく。`service_status` で確認でき、切断時には終了する。切断後も運転する Linux の既存 systemd ユニットには `-peer-unit <名前.service>` または `-ca-unit <名前.service>` を起動時に指定する。ユーザーサービスなら `-user-units` も指定する。管理者が指定したユニットに対し、ツールから `service: "peer"` または `"ca"` を渡して操作する。

```sh
hostmesh-mcp admin -user-units -peer-unit hostmesh-mcp.service call unit_status '{"service":"peer"}'
```

systemd のない機器で再起動後も運転する場合は、機器の init から `supervise` を起動する。既に動作中の別プロセスと同じ台帳を使うとロックが競合する。[Linux の監督と更新手順](UPDATING.md)
