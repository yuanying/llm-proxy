# 0003. ログインは同じ binary のサブコマンドで、token の置き場所で行う

- Date: 2026-10-30
- Status: Accepted

## Context

最初の token は、人がブラウザで ChatGPT にログインし、認可して得る。「Sign in with ChatGPT」は公開クライアントの OAuth（client secret を持たない、PKCE を使う authorization code の流れ）で、OpenAI の SDK を使うエージェントのいくつかが既に実装している。その流れには次の特徴がある。

- ログインのたびに新しいクライアントが登録される。認可の要求には決まったクライアント ID（動的な登録を表す値）を付け、発行されたクライアント ID は認可の戻りのクエリで返る。token の取得と refresh には、発行された ID を使う。
- 認可の要求には、インストールを表す ID（`urn:uuid:` の形）と、エージェントの名前の手がかりを付ける。
- 戻り先は `http://127.0.0.1:1455/auth/callback` に決まっている。
- scope に `chatgpt.tokens.use.direct` を含め、resource に `https://api.openai.com/v1` を指定する。返った scope にこれが無ければ、その token は API へ直接は使えない。

token は、それを使うプロセスのところにしか置きたくない（ADR 0002）。手元でログインしてファイルを運ぶと、手元にも token が残り、運ぶ途中にも置き場所ができる。一方、Proxy はクラスタの中の Pod で動き、Pod の 127.0.0.1 には人のブラウザから届かない。

## Decision

### `llm-proxy login` を Pod の中で動かす

ログインは、Proxy と同じ binary のサブコマンド `llm-proxy login` で行う。Proxy が動いている Pod に `kubectl exec -it` で入り、同じデータのディレクトリを指定して動かす。結果は ADR 0002 の `auth.json` に atomic に書く。動いている `serve` はファイルの更新に気付いて読み直すので、Pod を作り直さなくてよい。

流れは次のとおりである。

1. データのディレクトリの `device-id` を読む。無ければ UUID を作って書く。
2. PKCE の verifier と challenge（S256）、state、nonce を作る。
3. 認可の URL を組み立てて端末に表示する。クライアント ID は動的な登録を表す値、エージェントの名前の手がかりは既定で `llm-proxy`（設定で変えられる）、インストールの ID は `urn:uuid:<device-id>`。
4. 戻りを 2 つの道で待ち、先に届いた方を使う。
   - 端末に貼られた、ブラウザのアドレス欄の URL。
   - `127.0.0.1:1455` で待つ戻りの受け口。ポートが使われていれば、貼る道だけで待つ。
5. 戻りの URL の state が一致すること、認可のコードと発行されたクライアント ID があることを確かめる。`error` があれば、その値を出して終わる。
6. コードと verifier を token に交換する。返った値に ID token があること、access token・refresh token・有効期間があること、scope に `chatgpt.tokens.use.direct` があることを確かめる。
7. `auth.json` を書く。

全体の待ち時間は 10 分で、Ctrl-C でも止められる。

### 人に戻りを渡してもらう 2 つの道

ブラウザは認可のあと `http://127.0.0.1:1455/auth/callback?...` へ移ろうとする。人の手元には何も待っていないので、ページは開けない。それでもアドレス欄には、コードと state とクライアント ID を含んだ URL が残る。

| 道 | 人がすること | 向いているとき |
|---|---|---|
| 貼る | 開けなかったページのアドレス欄の URL を全部コピーし、`login` の端末に貼る | いつでも。追加の準備が要らない |
| port-forward | ログインの前に、別の端末で Pod の 1455 番を手元の 1455 番へ `kubectl port-forward` しておく | ブラウザが戻りを自動で届けられる。貼る手間が無い |

`login` は戻りの受け口を Pod の 127.0.0.1 に開くので、port-forward はそこへ届く。Pod の外のアドレスには開かない。認可のコードは PKCE の verifier が無ければ token に替えられないが、それでも受け口を他の Pod から叩ける形にはしない。

### 退けた案

**device code の流れ。** URL とコードを表示して、人が別の端末で入力する形で、戻り先が要らない。Pod の中のログインには最も向いている。しかし「Sign in with ChatGPT」の資料と既存の実装は、authorization code の流れしか示していない。使えると確かめられたら、そのときに足す。

**手元でログインし、ファイルを Pod へ運ぶ。** 手元のブラウザがそのまま戻りを受けられる。しかし手元にも token が残り、運ぶ手段（Secret、`kubectl cp`）にも残る。同じ refresh token を 2 か所が持つ状態を、運用の手順で避けることになる。

**`serve` がログインの画面を持つ。** ブラウザから Proxy の URL を開いてログインする。人の操作は最も楽だが、Proxy の入口に、token を書き換えられるページが増える。呼び手のための入口と、管理のための入口が同じになる。

## Consequences

token は、それを使う Pod のデータのディレクトリにしか書かれない。ログインする人は、Pod に `exec` できる権限を持っている必要がある。

貼る道では、人がアドレス欄の URL を正しくコピーする必要がある。URL の一部だけを貼れば、state の不一致やコードの欠落で失敗し、最初からやり直す。

OpenAI がこの流れ（動的なクライアントの登録、戻り先、scope）を変えれば、`login` も変える必要がある。token の形が変わらない限り、`serve` は影響を受けない。

## 関連 ADR

- [[0002-token-storage-and-refresh]] — 書いたファイルを `serve` がどう読み直すか
- [[0005-keeping-secrets-out-of-logs]] — ログインの途中の値を端末とログに出さないこと
