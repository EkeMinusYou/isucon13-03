# DNS・DB・アプリの役割分離

2026-10-02時点の構成と操作は[2台のHTTPS入口](dual-ingress.md)を参照する。
以下は役割分離時の記録。

2026-09-30。最新RUN `20260930-130243`でisucon-1が負荷60秒に119.16／120コア秒を消費し、
他の2台がほぼ未使用だったため、サービスを分離した。ベンチマーカーは実行していない。

## 現行構成

| ホスト | 稼働サービス | 接続先・保存先 |
| --- | --- | --- |
| isucon-1 | PowerDNS・DNS更新サービス・nginx | BIND backendで全ゾーンをメモリに保持。Aレコードはisucon-1の公開IPを返す。GoへHTTP/1.1 keepaliveで接続 |
| isucon-2 | MySQL | isupipeの保存先。内部IPでTCP接続を受ける |
| isucon-3 | Go | isucon-2のisupipeへ接続。画像はローカルの/var/lib/isupipe/iconsに保存し、Goから本文を返す |

役割外のGo・nginx・MySQL・PowerDNSは停止・無効化した。
競技DBはisucon-2で再作成し、isucon-3の公式初期化APIで初期データとDNSゾーンをロードした。
DNSのメモリ化と同期batchの追加変更は[DNS・書込み改善](dns-batching.md)を参照する。
isucon-1と3の旧ローカルDBはサービス停止後に利用しない。

## 構成と操作の正本

ホストの役割・内部IP・公開IPは`Taskfile.yml`のcontest valuesに置く。
`task gen`は次を生成する。

- `tools/contest/generated/topology.env`：DB接続先とDNSのAレコードの向き先。
- `tools/contest/generated/remote-users.sql`：localhostユーザーの認証方式と認証情報をDB内で参照し、必要な接続元のユーザーを作成する。認証情報は生成ファイルやログに出力しない。
- `mysql/mysql.conf.d/zz-topology.cnf`：DBホストの内部IPをlisten先とする設定。
- DNS更新サービスの環境・初期ゾーン、および既存のnginx upstream生成物。

アプリのsystemdとSQL初期化は配布された`/home/isucon/topology.env`を読む。
既存の`/home/isucon/env.sh`にある認証情報は配布・上書きしない。
DNS更新サービスは`/etc/powerdns/isupipe-dns.env`を読み、保存済みのゾーンを起動時に復元する。

```sh
task deploy-role-split-dry
task deploy-role-split
task check-role-split
```

`deploy-role-split`は計測プロセスとprofileの停止、Linux/amd64を確認してから、
既存の`task db-recreate`、nginx配布、役割適用を実行する。
DB再作成は全ホストのアプリ・DNS停止、役割外サービス停止、MySQL設定反映、
接続ユーザー作成、DB再作成、DNS起動、Go起動、初期化の順で行う。
初期化に失敗した場合は後続処理へ進まない。

通常の`deploy-all`はDBを再作成・初期化せず、MySQL・DNS・Go・nginxを依存順に反映する。
`deploy-role-split`は最後にこの通常配布も行い、書き込んだユーザー・DNSレコード・画像を再起動後に確認する。

## 実施結果

- Goアプリの既存テスト、deployctlの既存テスト、GoとsmokeのLinux/amd64クロスビルドを通過。
- shell構文、deploy dry-run、標準成果物契約の確認を通過。
- `task deploy-role-split`が正常終了。初期化は16.364秒で、公式の42秒制限以内だった。
- サービスの役割、nginx→Go→MySQLの疎通、DNSの役割を確認。
- `pipe.u.isucon.dev`と新規登録ユーザーのAレコードは`13.231.248.152`を返す。
- HTTPSの証明書検証を有効にして、ログインと8件のAPI読み取りを確認。
- アイコンの本文・hash・ETag、条件付きGET、内部配信URLのアクセス制限を確認。
- 新規ユーザー登録・ログイン・DNS反映・ユーザーサブドメインでのHTTPS取得を確認。
- MySQL・PowerDNS・Goのサービス再起動後もユーザー、DNSレコード、画像を取得できた。

ローカルの確認ログは`.task/topology-{before-hosts,prereqs,dry,artifacts,deploy}.log`。
smokeは初期ユーザーtest001の画像を更新し、role-split-smokeユーザーを登録する。
次のベンチの公式初期化でこれらはリセットされる。

スコアと負荷中のCPU・DB待ちの変化は、次のユーザー実行RUNで評価する。
今回の再起動確認はサービス再起動であり、OSの再起動は行っていない。

## nginxの移設

RUN `20260930-155539`は117,124点・PASS。負荷60秒で3号機は112.9／120コア秒を消費し、
Goが59.3、nginxが37.9コア秒だった。1・2号機の消費は各約25コア秒で、nginxを1号機へ移す。

- TaskfileのNGINX_HOSTSを1号機へ変更し、生成upstreamをAPIの接続先に使う。
- nginxはHTTP/1.1と空のConnectionヘッダでupstreamのkeepaliveを利用する。
- public assetsを1号機へ配布する。証明書は既存のものを保持する。
- アイコンのETag判定は単一Goに残し、200の画像本文を3号機から返す。If-Modified-Sinceだけでは304を返さない。
- DNS更新サービスは、保存済みの登録名とepochを復元し、新しい入口IPでゾーンを永続化する。変更時にSOA serialを進める。
- 新しい入口のTLS・静的ファイル・API疎通を確認してからDNSを再起動し、最後に3号機のnginxを停止・無効化する。

```sh
task deploy-nginx-split-dry
task deploy-nginx-split
```

配布対象は1号機のnginx・DNSと3号機のGo。2号機のMySQL再起動、DB再作成、公式初期化は行わない。
配布前後の競技DBの件数・tip合計、DNSの登録名とepochを比較する。
続いて画像更新・登録・DNS・HTTPSを確認し、Go再起動後に同じ画像と登録を取得する。
登録確認ユーザーが既に存在する場合は`REGISTRATION_SMOKE_PHASE=read`で実行する。
確認用の画像更新とユーザー登録は競技DBに保存され、次の公式初期化で消去される。

ログは`.task/nginx-split-{prereqs,dry,mysql-tests,deploy}.log`。
性能は次のユーザー実行RUNでCPU配置、投稿遅延・成功件数、予約成功、得点を比較する。

実施結果：`task deploy-nginx-split`は終了コード0で完了した。
ローカルrace test、DNSの入口変更・登録保持テスト、実MySQLの一時DB検証、Linux/amd64ビルドを通過。
1号機の証明書・静的ファイル・nginx→Go疎通、サービスの役割、DNSの入口を確認した。
配布前後の競技DBの件数・tip合計とDNS登録名・epochは一致した。
画像の本文・hash・ETag・304、登録ユーザーのDNS・サブドメインHTTPSを確認し、Go再起動後も取得できた。
ベンチマーカー、DB再作成、公式初期化、OS再起動は実行していない。
