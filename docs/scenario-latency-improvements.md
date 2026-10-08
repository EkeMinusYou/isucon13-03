# 視聴・モデレーションのシナリオ短縮

## 実装

1. 配信検索とユーザー別配信一覧を既存のreferenceStoreの索引から取得する。
   全件検索はID降順、ユーザー別はID昇順、タグ検索はリンクの重複を保持する。
   タグ名はutf8mb4_binの末尾空白を無視する比較に合わせる。
   タグ指定時のlimit無視、未知のタグ・負のlimitのHTTPステータスを維持する。
   初期化・再起動時にDBから復元し、予約のcommitと投稿状態の公開が成功した後に索引へ追加する。
2. NGワード集合のJSONとコメント全文をキーにMySQLの判定結果を再利用する。
   初回は既存のJSON_TABLEとLIKEを実行し、エラーはキャッシュしない。
   NGワード追加後はキーが変わるので以前の許可結果を使用しない。
3. コメント・リアクションの行JSONを再利用する。
   行の全内容と、投稿者・配信のJSON参照をキーにする。
   アイコン・タグ更新で参照が変わると再生成する。投稿とモデレーションによる
   一覧内容・順序・limitは既存のtimeline処理が決める。

キャッシュは16分割。NG判定の保持予算は合計4MiB、行JSONは合計32MiB。
各分割の予算超過時にその分割を空にし、予算を超える単一要素は保持しない。
これらは保持するキー・値等の見積予算であり、プロセス全体のRSS上限ではない。
キャッシュは永続化せず、各ストアの再構築時に破棄する。

既存の/debug/sql-poolsにng_cache_*とrow_json_cache_*のhit/miss・保持予算を追加した。
初回のNG判定が再出現する割合と、行JSONのヒット率を次のRUNで確認できる。

## デプロイと確認結果

- task inspect-dns-batching-prereqsで実ホストのLinux/amd64と計測・プロファイル停止を確認。
- task check-fast-paths成功。ローカルのGoアプリ全体のrace検証を含む。
- task check-app-improvements成功。実MySQLの一時DBで順序・タグ重複・limit、
  予約公開と再起動後の検索、NG判定の再利用・NG追加後の判定、モデレーション、
  アイコン・タグ更新後のJSON、WAL復元を確認した。
- task deploy-app-dryで対象と操作を確認後、task deploy-appを実行。
  isucon-3のGoアプリ・既存の付随資材を配布し、isupipe-goを再起動した。
- task check-app-deployment成功。稼働バイナリのSHA256一致と、
  isucon-1/isucon-2の両入口で認証API・検索・timelineのJSONとgzipを確認。
- 再起動前後でWAL generation=5dc1143d-f8ee-47b4-95f5-25f089dd61c2、
  durable_seq=published_seq=80637、rows=90966が一致。
  pending_bytes=0、failed=false。
- 読み取りsmoke後の行JSONキャッシュはhit=72、miss=24、保持予算=85159 bytes。
  このsmokeにはNG判定の投稿が含まれず、NGキャッシュのhit/missは0。
  NGキャッシュは実MySQLの一時DBで確認した。

ビルド成果物SHA256: ac7ddec5938eeede44c6f4054e8322f9000ad9f0e924b51b297d62170d677beb。

ログ: .task/scenario-{prereqs,local,mysql,deploy-plan,predeploy,deploy,smoke,before,after}.log。
ベンチマーカーは実行しておらず、シナリオ短縮量と得点への効果は未計測。
