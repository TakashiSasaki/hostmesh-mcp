# 発行用ピアを運用する

管理者が指定した 1 台で、同じ `hostmesh-mcp` バイナリから通常の `serve` と発行用の `ca serve` を別プロセスで起動する。CA は既定で `127.0.0.1:9443` に待ち受け、通常ピアの設定・台帳・起動状態から独立して動く。発行用ホストのピア証明書が期限切れでも、管理者はローカル CLI で再登録できる。オーバーレイ内の発行局の一意性は管理者が保証する。

## 新しい CA を一括生成する

初めてメッシュを作る場合は、管理者の信頼できる環境で次を実行する。親ディレクトリは自分が管理する場所を選び、`-dir` には新しく作成するディレクトリを指定する。`-server-name` には各ピアの `peer.renewal.url` で使う DNS 名または IP アドレスを指定する。URL 全体やポート番号は含めない。

```sh
hostmesh-mcp ca bootstrap -dir mesh-ca -server-name ca.example.internal
hostmesh-mcp ca config check -config mesh-ca/online/ca.json
```

| 生成先 | 内容と用途 |
| --- | --- |
| `mesh-ca/offline/` | 自己署名ルート CA の証明書 `root.pem` と秘密鍵 `root.key`。管理者がオフラインで保管する。 |
| `mesh-ca/online/` | 公開ルート証明書、中間 CA の証明書・秘密鍵、EST サーバーの証明書チェーン・専用秘密鍵、`ca.json`、初期化済みの登録・失効台帳。発行用ホストにこのディレクトリを配置する。 |

ルート・中間・EST 用にそれぞれ新しい ECDSA P-256 鍵を生成する。有効期間はルートが10年、中間が1年、EST サーバーが180日。EST 証明書には指定した名前または IP が入り、中間 CA が署名する。証明書は X.509 PEM、秘密鍵は暗号化なしの PKCS#8 PEM。秘密鍵の読み取り権限を発行用アカウントに限定して転送・保管する。各ピアに信頼の起点として配るファイルは公開証明書 `root.pem`。

発行用ホストで、配置した `online` ディレクトリを指定して起動する。

```sh
hostmesh-mcp ca serve -config mesh-ca/online/ca.json
```

既定の待受は `127.0.0.1:9443`。出口ピアの `peer.renewal.gateway` をこのアドレスに設定する。新しいピアの証明書は下の「ノードを登録する」の手順で発行する。`ca bootstrap` は新設時専用で、指定先がすでに存在すると終了する。運用中の CA を再起動するときは保存した設定と台帳を使って `ca serve` を実行する。

## 既存のルート CA を使って手動で準備する

既存の自己署名ルート CA の公開証明書を `root.pem` とする。ルート CA は中間 CA の発行を許可すること。以下は初回用の例で、ホスト名や期間は運用に合わせる。

**発行用ホスト**の CA 専用アカウントで、新規ディレクトリに中間 CA と EST サーバー用の鍵を別々に生成する。

```sh
umask 077
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out issuer.key
openssl req -new -key issuer.key -subj '/CN=Host Status Issuing CA' -out issuer.csr
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out est.key
openssl req -new -key est.key -subj '/CN=ca.example.internal' -out est.csr
```

**オフラインのルート署名環境**にこの2件の CSR を渡す。例ではルートから中間 CA と EST サーバーの証明書を発行する。

```sh
printf '%s\n' 'basicConstraints=critical,CA:TRUE,pathlen:0' 'keyUsage=critical,keyCertSign,cRLSign' > issuer.ext
openssl x509 -req -in issuer.csr -CA root.pem -CAkey root.key -CAcreateserial \
  -days 365 -sha256 -extfile issuer.ext -out issuer.pem
printf '%s\n' 'basicConstraints=critical,CA:FALSE' 'keyUsage=critical,digitalSignature' \
  'extendedKeyUsage=serverAuth' 'subjectAltName=DNS:ca.example.internal' > est.ext
openssl x509 -req -in est.csr -CA root.pem -CAkey root.key -CAcreateserial \
  -days 180 -sha256 -extfile est.ext -out est-chain.pem
```

発行用ホストに `root.pem`、`issuer.pem`、`est-chain.pem` を渡す。ルート秘密鍵はオフラインの署名環境で保管する。中間 CA が複数段なら、`issuer_cert_file` と `server_cert_file` はそれぞれの証明書を先頭に必要な中間証明書を続けた PEM とする。EST の DNS 名は申請元の `peer.renewal.url` と一致させる。発行用ピア自身のノード鍵も別に用意する。

