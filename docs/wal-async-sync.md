# 投稿・画像WALの非同期ディスク同期

2026-10-09。投稿と画像の変更・デプロイをユーザーが指示した。
電源断・強制リセットへの耐性は今回の対象外とする。
対象は3号機のGoアプリ。WAL形式ISUPWAL4と競技DBの配置は維持する。

## 根拠

最新finalized RUNは20261009-040007、496,199点、PASS。
task artifacts-runで必須成果物の内容検査が成功した。
DuckDBのload_windows内の04:00:24.601–04:01:23.601（59秒）の差分は、
投稿69,750イベント、画像193断片、43,159同期、同期時間44.162秒だった。
同期1回の平均は1.023ms、投稿と画像を合わせた1同期あたりの件数は1.621。
同期workerの累計時間をリクエストごとの待ち時間や得点改善率には換算しない。

## 動作

- writer goroutineが完全なWALフレームをファイルへ追記し、状態・画像索引を
  WAL順に公開してから要求へ完了通知する。queue投入だけでは成功にしない。
- 画像は全断片の追記とSHA256検証が済んでから読取り可能にする。
  保存中の同じ画像は完了通知を共有し、追記済みの同じ画像は再利用する。
  その後のMySQLへのハッシュ保存、commit、入口への通知の順序は維持する。
- fdatasyncは独立したworkerで100msごと、または未同期分が1MiB以上になった時に行う。
  同期中の追加フレームは次の同期境界へまとめる。同期対象は捕捉した完全フレームまで。
- published_seq / published_bytesは追記・公開済み位置、durable_seq / wal_bytesは
  同期成功済み位置を表す。両者は負荷中に一致しなくてもよい。
  投稿数・画像断片数・画像バイト数のカウンターは追記・公開時に増える。
- 同期待ちのメタデータは連番・位置・件数だけを保持し、要求channelや画像本体を
  次の同期まで保持しない。未同期量16MiBとWAL全容量512MiBの上限は維持する。
- 書込み・公開失敗はその要求を失敗にし、同期失敗は以後の受付・読取りを停止する。
  応答済み要求の成功を後から取り消すことはできない。
- writer終了時に残りを同期する。SIGTERM、SIGINT、サービスExecStopのSIGQUITでは
  HTTP要求を排出してからWALを閉じる。初期化時も旧writerを閉じてから再作成する。

応答前の同期完了を必要としないため、同期workerの収集期間は応答待ちに加わらない。
追加費用は公開位置の管理、定期timerと同期境界の管理。
画像の転送量やMySQLへのハッシュ保存は減らない。得点への効果は未測定。

## 確認と反映

- task check-fast-paths: race、同期停止中の投稿・画像の完了と読取り、同期境界、
  同一配信への後続投稿、画像の再利用、書込み・同期失敗、終了時の排出を確認する。
- task check-app-improvements: 一時DBで画像・ハッシュ・APIの整合性と、全WALイベントの
  復元、初期化、モデレーション・通報・統計を確認する。
- task deploy-app-dryで3号機だけの配布・サービス再起動を確認し、task deploy-appで反映する。
- 公開APIで投稿・画像を書き込み、task deploy-appで再起動後に両入口から復元を確認する。
  ベンチマーカーは実行しない。

ログは.task/async-wal-{prereqs,local,mysql,deploy-plan,deploy,write,restart,read}.log。

## 実施結果

- task check-fast-paths成功。Goアプリ全体のrace検証を含む。
- task check-app-improvements成功。1号機の一時DBを使い、競技DBは再作成しなかった。
- 3台のLinux/amd64と計測・プロファイル停止を確認し、ローカルでクロスコンパイルした。
- task deploy-app成功。3号機のGoアプリを配布・再起動した。
  稼働バイナリとローカルのSHA256の一致をtask check-app-deploymentで確認した。
  SHA256: 0b2af0a87f3b2a8ef4030736a4b48b61877ffa582a76bf875aac01678c38047d。
- 配布前後でWAL世代、durable_seq=published_seq=71305、使用量26,194,948 bytes、
  rows=82286を維持した。
- task check-app-deployment ICON_SMOKE_PHASE=writeで両入口の画像POST・GET、
  ハッシュ・ETagと条件付きGETを確認した。新規画像は1件・1断片、2回目は再利用した。
- task check-app-deployment MUTATIONS_SMOKE_PHASE=writeで9投稿イベントと
  モデレーション・削除後の通報・視聴・投げ銭・統計を確認した。
  smokeは最初の入口だけでモデレーションを書き込み、後の入口で同じ結果を読むように修正した。
  画像の更新は保存する投稿応答内のハッシュに影響するため、画像確認を先に実行した。
- task deploy-appで3号機を再起動後、両入口のMUTATIONS_SMOKE_PHASE=readと
  ICON_SMOKE_PHASE=readが成功した。
- 再起動前後でdurable_seq=published_seq=71315、使用量26,203,729 bytes、rows=82291が一致した。
  pending_bytes=0、failed=false。今回のAPI確認では10レコードを3回の同期へまとめた。
  これは負荷性能の測定ではない。ベンチマーカーは実行していない。

WAL世代: 30d4c8be-9a4f-4bc0-8705-fab45a72481a。
画像確認ログは.task/async-wal-icon-write.log。
状態比較は.task/async-wal-{before,after-deploy,after-write,after-restart}.log。
