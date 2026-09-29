# 計測確認: 20260930-065458

2026-09-30 JST。ユーザーが実行したRUNを確認した。スコアは **3,615 / PASS**。
標準成果物は収集できており、性能調査に利用できる。ただし、**performance_schema digestのSQL被覆不足**と
**dashboardの警告件数の過小表示**を確認した。全項目を完全な計測として扱うことはできない。
確認は保存済み成果物・DuckDB・dashboardと、isucon-1のMySQLに対する読み取りで行った。
ベンチの代理実行、サーバー設定変更、再デプロイ、保存済み成果物の上書きは行っていない。

数値と取得時刻は [measurement-review-evidence.json](measurement-review-evidence.json) に保存した。
集計には既存の `task q` と `runs/analysis.duckdb` を使用した。

## RUNと成果物

- source: `9d789426c8e3db4be86f5d96948e7923bcde5a93` / `main`。
- `run.json` は schema version 4 / finalized、collectorの事前cleanupは成功。
- `bench.log` のスコア・最終チェック成功・PASS、manifest、DuckDB `bench_results`、dashboardの値が一致。
- `task artifacts-run RUN=runs/20260930-065458` は成功。必須captureの内容検査を通過し、必須成果物のmissingはない。
- manifestのartifact statusは `ok` 72件、`empty` 2件。空なのはisucon-2/3の `pt-query-digest.json`。
  対応するテキスト出力は `No events processed`。両ホストのslow logは今回の負荷で対象SQLを記録していない。
  collector停止・ファイル欠損とは区別する。
- 未出力の加点・減点・ルート別得点・累積ベンチエラー件数は不明のまま保持し、0に置換していない。
  シナリオ別成功・失敗は `isupipe_scenario_counts` で7種類を読み取れる。

## 採取時間窓

| 窓 | JST | 意味 |
| --- | --- | --- |
| manifest / BENCHMARK marker | 06:55:01.731〜06:56:47.533（105.801秒） | 初期化・整合性チェック・負荷・終了処理 |
| `load_windows` / `analysis_windows` | 06:55:36.000〜06:56:47.533（71.533秒） | traffic thresholdで推定した解析窓 |
| ベンチの負荷開始ログ | 06:55:46.655 | 整合性チェック成功後の実負荷開始 |
| ベンチの負荷停止ログ | 06:56:46.656 | 約60秒の負荷終了 |
| ベンチの走行終了ログ | 06:56:47.270 | キャンセル等の終了処理完了 |

自動解析窓は実負荷開始より約10.7秒早く、整合性チェック末尾を含む。
終了thresholdを検出できず、collection endを採用した理由も保存されている。
「71.533秒の集計」を「純粋な60秒の負荷集計」として比較しない。
実負荷の開始〜走行終了で絞ると、3台のproc/service/disk/MySQL/SQL poolすべてに61時点のサンプルがある。

CPU/fgprofは各ホスト06:55:00.33〜00.35に開始し、180秒間取得した。
初期化から負荷終了までを覆うが、負荷終了後のidleも含む。
heap/allocs/goroutineは06:56:00.34〜00.37に取得しており、実負荷中のsnapshotである。
CPU・fgprofの全180秒集計、時点のheap/goroutine、プロセス起動からのallocs累積を同じ母数で比較しない。

## 標準計測の実負荷確認

| 対象 | Evidence | 判断 |
| --- | --- | --- |
| host CPU・memory・disk・network | 各ホストのproc/diskが解析窓内71時点、非有限値0 | 実負荷で確認済み |
| service | 各ホストのisupipe-go/nginx/mysql/pdns、解析窓内71時点、availableは全時点1 | 実負荷で確認済み |
| MySQL status | 各ホスト72時点。SELECT等のcounterと接続・lock・buffer poolが読み取れる | 実負荷で確認済み |
| SQL pool | 各ホスト72時点。isucon-1でin_use最大10、wait countとwait durationに増加あり | 実負荷で確認済み |
| 周期計測の品質 | 15系列すべてcoverage 100%、最大gap 1.009秒、非有限値0。採取counterに減少なし | 有意な欠落なし |
| Go CPU / heap / allocs / goroutine / fgprof | 3台×5種類を保存し、DuckDB・dashboardが解析。isucon-1の5種類は `go tool pprof -top` でも解析 | 実負荷で確認済み |
| nginx / alp / upstream | access 3,671行と集計の件数が一致し、APIとupstreamを分類 | 実負荷で確認済み |
| user-transition | API 3,668件すべて分類。203セッション、3,240遷移、30シナリオ群 | 実負荷で確認済み |
| slow log / slp / pt-query-digest | isucon-1で12,499件、58クエリ群。slp・pt-query-digest・dashboardで読み取れる | 実負荷で確認済み、10ms以上が対象 |
| performance_schema digest | ファイル・取り込みは成功するが、主要なprepared SQLが未収録 | 被覆不足 |
| MySQL lock waits | 各ホスト1,431時点（50ms）。isucon-1で待ちを5時点観測し、取得timeoutが2回 | 一部取得失敗あり |
| app/service journal・nginx/MySQL error・kernel/OOM | 同一RUNの時間窓で取得。appのHTTP 500等を追跡でき、kernel/OOM等はイベントなし | 取得経路確認済み |