**発行用ホスト**で設定と台帳を準備する。相対パスは CA 設定ファイルのディレクトリを基準に解決する。既定のノード証明書の有効期間は 720 時間（30日）で、署名チェーンの期限が上限になる。

```sh
hostmesh-mcp ca config example > ca.json
hostmesh-mcp ca config check -config ca.json
hostmesh-mcp ca init -config ca.json
hostmesh-mcp ca serve -config ca.json
```

`ca config check` は証明書・鍵・既存台帳を検査する。`ca init` は新規の台帳を作る。中間 CA の鍵と台帳は CA アカウントだけが管理し、設定、鍵、台帳を一緒にバックアップする。特に失効後は最新の台帳を保管する。古い台帳を復元すると、その後に記録した失効も巻き戻る。

出口ピアには `"renewal": {"gateway":"127.0.0.1:9443"}` を、更新元には `"renewal": {"url":"https://ca.example.internal/.well-known/est/simplereenroll"}` をそれぞれの `peer` 設定内に追加する。中継ピアの設定と経路の例は [メッシュ経由の自動更新](README.md#既存のピア接続による証明書の自動更新) を参照。

## ノードを登録する

**新しく参加するピア上**で、信頼できる親ディレクトリから次を実行する。CA の証明書や接続情報がまだ手元になくても実行できる。

```sh
hostmesh-mcp peer csr -dir mesh-peer
```

`mesh-peer/node.key` がこのピア専用の ECDSA P-256 秘密鍵（PKCS#8 PEM）、`mesh-peer/node.csr` がその鍵で署名した登録用 CSR（PKCS#10 PEM）。秘密鍵はピアに保管し、**管理者に渡すのは `node.csr` だけ**。出力先には新しいディレクトリを指定する。同じパスで再実行すると既存の鍵と CSR は保護される。登録をやり直すときは保管済みの CSR を再送できる。既存ノードの鍵を引き継ぐ移行では、その鍵で作った CSR を使う。

**CA ホスト上**で、管理者が所有者・公開鍵を確認し、承認する内容を `node.json` に記入する。新規ノードには固有の UUID v4 を割り当て、既存ノードには現在の `node_id` を指定する。

```json
{
  "node_id": "7043f65e-b1f6-489d-a0d2-5d402035735d",
  "dns_names": ["sensor.example.internal"],
  "ip_addresses": ["192.0.2.10"],
  "seeds": ["peer-a.example.internal:7443", "peer-b.example.internal:7443"]
}
```

`node_id` は正規表記の UUID v4。名前、IP、シードは任意。証明書の用途は既定で clientAuth と serverAuth。発信専用ノードには `"client_only": true` を指定できる。CA は CSR で鍵の所持を検証し、発行内容には管理者が登録したプロファイルを使う。

```sh
hostmesh-mcp ca enroll -config mesh-ca/online/ca.json -profile node.json -csr node.csr > node-issued.pem
```

コマンドの成功後、ノードに `node-issued.pem` と `root.pem` を渡す。前者を `peer.cert_file`、後者を `peer.ca_file`、参加ピアに保管した `mesh-peer/node.key` を `peer.key_file` に設定する。その後の EST 更新は mTLS 接続のノード ID、登録済みの公開鍵、CSR の署名と公開鍵を毎回確認する。

## 失効と再登録

漏洩した鍵を、その鍵で発行した任意の世代の証明書から特定できる。CA の稼働中に実行でき、同じ鍵のすべての世代で以後の再発行が拒否される。

```sh
hostmesh-mcp ca revoke -config ca.json -cert compromised-node.pem
```

証明書が手元にない場合は `-node-id <UUID>` で、その時点の登録鍵を失効できる。ノード上で管理者が新しい鍵を生成し、新しい CSR と従来のプロファイルで `ca enroll` を実行する。以前の鍵の失効記録は再登録後も保持される。EST での自動更新は同じ鍵だけに許可する。

この失効台帳を参照するのは発行局。既存のピア接続が証明書の期限前に遮断されるようにするには、ピア側で `deny_nodes` を適用する。本機能の HTTP 窓口は mTLS 専用の `POST /.well-known/est/simplereenroll`。登録・失効・鍵交換は管理者のローカル操作で行う。
