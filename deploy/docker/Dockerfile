# syntax=docker/dockerfile:1
#
# ssh-gete headless のコンテナイメージ（docs/headless.md、phase6-headless.md §9.2）。
#
# frontend/wailsjs の生成に Wails CLI と WebKit の開発用ライブラリが要るため、
# イメージの中ではビルドしない。release の headless バイナリを入れる:
#
#   mkdir -p dist/linux-amd64 dist/linux-arm64
#   tar -xzf ssh-gate-linux-x64-headless-v1.01.tar.gz   -C dist/linux-amd64 ssh-gete
#   tar -xzf ssh-gate-linux-arm64-headless-v1.01.tar.gz -C dist/linux-arm64 ssh-gete
#   docker buildx build --platform linux/amd64,linux/arm64 -f deploy/docker/Dockerfile -t ssh-gete:headless .
#
# ベースイメージは digest で固定する（release.yml の action を SHA で固定しているのと同じ方針）。
# gcr.io/distroless/static-debian12:nonroot の multi-arch index（2026-08-21 公開）。
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS datadir

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

ARG TARGETARCH
COPY --chmod=0755 dist/linux-${TARGETARCH}/ssh-gete /usr/local/bin/ssh-gete

# DB・known_hosts・管理トークン・TLS 鍵・secrets.json の置き場所。
# VOLUME より前に nonroot（65532）の 0700 で作っておく。無いと VOLUME が root:root 0755 の
# 空のディレクトリを作り、新しい名前付きボリュームもその所有者を引き継ぐので、nonroot の
# プロセスが何も書けない（管理トークンも ssh-gete.lock も作れずに終了する）。distroless には
# シェルが無いので、空の /home/nonroot を中身ごと（＝空のまま）/data としてコピーする。
COPY --from=datadir --chown=65532:65532 --chmod=0700 /home/nonroot /data
ENV SSH_GETE_CONFIG_DIR=/data
VOLUME ["/data"]

# コンテナの中で 127.0.0.1 に待ち受けてもホストから届かないので、MCP は全インターフェースで待ち受ける。
# 127.0.0.1 以外の待受は Bearer が必須なので、SSH_GETE_TOKEN（または Web 画面でのトークン設定）が
# 無いうちは MCP は待受を始めない（Web 管理画面は動く）。この環境変数は設定ファイルの
# mcp.listenAddress / mcp.port より優先する。変えるときは `-e SSH_GETE_MCP_ADDR=...` で上書きする。
ENV SSH_GETE_MCP_ADDR=0.0.0.0:8787

# 8787: MCP、8788: Web 管理画面
EXPOSE 8787 8788

USER nonroot:nonroot

# Web 管理画面も同じ理由で既定は 0.0.0.0 ＋自己署名 TLS（127.0.0.1 以外は TLS が必須）。
# ホストでは `-p 127.0.0.1:8788:8788` のようにループバックにだけ公開することを推奨する。
# 設定ファイルは任意。使うときは `--config /config/config.yaml` を CMD に足す（compose.example.yaml）。
# CMD を差し替えると --listen / --tls-self-signed も消えるので、一緒に書き直すこと。
ENTRYPOINT ["/usr/local/bin/ssh-gete", "--headless", "--data-dir", "/data"]
CMD ["--listen", "0.0.0.0:8788", "--tls-self-signed"]
