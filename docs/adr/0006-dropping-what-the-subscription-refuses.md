# 0006. 応答の生成の要求から、サブスクリプションが断る欄と項目を落とす

- Date: 2026-11-14
- Status: Accepted

## Context

ChatGPT のサブスクリプションの token で Responses API を呼ぶと、API キーなら受け付けられる欄や input の項目の一部で、上流が 400 を返す。エラーの `code` は `subscription_sharing_unsupported_capability` で、文には受け付ける input の項目の種類が並ぶ（テキスト・画像・ファイルのメッセージ、項目の参照、reasoning の項目、function call など）。

呼び手のエージェントが使う pi-ai は、この制限を知っている。ただし制限を当てるのは、「Sign in with ChatGPT」で直接 OpenAI と話していると判定したときだけである。判定の条件は、接続先が `https://api.openai.com/v1` であることと、鍵が `sk-` で始まらないことである。接続先を llm-proxy にすると判定が偽になり、pi-ai はサブスクリプションが断るものを送る。実際に、llm-proxy 越しに呼んだエージェントがこの 400 で止まった。

避ける手は呼び手の側にもある。モデルの設定で該当する機能を 1 つずつ切るか、接続先を公式の URL のままにして通信の行き先だけを llm-proxy へ差し替えるかである。どちらも、呼び手がサブスクリプションの都合を知り、呼び手ごとに設定を合わせることになる。llm-proxy の役目は、1 つのサブスクリプションを複数の呼び手が普通の Responses API として使えるようにすることなので、この都合は llm-proxy の側で吸収したい。

ADR-0001 の Decision 項 1（変えるのは資格情報だけ）は、要求と応答の本文には触れず、読みもしないと決めた。その退けた案「本文を読んで直す」は、呼び手の SDK の版によって送る欄が変わり、Proxy がそれを追いかけることになるのを理由にしていた。この追いかけは、上のとおり呼び手の側でも同じように起きている。呼び手が 1 つずつ追うより、Proxy が 1 か所で追う方が少なくて済む。

pi-ai は、会話の途中で考える量（reasoning の effort）を変える機能を `configuration_update` という input の項目で送る。この機能を使うとき、本文の `reasoning.effort` は固定の `medium` になり、本当の量は input の最後の `configuration_update` が運ぶ。この項目を落とすだけでは、考える量が黙って `medium` になる。

## Decision

### 対象は `POST /v1/responses` の要求の本文だけ

本文を読んで直すのは、`POST /v1/responses` の要求の本文だけである。ほかの route の要求、応答の本文とヘッダは、ADR-0001 のとおり読まずに流す。要求のヘッダの扱いも ADR-0001 のまま変えない。

### 知っているものだけを落とす

落とすものは一覧で決め、一覧に無いものはすべて残す。上流のエラーの文にある「受け付ける項目」の一覧で絞る方式（許す一覧）は採らない。許す一覧で絞ると、上流が受け付けるようになった項目や、Proxy が知らない新しい項目を、呼び手に知らせずに落とすからである。一覧に無いものを上流が断れば、呼び手はそのエラーをそのまま受け取る（ADR-0001 の Decision 項 4（失敗したときに返すもの））。

一覧の基準は、pi-ai が「Sign in with ChatGPT」と判定したときに送らないものとする。サブスクリプションで実際に動いている呼び手の振る舞いであり、確かめられる一次資料があるからである。

| 落とすもの | 場所 |
|---|---|
| `configuration_update` の項目 | `input` の配列の中 |
| `prompt_cache_retention` | 本文の最上位 |
| `prompt_cache_options` | 本文の最上位 |
| `max_output_tokens` | 本文の最上位 |
| `temperature` | 本文の最上位 |

一覧を変えるときは、この ADR を更新する。

### 考える量は最後の `configuration_update` から移す

`configuration_update` を落とすとき、最後のものが `reasoning.effort` を持っていれば、その値を本文の `reasoning.effort` にする。本文にすでに別の値があっても、最後の `configuration_update` を正とする。`reasoning` のほかの欄（`summary` など）は残す。

これは、pi-ai が `configuration_update` を使うときに本文の `reasoning.effort` を固定の値にし、本当の量を最後の項目にだけ載せる、という振る舞いに依存している。呼び手がこの機能を切っていれば `configuration_update` は来ないので、本文の `reasoning.effort` は変わらない。

