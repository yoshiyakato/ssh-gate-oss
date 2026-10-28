# headless モードと Web 管理画面

ssh-gete はウィンドウを出さずに動かせます（`--headless`）。その場合、MCP サーバーと Web 管理画面だけが動き、承認・接続先の編集・ターミナル・設定はブラウザから操作します。Linux サーバー、コンテナ、macOS / Windows で常駐させるときに使います。

設計の詳細は [docs/spec/phase6-headless.md](spec/phase6-headless.md)（REQ-015）にあります。この文書は使い方だけをまとめたものです。

## 1. どのバイナリを使うか

| 成果物 | 中身 | GUI | `--headless` | 必要なもの |
|---|---|---|---|---|
| `ssh-gate-mac-universal-v1.01.zip` / `ssh-gate-win-*-v1.01.zip` | GUI 版（Wails） | あり | 使える | macOS / Windows のデスクトップ |
| `ssh-gate-linux-x64-headless-v1.01.tar.gz` | headless 専用（Linux amd64） | なし | 常に headless | なし（静的リンク。GTK / WebKit 不要） |
| `ssh-gate-linux-arm64-headless-v1.01.tar.gz` | headless 専用（Linux arm64） | なし | 常に headless | なし |
| `ssh-gate-mac-arm64-headless-v1.01.zip` / `ssh-gate-mac-x64-headless-v1.01.zip` | headless 専用（macOS Apple シリコン / Intel） | なし | 常に headless | なし（署名なし） |
| `ssh-gate-win-x64-headless-v1.01.zip` / `ssh-gate-win-arm64-headless-v1.01.zip` | headless 専用（Windows。コンソールアプリ） | なし | 常に headless | なし |

- Linux の GUI 版は GTK と WebKit を動的リンクするので、サーバーでは headless 専用ビルドを使ってください。
- headless 専用の tar.gz には `ssh-gete`、systemd の unit（`ssh-gete.service`）、設定ファイルの例（`headless.example.yaml`）、この文書が入っています。zip（macOS / Windows）には unit を除いた同じものが入っています。
- headless 専用ビルドはどれも cgo を使わない（`CGO_ENABLED=0`）単体の実行ファイルです。Wails を含まないので、WebView も要りません。
- macOS の headless 専用ビルドは署名していません。ダウンロードしたものは、展開した後に `xattr -d com.apple.quarantine ssh-gete` で隔離属性を外してから起動してください。
- どのアーカイブも、release に添付される `SHA256SUMS.txt` で照合できます（`shasum -a 256 -c SHA256SUMS.txt --ignore-missing`）。
- 自分でビルドする場合（`frontend/dist` が必要。GUI 版のビルドで作られます）:

  ```sh
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags headless -trimpath -o ssh-gete .
  ```

  `-tags headless` を付けると Wails を import しないので、C コンパイラも GTK / WebKit の開発用ライブラリも要りません。Linux 以外の OS からでも同じコマンドでクロスビルドできます（`GOOS` は `linux` / `darwin` / `windows`、`GOARCH` は `amd64` / `arm64`）。`frontend/dist` は GUI 版と同じ `cd frontend && npm ci --ignore-scripts && npm run build` で作れます（その前に `wails generate module` で `frontend/wailsjs` を作る必要があります）。

## 2. 起動

```sh
ssh-gete --headless --config /etc/ssh-gete/config.yaml --data-dir /var/lib/ssh-gete
```

