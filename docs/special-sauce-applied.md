# 秘伝のタレの適用結果

`isucon-special-sauce`の対象指定なしとして、`docs/special-sources/`のルートREADMEを除く全5文書を調査・適用した。カタログの5リンクと実ファイルは一致し、未掲載文書はなかった。

出典は[nginx](special-sources/nginx.md)、[MySQL](special-sources/mysqld.md)、[パラメーター補間](special-sources/interpolate-params.md)、[MySQLのFD上限](special-sources/file-descriptor.md)、[カーネルパラメーター](special-sources/kernel-parameters.md)。競技要件は[公式注意事項の追試](official/cautionary_note.md#追試)を優先した。

## 採用した差分

| 生成元・管理ファイル | 項目 | 反映前 | 反映後 | 正規deploy・対象 |
| --- | --- | --- | --- | --- |
| `nginx/nginx.conf` | worker FD上限 | systemd soft 1024 / hard 524288、worker指定なし | `worker_rlimit_nofile 16384` | `deploy-nginx`、1号機reload |
| 同上 | worker接続数 | 768 | 2304 | 同上 |
| 同上 | `multi_accept` | 既定off | on | 同上 |
| 同上 | `server_tokens` | 既定on | off | 同上 |
| 同上 | クライアント`keepalive_requests` | 未指定 | 1000000 | 同上 |
| 同上 | `tcp_nodelay` | 既定on | onを明示 | 同上、実効動作は同じ |
| `mysql/mysql.conf.d/mysqld.cnf` | buffer pool | 134217728 bytes | 1073741824 bytes（1 GiB、設定は`1G`） | `deploy-mysql`、2号機restart |
| 同上 | `innodb_flush_log_at_trx_commit` | 1 | 2 | 同上 |
| 同上 | `innodb_flush_method` | fsync | O_DIRECT | 同上 |
| 同上 | `max_connections` | 151 | 10000 | 同上 |
| `etc/systemd/system/mysql.service.d/limits.conf` | `LimitNOFILE` | 10000 | 1006500 | 同上、daemon-reloadを含む |
| `etc/sysctl.conf` | `net.core.somaxconn` | 4096 | 65535 | `deploy-sysctl`、全3台で即時適用 |
| 同上 | `net.ipv4.ip_local_port_range` | 10000 65535 | 10000 60999 | 同上、資料の指定範囲に統一 |
| 同上 | `net.core.rmem_max` / `net.core.wmem_max` | 各212992 | 各16777216 | 同上 |

`Taskfile.contest.yml`に読み取り確認用の`inspect-special-sauce`と`check-special-sauce`を追加した。既存の`check-bulk-writes-mysql`もflush設定2を検査するように更新し、設定1を前提とした表示を修正した。

## 適用済み・追加変更のない項目

- Goの`conf.InterpolateParams = true`は`webapp/go/main.go`で適用済み。
- `worker_processes auto`、`sendfile on`、`tcp_nopush on`、`types_hash_max_size 2048`は適用済み。
- app upstreamは`Taskfile.yml`の`gen`で`keepalive 60`を生成済み。icon readerも`keepalive 64`を持ち、両方のproxy locationにHTTP/1.1と空のConnectionヘッダーが適用済み。生成元と経路を維持した。
- binary logは既に無効で、レプリケーションのconnection/applier channelもなかった。`disable-log-bin`は既存記述を維持した。
- アプリの既存接続上限10・idle上限10は維持した。資料に置換値はなく、固定上限を追加する指示もない。反映前のプールは9接続・使用中0、プロセス起動後累積のwait count 3 / wait duration 2827358 nsだった。この値は同一RUNの負荷窓ではないため、律速判断には使っていない。
- nginxのsystemd hard上限524288はworker指定16384を設定できる。実プロセスでsoft/hard両方16384を確認できたため、systemd drop-in追加とnginx restartは不要だった。

公式要件・ソフトウェア・ホスト上限によって適用できなかった項目はない。全ホストの`fs.nr_open=1048576`、DBホストのメモリー約3.6 GiB・`MemoryMax=infinity`・ext4を確認した。

`innodb_flush_log_at_trx_commit=2`はコミットごとにログを書き、通常は約1秒ごとにディスクへflushする。OS障害・電源断では未flush分を失い得る。通常の停止・再起動時の保存と、この障害条件を区別して採用した。[MySQL公式仕様](https://dev.mysql.com/doc/refman/8.0/en/innodb-parameters.html#sysvar_innodb_flush_log_at_trx_commit)と[終了時の動作](https://dev.mysql.com/doc/refman/8.0/en/innodb-parameters.html#sysvar_innodb_fast_shutdown)を確認した。`tcp_nodelay`の既定値は[nginx公式仕様](https://nginx.org/en/docs/http/ngx_http_core_module.html#tcp_nodelay)による。

## 実装・deploy・確認

進行中RUNのstate fileがないこと、全ホストのcollectorが停止していること、稼働GoのCPU/fgprofが非実行であることを確認した。OS/archは全ホストLinux/x86_64だった。Goコード変更がなく、Goアプリのビルド・再配布・再起動は不要だった。

`task gen`、`git diff --check`、`task --dry deploy-sysctl deploy-mysql deploy-nginx`を通過した。続いて`task deploy-sysctl`、`task deploy-mysql`、`task deploy-nginx`を順に実行し、全て終了コード0で完了した。設定検査は正規deploy内の`mysqld --validate-config`と`nginx -t`で実施した。

`task check-special-sauce`で全ホストのsysctl、MySQLの5変数、systemdとMySQL実プロセスのFD上限、nginxの読み込まれた設定と2 workerのFD上限を確認した。MySQLの接続上限エラーは0だった。`Aborted_connects`は初回確認で0、TCP疎通確認後の再確認で1だった。`check-network`は認証せずTCP接続を閉じるため、このカウンターだけでアプリの接続障害とは判断しない。`check-roles`と`check-network`も通過した。再確認時のRSSはMySQLが498376 kB、nginx workerが8448 / 13312 kBだった。

`task check-app-deployment`で稼働Goバイナリの一致、既存索引、HTTPSログインと8認証APIの読み取りを確認した。DB再作成、公式初期化、OS再起動、ベンチは実行していない。

確認ログは`.task/special-sauce-{before,plan,sysctl,mysql,nginx,after,api}.log`。

次のユーザー実行RUNでは、同じ負荷時間窓のスコア・成功リクエスト数・API遅延、MySQLのread/flushとI/O待ち、接続エラーとプール待ち、nginxの再接続・FD・メモリーを比較する。追加設定をまとめて反映したため、単一項目ごとの得点寄与はこのdeployだけでは確定しない。
