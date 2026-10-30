# 0002. token を 1 つのファイルに持ち、1 つのプロセスが refresh する

- Date: 2026-10-30
- Status: Accepted

## Context

「Sign in with ChatGPT」の OAuth は、access token と refresh token と期限を返す。access token は期限が来ると使えなくなり、refresh token で新しい組を取り直す。refresh token は 1 回きりで、使うと新しい refresh token が返り、古いものは使えなくなる。

このため、同じ refresh token を 2 か所が持つと、先に refresh した方だけが生き残る。後から refresh した方は失敗し、ログインし直すしかなくなる。Pod の再起動をまたいで token を残すには、ファイルに書く必要もある。書いている途中でプロセスが落ちて、ファイルが半分だけになれば、token はすべて失われる。

## Decision

### データのディレクトリに 1 つのファイル

token はデータのディレクトリ（設定で指定する。既定は無い）の `auth.json` に置く。

| 項目 | 中身 |
|---|---|
| access token | 上流へ付ける Bearer の値 |
| refresh token | 次の refresh に使う値 |
| 期限 | access token の期限の時刻。上流が返した有効期間から計算した、本当の期限 |
| クライアント ID | ログインのときに発行されたクライアント ID（ADR 0003）。refresh に要る |
| scope | 認められた scope の一覧 |

ファイルの権限は 0600、ディレクトリは 0700 で作る。ログインのときに使う端末の ID（ADR 0003）は、同じディレクトリの `device-id` に別に置く。token のファイルを消してログインし直しても、端末の ID は変わらない。

### 切れる前に refresh する

access token の残りが「refresh の余裕」（既定 10 分、設定で変えられる）を切ったら refresh する。確かめる時機は 2 つある。

- 要求を転送する前。残りが余裕を切っていれば、その場で refresh してから転送する。
- 1 分ごとの見回り。要求が来ない間にも期限は近づくので、最初の要求が refresh を待たされないようにする。

refresh は同時に 1 つしか走らせない。refresh の最中に来た要求は、その結果を待つ。refresh token を同じプロセスの中で 2 回使わないためである。

refresh に失敗したとき（上流が断った、届かなかった）は、30 秒は次の refresh を試さない。その間も、今の access token の期限がまだ来ていなければ、それで転送を続ける。期限が来ていれば 503 を返す（ADR 0001）。上流が refresh token を断った場合は、ログインし直すしかないことをログに出す。

### 書き換えは atomic に

ファイルは、同じディレクトリに一時ファイルを作り、0600 にして中身を書き、fsync し、`auth.json` へ rename し、ディレクトリを fsync する、という順で書き換える。rename は同じファイルシステムの中では atomic なので、読む側に見えるのは古い中身か新しい中身のどちらかだけである。一時ファイルを同じディレクトリに作るのは、別のファイルシステムをまたぐと rename が atomic でなくなるからである。

refresh に成功したのに書き込みに失敗したときは、新しい組をメモリに持ったまま使い続け、次の見回りで書き直す。古い refresh token はもう使えないので、書けないまま落ちるとログインし直すことになる。それはログに出す。

### 書くのは 1 つのプロセスだけ

refresh してファイルを書くのは `llm-proxy serve` のプロセス 1 つだけである。`serve` は起動するとデータのディレクトリのロックファイルに排他ロックを取り、取れなければ起動を拒否する。同じディレクトリを 2 つの `serve` が使うと、片方が refresh token を使った時点でもう片方が切れるからである。Kubernetes では replicas を 1 にし、更新は古い Pod を止めてから新しい Pod を作る形（Recreate）にする。

例外は `llm-proxy login`（ADR 0003）で、ログインの結果を同じファイルに書く。ログインは人が意図して行う操作であり、書くのは結果の 1 回だけである。`serve` は refresh token を使う前と要求のたびにファイルの更新を確かめ、変わっていれば読み直す。ログインの後に `serve` を作り直す必要は無い。

### 退けた案

**Kubernetes の Secret に書く。** Pod の外にも残り、バックアップの仕組みにも乗る。しかし Secret に書くには API サーバへの書き込みの権限が要り、Proxy が Kubernetes の client を持つことになる。refresh のたびに Secret が書き換わるので、マニフェストを GitOps で管理していると差分の扱いも難しい。ファイルなら、置き場所（PVC など）は配置の側が決められる。

**ファイルを書かず、メモリだけに持つ。** 漏れにくいが、再起動のたびにログインし直すことになる。

## Consequences

token は 1 か所にしか無く、refresh token を使うのは 1 つのプロセスだけである。複数の呼び手が同じアカウントを使っても、互いの token を切らない。

`serve` は 1 つしか動かせない。`serve` が落ちている間は、すべての呼び手がモデルを使えない。

データのディレクトリを失えば、ログインし直す。token のファイルは平文であり、ディレクトリを読める者は token を読める。

## 関連 ADR

- [[0001-pass-through-proxy]] — token が無いときに返すもの
- [[0003-login-inside-the-pod]] — 最初の token をどう得るか
- [[0005-keeping-secrets-out-of-logs]] — token をログに出さないこと
