# 計測補修: 20260930-065458

2026-09-30 JST。[確認レポート](measurement-review.md)で見つかった集計の問題を補修した。
GoアプリをLinux/amd64向けにビルドし、`task deploy-app`でisucon-1/2/3へ配布・再起動した。
適用前に対象・再起動の影響をdry-runで確認し、稼働中RUNがないことを確認した。

## 修正と検証

| 項目 | 修正 | 確認結果と状態 |
| --- | --- | --- |
| GoアプリのSQL digest | mysql driverの`InterpolateParams`を有効にし、driverがエスケープしたSQLを通常のqueryとして送る | 3台の認証APIが成功し、各DBのdigestにlivestream_tags SELECTを8回ずつ記録。疎通確認済み・次ベンチ待ち |
| 警告件数 | timestampだけによるDISTINCTを削除し、同じミリ秒の独立したログを数える | 保存済みRUNの66行を66イベントとして解析。dashboard APIの件数合計も66。実負荷成果物で確認済み |
| 解析窓 | `tools/contest/load-phase.yaml`の開始・停止パターンで実負荷時刻を優先する | 06:55:46.655〜06:56:46.656の60.001秒へ修正。dashboardのhost系列も各60時点。実負荷成果物で確認済み |

SQLの送信経路変更はMySQL 8.0のprepared executionのdigest未収録を回避するためのもの。
SQL文字列へ手動で値を連結せず、既存のmysql driverにパラメータのエスケープを任せる。
取得・回収・artifactの形式は維持し、新しいcollectorを追加していない。
今回のAPI確認後のWHERE付きSELECTのdigest件数はisucon-1で58、isucon-2/3で各53。
これらは短い疎通確認の件数であり、ベンチの処理量・得点として扱わない。

解析窓の正規表現は競技固有ファイルへ置き、汎用analysisctlはISO prefixと設定されたパターンだけを扱う。
collection markerの範囲内に開始・停止が各1行ある場合に採用する。
未取得時は既存のtraffic thresholdへ戻す。時系列はrawから再取り込みし、dashboardとDuckDBが同じ窓を使う。
端の秒も、個々のrequest startを負荷窓で絞ってから件数を集計する。

## 実施した確認

- analysisctlの全テストと、同時刻の警告がdashboard APIで3件のまま保持される回帰テストが成功。
- 高い整合性チェックtrafficより明示的な負荷窓を優先するテスト、端の小数秒と停止行欠損時のfallbackを確認。
- `task setup-smoke`で3台へloginと6種類の認証後APIを実行し、全件成功。
  15個のprofile、host/service/MySQL/pool、アクセス・遷移・SQL集計を取得した。
- `task setup-readers-check`でsmokeのDuckDB取り込みとdashboard parserが成功。
  smokeは `.task/setup-smoke-20260930-071510/` に保存し、競技RUNを新規作成していない。
- `task q-build`で派生DBを更新し、既存RUNの警告66件・60.001秒の解析窓を確認。
  dashboardのscores/timeline/metrics APIも200で、警告・host系列の内容を確認。
- `task artifacts`、`task artifacts-run RUN=runs/20260930-065458`、`git diff --check`が成功。

修正後の数値は [measurement-repair-evidence.json](measurement-repair-evidence.json) に保存した。
保存済みRUNの元ログ・profile・run.jsonと、前回確認時のEvidenceは変更していない。

## 残る制限

過去のRUNで記録されなかったSQL別digest値を、接続設定変更から遡って復元することはできない。
そのRUNのSQL分析には保存済みslow log集計を用いる。修正後アプリの実負荷中の被覆は次のユーザー実行ベンチで確認する。

PowerDNSのSQLは引き続きprepared statementを使うため、今回のアプリ接続設定の変更はそのdigest未被覆を直すものではない。
PowerDNSのSQL分析には既存のslow logとMySQL status・service metricsを用いる。
このprepared statementの利用は[PowerDNSの公式仕様](https://docs.powerdns.com/authoritative/backends/generic-sql.html#queries)とも一致する。

元RUNのlock観測の2回のtimeoutは、保存済みの欠損として残る。0待ちへ置換しない。
周期・timeout設定は維持しており、負荷中の取得失敗が今後必ず0になるとは保証しない。
計測有無によるスコア差・スコア再現性も、この疎通確認では評価していない。
