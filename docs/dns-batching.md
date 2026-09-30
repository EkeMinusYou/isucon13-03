# DNSメモリ化・同期batch

2026-09-30、改善案①②を実装。比較基準はfinalized RUN `20260930-144223`、115,456点、PASS。
負荷60秒でMySQLは99.4コア秒、アプリのDB接続取得待ちは延べ41.09秒だった。
RUN全体のアプリCOMMITは46,301回、DB側の累計実行時間158.62秒。
負荷60秒の値とRUN全体の値、並行する待ちの合計は分けて比較する。

## DNS

- 1号機のPowerDNSをBIND backendへ変更し、既知名とNXDOMAINをメモリで処理する。ワイルドカードは追加しない。
- 同じGoバイナリを`ISUPIPE_MODE=dns-control`で常駐させ、3号機からの登録・初期化を受ける。
- 登録は最大32件・収集待ち2msでまとめ、ゾーンをfsync・rename・ディレクトリfsyncで永続化する。
- `pdns_control bind-reload-now`で公開し、SOA serialの一致をDNS応答で確認してから登録APIへ成功を返す。
- ゾーンは`/var/lib/isupipe/dns/u.isucon.dev.zone`に保存し、再起動時に登録と世代を復元する。
- 公式初期化で動的登録を消去し、世代を更新する。初期化以前の世代の更新は拒否する。
- 初期ゾーンは公式資材を利用する。名前はゾーンファイル用にエスケープする。
- SQL backend、DNS用SQLスキーマと初期化スクリプト、アプリの`pdnsutil`起動を削除する。

登録のMySQL COMMIT後にDNSの公開を待つ。DNS障害時はAPIを成功扱いせず500を返すが、
COMMIT済みユーザーはDBに残る。MySQLとDNSをまたぐ分散トランザクションは追加していない。

## 書込み

- コメント・リアクション・入退室・NG登録・通報を、配信IDの剰余で4本のwriterへ振り分ける。各writerは最大32件・収集待ち1msの同期batchへまとめる。
- 同じ配信は同じFIFOへ入り、キューの順に更新する。別writerに振り分けられた配信は並行処理する。
- writerのトランザクションをREAD COMMITTEDにし、NG削除のgap lockで別配信の投稿を止めない。SAVEPOINT・COMMIT後の応答とMySQLの永続性は維持する。
- 正本はMySQL。COMMIT成功後にHTTP応答を返し、WALや非同期永続化は追加しない。
- SAVEPOINTで各操作の失敗を分離する。COMMITやSAVEPOINT復旧に失敗したbatchは全件を失敗扱いにする。
- クライアント切断で他の操作を取り消さない。実行中の操作はworkerの独立した期限で処理する。
- コメントのNG判定をEXISTSの1 SQLにまとめ、既存LIKEの照合順序・ワイルドカードの意味を維持する。
- コメント・リアクション・NG一覧の単一SELECTでBEGIN/COMMITを除去し、アイドル接続上限を10へ揃える。
- 通報時のコメント本文・tip・投稿者・配信・時刻を保存し、モデレーションで元コメントを削除した後も通報を取得できるようにする。
- 登録と予約は既存の独立トランザクションを維持する。初期化前にはwriterを停止し、受理済み操作を完了させる。

## 操作と確認

```sh
task check-icon-unit
task check-app-improvements
task db-recreate-dry
task deploy-dns-batching
```

配布対象は1号機のDNS、2号機のDB、3号機のGo。`deploy-dns-batching`は計測停止を確認し、
正規`task db-recreate`でDBを再作成して公式初期化を行う。更新確認後にDNSとGoを再配布・再起動し、保存済み状態を再取得する。

ローカルrace testと実MySQLの一時DB検証は成功した。17件の更新を2回のCOMMITに集約し、
部分失敗・COMMIT失敗・切断の分離、NG判定の互換性、削除後の通報内容と入退室の整合性を確認した。
作業ログは`.task/dns-batching-{mysql-tests,dry,deploy}.log`。

`task deploy-dns-batching`は終了コード0で完了した。公式初期化は15.725秒で、42秒制限内。
新規登録直後のDNSとサブドメインHTTPS、認証API、予約・コメント・画像を確認し、DNS・Goのサービス再起動後も取得できた。
PowerDNSのゾーンがメモリへ読み込まれ、未知名はNXDOMAINを返すこと、2号機の`isudns`が存在しないこと、各ホストの役割も確認した。
OSの再起動は実施していない。

## 単一writerの評価と4並列化

ユーザー実行RUN `20260930-153702`は98,354点・PASSで、変更前の115,456点から14.8%低下した。
負荷60秒ではMySQLのCPU消費99.44→17.83コア秒、SELECT42.67万→4.19万回、DB接続取得待ちの延べ時間41.089→0.065秒となった。
通報一覧のHTTP 500は24→0件になった。

一方、同じ60秒のコメント投稿は平均20.16→33.49ms、リアクションは15.89→36.56msとなり、
コメントの201応答は6,723→6,020件へ減った。180秒のfgprofでは単一writerの完了待ちが延べ475.06秒あり、
DB接続待ちを解消した後に全配信の直列化が制約となった可能性が高い。
予約の400応答も1→1,251件に増加した。予約枠不足の影響は分離できていないが、投稿遅延は枠不足がない最初の20秒から悪化していた。

ユーザーの合意を受け、4本の配信別writerへ変更した。MySQLの接続上限10、待ちキューの総容量256、
各batchの32件・1msは維持する。予約処理、DNS、スキーマの変更はなく、配布対象は3号機のGo。

`task check-icon-unit`のrace testと`task check-app-improvements`の一時MySQL検証に成功。
4配信の同時実行・各配信の更新順序・停止時の全キュー消化、
空のコメント範囲をDELETEする間の別配信への投稿、同じ配信でNG登録に続く投稿の拒否を確認した。
既存の部分失敗・COMMIT失敗・切断・NG照合・通報・入退室の検証も通過した。

```sh
task deploy-parallel-writers
```

この配布は公式初期化・DB再作成を行わず、配布前後の競技DBの件数・tip合計を比較し、
稼働バイナリ、認証API、nginx→Go→MySQLの接続を確認する。
作業ログは`.task/parallel-writers-{prereqs,mysql-tests,dry,deploy}.log`と`.task/parallel-writers-data-{before,after}.tsv`。

`task deploy-parallel-writers`は終了コード0で完了した。3号機の稼働バイナリのSHA-256がローカルビルドと一致し、
HTTPSログインと8件の認証API、nginx→Go→MySQLの接続を確認した。
配布前後のユーザー・配信・コメント・リアクション・通報・NG・視聴履歴・画像の件数とtip合計は一致した。
公式初期化・DB再作成・DNSサービス再起動は行っていない。

エージェントによるベンチ実行は行わない。次のユーザー実行RUNで単一writerのRUNと比較し、
スコア・投稿の遅延と処理量・writer完了待ち・DB接続取得待ち・COMMIT・CPUを評価する。
