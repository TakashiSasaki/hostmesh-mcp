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
