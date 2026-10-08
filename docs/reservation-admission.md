# 初回予約を優先する予約受付

2026-10-09。ユーザーの実装・デプロイ指示により、isucon-3のGoアプリへ反映する。

## 変更

- DBトランザクションの前で、到着した予約要求を20msの共通受付窓に集める。
- 要求した時間帯に重なる予約をまだ持たない利用者を優先する。
  同条件では予約時間の短い要求、既存予約の少ない利用者、到着順の順で選ぶ。
  未確認の収益予測や固定の人気ランキングは使わない。
- 100ms以上待った要求は、初回・時間の優先順位より先着順を優先する。
  実行中のトランザクションを追い越さないため、全待機時間の上限ではない。
- 重なる時間帯と同じ利用者の予約を同時にDBへ入れない。
  別の時間帯・利用者の要求は最大10件並行する。
- 成功した予約の公開後に受付を解放し、次の選択で更新済みの予約数を使う。
  予約失敗の100ms待機は、rollbackと受付解放の後に行う。
- 待機中のキャンセルは要求をキューから除く。予約枠を消費しない。
- 予約の可否と枠数の減算は既存のFOR UPDATEとDBトランザクションで決める。
  期間・タグ・返却内容・永続化は維持し、優先度だけを理由に拒否しない。
- 初期化・通常再起動時はDBから復元した配信索引で優先順位を判断する。
  キューは永続化しない。計測用のカウンター・ログは追加しない。

## 確認

- task check-request-pacing-local: Goアプリ全体のrace検証が成功。
  初回優先、短時間優先、100ms経過後の先着順、非競合期間の並行処理、
  同一利用者の成功後の優先順位更新、キャンセル・解放を確認した。
- task check-app-improvements: Linux/amd64の実MySQL一時DBで成功。
  既存予約を持つ利用者と初回利用者が競合する残り1枠を初回利用者が取得した。
  rollback、枠の境界、キャンセル、配信情報・索引のDBからの復元も確認した。
- task inspect-dns-batching-prereqs: 全3ホストのLinux/amd64と計測停止を確認。
- task deploy-app-dry: 配布先isucon-3とisupipe-goの再起動を確認。

## デプロイ結果

- task deploy-appでLinux/amd64へクロスコンパイルし、isucon-3へ配布。
  isupipe-goの通常再起動が成功した。
- task check-app-deploymentで稼働バイナリとローカル成果物のSHA256一致を確認。
  isucon-1/isucon-2の両入口でログイン、認証API、検索・timelineのJSONとgzipが成功。
- 再起動前後でWALのgeneration、durable_seq、published_seq、使用量、rowsが一致。
  generation=c9205b9d-2b89-46a0-9298-a9dfc53bf6d6、durable_seq=published_seq=88752、
  使用量29,487,839 bytes、rows=97277、pending_bytes=0、failed=false。
- ビルド成果物SHA256:
  2550a8823ce000a9aefb7070070ab06dd6b1ce60cc6fcb97b02db3ced52a3c73。

得点への効果は未計測。ベンチマーカーは実行していない。

ログ: .task/reservation-admission-*.log
