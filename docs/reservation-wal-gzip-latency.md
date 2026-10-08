# 予約WAL化とgzip処理時間の改善

2026-10-09に、合意した3案を順番に実装し、`task deploy-app`でisucon-3のGoアプリへ反映した。

1. タグ登録を複数行の単一INSERTへまとめてデプロイした。
2. 予約枠・配信情報・タグを既存WALで扱い、予約処理のSQL往復を除去した。①のSQL処理はこの変更で置き換えた。
3. gzip writerを`github.com/klauspost/compress/gzip` v1.19.2のレベル1へ置き換えた。依存の要求に合わせ、go.modのGoバージョンを1.24にした。ビルドは既存のローカルGo 1.27.1からLinux/amd64へクロスコンパイルした。

## 予約の正本と復元

MySQLの`reservation_slots`・`livestreams`・`livestream_tags`は初期状態と変更前の受理済み予約を含むbaselineとして読む。変更後の予約はDBへ投影せず、WALに配信情報とソート済みタグID列を保存する。SQLだけを読むと変更後の予約・残枠は取得できない。

予約gateで枠チェックからWAL受付・公開までを直列化する。成功時は完全なWAL書込み、メモリ上の枠消費と統計登録、検索・閲覧への参照公開を終えてから201を返す。収集タイマーや予約成功への固定待機は設けず、満枠400の既存100ms待機を維持する。

通常再起動ではSQL baselineから枠を読み、WAL順に消費する。次の配信IDはSQLの最大ID・AUTO_INCREMENTとWALの受理済みIDから復元する。参照カタログはSQLの配信・タグとWAL予約を合わせて検索索引を再構築する。`POST /initialize`は公式SQL初期化後の新しいWAL世代から同じ状態を構築する。

## gzip方式の比較

稼働中APIから取得した同一JSONを使い、isucon-1のLinux/amd64上でwriterを再利用して比較した。各方式3回の中央値であり、HTTP往復時間や競技スコアの測定ではない。

| JSON | 元サイズ | 標準レベル1 | 採用レベル1 | 圧縮時間の削減 | 圧縮後サイズ（両方式） |
| --- | ---: | ---: | ---: | ---: | ---: |
| 検索10件 | 9,377 bytes | 65.265 µs | 54.429 µs | 16.6% | 2,981 bytes |
| コメント12件 | 17,929 bytes | 38.648 µs | 30.269 µs | 21.7% | 1,587 bytes |
| リアクション12件 | 17,116 bytes | 30.523 µs | 24.489 µs | 19.8% | 1,117 bytes |

レベル2も比較したが、タイムラインの処理時間が標準レベル1を上回ったため採用しなかった。全方式で標準gzip readerにより元JSONと同一であることを検証した。

再確認は`task collect-response-compression-samples`、`task check-response-compression`で行う。保存先は`.task/response-compression/`。アプリ側の計測カウンターやログは追加していない。

## 確認結果

- 最終コードの`task check-request-pacing-local`によるrace検証と、`task check-app-improvements`による実MySQLの一時DB検証が成功した。
- 最後の1枠の同時予約、失敗時の残枠維持、重複タグ、通常再起動後の残枠・配信情報・次ID、受付前と受付後のキャンセルを確認した。
- MySQLの唯一の接続を占有した状態でも、再起動後の予約が成功した。
- ②デプロイ後に`task check-app-deployment RESERVATIONS_SMOKE_PHASE=write`で1件作成し、③デプロイ時の再起動後に`RESERVATIONS_SMOKE_PHASE=read`で両nginx入口から配信情報と重複タグを確認した。本番の確認予約は枠を選択しない従来の挙動を用い、枠の消費と復元は一時DBテストで確認した。
- 再起動前後でWAL世代`24d8de84-cdaf-43b5-88db-d35dfde639f9`、published/durable sequence `87749`、bytes `29283377`が一致し、failed=false・pending=0を確認した。

詳細ログは`.task/reservation-bulk-*`、`.task/reservation-wal-*`、`.task/reservation-gzip-final-*`、`.task/response-compression-compare.log`にある。競技ベンチマーカーは実行しておらず、予約HTTPレイテンシとスコアへの効果は次のRUNで評価する。
