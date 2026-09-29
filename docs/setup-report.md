# ISUCON13 セットアップ確認

追記: ユーザー実行RUN `20260930-065458` を実負荷で確認した。
GoアプリのSQL digest・警告件数・解析窓を補修して3台へ適用した。
警告・解析窓は既存実負荷成果物で確認済み、修正後アプリのSQL計測は疎通確認済み・次ベンチ待ち。
詳細は [RUNの計測補修](../runs/20260930-065458/measurement-repair.md) を参照。以下は初回セットアップ時点の記録。

2026-09-30 JST。設定・デプロイ・疎通・分析の確認を完了した。標準計測は「疎通確認済み・ベンチ待ち」。
この作業ではベンチマーカーを実行していない。保存済みbaseline RUNは現在の`runs/`にはなく、RUNを新規作成していない。
負荷中の採取窓・計測負荷・スコア再現性の確認はユーザーが実行するbaselineを待つ。

## 対象構成

| SSH alias | 内部IP | DNSが返す公開IP | 管理対象 |
| --- | --- | --- | --- |
| isucon-1 | 172.31.40.191 | 54.250.213.246 | isupipe-go / nginx / mysql / pdns |
| isucon-2 | 172.31.37.38 | 18.177.140.41 | 同上 |
| isucon-3 | 172.31.41.146 | 13.231.248.152 | 同上 |

全ホストはLinux 6.2 / amd64、2 vCPU、約3.6 GiBメモリ、cgroup v2。MySQLは8.0.35。
各nginxは同じホストのGoアプリ8080へ転送し、アプリ・PowerDNSは各ホストのMySQLに接続する。
独立した3台の既存構成を継承した。生成される`upstream app`は現行vhostの転送先には使われない。
管理対象はローカルを正本とし、`task setup-*`で取得、`task deploy-*`で配布する。Go以外の参考実装は取得・参照のみ。
TLS秘密鍵とホスト固有`env.sh`はGit/deployへ含めない。nginx証明書は既存ホストのものを使用する。
提供された証明書のissuerは`isucon13 practice CA`、有効期間は2026-09-29〜2029-01-01。
smokeは公開証明書を信頼ストアに加え、証明書とホスト名を検証してHTTPS接続する。
OS・SSH・chrony等の基盤サービスは変更対象外。公式の外部メディア配信も対象外。

## DB再作成

`task db-recreate`と`task db-recreate-dry`を整備した。
対象は全`MYSQL_HOSTS`の`isupipe`と`isudns`。システムDB・既存DBユーザーの権限は再作成対象に含めない。
GoとPowerDNSの書込み元停止を確認してから、競技DBを破棄し、取得済みアプリschemaとPowerDNSパッケージschemaを適用する。
DNS起動、Go起動、各独立DBに対する公式`POST /api/initialize`の順に進む。エラー時は後続処理へ進まない。
初期化timeoutは公式の42秒に合わせた。配布Go実装の言語出力`language: golang`を継承する。

`RESET_INPUTS`にはSQL初期化スクリプト・全初期データSQL・DNS初期化スクリプト・zone・DNS schemaを列挙し、
通常app deployと再作成planの両方でSQL/DNS資材を配置する。
初期化が使う`/home/isucon/env.sh`とPowerDNSの既存起動補助`/opt/init_zone_once.sh`は取得内容を確認した。

dry-runで対象・転送先・停止と初期化の依存順を確認済み。実DBの再作成は今回実行していない。
読取確認では各ホストに`isupipe`12テーブル・`isudns`7テーブルが存在し、認証APIとDNS応答が正常だった。

## 標準計測

以下の保存先は本番の`runs/<RUN_ID>/`を基準とする。今回の疎通成果物は`.task/setup-smoke-20260930-064239/`に保存した。

