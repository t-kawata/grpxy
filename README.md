# Go Reverse Proxy

## 概要

このプロジェクトは、Go言語で実装された高性能・柔軟なリバースプロキシサーバーです。
複数ドメイン・ワイルドカードドメイン・バックエンドのラウンドロビン分散、最大同時リクエスト数・キュー制御、
SSL（TLS）対応、設定ファイルのホットリロードなど、実用的な機能を備えています。

---

## 主な機能

- **複数ドメイン・サブドメイン対応**
  ワイルドカード（例: `*.example.com`）による柔軟な振り分け
- **ラウンドロビンによるバックエンド分散**
  バックエンド複数指定時に自動で分散
- **最大同時リクエスト数・キュー制御**
  アプリ毎にリクエスト数・キューサイズを設定可能
- **SSL（TLS）対応**
  証明書（Let’s Encrypt等で取得したもの）を指定してHTTPSで待受可能
- **設定ファイル（TOML形式）による柔軟な構成**
  サーバー再起動不要のホットリロード
- **X-Forwarded-Hostヘッダ自動付与**
  バックエンドに元のホスト情報を伝達

---

## 使い方

### 1. 依存ライブラリのインストール

```
go get github.com/BurntSushi/toml
go get github.com/fsnotify/fsnotify
go get github.com/gobwas/glob
```

### 2. 証明書の準備（HTTPS利用時）

Let’s Encrypt等で取得した証明書（`cert.pem`/`key.pem`など）を用意してください。
自己署名証明書の場合は下記のコマンド例を参考にしてください。

```
openssl req -x509 -newkey rsa:2048 -keyout key.pem -out cert.pem -days 365 -nodes -subj "/C=JP/ST=Tokyo/L=Tokyo/O=Example/CN=example.com"
```

### 3. 設定ファイル（config.toml）を作成

#### 基本例

```
[global]
max_queue_size = 1000
listen_port = ":8443"
tls_cert_path = "cert.pem"
tls_key_path = "key.pem"
cdn_port = ":4112"
cdn_root = "/usr/local/pmao/ts/web"

[apps.api]
server_name = "*.example.com"
backends = [
    "http://backend1:8080",
    "http://backend2:8080"
]
max_requests = 100
queue_size = 50
load_balance = "round_robin"

[apps.admin]
server_name = "admin.example.com"
backends = ["http://admin-backend:9000"]
max_requests = 10
queue_size = 5
load_balance = "round_robin"
```

#### 各設定項目の説明

- **[global]**
  - `max_queue_size`: 全体の最大キューサイズ（現状は未使用、将来拡張用）
  - `listen_port`: プロキシが待ち受けるポート（例: `":8080"`、`":443"` など）
  - `tls_cert_path`, `tls_key_path`: SSL証明書・秘密鍵のパス。両方指定でHTTPS有効。
  - `cdn_port`: CDNが待ち受けるポート（例: `":4112"`、`":4444"` など）
  - `cdn_root` = CDNのルートディレクトリ（ `/usr/local/pmao/ts/web` など）

- **[apps.]**
  - `server_name`: 対象ドメイン（ワイルドカード可）
  - `backends`: 振り分け先バックエンドURLリスト
  - `max_requests`: 同時処理できる最大リクエスト数
  - `queue_size`: キューに入れられるリクエスト数（超過時は503返却）
  - `load_balance`: 現状は `"round_robin"` のみ対応

---

## 実現できるパターン例

- **1つのプロキシで複数ドメイン・サブドメインを同時ハンドリング**
- **ワイルドカードドメインで複数サービスを1つの設定で管理**
- **バックエンドを複数指定してラウンドロビン分散**
- **アプリごとに同時リクエスト数やキューサイズを個別設定**
- **SSL/TLSによるセキュアな通信（Let’s Encrypt等の証明書利用可）**
- **設定ファイルを書き換えるだけで即時反映（ホットリロード）**

---

## 起動方法

```
go run main.go -f /path/to/config.toml
```

---

## 動作確認例

```
curl -k https://api.example.com:8443/your/path
curl http://admin.example.com:8080/
```

---

## 注意事項・制限

- **HTTPS利用時は証明書・秘密鍵の両方指定が必須です**
- **Let's Encryptの自動証明書取得・更新には非対応です（証明書ファイルを手動で用意してください）**
- **現状、`load_balance` は `round_robin` のみ対応です**
- **キューサイズを超えるリクエストは503（Service Unavailable）で即時応答されます**
- **設定ファイルの構文エラーや証明書の不備があると起動に失敗します**
