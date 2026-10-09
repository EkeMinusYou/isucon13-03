# 視聴入口とGoの同居・アイコン読取りの集約

2026-10-09。ユーザーの実装・デプロイ指示により、既存の`task deploy-dual-ingress`で反映した。

| ホスト | 役割 |
| --- | --- |
| isucon-1 | Go DNS・MySQL |
| isucon-2 | 共通入口のnginx・アイコンハッシュの読取りサービス |
| isucon-3 | 配信者側入口のnginx・Go・アイコン本体のWAL正本 |

配信者ドメインはisucon-3へ、`pipe.u.isucon.dev`はisucon-2へ向ける。
視聴APIのnginxからGoへの転送は同じisucon-3内で完結する。Goの状態管理とWALは単一のまま。
アイコン読取りサービスはisucon-2だけに置き、isucon-3のアイコンGETは同居Goへ転送する。
isucon-1のnginx・アイコン読取りサービスを停止・無効化した。

## 実装

- `Taskfile.yml`の`NGINX_HOSTS`を`isucon-3 isucon-2`、`ICON_READ_HOSTS`を`isucon-2`へ変更。
- `task gen`で共通nginx設定からホスト別設定を生成する。isucon-2のネイティブ304処理を維持し、isucon-3のアイコンlocationはGoへ転送する。生成ディレクトリはgit管理対象外。
- deployとconfig-checkは同じホスト別nginx設定を使う。競技固有のconfig-check宣言は`tools/contest/`へ置いた。
- DNSの初期ゾーンでNSのAレコードをDNSホストの公開IPに固定し、Go DNSでNSのA応答とglueにその値を使う。共通入口・配信者入口・DNSホストを分離する。
- 新入口を起動してからDNSを切り替え、役割外のnginx・アイコン読取りサービスを停止する。
- アイコン更新の通知先もisucon-2だけに変更。余分な読取りサービスの定期同期・nginxへの公開を除いた。

## 確認

- 全3ホストのLinux/amd64と計測停止、両nginxホストの証明書、Goと読取りサービスのセッション署名鍵の一致を確認。
- `task config-check-dns`のrace検証が成功。共通入口、配信者入口、NSのA/glue、DNS登録の再起動後の復元を確認。
- ローカルでLinux/amd64へクロスコンパイルし、正規Task経路で配布。実nginxの構文検査と役割・接続先・DNS応答の確認が成功。
- 切替前後でDNS世代・serial・登録件数・ログサイズが一致。
- 切替前後でWAL世代`aae40269-ceae-424d-bd19-241e2a0b0bbb`、sequence `91969`、bytes `30568498`、rows `100470`が一致。failed=false、pending=0。
- 両HTTPS入口でログイン、認証API、検索・timelineのgzip、アイコンの内容・hash・ETag・条件付きGETが成功。両入口でのアイコン更新が2秒以内に反映された。
- 確認用のアイコン更新と利用者登録後、`task deploy-app`と`task deploy-dns`で通常再起動。再起動前後でWALのsequence `91972`、bytes `30590164`、rows `100470`と世代が一致し、DNS登録状態も一致。両入口からアイコン・利用者・登録サブドメインを取得できた。
- isucon-2のnginx直接304と、読取りサービス・共有辞書のfresh状態を確認。

競技ベンチマーカーは実行していない。レイテンシと得点への効果は次のRUNで比較する。
ログは`.task/colocated-ingress-*.log`、`.task/dual-ingress-{wal,dns}-{before,after}.*`にある。
