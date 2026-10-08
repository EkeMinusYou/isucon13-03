# コメント・リアクションGETのgzip無効化

2026-10-09、ユーザーの指示により実装し、`task deploy-app`でisucon-3へ反映した。

## 変更

- `GET /api/livestream/:id/livecomment`と`GET /api/livestream/:id/reaction`は、
  `Accept-Encoding: gzip`を受けても非圧縮JSONで返す。
- JSON内容・順序・limit・空配列・pretty・末尾改行・認証の処理を維持する。
- 検索の完成応答キャッシュとgzip/identityのネゴシエーションを維持する。
- 不要になったリクエストごとの圧縮処理を削除し、
  `task check-app-deployment`のsmokeを検索gzip・timeline非圧縮の確認へ変更した。

## 確認結果

- `task inspect-dns-batching-prereqs`で計測停止とLinux/amd64を確認。
- `task check-fast-paths`成功。実際のtimelineハンドラの非圧縮JSON、limit、pretty、
  検索のgzipネゴシエーションを含むGoアプリ全体のrace検証を実施。
- `task deploy-app-dry`でisucon-3の配布と再起動を確認し、`task deploy-app`成功。
- `task check-app-deployment`成功。稼働バイナリのSHA256一致、認証API、
  isucon-1/isucon-2の両入口で検索gzip・timeline非圧縮を確認。
  検索は9572→3045 bytes、コメントは17946 bytes、リアクションは17433 bytes。
  timelineはidentity/gzip指定で同一JSONだった。これらは負荷走行の平均ではない。
- 再起動前後でWAL世代`516d301d-4460-4854-b5b4-e1ca739c078d`、
  rows=103707、durable_seq=published_seq=94002、wal_bytes=30347430が一致。
  pending_bytes=0、failed=false。

アプリSHA256: `2d09d56e40c39d8087f009626575ca58457ca4820656976895a1ae3ed5b2c385`。

ログ: `.task/timeline-identity-{prereqs,local,deploy-plan,deploy,smoke,before,after}.log`。

ベンチは実行していない。比較基準はgzip有効時のRUN `20261009-060012`、618605点。
次のRUNでスコア・対象APIの時間・送信量・nginxとアプリのCPUを同一負荷窓で比較する。