| フラグ | 意味 | 既定 |
|---|---|---|
| `--headless` | ウィンドウを出さない（headless 専用ビルドでは省略可） | GUI 版では off |
| `--config <path>` | YAML の設定ファイル。環境変数 `SSH_GETE_CONFIG` でも可。暗黙の探索はしない | なし（DB だけで動く） |
| `--listen <addr>` | Web 管理画面の待受。`web.listen` より優先 | `127.0.0.1:8788` |
| `--data-dir <dir>` | DB・known_hosts・管理トークン・TLS 鍵・`secrets.json` の場所 | OS の設定ディレクトリ |
| `--tls-cert` / `--tls-key` | TLS の証明書と鍵（PEM） | なし |
| `--tls-self-signed` | 自己署名証明書を自動で作って使う | off |
| `--print-admin-token` | 管理トークンを表示して終了（無ければ作る） | — |
| `--rotate-admin-token` | 管理トークンを作り直して終了。動作中のプロセスのログインもすべて無効になる | — |
| `--set-admin-password` | 管理パスワードを標準入力から読んで設定して終了（端末では確認の入力あり）。削除は Web 画面の `POST /api/admin/password` で | — |
| `--check-config` | 設定ファイルを検証し、DB にどう反映されるかを表示して終了（DB には書かない） | — |
| `--version` | 版を表示して終了 | — |

優先順位は「フラグ > 環境変数（`SSH_GETE_*`）> 設定ファイル > DB > 既定値」です。

終了コード: 0 正常、1 実行時エラー、2 設定・引数の誤り（安全でない待受の拒否を含む）、3 同じデータディレクトリで別のプロセスが動作中。

停止は SIGINT / SIGTERM（Windows は Ctrl+C）です。実行中の Web の呼び出しを最大5秒待ち、MCP を止め、DB を閉じてから終了します。2回目のシグナルで即座に終了します。SIGHUP では設定を読み直しません。設定を変えたら再起動してください。

## 3. Web 管理画面へのログイン

1. 管理トークンを表示します。トークンは起動ログには出ません。

   ```sh
   ssh-gete --print-admin-token --data-dir /var/lib/ssh-gete
   ```

2. ブラウザで `http://127.0.0.1:8788/`（TLS のときは `https://…`）を開き、トークンを入力します。
3. ログインは 127.0.0.1 からでも必須です。同じ PC の他のユーザーや、別のポートで動くページからの操作を防ぐためです。
   - ブラウザは管理の Cookie を同じホストの別のポート（開発サーバーなど）にも送ります。そのため Cookie だけでは `/api/*` を呼べないようにしてあり、ログインのたびに渡すセッション鍵（このオリジンの localStorage に置き、Cookie には入れない）も要ります。ブラウザの設定でサイトのデータ（localStorage）を禁止していると、ログインしても画面が動きません。
4. トークンが漏れたかもしれないときは `--rotate-admin-token` で作り直します。

### 管理パスワード（任意）

トークンの代わりに、覚えやすいパスワードでもログインできます。既定では設定されておらず、トークンだけが使えます。

- 設定は2通りです。
  - サーバーで `ssh-gete --set-admin-password --data-dir /var/lib/ssh-gete` を実行し、標準入力から入れる（端末では確認のためもう一度入力します。入力した文字は画面に表示されます）。
  - トークンでログインしてから `POST /api/admin/password {"current":"<今のトークンかパスワード>","password":"<新しいパスワード>"}` を呼ぶ。`password` を空にすると削除です。
- 12 文字以上、1024 バイト以下。前後の空白、制御文字、`sgadm_` で始まる値（管理トークンと紛らわしいもの）は受け付けません。MCP の Bearer トークンやエージェント用トークンと同じ値も受け付けません（MCP クライアントがそのまま管理者としてログインできてしまうため）。
- 保存は argon2id のハッシュだけで、`<dataDir>/admin-password`（0600）に置きます。場所は `web.adminPasswordFile` で変えられます。0600 より広い mode や壊れた中身のときは、パスワードでのログインを止めます（トークンは使えます）。
- 設定・変更・削除をすると、全員のログインが無効になります。ファイルを外から書き換えた場合も数秒以内に同じく無効になります。
- ログイン画面は、`sgadm_` で始まる入力をトークン、それ以外をパスワードとして送ります。総当たりのロックはトークンと共通です。同時に送られた試行も結果が出る前から数えるので、1つの接続元から一度に照合されるのは 10 件、全体でも 100 件までです（それを超えた分は 429）。
- パスワードはトークンより推測されやすいので、LAN に公開するときは長いものにしてください。トークンはいつでも復旧手段として使えます。

