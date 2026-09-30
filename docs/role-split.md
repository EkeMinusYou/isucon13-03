# DNS・DB・アプリの役割分離

2026-09-30。最新RUN `20260930-130243`でisucon-1が負荷60秒に119.16／120コア秒を消費し、
他の2台がほぼ未使用だったため、サービスを分離した。ベンチマーカーは実行していない。

## 現行構成

| ホスト | 稼働サービス | 接続先・保存先 |
| --- | --- | --- |
| isucon-1 | PowerDNS | isucon-2のisudnsを参照し、Aレコードはisucon-3の公開IPを返す |
| isucon-2 | MySQL | isupipe・isudnsの共通保存先。内部IPでTCP接続を受ける |
| isucon-3 | Go・nginx | isucon-2のisupipeへ接続。画像はローカルの/var/lib/isupipe/iconsに保存 |

役割外のGo・nginx・MySQL・PowerDNSは停止・無効化した。
競技DBはisucon-2で再作成し、isucon-3の公式初期化APIで初期データとDNSゾーンをロードした。
isucon-1と3の旧ローカルDBはサービス停止後に利用しない。

## 構成と操作の正本

ホストの役割・内部IP・公開IPは`Taskfile.yml`のcontest valuesに置く。
`task gen`は次を生成する。

- `tools/contest/generated/topology.env`：DB接続先とDNSのAレコードの向き先。
- `tools/contest/generated/remote-users.sql`：localhostユーザーの認証方式と認証情報をDB内で参照し、必要な接続元のユーザーを作成する。認証情報は生成ファイルやログに出力しない。
- `mysql/mysql.conf.d/zz-topology.cnf`：DBホストの内部IPをlisten先とする設定。
- 各ホストのPowerDNS接続先、および既存のnginx upstream生成物。

アプリのsystemd、SQL初期化、DNS初期化は配布された`/home/isucon/topology.env`を読む。
既存の`/home/isucon/env.sh`にある認証情報は配布・上書きしない。
PowerDNSの起動時処理も同じ向き先を使い、既存ゾーンがあれば書き換えない。

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
