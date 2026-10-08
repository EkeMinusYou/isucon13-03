# 投稿JSONとtimeline gzipの再利用

2026-10-09、ユーザーの指示により①POST応答のJSON再利用と
②timeline gzipのグループ再利用を実装し、`task deploy-app`でisucon-3へ反映した。
GoアプリをLinux/amd64へクロスビルドし、isupipe-goを再起動した。

## ②の修正（2026-10-09）

保存済みRUNの比較で、グループ再利用は圧縮CPUを減らせず、
応答全体を読むSHA256計算の負担が増えていた。
RUN `20261009-063406` と `20261009-064341` の60秒負荷窓では、
アプリCPUは66.88→72.88 CPU秒、コメントGET平均は0.874→1.112 ms、
リアクションGET平均は0.831→1.051 msだった。
約73秒のCPUプロファイルでは、SHA256は1.51→12.69 CPU秒になった。
計測削除後のRUN `20261009-065902` でもSHA256は11.92 CPU秒で、
アプリCPUは73.77 CPU秒、スコアは612891点だった。

ユーザーの修正指示により、②のグループ再利用を取り除き、
コメント・リアクションGETを従来の応答全体のgzip圧縮へ戻した。
グループ境界の計算、応答ハッシュ、32MiBのgzipキャッシュ、member連結を削除した。
①のPOST応答JSON再利用と、既存の行JSON・配信一覧応答の再利用は維持する。
gzip交渉、pretty、改行、非圧縮時の応答内容は従来の処理を使う。
新しい計測は追加せず、競技ベンチは実行しない。

修正版は以下を確認し、`task deploy-app`でisucon-3へ配布・再起動した。

- `task check-post-json`成功。既存のPOST確定済み応答、JSON参照更新、
  pretty、gzip交渉、並行利用のテストをrace付きで実行した。
- `task inspect-dns-batching-prereqs`でLinux/amd64と計測停止を確認し、
  `task deploy-app-dry`で配布先とisupipe-go再起動を確認した。
- `task check-app-deployment`成功。稼働バイナリの一致、両HTTPS入口の認証API、
  検索・コメント・リアクションのgzipと展開後のJSON一致を確認した。
- 再起動前後でWAL世代`57014cdf-85e4-4090-9c21-07942a40f351`、rows=102411、
  durable_seq=published_seq=92683、wal_bytes=30105652が一致した。
  pending_bytes=0、failed=false。

修正版アプリSHA256: `f8ede936ffa61e85962051d87a6bc25a3db08f8e25f0c0f1fd694dfe090b1dba`。
ログは`.task/timeline-gzip-fix-{local,prereqs,deploy-plan,deploy,before,after,smoke}.log`。
修正版の負荷時の性能・スコアは未評価。

以下は最初の実装・配布時の記録。

## ① POST応答のJSON再利用

対象はコメント・リアクションの投稿API。
既存の投稿者・配信JSONと行JSONキャッシュを使い、応答全体の再エンコードを除去した。
書込み前に参照を解決し、書込み完了後にアイコンのスナップショットを更新して、
確定したIDを含むJSONを返す。参照JSONを変更せず、POST応答でも行JSONを温める。
201、非圧縮JSON、pretty、改行、エスケープ、認証、NG判定を維持する。

## ② timeline gzipのグループ再利用

対象はコメント・リアクションのGET。

- 64行以下は応答全体を1グループとして再利用する。
- 65行以上では、行IDから決定した区切りで32行以上のグループを作る。
  通常は128行を上限にし、末尾32行未満は直前のグループへ取り込む。
  先頭への追加や削除で、変更されていない内部グループを再利用できる。
- JSONの正確なバイト列をSHA256で識別し、圧縮済みgzip memberを保持する。
  句読点、順序、limit、行内容、投稿者・所有者のアイコン、タグ更新はすべてキーへ反映する。
  gzip memberを連結して返し、展開したJSONは従来と一致する。
- prettyは整形後の応答全体を再利用する。gzip除外指定、1024 bytes未満、
  非GET、圧縮でサイズが増える応答の扱いを維持する。
- 16分割のキャッシュへ各2MiB、合計32MiBの保持予算を設ける。
  初期化・参照ストア再作成時に破棄し、WAL・DBへ圧縮キャッシュを永続化しない。

細かいグループを使った初回の実データ確認では、リアクションのgzipサイズが
1799→3444 bytes（1.914倍）になったため、上記の行数条件へ調整した。
最終版では同じ確認対象が1799 bytesで、従来と一致した。
32MiBのキャッシュ保持とSHA256・グループ結合の仕事が増える。
大型応答では圧縮辞書がグループ間で共有されないため、転送量が増える可能性が残る。

当初、この変更へgzipキャッシュの指標を無断で追加した。
2026-10-09、ユーザーの指摘を受け、この会話で追加した行JSON・一覧応答・gzipの
キャッシュ指標と計数処理を削除し、再デプロイした。
詳細は[判断と変更履歴の調査](measurement/unauthorized-cache-metrics-20261009.md)を参照。

## 確認結果

- `task check-fast-paths`成功。Go全体のrace検証とcollector検証を実施。
- `task check-post-group-json`成功。POSTのJSONを確定済み行のtyped応答と比較し、
  投稿追加、削除、limit、pretty、アイコン・タグ更新、キャッシュ予算、
  並行利用とnet/httpの自動gzip展開を確認した。
  256～257行のfixtureでは従来gzipに対し1.087～1.126倍のサイズだった。
  fixtureの圧縮率は負荷走行時の平均ではない。
- `task check-app-deployment MUTATIONS_SMOKE_PHASE=write`で投稿を確認し、
  最終版への再起動後に`MUTATIONS_SMOKE_PHASE=read`で同じ受理済みデータ、
  モデレーション結果、reportのスナップショット、統計を確認した。
- 両入口でgzip指定とidentity指定を比較し、展開後のJSON一致を確認した。
  最終版の確認対象はコメントlimit=12で18053→1998 bytes、limit=64で56087→3617 bytes、
  リアクションlimit=12で17415→1156 bytes、limit=64で46682→1799 bytes。
  いずれも従来の応答全体gzipと同じサイズだった。検索gzipも維持。
- 最終再起動前後でWAL世代`6ae1e8b7-938b-43e6-8161-257c6fe05c70`、
  rows=103662、durable_seq=published_seq=93945、wal_bytes=30327320が一致。
  pending_bytes=0、failed=false。

アプリSHA256: `2373a8dff3288c341b96adb1edce75de42e175106c662b61ba8b01a6de8b54c9`。
ログ: `.task/post-group-gzip-{prereqs,local,focused,deploy-plan,deploy-final,smoke-final,before-final,after-final}.log`。

競技ベンチは実行していない。比較基準はgzip復元後のRUN `20261009-063406`、617052点。
次のRUNで同一負荷窓のスコア、対象APIの完了数・時間・転送量、アプリとnginxのCPU、
既存の計測だけで比較する。追加計測はユーザーの明示的な指示なしに行わない。
