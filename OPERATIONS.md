# 管理者が CLI で hostmesh-mcp を運用する

操作したいホストにログインし、ピアまたは CA を運転する OS アカウントでコマンドを実行します。例は `hostmesh-mcp` が PATH にあるシェル用です。Windows では同じ引数を `hostmesh-mcp.exe` に渡せます。PowerShell での JSON ファイルの作成方法はインストールガイドにあります。[OS 別の導入手順](INSTALL.md)

## ピアを起動して監視する

以下の例では `peer.json` を置くディレクトリで、設定の作成から `config check`、`serve` まで実行します。Linux・macOS での一時的な試運転なら、まず次の 2 行で作業場所を用意してください。ソースからビルドした場合は、移動先でもコマンドを呼べるように、実行ファイルの絶対パスを使うか PATH に追加します。

```sh
trial_dir=$(mktemp -d)
cd "$trial_dir" || exit 1
```

1. 初回は新しい作業ディレクトリで、待ち受けるピアの設定を生成します。既存ピアの設定を変更するときは、使用中の `peer.json` を編集して手順 2 へ進みます。

   ```sh
   hostmesh-mcp config example > peer.json
   ```

   同じホストに別のピアがいる場合は、新しい作業ディレクトリ内の `peer.json` を次の内容にして試せます。両方の `:0` は OS が起動時に空きポートを割り当てる指定です。このローカル試験では固定の `peer.advertise` は省きます。`./state` は `serve` を実行する作業ディレクトリが基準です。

   ```json
   {
     "state_dir": "./state",
     "mcp_listen": "127.0.0.1:0",
     "peer": {"listen": "127.0.0.1:0"}
   }
   ```

2. ローカルで試すときはひな型のまま検査して起動します。検査の成功は `Configuration OK`、起動の成功はログの `node_id` と `MCP listening on` で確認できます。

   ```sh
   hostmesh-mcp config check -config peer.json
   hostmesh-mcp serve -config peer.json
   ```

3. 監視用 MCP クライアントをログの HTTP URL（ひな型では `http://127.0.0.1:8787/mcp`）に接続します。`server_config` に `{}` を渡し、`node_id`、`state_dir`、`peer.security` を確認します。`host_list` に `{}` を渡すと既知のノード、`host_status` に `{"sections":["memory"]}` を渡すと自ホストのメモリを取得できます。[別ホストを指定する例](README.md#2-台を接続する)
4. 手動で起動したピアは端末で Ctrl+C を押すと停止します。端末にプロンプトが戻り、別の端末から同じ監視 URL へ接続すると接続拒否になることを確認します。設定変更は `config check` で検査し、同じ設定と状態ディレクトリで再起動して適用します。

`config check` が成功しても、起動時に `address already in use` が出ることがあります。エラーが `MCP listener` なら監視用の `mcp_listen`、ピアの待受なら `peer.listen` のポートを確認してください。同じホストでの試運転には空いているポートと専用の `state_dir` を使います。[複数ピアの設定例](README.md#同じホストで独立したピアを並行起動する)

別ホストをメッシュに加えるときは、待受側に参加側から到達できる `peer.listen` と `peer.advertise` を設定します。参加側は `config example -join` でひな型を作り、`peer.seeds` に待受側を指定します。監視クライアントは各ピアのループバック HTTP に接続します。証明書を使う場合は両側のひな型に `-mtls` を追加し、次の登録手順で取得したファイルを指定します。[mTLS 設定例](README.md#ピア間の-mtls)

### curl で監視用 MCP を照会する

監視用クライアントを登録する前でも、別の端末から次の例でメモリを取得できます（Linux・macOS のシェル用）。1 行目の URL を起動ログの `MCP listening on`、または管理 MCP の `peer_start` が返した `details.mcp_url` の値に置き換えます。値にはすでに `/mcp` が含まれるので、そのまま使います。

```sh
MCP_URL='http://127.0.0.1:8787/mcp'
mcp_post() {
  curl -fsS --max-time 10 "$MCP_URL" \
    -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' \
    -H 'MCP-Protocol-Version: 2025-06-18' --data-binary "$1"
}
mcp_post '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"cli-check","version":"1"}}}'
mcp_post '{"jsonrpc":"2.0","method":"notifications/initialized"}'
mcp_post '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"host_status","arguments":{"sections":["memory"]}}}'
```

最後の応答では `result.structuredContent.node.node_id` を起動時のノード ID と照合し、`result.structuredContent.sections.memory.total_bytes` で対象ホストの総メモリを確認できます。

## CA の新設・登録・失効

同じホストで mTLS ピアを試運転する場合、待受側 A の登録プロファイルに `"ip_addresses":["127.0.0.1"]` を入れます。上の `:0` の設定例の `peer` に A の `ca_file`、`cert_file`、`key_file` を追加し、固定の `advertise` は省いて起動します。参加側 B の証明書に記載するシードは、A の起動ログの `peer listening on` に表示された実際の `127.0.0.1:ポート番号` です。運用時に証明書へシードを記録する場合は、再起動後も B から到達できる固定のピア用アドレスを使います。[両方向から mTLS 接続を確認する手順](README.md#ピア間の-mtls)

| 操作するホスト | 作業 |
| --- | --- |
| CA ホスト | 初回は `ca bootstrap` で鍵・証明書・台帳を作り、`ca config check` で検査します。証明書更新を受け付けるには `ca serve` を起動します。同じホストで監視 HTTP も提供する場合は、通常ピアの `serve` を別プロセスで起動します。[CA の新設と起動](CA.md#新しい-ca-を一括生成する) |
| 参加ピア → CA ホスト | 参加ピアで `peer csr` を実行し、CSR を管理者に渡します。秘密鍵は参加ピアに置きます。CA ホストで `ca inspect` を実行し、所有者から別経路で受け取った指紋と照合します。[登録コマンドと照合する値](CA.md#ノードを登録する) |
| CA ホスト → 参加ピア | 承認する場合は `ca enroll` で発行し、証明書チェーンと公開ルート証明書を参加ピアに返します。見送る場合は検査で終了します。参加ピアで `cert_file`、`ca_file`、`key_file` を設定してからピアを起動します。[登録プロファイルと発行例](CA.md#ノードを登録する) |
| CA ホスト | 鍵が漏洩したときは `ca revoke` でその鍵による再発行を止めます。接続中のピアも遮断する手順と、新しい鍵での登録方法は運用ガイドにあります。[失効と再登録](CA.md#失効と再登録) |

## 常駐運転と日常の確認

Linux でログアウト後も運転する場合は、ホストの起動方式に合う手順を使います。

- **systemd を使うホスト**では、ピアをユニットに登録します。ガイドのユーザーユニットなら `systemctl --user status hostmesh-mcp.service` で稼働を確認できます。更新候補の確認は `hostmesh-mcp update check -config updater.json`、適用は `update apply` です。[ユニットの準備と更新](UPDATING.md#systemd-サービスの初回準備)
- **systemd のない機器**では、機器の init から `supervise -config` で監督プロセスを起動します。監督用と子ピア用の実行ファイルを配置し、設定した間隔で更新を確認します。[小型機での配置・起動・即時更新](UPDATING.md#systemd-を使わない機器での起動)

操作名を調べるには `hostmesh-mcp help administration`、登録手順は `hostmesh-mcp help enrollment` を使います。管理ツールをシェルから呼ぶときは、たとえば `hostmesh-mcp admin describe config_check` で入力形式を確認できます。[管理操作と CLI の対応表](ADMIN.md#必要な操作を探す)
