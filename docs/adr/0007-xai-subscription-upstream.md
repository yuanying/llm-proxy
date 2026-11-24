# 0007. xAI のサブスクリプションは別のプロセスで中継する

- Date: 2026-11-24
- Status: Accepted

## Context

信頼できる呼び手に、X Premium のサブスクリプションで Grok の Responses API を使わせたい。たとえば組み込みの道具 `x_search` で X の投稿を検索できる。xAI の「Sign in with SuperGrok or X Premium」は、ブラウザで認可する device code flow を使い、Responses API に送る Bearer token を得る。llm-proxy は ChatGPT の認証だけを扱うため、このままでは呼び手が xAI の token の保管と refresh を引き受けることになる。本物の token を渡さず、ログインと refresh の責任を Proxy に集めたい。

## Decision

### 1. 上流と資格情報をプロセスごとに分ける

`serve` と `login` は共通のプロバイダー設定で OpenAI か xAI を選ぶ。既定は OpenAI で、既存の設定・ログイン・資格情報はそのまま使える。プロバイダーは起動時に決め、要求では切り替えない。不明なプロバイダーでは起動を拒否する。各プロバイダーは別のデータのディレクトリを使い、選択と一致しない資格情報は refresh にも転送にも使わない。使える token がなければ 503 を返す。

### 2. xAI は利用する Pod で device code flow を行う

`kubectl exec -it` で利用する Pod の中の `llm-proxy login` を動かす。端末に認可の URL と人が入力するコードだけを表示し、ブラウザから Pod に戻る受け口は作らない。認可が拒まれた場合や期限が切れた場合は token ファイルを書かない。成功したら ADR-0002 の Decision 項 3（書き換えは atomic に）と項 4（書くのは 1 つのプロセスだけ）に従い `auth.json` を置き換え、動いている `serve` が読み直す。

既定では pi が xAI のサブスクリプション認証に使う公開クライアント ID を使う。サブスクリプション用のクライアントを別途登録せず、既知の構成で始めるためである。ログイン時に設定で変更でき、refresh はログイン時の ID を使う。ID は公開の識別子であって秘密ではない。xAI がこのクライアントを制限したり認可方式を変えたりすれば、互換性のあるクライアントや方式に変更し、ログインし直す必要がある。

ADR-0002 の Decision 項 2（切れる前に refresh する）の先回りの refresh を共用する。xAI の応答が新しい refresh token を含まないときは前のものを残す。token と機械が使う device code は端末・ログ・エラーに出さない。

### 3. xAI への要求は本文を読まずに中継する

xAI の上流は既定で xAI の公開 Inference API の `/v1` とする。公式 API にある `POST /v1/responses` と `GET /v1/models` を既定の route とし、サブスクリプションで許される操作は上流の判断に任せる。上流のエラーも応答もそのまま返す。呼び手の認証と転送ヘッダの扱いは従来どおりで、`Authorization` は選択した上流の token に置き換える。

ADR-0006 の Decision 項 1（対象は `POST /v1/responses` の要求の本文だけ）の書き換えは OpenAI モードに限る。xAI では、`x_search` を含む `tools` も他の欄も、本文をバイト単位で変えず、バッファせずに流す。応答のストリーミングはどちらのモードでも変えない。

### 本 ADR で決定しないこと

- 設定名と端末の操作手順（委譲先: README の「Commands」と「Running in Kubernetes」）。
- xAI の上流が断る項目を吸収するか（委譲先: 書き換えが必要になった時点で ADR を追加する）。

## Consequences

呼び手は xAI の token を持たずに Responses API を使える。OpenAI とは別の Pod とデータのディレクトリの管理が必要になり、1 つの Proxy で両方は使えない。xAI の利用枠は同じサブスクリプションを使う呼び手と契約者が共有する。上流が断る欄は呼び手が直す。

## Alternatives Considered

- **呼び手ごとに xAI の token を持つ。** 中継は不要だが、呼び手ごとにログインと refresh の運用が必要になり、本物の token を渡す先が増える。
- **1 つの `serve` で両方の上流を選べるようにする。** Pod は減るが、プロバイダーごとの資格情報・許可する主体・データの管理を 1 プロセスに持ち込むことになる。

## Related ADRs

- Related: [ADR-0001 Responses API をそのまま通す Proxy にする](0001-pass-through-proxy.md) — 転送の原則を共用し、xAI の既定の上流を追加する。
- Related: [ADR-0002 token を 1 つのファイルに持ち、1 つのプロセスが refresh する](0002-token-storage-and-refresh.md) — 保管、読み直し、refresh の共通規則。
- Related: [ADR-0003 ログインは同じ binary のサブコマンドで、token の置き場所で行う](0003-login-inside-the-pod.md) — Pod 内での操作を共用し、xAI には別の認可方式を使う。
- Related: [ADR-0004 gated のアサーションを確かめて、直接の到達を断る](0004-refusing-direct-access.md) — 呼び手の認証は変更しない。
- Related: [ADR-0005 鍵・token をログに出さず、アクセスログは決めた欄だけにする](0005-keeping-secrets-out-of-logs.md) — 秘密を出さない規則を共用する。
- Related: [ADR-0006 応答の生成の要求から、サブスクリプションが断る欄と項目を落とす](0006-dropping-what-the-subscription-refuses.md) — 書き換えは OpenAI モードに適用する。

## References

- pi-ai（npm `@earendil-works/pi-ai`）の `dist/auth/oauth/xai.js` の `requestDeviceCode`、`pollForTokens`、`refreshXaiToken`、`credentialsFromTokenResponse`（2026-11-24 確認）。
- [xAI Docs: X Search](https://docs.x.ai/developers/tools/x-search)、[Create new response](https://docs.x.ai/developers/rest-api-reference/inference/responses)、[List models](https://docs.x.ai/developers/rest-api-reference/inference/models)（2026-11-24 確認。route と `x_search` の仕様。サブスクリプションでのモデル一覧の可否は保証しない）。
