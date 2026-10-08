# 配信一覧の完成応答再利用とNG判定のインメモリ化

2026-10-09、追加提案①と③を実装し、`task deploy-app`でisucon-3へ反映した。

コメント・リアクションGETのgzipは、後続の[非圧縮比較](timeline-identity.md)で一度無効化し、
スコア低下を受けて有効へ戻した。

## ① 配信一覧・検索のJSONとgzipを再利用

対象は`GET /api/livestream/search`、`GET /api/livestream`、
`GET /api/user/:username/livestream`。認証とリクエストの検証は従来どおり行う。

- 検索条件・所有者・結果索引の件数・pretty指定・タグカタログをキーとし、
  完成したJSONを再利用する。検索ではgzipも保持し、Accept-Encodingに応じて返す。
- 索引はストアの存続中にcommit済みの配信だけを追加するため、件数を世代として使う。
  所有者別・タグ別の一覧は、無関係な配信の追加では無効にしない。
- 応答に含まれる所有者のアイコンハッシュをキャッシュ取得時に検証する。
  アイコン・タグ更新後は応答を作り直し、公開済みのバイト列を変更しない。
- ID順序、タグリンク重複、タグ指定時のlimit無視、空配列、エラーのHTTPステータス、
  pretty、改行、gzip除外指定を維持する。
- 保持予算は16分割で各8MiB、合計128MiB。予算超過時は分割を空にし、
  予算を超える単一応答は保持しない。起動時に予算分を確保する実装ではない。
  アイコン更新による置換では、同じキーの古い応答の見積容量を差し引く。

`/debug/sql-pools`に`list_response_cache_hits_total`、
`list_response_cache_misses_total`、`list_response_cache_budget_bytes`を追加した。
アイコン更新で無効になった応答はmissとして数える。予算はキー・値等の見積容量であり、
プロセス全体のRSS上限ではない。
これらの指標は当時の変更記録。ユーザーの許可なく追加した計測として、
2026-10-09の指摘後に計数処理を含めて削除した。

## ③ NG判定・モデレーションの削除対象検索からSQLを除去

`task extract-ng-collation`でMySQL 8.0.35-0ubuntu0.22.04.1から
全1,112,064個の有効なUnicode文字のウェイトを取得した。
BMPは各2バイト、補助平面は共通のFFFDであることを全件検証し、
131,074バイトの表をGoバイナリへ埋め込んだ。
取得ツールはデータベースを変更しない。取得失敗時は埋込表を上書きしない。

表のSHA256: `e76a2c49f75648f7d2580cca4490703f1696b6ad0cf67341480ea3095e2499db`。

NGワードを追加・復元するときに`% + word + %`全体をコンパイルし、
コメント投稿と既存コメントの削除対象判定に使用する。大文字小文字・アクセント等は
`utf8mb4_general_ci`のウェイトで比較する。`%`、`_`、バックスラッシュ、
末尾バックスラッシュが追加の`%`をエスケープする場合も扱う。
VARCHARの等値比較による末尾空白の無視はLIKEに持ち込まない。

初期DBの未走査コメント、他の投稿者のNGワードがある配信の全走査、
所有者のNGワードのみで走査済みの場合の新規ワードだけの判定を維持する。
削除IDは従来のWALに保存し、支払い・統計の公開処理と再起動後の復元は同じ経路を使う。
NGワード集合のJSONと判定結果キャッシュは不要になったため削除した。
`ng_cache_*`指標も削除し、NGワードの保持容量はコンパイル済みトークンを含めて計上する。

## 確認結果

- `task check-fast-paths`成功。Goアプリ全体のrace検証を含む。
- `task check-app-improvements`成功。実MySQLの一時DBで、
  全BMP文字のウェイト代表文字との比較、補助平面、特殊文字、ランダムパターンを含む
  68,563ケースの判定がネイティブLIKEと一致した。
  予約公開、アイコン・タグ更新、NGワード追加、支払い・統計、WAL復元の既存検証も成功。
- 完成JSONと型付き応答の一致、同一応答の再利用、更新時の再生成、
  重複と順序、gzip/identity/pretty、アイコン更新との同時実行を確認した。
- 計測停止とLinux/amd64を確認し、`task deploy-app-dry`で対象を確認後に
  `task deploy-app`を実行。isucon-3の`isupipe-go`を再起動した。
- `task check-app-deployment`成功。稼働バイナリのSHA256が一致し、
  isucon-1/isucon-2の両入口で認証API、検索・timelineのgzipと展開後JSONを確認した。
- 再起動前後でWAL世代`11d5ef8a-67be-4ccb-a234-31be7e43eb25`、
  durable_seq=published_seq=85248、rows=95323、wal_bytes=28749566が一致した。
  pending_bytes=0、failed=false。
- 読み取りsmoke後の完成応答キャッシュはhit=7、miss=3、保持見積=23712 bytes。
  負荷走行時のヒット率やスコアを示す数値ではない。

アプリSHA256: `ce7e77be42efc7eadf12f7674f60b534a29fcc98fd8c1947842b325788f8f9e6`。

ログ: `.task/ng-collation-extract.log`、
`.task/response-ng-{local,mysql,deploy-plan,predeploy,deploy,smoke,before,after}.log`。
ベンチマーカーは実行しておらず、スコアへの効果は次のベンチで評価する。