MCP の Bearer トークン（エージェント用）と管理トークンは別物で、互いに通りません。同じ値にすると起動を拒否します。動作中も、Web 画面の MCP 設定で管理トークンや管理パスワードを Bearer として保存することはできません。

### キーボードショートカット（ブラウザでは Alt+Shift）

ブラウザは Cmd+T / Cmd+W（macOS）、Ctrl+Shift+T / Ctrl+Shift+W（Windows・Linux）やタブの切り替えを自分で使い、ページには渡しません（Cmd+W なら管理画面のタブそのものが閉じます）。そのため Web 管理画面では、ターミナルのショートカットをどの OS でも **Alt+Shift**（macOS では Option+Shift）にしています。

| 操作 | GUI 版（macOS / その他） | Web 管理画面 |
|---|---|---|
| 新しいシェル | Cmd+T / Ctrl+Shift+T | Alt+Shift+T |
| タブを閉じる | Cmd+W / Ctrl+Shift+W | Alt+Shift+W |
| 次・前のタブ | Cmd+Shift+] [ / Ctrl+Shift+] [ | Alt+Shift+] [（または PageDown / PageUp） |
| n 番目のタブ | Cmd+1..9 / Ctrl+Shift+1..9 | Alt+Shift+1..9 |
| 次・前のサーバー、すべて | Cmd+Alt+→ ← 0 / Ctrl+Shift+Alt+→ ← 0 | Alt+Shift+→ ← 0 |

Cmd+W などを押してブラウザのタブを閉じても、サーバー側の SSH セッションは残り、次に開いたときに戻ります。

### 平文 HTTP で使うときの制限

127.0.0.1 の平文 HTTP では、ブラウザの同時接続数（1ホスト6本）の制限のため、ターミナルのタブを6枚以上開くと操作が詰まります。5枚までにするか、TLS（`--tls-self-signed` でも可）を使ってください。

## 4. LAN から使う（TLS 必須）

127.0.0.1 以外で待ち受けるときは TLS が必須で、無ければ起動を拒否します。

```yaml
web:
  listen: 0.0.0.0:8788
  publicURL: https://gate.example.lan:8788   # ホスト名でアクセスするとき
  tls:
    mode: self-signed      # または files（cert / key を指定）
```

自己署名のときは、起動ログに出る SHA-256 の指紋をブラウザの証明書の指紋と照らし合わせてから例外を許可してください。リバースプロキシの後ろに置くときは `publicURL` に外から見える URL を書きます。`https://gate.example.lan:443` のように既定のポート（https の 443、http の 80）を書いた場合は、ブラウザに合わせてポートなし（`https://gate.example.lan`）として扱います。

`Strict-Transport-Security`（HSTS）は、https の `publicURL` がホスト名（localhost・IP 以外）のときに、そのホスト名あての要求にだけ付けます。HSTS はポートを区別しないので、`https://localhost:8788` で開いたときに付けると、同じ PC の `http://localhost:<別のポート>`（開発サーバーや MCP の `http://localhost:8787`）まで 1 年間 https に書き換えられてしまうためです。

## 5. 設定ファイル

例: [deploy/headless.example.yaml](../deploy/headless.example.yaml)。主な規則は次のとおりです。

- 形式は YAML（`version: 1`）。知らないキーは誤りになり、起動を拒否します。平文のパスワードを書くキーはありません。
- `${VAR}` が展開されるのはパスを表すキー（`*File`、`keyPath`、`tls.cert`、`tls.key`、`dataDir`）だけです。
- Unix では、設定ファイルに group / other の書き込み権があると起動を拒否します。
- `web:` は DB に入らず、設定ファイルとフラグだけで決まります。
- エージェントの承認バイパスとエージェント用トークンは設定ファイルでは扱いません（Web 画面からだけ操作します）。

### 設定ファイルと Web 画面での変更の関係（`apply`）

| `apply` | 動き |
|---|---|
| `seed`（既定） | 設定ファイルは起動時の初期値です。設定ファイルの項目が前回の起動から変わっていなければ、Web 画面での変更（DB）を優先します。変わっていれば、その項目に書かれたキーだけを上書きします |
| `enforce` | 起動のたびに設定ファイルの内容で上書きします。Web 画面で変えた値は次の起動で戻ります（GitOps 向け） |

- 設定ファイルから消した接続先は、既定では DB に残ります。`prune: true` のときだけ、設定ファイルが作った接続先を消します（Web 画面で作った接続先は消しません）。
- 反映の前に `--check-config` で「作成・上書き・維持・削除」の予定を確認できます。

## 6. パスワードと秘密情報の保存先（`secrets.backend`）

| `secrets.backend` | 保存先 | Web 画面からの保存 | 向いている場面 |
|---|---|---|---|
| `keychain`（既定） | OS のキーチェーン（macOS キーチェーン、Windows 資格情報マネージャー、Linux Secret Service） | できる。キーチェーンが使えなければ拒否（「このセッションだけ使う」は可） | macOS / Windows / デスクトップの Linux |
| `file` | `<dataDir>/secrets.json`（0600） | できる | キーチェーンの無い Linux サーバー |
| `env` | 設定ファイルで対応づけた環境変数だけ | できない（読み取り専用） | コンテナ、systemd の `LoadCredential` |

- 環境変数 `SSH_GETE_KEYRING`（`keychain` / `file` / `env`）があれば、設定ファイルより優先します（緊急時のため）。
- `memory`（メモリだけに置く）はテスト専用です。通常の実行では `secrets.backend: memory` は設定の誤りとして起動を拒否し、`SSH_GETE_KEYRING=memory` は警告を出して無視します。
- `secrets.file` は `backend: file` のときだけ書けます。省略時は `<dataDir>/secrets.json`、相対パスは `dataDir` からの相対です。
- 既に DB に平文で残っていたパスワードは、起動時に選んだ保存先へ移ります。
- サーバーで `keychain` のまま起動し、キーチェーンが使えないときは、起動ログに「パスワードは保存できません」と出ます。起動は続き、鍵認証と ssh-agent は使えます。

### `secrets.json` について

- 中身は暗号化していません（復号の鍵を同じマシンに置くことになり、意味がないため）。**このファイルを読める人はパスワードを読めます。** データディレクトリはサービス用のユーザーだけが読めるようにしてください（systemd の unit では `StateDirectoryMode=0700`、`UMask=0077`）。
- Unix で mode が 0600 より広いと読み込まず、パスワードの読み書きが失敗します（ログに `chmod 600` の案内が出ます）。直せば再起動しなくても使えるようになります。
- 書き込みは一時ファイル → fsync → rename で行うので、途中で止まっても前の内容が残ります。
- 上限は1項目 64 KiB、ファイル全体 8 MiB です。

### 環境変数やファイルからパスワードを渡す

接続先ごとに、どの backend でも次の2つが使えます。

```yaml
connections:
  - name: legacy-db
    auth: password
    passwordEnv: LEGACY_DB_PASSWORD                    # 環境変数から
  - name: legacy-db-2
    auth: password
    passwordFile: ${CREDENTIALS_DIRECTORY}/legacy-db-2 # 0600 のファイルから（末尾の改行1つは取り除く）
```

- `passwordEnv` / `passphraseEnv` で対応づけた値は、保存先の値より優先して使われます。Web 画面で別の値に変えることはできません（環境変数を変えて再起動してください）。
- `passwordFile` は group / other が読めると拒否します。値は `backend` が `file` / `keychain` ならそこへ保存し、`env` ならメモリにだけ置きます（起動のたびに読み直します）。

Linux では、鍵認証（鍵ファイルはサービス用ユーザーだけが読める場所に置く）か ssh-agent を基本にし、パスワードが必要な接続先だけ `LoadCredential=` か `passwordEnv` を使うことを勧めます。

## 7. systemd（Linux）

unit の例: [deploy/systemd/ssh-gete.service](../deploy/systemd/ssh-gete.service)

```sh
sudo useradd --system --home-dir /var/lib/ssh-gete --shell /usr/sbin/nologin ssh-gete
sudo install -m 0755 ssh-gete /usr/local/bin/ssh-gete
sudo install -d -m 0750 -o root -g ssh-gete /etc/ssh-gete
sudo install -m 0640 -o root -g ssh-gete headless.example.yaml /etc/ssh-gete/config.yaml
sudoedit /etc/ssh-gete/config.yaml
sudo -u ssh-gete ssh-gete --headless --config /etc/ssh-gete/config.yaml --data-dir /var/lib/ssh-gete --check-config
sudo install -m 0644 ssh-gete.service /etc/systemd/system/ssh-gete.service
sudo systemctl daemon-reload
sudo systemctl enable --now ssh-gete
sudo -u ssh-gete ssh-gete --print-admin-token --data-dir /var/lib/ssh-gete
journalctl -u ssh-gete -f
```

- `ProtectHome=yes` なので、SSH の鍵は `/etc/ssh-gete/keys/`（`ssh-gete` ユーザーだけが読める 0600）などに置きます。ssh-agent を使うときは `SSH_AUTH_SOCK` を渡し、`ProtectHome` を調整します。
- パスワードは `LoadCredential=<name>:/etc/ssh-gete/secrets/<name>` と `passwordFile: ${CREDENTIALS_DIRECTORY}/<name>` の組み合わせで渡せます。
- MCP の Bearer トークンは `EnvironmentFile=/etc/ssh-gete/env`（root:root 0600、`SSH_GETE_TOKEN=…`）で渡します。
- シリアルを使うときは `SupplementaryGroups=dialout` と `DeviceAllow=` のコメントを外します。

## 8. Docker

Dockerfile: [deploy/docker/Dockerfile](../deploy/docker/Dockerfile)（distroless の nonroot。シェルはありません）。イメージの中ではビルドせず、release の headless バイナリを入れます。

```sh
mkdir -p dist/linux-amd64 dist/linux-arm64
tar -xzf ssh-gate-linux-x64-headless-v1.01.tar.gz   -C dist/linux-amd64 ssh-gete
tar -xzf ssh-gate-linux-arm64-headless-v1.01.tar.gz -C dist/linux-arm64 ssh-gete
docker buildx build --platform linux/amd64,linux/arm64 -f deploy/docker/Dockerfile -t ssh-gete:headless --load .

docker volume create ssh-gete-data
docker run --rm -v ssh-gete-data:/data ssh-gete:headless --print-admin-token

# 設定ファイルなしで試す（接続先は Web 画面で登録する）
docker run -d --name ssh-gete \
  -p 127.0.0.1:8788:8788 -p 127.0.0.1:8787:8787 \
  -v ssh-gete-data:/data \
  -e SSH_GETE_TOKEN \
  ssh-gete:headless

# 設定ファイルを使う（CMD を書き直すときは --listen と --tls-self-signed も書く）
docker run -d --name ssh-gete \
  -p 127.0.0.1:8788:8788 -p 127.0.0.1:8787:8787 \
  -v ssh-gete-data:/data -v "$PWD/config.yaml:/config/config.yaml:ro" \
  -e SSH_GETE_TOKEN -e LEGACY_DB_PASSWORD \
  ssh-gete:headless --config /config/config.yaml --listen 0.0.0.0:8788 --tls-self-signed
```

ブラウザでは `https://127.0.0.1:8788/` を開きます（自己署名なので、起動ログ `docker logs ssh-gete` の指紋を確かめてから例外を許可します）。

- 既定の CMD は `--listen 0.0.0.0:8788 --tls-self-signed` だけで、設定ファイルなしで起動します。コンテナの中で 127.0.0.1 に待ち受けてもホストから届かないので 0.0.0.0 にしており、127.0.0.1 以外なので TLS が必須です。ホストでは `-p 127.0.0.1:…` でループバックにだけ公開することを勧めます。
- 設定ファイルを渡すときは CMD を丸ごと書き直すので、`--config` に加えて `--listen 0.0.0.0:8788 --tls-self-signed` も書いてください（`--config` だけにすると Web が 127.0.0.1 で待ち受け、ホストから届きません）。設定ファイルが無いパスを `--config` に渡すと終了コード2で止まります。
- MCP は、イメージの環境変数 `SSH_GETE_MCP_ADDR=0.0.0.0:8787` で全インターフェースに待ち受けます。127.0.0.1 以外なので Bearer が必須で、`SSH_GETE_TOKEN` を渡す（または Web 画面でトークンを設定する）までは MCP は待受を始めません（Web 管理画面は動きます）。この環境変数は設定ファイルの `mcp.listenAddress` / `mcp.port` より優先します。変えるときは `-e SSH_GETE_MCP_ADDR=…` で上書きしてください。
- ホスト側のポートを変える（`-p 127.0.0.1:9443:8788` など）か、LAN の別の PC からホストの IP やホスト名でアクセスするときは、ブラウザから見た URL がコンテナの中からは分からないので、`web.publicURL`（例: `https://gate.example.lan:8788`）を設定ファイルに書いてください。書かないと Host / Origin の検査で 403 になります。
- `/data` はイメージの中で nonroot（uid 65532）の 0700 として作ってあり、新しい名前付きボリュームはこの所有者と mode を引き継ぎます。ホストのディレクトリを bind mount するときは、先に `chown 65532:65532` と `chmod 0700` をしておいてください（root のままだと書き込めずに止まります）。
- キーチェーンは無いので、`secrets.backend: env`（Docker の secret を環境変数に）か `file`（`/data/secrets.json`）を選んでください。何も選ばないと、パスワードは保存できません（鍵認証と ssh-agent は使えます）。
- 設定ファイルは group / other に書き込み権があると起動を拒否します（ホスト側で `chmod 0640` か `0600`）。
- docker compose の例: [deploy/docker/compose.example.yaml](../deploy/docker/compose.example.yaml)

## 9. macOS / Windows で常駐させる

- GUI 版のバイナリに `--headless` を付けて起動できます。macOS では `ssh-gete.app/Contents/MacOS/ssh-gete --headless …`。
- Windows の GUI 版は `--headless` のとき親のコンソールにつなぎ直してログを出します。サービスとしての登録はまだ用意していないので、タスクスケジューラ（ログオン時に実行）か NSSM を使ってください。
- GUI 版と headless を同じデータディレクトリで同時に動かさないでください（MCP のポートも競合します）。headless は2つ目の起動を終了コード3で拒否します。

## 10. うまく動かないとき

| 症状 | 確認すること |
|---|---|
| 終了コード2で止まる | 起動ログの理由。知らないキー、設定ファイルの権限、127.0.0.1 以外で TLS なし、MCP と Web の待受の衝突、管理トークンと `SSH_GETE_TOKEN` が同じ |
| 終了コード3で止まる | 同じデータディレクトリで別の ssh-gete が動いていないか |
| パスワードが保存できない | `secrets.backend`。サーバーでは `file` か `env` を選ぶ。`file` なら `secrets.json` の mode が 0600 か |
| ブラウザで 403 になる | `publicURL` と実際にアクセスしている URL（ホスト名・ポート）が一致しているか |
| ログインしてもすぐログイン画面に戻る | ブラウザでこのサイトのデータ（localStorage）が禁止されていないか |
| Docker で「ロックを取れません」や EACCES で止まる | `/data` に bind mount したホストのディレクトリが uid 65532 のものか |
| タブを増やすと固まる | 平文 HTTP の同時接続数の制限。TLS にする |
