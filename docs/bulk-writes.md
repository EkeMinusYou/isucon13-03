# binlog無効化・投稿SQLの一括化

2026-09-30。ユーザーの実装・デプロイ指示により、2号機のMySQL設定と3号機のGoを変更した。
比較基準はfinalized RUN `20260930-161926`、155,114点、PASS。
この変更後のスコアは未計測。エージェントはベンチマーカーを実行していない。

## 変更

- MySQLで`disable-log-bin`を設定し、`innodb_flush_log_at_trx_commit=1`を明示した。
  現行の単一MySQL構成はレプリケーションを利用しない。InnoDBの同期保存は維持する。
- writerの操作をコメント・リアクション・NG登録・通報・入退室の型付きデータへ変更した。
  任意の処理関数はトランザクションを制御するテスト内でのみ使用する。
- 配信IDで振り分ける4本のFIFO、最大32件、収集待ち1ms、READ COMMITTED、workerの5秒期限を維持した。
- NG登録・通報・入退室を区切りとし、その間のコメントとリアクションをそれぞれ受付順にまとめる。
  区切りを越えた並べ替えは行わない。
- コメントのNG判定を番号付きのUNION ALLで1つのSELECTへまとめた。
  `utf8mb4_general_ci`とLIKEのワイルドカード・エスケープの意味を維持する。
- 通過したコメントとリアクションを、テーブルごとに複数行VALUES INSERTで保存する。
  SAVEPOINTは一括INSERTごと、および区切りの操作ごとに設定する。
- 全行を自動採番する通常のVALUES INSERTに限定し、LastInsertIdと行位置から応答IDを復元する。
  アプリの接続は採番間隔・offsetを1へ固定する。独自の採番器・スキーマ変更は追加していない。
- 文字数超過などの行データエラー時は、そのグループをSAVEPOINTまで戻して分割し、不正な行を分離する。
  NG拒否は400、DBの行データエラーは従来どおり500となり、他の正常な操作は保存できる。
- デッドロック、接続断、期限超過、COMMIT失敗ではトランザクション全体を失敗扱いにする。
  COMMIT結果が不明な操作の自動再実行は行わず、成功応答を返さない。
- コメント・リアクションの応答に必要な参照を投入前に確認する。
  応答の組み立てをwriterから移し、COMMIT後にIDと最新のアイコンhashを反映する。
  正本と一覧取得は引き続きMySQLに置く。

正常なコメント8件を同じ区間へまとめられる場合、BEGIN・COMMITを除くSQL往復は24回から3回になる。
リアクション8件では16回から2回になる。一括化できる実際の件数と増点量は次のRUNで評価する。

## 確認

- `task inspect-dns-batching-prereqs`で計測停止と実ホストのLinux/amd64を確認した。
- `task check-icon-unit`のローカルrace testを通過した。
- `task check-app-improvements`の実MySQL一時DB検証を通過した。
  4並列の各16コメント・16リアクションのIDと保存行の対応、NG照合、文字数超過の分離、
  実行前キャンセル、投稿→NG登録→投稿、通報→削除→通報、COMMIT失敗を確認した。
  既存の部分失敗、クライアント切断、キュー消化、別配信の進行、予約、統計、アイコンの検証も通過した。
- `task build-bulk-writes`でGoとsmokeをLinux/amd64へクロスビルドした。
- `task --dry deploy-bulk-writes`で配布先と再起動・検証の順序を確認した。

## デプロイ結果

`task deploy-bulk-writes`は終了コード0で完了した。
この競技固有タスクは既存の`task deploy-mysql`と`task deploy-app`で反映する。
2号機のMySQL設定と3号機のGoを配布・再起動した。
1号機には確認用smokeバイナリを配置してHTTPSで検証した。nginx・DNSの設定変更や再起動は行っていない。

- MySQLの実効値はbinlog OFF、InnoDB同期保存1、採番間隔・offset 1だった。
- 3号機の稼働バイナリのSHA-256がローカルビルドと一致した。
- 配布前後のユーザー・配信・コメント・リアクション・通報・NG・視聴履歴・画像の件数とtip合計は一致した。
- HTTPSログインと8件の認証API読み取りを確認した。
- 初期ユーザーtest001のNG登録のない配信へ、確認用コメント1件（tip=7）とリアクション1件を書き込んだ。
  POST応答のIDと保存内容を対応付けて確認した。
- その後、MySQLとGoを再配布・再起動し、確認用の本文・tip・投稿者・配信とリアクションを取得できた。
  再起動前後の件数とtip合計も一致した。

確認用の2件は競技DBに残り、次の公式初期化で消去される。
DB再作成、公式初期化、OS再起動は行っていない。
ローカルログは`.task/bulk-writes-{prereqs,unit,mysql,build,dry,deploy}.log`。
件数・tipの比較は`.task/bulk-writes-{data,persisted}-{before,after}.tsv`。

次のユーザー実行RUNでは、同じ負荷60秒の得点、投稿成功数、平均・p95遅延、SQL回数、
DB接続待ち、CPUを比較する。SQL digestと180秒のprofileは負荷外を含む補助根拠として分けて扱う。