| 計測 | 対象・生成元・有効条件 | 保存先・読み手 | 状態・根拠 |
| --- | --- | --- | --- |
| スコア・合否 | ユーザーがベンチ結果を入力。自動取得形式は`tools/contest/bench-patterns.json`に宣言 | `scores.tsv`, `run.json`, `bench.log`, 原文`benchmark-result.txt`; DuckDB `bench_results`、dashboard scores | 疎通確認済み・ベンチ待ち。成功・失敗・未判定・公式形式のfixtureで検証。未取得スコアはNULL |
| 内訳・ペナルティ | 公式資料のスコアは負荷中Tip合計。取得済み出力にはシナリオ成功/失敗数がある | 原文を保持。`isupipe_scenario_counts`、`bench_errors`、`bench_warning_events` | 疎通確認済み・ベンチ待ち。独立した加点・減点・route別点数は取得済み仕様・出力にないためNULL/0行。0点として補完しない |
| Go CPU / heap / allocs / goroutine / fgprof | 全APP_HOSTS、loopback:6060の専用listener。5種類が既定有効。CPU/fgprofは180秒、snapshotは60秒後 | `<host>-go-*.pprof`, `<host>-fgprof.pprof`; `go tool pprof`, DuckDB `pprof_*`, dashboard profile parser | 疎通確認済み・ベンチ待ち。5秒採取を3台で実施し15ファイル・39 sample typesを解析。CPUとgoroutine wall timeを区別 |
| profile開始確認 | 全APP_HOSTS、`/debug/profiles/status`でRUN所有元を確認 | 通常bench runnerが採取開始を確認、先行収録後に負荷開始、回収を待つ | 疎通確認済み・ベンチ待ち。smokeで3台の開始を確認。専用listenerは127.0.0.1に限定 |
| nginx JSONログ・ローテート | 全NGINX_HOSTS。実効vhostは共通access_logを継承。`escape=json`、時刻・HTTP・upstream・session列 | `raw/access-<host>.log.zst`; alp, `endpoints_by_ingress`, `upstreams_by_ingress`, dashboard | 疎通確認済み・ベンチ待ち。reopen後の21件を回収・圧縮・集計。全3入口が分析に現れる |
| user-transition | 全NGINX_HOSTS。アプリ内session UUIDをHMAC化した識別子を専用response headerへ出し、nginxでログ化。headerは外部応答から除去 | `user-transitions.json`; DuckDB `user_transition_edges/scenarios`, dashboard parser | 疎通確認済み・ベンチ待ち。21件分類、3セッション・18遷移、未分類/時刻不正/識別列欠損0。生Cookieは保存せず、集計にも識別値を残さない |
| MySQL status | 全MYSQL_HOSTS、root Unix socket、1秒周期 | `<host>-mysql-status.tsv`; DuckDB `metrics`, dashboard MySQL parser | 疎通確認済み・ベンチ待ち。各ホスト6 samplesを回収し、時間窓内の3台分を取り込んだ |
| slow log・digest | 全MYSQL_HOSTS。FILE出力、slow有効、閾値0.01秒。performance_schema有効、RUN前digest reset | `<host>-slp.tsv`, `<host>-pt-query-digest.*`, `<host>-mysql-digest.tsv`; DuckDB `queries`, dashboard slow-query | 疎通確認済み・ベンチ待ち。各ホストの20ms読み取りprobeがslow logへ出力され、slp・pt-query-digest・P_Sを集計できた |
| DB lock wait | 全MYSQL_HOSTS。既存の50ms周期collector、query timeout40ms | `<host>-mysql-lock-waits.tsv`; DuckDB `mysql_lock_waits` | 疎通確認済み・ベンチ待ち。各ホスト118〜120行、capture_error0。今回の軽い疎通では競合は発生していない |
| SQL pool | 全APP_HOSTS、既存`/debug/sql-pools`、1秒周期 | `<host>-sql-pool-metrics.tsv`; DuckDB `metrics` source sql_pool、dashboard metric reader | 疎通確認済み・ベンチ待ち。全3ホストで時刻・pool counterを回収し取り込み確認 |
| ホスト・disk・ネットワーク | 全ALL_HOSTS、`/proc`・`/sys`、1秒周期 | `<host>-proc/disk-metrics.tsv`; DuckDB `metrics`, dashboard host parser | 疎通確認済み・ベンチ待ち。各ホスト6〜7 samples、capture_error0 |
| service・依存サービス | 全ホストのisupipe-go/nginx/mysql/pdns、unit/process/cgroup対応、1秒周期 | `<host>-service-metrics.tsv`, `<host>-service-journal.log`; DuckDB metrics/runtime_logs、dashboard service parser | 疎通確認済み・ベンチ待ち。全3台の4サービスを解析。PowerDNSはcontrol socketとDNS A応答も確認。DNS処理別の追加時系列は今回の標準範囲外 |
| app・nginx・MySQL障害ログ | 管理対象全ホスト、同じsmoke/RUN時間窓のjournal・error.log | `<host>-app-journal/nginx-error/mysql-error/service-journal.log`; DuckDB `runtime_logs`、原文 | 疎通確認済み・ベンチ待ち。回収と3台分の取り込み確認。時間窓内のMySQL errorは0件で、読取失敗とは区別 |
| kernel / OOM | 全ALL_HOSTS、同じ時間窓のkernel journal | `<host>-kernel.log`, `<host>-oom.log`; DuckDB `runtime_logs`、原文 | 疎通確認済み・ベンチ待ち。回収済み。対象窓に障害イベントはなく、故障注入は行っていない |
| 通常RUN接続・欠損検査 | 有効宣言から自動profile、collector、digester、必須成果物を決定。全対象ホストをrolesに記録 | `run.json` artifact status/missing/quality、`task artifacts-run`, DuckDB/dashboard | 疎通確認済み・ベンチ待ち。`task artifacts`契約一致と既存tool testsを確認。実負荷の時間窓/必須成果物検査は次baselineで確認 |

