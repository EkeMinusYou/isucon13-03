# 2台のHTTPS入口

2026-10-02。RUN `20261002-002105`の負荷60秒で1号機が103.3／120 core秒、
2号機が16.0／120 core秒を消費していたため、HTTPS入口を1・2号機へ配置した。
ユーザー実行RUN `20261002-011044`はPASS、スコア366,404から468,312へ27.8%改善した。

## 配置とDNS

| ホスト | 役割 | DNSのA応答 |
| --- | --- | --- |
| isucon-1 | nginx、アイコンreader、権威DNS、MySQL | 登録済み配信者ドメインは54.250.213.246 |
| isucon-2 | nginx、アイコンreader | 共通ドメインpipe.u.isucon.devは18.177.140.41 |
| isucon-3 | Goアプリ、投稿WAL、アイコン画像の永続化 | 両入口から172.31.41.146:8080へ接続 |

DNSの問い合わせ先は1号機。共通ドメインと配信者ドメインでAレコードを1つ返し、
TTL 300、NXDOMAIN、AAAAのNODATA、NSのglueを維持する。両入口は全APIに対応し、
HostとCookieをそのまま3号機へ送る。書込みの所有者とWALは3号機のみ。

配置の正本は`Taskfile.yml`のcontest values。`DNS_COMMON_HOST`が共通ドメインの入口、
`DNS_RECORD_ADDRESS`が配信者ドメインの入口を指定する。`task gen`は共通の`topology.env`と
readerのホスト別listen設定を生成する。reader unitは`/home/isucon/icon-read.env`を読む。

## MySQLの再配置

RUN `20261002-011044`の負荷60秒では、1号機54.4／120 core秒、2号機90.1／120 core秒を消費した。
2号機ではnginxが55.4、readerが6.9、MySQLが24.2 core秒を使い、CPU 80%以上が11／60区間あった。
共通ドメインの入口とMySQLのCPU競合を減らすため、MySQLを1号機へ移設した。

同RUNの1秒ごとのCPU需要を移し替えた試算では、1号機の平均65.5%・最大76.8%、
2号機の平均54.9%・最大72.8%となる。処理量・処理コストを固定した試算であり、
実際のスコアとDNSとのディスク競合は次のユーザー実行RUNで評価する。

`Taskfile.yml`の`MYSQL_HOST`と`MYSQL_HOSTS`を1号機へ変更し、`task gen`でDB接続先と
MySQLのlisten設定を生成する。計測停止と実ホストのLinux/amd64を確認してから、既存経路を使う。

```sh
task build
task db-recreate-dry
task db-recreate
task apply-roles
task check-role-split
task check-bulk-writes-mysql
task check-icon-read
task check-icon-edge
task check-app-deployment ICON_SMOKE_PHASE=write ICON_EDGE_CHECK=true CROSS_INGRESS_CHECK=true
```

DB再作成は全ホストのアプリ・DNSを停止し、役割外のMySQLを停止・無効化した後、
1号機のMySQL設定・接続ユーザー・schemaを反映する。DNS、3号機Go、両readerを起動して
公式initializeを実行し、`task apply-roles`で1号機MySQLの自動起動も有効にする。
DB再作成直後は`/api/tag`が503を返すため、この起動経路ではHTTP listenerの200または503を
確認してinitializeへ進む。通常のアプリdeployは引き続き200応答を待つ。

確認ログは`.task/mysql-relocation-*.log`。Goの処理、両入口のドメイン割当、投稿WALの配置は変更しない。

`task db-recreate`と`task apply-roles`が正常完了し、initializeは約6.2秒で完了した。
1号機MySQLの稼働・自動起動、2号機MySQLの停止・無効化、3号機からの接続と10索引を確認した。
両入口の認証API、共通Cookie、双方向のアイコン更新の2秒以内反映、ローカル304も通過した。
登録・投稿fixtureは共有状態へ一度だけ書き込み、両入口で読取りを確認した。
1号機MySQLと3号機Goを再起動した後も、ユーザー・DNS・画像・投稿・モデレーション・
報告snapshot・統計・支払いの読取り比較が通過した。

## アイコンの整合性

3号機は両readerへ同じ更新を並列に通知し、initializeでは両方を並列に無効化する。
snapshot取得の接続許可も両readerの内部IPへ拡張した。readerは100ms間隔の差分取得と
750msのleaseを維持する。通知が失敗しても期限切れのmetadataでは304を返さず、
画像本体・不明なユーザー・期限切れの読取りは3号機へ問い合わせる。

公開に使うnjs辞書は各入口のreaderが更新する。画像本体の保存先は増やさない。
nginx access logへ`vhost`を追加し、次のRUNで共通ドメインと配信者ドメインの偏りを確認できるようにした。

## 操作と確認

```sh
task check-dual-ingress-local
task deploy-dual-ingress-dry
task deploy-dual-ingress
```

deployは計測停止、Linux/amd64、証明書、両readerのセッション署名鍵を確認する。
moduleの配布・ABI確認を先に完了させ、その後に設定を配布する。
設定ディレクトリの検査中に同じ場所へmoduleを並列転送しない。
サービス操作はアプリ、reader、nginx、DNSの順。アプリはHTTP応答が可能になるまで待つ。
MySQL再起動・DB再作成・initializeはこのdeployに含まない。

実施した確認は以下のとおり。

- Goのrace検証。DNSの分岐と登録状態の復元、通知の並列化と両readerの無効化、接続元制限を含む。
- GoアプリとsmokeのLinux/amd64クロスビルド、dry-run、`git diff --check`。
- `task deploy-dual-ingress`が終了コード0で完了。サービスの役割、DB接続、稼働バイナリの一致を確認。
- 両readerと両njs辞書が同じ世代を取得し、freshであることを確認。
- 両入口で証明書を検証し、ログインと8認証API、画像本文・hash・ETag、内部URLのアクセス制限を確認。
- 共通・配信者ドメインのDNS分岐、同一Cookieの両入口での利用、双方からの画像更新の2秒以内反映を確認。
- 各入口のローカルaccess logで、upstreamを使わない304とsession identityの記録を確認。

確認ログは`.task/dual-ingress-{build,plan,deploy,check-plan}.log`。
smokeは初期ユーザーtest001のアイコンを更新する。次の負荷走行の公式initializeでリセットされる。
JSON圧縮は今回の実装に含まない。

次のRUNでは得点・投げ銭処理量・視聴完了数に加え、同じ負荷区間の入口別CPU/core秒と
CPU pressure、Host別処理量・転送量、WAL同期の待ちを比較する。
MySQL再配置後のCPU需要と得点は未測定なので、この配置で均等になるとは仮定しない。