周期計測のcoverageは既存の品質定義による値であり、解析窓の全ミリ秒にサンプルがあるという意味ではない。
goroutineのsymbol解決にはisucon-1/2で各1 sampleの `[unknown]` が残る（それぞれ3 / 2 goroutine）。
ファイルの破損やprofile取得失敗ではないが、これらのstackの詳細は判断できない。
PowerDNSはservice resource/journal、DNSのSQLはslow log、名前解決の成功・失敗はbench.logで確認できる。
専用のDNSリクエスト遅延時系列はない。task-state/nginx on-CPU/block/mutexは有効化していない。

## 負荷の配置とuser-transition

isucon-1は3,669リクエスト、isucon-2/3は各1リクエストで、後者はmarker時間窓外の確認アクセス。
今回のベンチ負荷はisucon-1に流れている。各nginx→同一ホストのGo、各Go/PowerDNS→ローカルMySQLの構成と整合する。
他2台の小さいprofileや空のslow集計を、計測失敗あるいは3台で負荷分散できた証拠としない。

user-transitionの未分類API・不正JSON・不正時刻はすべて0。
識別可能なリクエストは3,443件、未識別は225件で、内訳をrawから照合した。

- 登録成功後、ログイン前の `POST /api/register`（201）: 197件。
- クライアントが応答header取得前に切断した499: 19件。これらはセッションを結び付けられない。
- その他の未認証/public API・認証失敗・登録失敗: 9件。

正常な認証後の応答で、理由不明の識別列欠落は見つからなかった。
session_idはHMACで匿名化した値で、集計JSONに識別値は残さない。

## 修正が必要な集計と限界

### performance_schema digestに主要SQLが含まれない

同じRUNで `SELECT * FROM livestream_tags WHERE livestream_id = ...` はslow log/slpに4,517件、165.034秒あるが、
performance_schema digestの対応するSELECTは0件。performance_schemaの全集計は14,426件・29.599秒で、
初期化SQLと計測用SQLが含まれている。これを全アプリSQLの処理時間・件数と解釈してはいけない。

負荷後の読み取り確認では、statementのinstrument・consumer・アプリ/PowerDNS threadは有効、digest lostは0。
`statement/com/Execute` に実行が記録される一方、digestのlivestream_tagsにはTRUNCATE/ALTER/INSERTしかない。
Go/PowerDNSのprepared statement経路による既知の未被覆と整合する。
同種の問題はMySQLの[9.6.0公式修正記録（Bug #114480 / #36445818）](https://dev.mysql.com/doc/relnotes/mysql/9.7/en/news-9-6-0.html)にも記載されている。
このRUNでの原因の判断は、保存済みSQLの不一致と負荷後設定の照合によるものである。

現在のSQL調査はslow log/slpを使う。10ms未満のSQL、全SQLの正確な件数、SQL別の索引未使用・一時表などを
このdigestだけで説明することはできない。取得成功のstatusは被覆の完全性を保証していない。
アプリの接続設定・prepared SQLの観測方式の補修が必要だが、この確認作業では変更していない。

### dashboardの警告件数が66件から5件になる

`bench_errors`には66行（HTTP 500の警告1行、終了時のcontext canceled 65行）が保存され、メッセージ欄でも全件を読める。
しかし `bench_warning_events` がtimestampだけでDISTINCTを取り、5時刻へ縮約する。
dashboardの `timeline.buckets[].scenario_warnings` の合計は5件となり、件数グラフが過小表示される。
競技用意味ビュー `tools/contest/analysis-schema/bench-errors-semantic.sql` の修正が必要。
元のログは残っており、再ベンチなしで修正後の再解析・表示を検証できる。

### lock待ちの2回のtimeout

isucon-1の06:55:51.627と06:56:42.477に `context deadline exceeded`。
50ms観測のquery timeout 40msによる2時点の未観測であり、その時点を待ち0と扱わない。
残りの周期観測は継続している。取得できた5時点の待ちが全待ち件数を表すものでもない。

## 読み手の確認

dashboardを既存の `task dashboard` で起動し、runs/scoresとRUNのalp/slowquery/metrics/fgprof/pprof/timeline/mysql/upstream/user-transitionsを確認した。
全APIが200で、対象ホスト・系列・profile・遷移・スコアの内容も読み取れた。
実画面でPASS/3,615、3台の役割、203セッション/3,240遷移、MySQL・serviceの時系列、CPU/fgprofの値を確認し、console errorは0。
警告の件数グラフには上記の既知の不一致が残る。
計測有無の比較RUNがないため、計測によるスコアへの影響・スコア再現性は未検証。
内蔵Browser接続はローカル設定の読込エラーで起動できず、既存の `tools/browser` のPlaywright CLIで確認した。
保存済みRAW・profile・run.jsonを更新せず、確認レポートとEvidenceだけを追記した。
