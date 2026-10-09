# パスワード照合とセッション検証の再利用

2026-10-09に、合意したCPU削減案①②をGoアプリへ実装し、`task deploy-app`でisucon-3へ反映した。

- 登録のDB commit後に、ユーザーID・保存済みbcryptハッシュに対応するパスワードのHMAC照合情報を保持する。ログインでは現在のDB値を読み、入力の照合情報が一致した場合にbcryptの再計算を省く。不一致・キャッシュ未登録の場合は従来のbcrypt照合を行い、成功だけを保持する。平文パスワードを保持せず、DBに保存するbcryptとコストを変更しない。
- 発行済みまたは署名検証済みのCookieに対応する認証情報を保持する。取得ごとに期限を確認し、未登録Cookieは従来の署名検証へ進む。署名鍵の変更で両キャッシュを交換し、initializeの排他区間でも両キャッシュを空にする。Cookieの内容・有効期限・計測用session IDは従来と同じ。
- 既存のboundedCacheを16分割して使用する。保持量の予算はパスワード照合情報が合計2 MiB、セッションが合計4 MiB。満杯になったshardは破棄し、認証結果は従来処理から再構築する。Cookie文字列を複製し、大きなリクエストヘッダー全体の保持を防ぐ。

nginx・DNS・DBの配置や設定は変更せず、Goアプリの通常再起動だけを行った。キャッシュは再起動時に消え、DBのbcryptと既存Cookieの署名から再構築する。

## 確認結果

- `task check-auth-caches-local`が成功。誤ったパスワード、保存ハッシュの変更、bcryptの長い入力の扱い、キャッシュの初期化、Cookie改ざん、期限境界、署名鍵変更、無効なuser ID、並行アクセスをrace検証した。
- `task check-app-improvements 'APP_IMPROVEMENTS_TEST_RUN=^TestImprovementsMySQL$/^authentication_caches$'`が成功。isucon-1の一時DBで登録直後の照合情報の公開、bcryptコスト、繰り返しログイン、認証API、パスワード変更後の拒否・成功、キャッシュを空にした後のログインを確認した。
- 全ホストのLinux/amd64と計測停止を確認し、ローカルでクロスコンパイルした。`task check-app-deployment`で稼働バイナリのSHA256一致、両nginx入口のログイン・認証API・JSON/gzip応答を確認した。
- 再起動前後でDB件数・tip合計とWALのgeneration、durable/published sequence、bytes、rowsが一致した。sequence=90352、bytes=30,241,569、rows=98,978、pending=0、failed=false。WALの実行中カウンターは再起動でリセットされるため、永続状態とは分けて確認した。
- 配布バイナリSHA256: `ba669aa739bab28be7719db28b6e5bae4c83e8d7cb1c8786d67ebf4d250d9e38`。

ログは`.task/auth-caches-{local,mysql,prereqs,deploy-plan,deploy,smoke}.log`と、`.task/auth-caches-{wal,data}-{before,after}.*`にある。競技ベンチマーカーは実行していない。CPU削減量・レイテンシ・得点への効果は次のRUNで評価する。