追加のtask-stateとnginx on-CPUは「意図的に無効」。現段階に用途の指定がなく、標準計測でのbaseline取得を優先する。
任意のアプリ内部snapshot・block/mutexは今回対象外。既定collectorの負荷比較と再起動後スコア再現性は未実施で、初期setup完了条件には含めない。

## 確認結果とEvidence

- `task setup-check`：設定構文、Goクロスbuild、全Go tool tests、成果物契約、deploy dry-runを通過。
- `task deploy-all`：全3台へsysctl、MySQL、PowerDNS、Go、nginxを正規配布。MySQL/PowerDNS/Go restartとnginx reload成功。
- `task apply-roles`, `task check-roles`, `task check-network`, `task check-pdns`：サービスenable/active、アプリ・DB接続、DNS A応答を確認。
- smokeの窓は`2026-09-29T21:42:42.690692541Z`〜`21:42:45.489092240Z`。ベンチではなく、認証API、短いSQL、2秒のsampling待ちのみ。
- `task setup-readers-check SMOKE_DIR=<上記ディレクトリの絶対パス>`：全3台のメトリクス・profile・alp・upstream・遷移・SQL・障害ログをDuckDBとdashboard parserで確認。検証はtemporary fixtureで行い、本物のRUNやスコア履歴を作成していない。
- `go tool pprof -top`：CPU 5秒採取で50ms sample、fgprof 5秒採取で50.21 goroutine秒。低負荷の疎通値で、律速の判断には使わない。
- dashboard frontendは`npm run build`で確認。実負荷データの画面確認は次baselineを待つ。
- スコアadapterの書式は保持されていた分析DBの既存ログ（`20260930-062443`）を参照。このRUNの成果物ディレクトリは現在存在せず、今回のbaseline確認済みには含めない。

ローカルEvidenceは`.task/setup-current-{check,deploy,roles,network,readers,db-recreate-dry,mysql-error}.log`。
smoke成果物をRUNの完了記録に紛れ込ませず、この報告へ記録した。

## 次のbaseline

`task bench-manual`を実行し、採取開始の案内後すぐにユーザーがベンチを開始する。
結果原文をローカルファイルへ保存して同じRUNへ渡し、スコア・合否を入力する。
`task artifacts-run`でstatus/missing/qualityを確認し、`task q`と`task dashboard`で分析・表示を確認する。
失敗した場合も通常回収を完了する。現在`BENCH_HOST`/`BENCH_COMMAND`は未指定で、自動代理実行の設定はしていない。