### 残すもの

思考の続きと prompt cache の鍵は、ADR-0001 の Context の理由のとおり残す。reasoning の項目（暗号化された中身を含む）、本文の `prompt_cache_key`、ヘッダの `session_id`、function call とその出力、ほかの欄と項目は、呼び手が書いたとおりに送る。落とすものが無い要求は、本文をバイト列として変えずに送る。

### 本文を読み切ってから送る

対象の要求では、本文を最後まで読んでから上流へ送る。ADR-0001 の Decision 項 3（ストリームは届いたそばから流す）のうち、要求の本文を読んだそばから送る部分は、この route では成り立たなくなる。応答は今までどおり届いたそばから流す。

- 読む量に上限を置く。上限を超える要求は上流へ送らず、413 で断る。
- `Content-Encoding` が付いた（`identity` 以外の）要求は、読まずにそのまま送る。伸張して直すと、圧縮の仕方まで Proxy が決めることになる。落とすべきものが入っていれば上流が断り、呼び手はそのエラーを受け取る。
- 本文が 1 つの JSON オブジェクトとして読めないときは、読んだとおりに送る。Proxy が独自のエラーを足さず、上流の判断に任せる。

### 落としたことを名前でログに残す

黙って落とすと、呼び手は指定が効いたと思う。落とした欄と項目、移した欄があった要求では、その**名前**を 1 要求につき 1 行、アクセスログとは別の行に出す。行には要求のパスと、アサーションで確かめた主体も書く。値は書かない（ADR-0005）。何も落とさなかった要求では出さない。

### 本 ADR で決定しないこと

- 上限の値と、ログの行の欄の名前（委譲先: README の「What passes」と、`internal/proxy` のコード）
- 応答の生成以外の route で、サブスクリプションが断るものを直すか（委譲先: その route を通すと決める ADR。いまは ADR-0001 の既定の route だけを通す）
- pi-ai 以外の呼び手が送る、サブスクリプションの断るもの（委譲先: その呼び手が見つかったときに、この ADR の一覧を更新する）

## Consequences

呼び手は接続先を llm-proxy にして、普通の Responses API として話せる。サブスクリプションの都合に合わせたモデルの設定や、接続先の偽装は要らなくなる。

代わりに、llm-proxy は pi-ai の版と上流の制限を追いかけることになる。一覧に無いものをサブスクリプションが新しく断れば、呼び手はまた 400 で止まり、この ADR の一覧を足すまで直らない。考える量を移す処理は pi-ai の今の振る舞いに依存しており、pi-ai がその形を変えれば効かなくなる。

`max_output_tokens` と `temperature` を落とすので、呼び手が指定した出力の上限やサンプリングはサブスクリプションでは効かない。ログを見れば、どの呼び手のどの指定が落ちたかは分かる。

応答の生成の要求は、本文を読み切るまで上流へ送られない。画像やファイルを本文に載せた要求ほど、上流へ送り始めるのが遅れる。上限を超える要求は断られる。

## Alternatives Considered

**呼び手の側で避ける（ADR-0001 のまま）。** モデルの設定で機能を切るか、接続先を公式の URL に見せかける。Proxy は単純なままだが、呼び手ごとにサブスクリプションの都合を知って設定を合わせ続けることになる。

**許す一覧で絞る。** 上流のエラーの文にある受け付ける項目だけを残す。断られることは無くなるが、Proxy が知らない項目を呼び手に知らせずに落とす。上流が受け付ける範囲を広げても、Proxy を直すまで使えない。

## 関連 ADR

- [[0001-pass-through-proxy]] — 一部を置き換える。Decision 項 1（変えるのは資格情報だけ）の「本文には触れない。読みもしない」と、Decision 項 3（ストリームは届いたそばから流す）の要求の本文の部分を、`POST /v1/responses` の要求について置き換える
- [[0005-keeping-secrets-out-of-logs]] — 落とした名前の行に値を書かない

## References

- pi-ai 1.5.2（npm `@earendil-works/pi-ai`）の `dist/api/openai-responses.js` の `isChatGPTSignIn`・`buildParams`・`getConfigurationUpdateEffort`、`dist/api/openai-responses-shared.js` の `pushConfigurationUpdate`（2026-11-14 確認）
- 上流が返したエラー `subscription_sharing_unsupported_capability`（`param: input`）の本文（2026-11-14 確認）
