# Discord Computer News Bot

日本語のプログラミング／コンピュータ関連ニュースを公開RSS・Atomフィードから収集し、決定論的に順位付けしてDiscordへ投稿するGo製Botです。記事本文が不足する場合だけ、`robots.txt`を尊重して公開記事ページを補助的に解析します。

このプロジェクトは、LLM、ローカルLLM、有料API、APIキーを必要とするニュースサービスを使用しません。要約は原文から文を選ぶ抽出型であり、生成・翻訳は行いません。通常運用時の外部通信先は、設定したフィード、許可された記事ページとその`robots.txt`、Discord公式APIだけです。

## 主な機能

- RSS 2.0、Atom、RSS 1.0/RDFを各フィード最大50件まで解析
- 48時間から最大7日まで候補期間を広げ、重複を除いて上位5件を選定
- 固定ルールによる監査可能なスコアリングと抽出的な日本語要約
- Discord Embedの全サイズ制限、Unicode境界、メンション無効化に対応
- `/brief now|preview|status|feeds` Slash Command
- 5フィールドcron、タイムゾーン対応、同時実行防止、graceful shutdown
- SQLiteによるrun、取得結果、スコア、投稿状態、再投稿抑止の永続化
- SSRF、DNS rebinding、危険なredirect、巨大response、MIME不一致への防御
- Discord不要のlive dry-runと、ネットワークを一切使わない固定fixture
- CGO不要のWindows／Linux単一バイナリ

## アーキテクチャ

```text
CLI / gocron / Discord Slash Command
                 |
                 v
            brief.Service
       +---------+----------+
       |         |          |
 RSS/Atom     SQLite     Discord Publisher
       |
 safe HTTP -> feed parser -> optional HTML/readability
       |                         |
       +-> normalize -> dedupe -> rank -> extractive summary
```

主なパッケージの責務は次のとおりです。

| パッケージ | 責務 |
|---|---|
| `cmd/bot` | CLI、設定読込、signal処理、JSONログ |
| `internal/app` | 実行モード、Gateway、Slash Command、scheduler、終了処理 |
| `internal/brief` | 収集から投稿までの一連の処理 |
| `internal/httpclient`, `internal/security` | HTTP制限、robots、SSRF／DNS防御 |
| `internal/feed`, `internal/htmlparser` | feed解析と補助的な本文・metadata抽出 |
| `internal/article` | URL／title正規化、完全・近似重複排除 |
| `internal/ranking`, `internal/summarizer` | 決定論的ランキングと抽出的要約 |
| `internal/discord` | command定義、Embed生成・分割、nonce付き投稿 |
| `internal/storage` | migration、監査情報、投稿状態、日次実行状態 |
| `internal/fixture` | 固定時計を含む完全オフラインfixture |

## 必要環境

- Go 1.25以降
- Windows 11、またはsystemdを利用できるLinux
- 通常モード／投稿型`--once`ではDiscord ApplicationとBot Token
- 書き込み可能なSQLite保存先
- live実行では外向きHTTPS接続。受信ポートの開放は不要

Dockerfileは初期版では提供していません。

## クイックスタート

### 1. ビルド

Windows PowerShell:

```powershell
Set-Location C:\Code\Discord_ComputerNewsBot
New-Item -ItemType Directory -Force .\bin | Out-Null
$env:CGO_ENABLED = "0"
go mod download
go mod verify
go build -trimpath -o .\bin\discord-news-bot.exe .\cmd\bot
```

Linux:

```bash
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -o bin/discord-news-bot ./cmd/bot
```

### 2. まずfixtureで確認

Discord設定やネットワーク接続なしで、固定された日時とローカルfeedだけを使います。

```powershell
.\bin\discord-news-bot.exe --once --dry-run --fixture .\testdata\feeds
```

`--fixture`は`--once`必須で、自動的にdry-runになります。外部HTTP、DNS、Discord投稿を行いません。SQLiteにはfixture runと記事metadataが監査用に記録されるため、`DATABASE_PATH`の親ディレクトリには書き込み権限が必要です。

### 3. live dry-run

実フィードを取得して結果を標準出力へ表示しますが、Discordへは投稿しません。

```powershell
.\bin\discord-news-bot.exe --once --dry-run
```

live dry-runもrun、feed取得結果、記事URL、スコアをSQLiteへ記録します。記事本文そのものとDiscordのdelivery履歴は保存しません。

### 4. Discordへ1回だけ投稿

```powershell
$secureToken = Read-Host "Discord Bot Token" -AsSecureString
$env:DISCORD_BOT_TOKEN = [Net.NetworkCredential]::new("", $secureToken).Password
$env:DISCORD_CHANNEL_ID = "123456789012345678"
.\bin\discord-news-bot.exe --once
Remove-Item Env:DISCORD_BOT_TOKEN
Remove-Variable secureToken
```

### 5. 常駐実行

```powershell
$env:DISCORD_APPLICATION_ID = "123456789012345678"
$env:DISCORD_GUILD_ID = "123456789012345678" # 開発中は推奨。global登録なら未設定
$env:DISCORD_CHANNEL_ID = "123456789012345678"
.\bin\discord-news-bot.exe
```

常駐モードはDiscord Gatewayへ接続し、既定では`Asia/Tokyo`の毎日08:00に投稿します。同時にSlash Commandを登録します。

## CLI

| コマンド | 動作 |
|---|---|
| `go run ./cmd/bot` | Discord Gatewayとschedulerを開始 |
| `go run ./cmd/bot --once` | live feedから1回生成・投稿して終了 |
| `go run ./cmd/bot --once --dry-run` | live feedから生成し、投稿せず標準出力へ表示 |
| `go run ./cmd/bot --once --dry-run --fixture ./testdata/feeds` | 固定fixtureだけで完全オフライン実行 |
| `go run ./cmd/bot --config path/to/feeds.yaml` | 別のfeed設定を使用 |

フラグ:

- `--once`: 1回実行して終了
- `--dry-run`: Discord投稿を無効化し、結果を標準出力へ表示
- `--fixture DIR`: `DIR/manifest.yaml`とローカルfeedを使用。`--once`必須、dry-runを強制
- `--config FILE`: feed YAML。既定は`config/feeds.yaml`。fixtureモードでは使用しない

終了コードは成功`0`、設定／引数の問題`2`、初期化・収集・投稿などの実行失敗`1`です。ログは本文を含まないJSONとして標準エラーへ出力されます。

## 環境変数

`.env.example`を参照できますが、Botは`.env`を自動読込しません。実行プロセスの環境、Task Scheduler、またはsystemdの`EnvironmentFile`から注入してください。Tokenをソース、設定YAML、起動引数、ログへ書かないでください。

| 変数 | 既定値 | 用途／必須条件 |
|---|---|---|
| `DISCORD_BOT_TOKEN` | なし | 秘密値。Discordへ投稿する全モードで必須 |
| `DISCORD_APPLICATION_ID` | なし | Application ID。常駐モードで必須 |
| `DISCORD_GUILD_ID` | なし | 設定時はguild command、未設定時はglobal commandを登録 |
| `DISCORD_CHANNEL_ID` | なし | 投稿先Channel ID。Discordへ投稿する全モードで必須 |
| `DISCORD_ALLOWED_USER_IDS` | なし | `/brief now`と`preview`を許可するUser IDのカンマ区切り |
| `APP_TIMEZONE` | `Asia/Tokyo` | cron、表示日時、同日判定に使うIANA timezone |
| `BRIEF_CRON` | `0 8 * * *` | 秒を含まない5フィールドcron |
| `DATABASE_PATH` | `data/bot.db` | SQLiteファイル。親ディレクトリは自動作成 |
| `LOG_LEVEL` | `info` | `debug`、`info`、`warn`、`error` |
| `HTTP_USER_AGENT` | repository URLを含むBot既定値 | feed、記事、robots取得時のUser-Agent |
| `MAX_ARTICLE_BYTES` | `2097152` | 展開・文字コード変換前に制限する記事responseの最大byte数。正の整数 |
| `ARTICLE_FETCH_ENABLED` | `true` | 情報不足記事のHTML補完を有効化 |
| `COMMAND_STATUS_PUBLIC` | `false` | `true`ならguildメンバーが`status`と`feeds`を実行可能 |

モードごとのDiscord必須値:

| モード | Token | Application ID | Channel ID |
|---|---:|---:|---:|
| 常駐 | 必須 | 必須 | 必須 |
| `--once`投稿 | 必須 | 不要 | 必須 |
| `--once --dry-run` | 不要 | 不要 | 不要 |
| fixture | 不要 | 不要 | 不要 |

boolean値はGoの`strconv.ParseBool`が受理する形式を使用してください。`APP_TIMEZONE`が不正、cronが5フィールドでない、snowflake IDが数値でない、または必須値がない場合は起動を中止します。

## Discord Applicationの設定

1. [Discord Developer Portal](https://discord.com/developers/applications)でApplicationを作成します。
2. **General Information**のApplication IDを`DISCORD_APPLICATION_ID`へ設定します。
3. **Bot**でBotを作成してTokenを取得し、`DISCORD_BOT_TOKEN`へ安全に注入します。Tokenは再表示できない場合があるため、漏えい時は即座にresetしてください。
4. Privileged Gateway Intentsは有効化しません。このBotは`IntentsNone`で動作し、**Message Content Intentは不要**です。
5. **OAuth2 / URL Generator**またはInstallation設定で`bot`と`applications.commands` scopeを選び、次のBot権限だけを付けてguildへ招待します。
   - View Channels
   - Send Messages
   - Embed Links
6. DiscordクライアントのDeveloper Modeを有効にし、投稿先を右クリックしてChannel IDをコピーし、`DISCORD_CHANNEL_ID`へ設定します。Server IDは`DISCORD_GUILD_ID`、利用者はUser IDとして同様にコピーできます。

開発中は`DISCORD_GUILD_ID`を設定してください。guild commandは対象guildへすぐ反映されやすく、動作確認が簡単です。未設定ではglobal commandとして登録され、Discord側の反映に時間がかかることがあります。Botは既存の他commandを削除せず、同名の`/brief`だけを作成または更新します。

Bot自身にAdministrator権限を与える必要はありません。下記の「Administrator」はSlash Commandを実行するユーザー側のguild権限です。

### Slash Commandと認可

すべてguild内専用で、応答は利用者だけに見えるephemeralです。

| Command | 動作 | 認可 |
|---|---|---|
| `/brief now` | その場で収集し、設定Channelへ投稿 | 許可UserまたはAdministrator |
| `/brief preview` | 収集し、投稿せずEmbedをpreview | 許可UserまたはAdministrator |
| `/brief status` | uptime、次回実行、最終run、件数、DB pathを表示 | 既定は上記と同じ。公開設定可能 |
| `/brief feeds` | 有効feedごとの最終結果・件数・時刻を表示 | 既定は上記と同じ。公開設定可能 |

`DISCORD_ALLOWED_USER_IDS`が1件以上なら、その一覧が`now`／`preview`の許可リストになります。未設定なら、実行者にguildのAdministrator権限が必要です。`COMMAND_STATUS_PUBLIC=true`の場合だけ、`status`／`feeds`は全guildメンバーへ公開されます。

定期実行、`now`、`preview`はプロセス内で排他されます。競合した手動commandは待たずに「別のブリーフィングを実行中」と応答します。`now`／`preview`はまずdeferred responseを返し、runは最大5分でtimeoutします。

## 初期フィード

`config/feeds.yaml`には次の12件を同梱しています。すべて`language: ja`で、認証やAPI keyを必要としません。

| 情報源 | weight | category | official |
|---|---:|---|---:|
| [Publickey](https://www.publickey1.jp/atom.xml) | 1.1 | `general` | no |
| [gihyo.jp](https://gihyo.jp/feed/rss2) | 1.0 | `general` | no |
| [CodeZine](https://codezine.jp/rss/new/20/index.xml) | 1.0 | `general` | no |
| [GitHub Blog 日本語](https://github.blog/jp/feed/) | 1.2 | `platform` | yes |
| [AtCoderInfo](https://info.atcoder.jp/feed) | 1.1 | `competitive_programming` | yes |
| [IPA 重要なセキュリティ情報](https://www.ipa.go.jp/security/alert-rss.rdf) | 1.3 | `security` | yes |
| [JVN](https://jvn.jp/rss/jvn.rdf) | 1.2 | `security` | yes |
| [JPCERT/CC](https://www.jpcert.or.jp/rss/jpcert.rdf) | 1.3 | `security` | yes |
| [Hatena Developer Blog](https://developer.hatenastaff.com/feed) | 1.0 | `engineering` | yes |
| [LINEヤフー Tech Blog](https://techblog.lycorp.co.jp/ja/feed/index.xml) | 1.0 | `engineering` | yes |
| [CyberAgent Developers Blog](https://developers.cyberagent.co.jp/blog/feed/) | 1.0 | `engineering` | yes |
| [DeNA Engineering](https://engineering.dena.com/index.xml) | 1.0 | `engineering` | yes |

### フィードの追加・保守

各entryは次の形式です。

```yaml
feeds:
  - name: Example Official Blog
    url: https://example.com/feed.xml
    enabled: true
    weight: 1.0
    language: ja
    category: engineering
    is_official: true
```

- `name`、`url`、`language`、`category`は空にできません。
- `weight`は`0`より大きく`5`以下です。通常は`1.0`を基準にし、恣意的に大きくしないでください。
- nameとURLの重複は大文字小文字を無視して拒否されます。未知のYAML fieldも拒否されます。
- 実装は公開HTTP(S) URLを受理しますが、運用上はHTTPSを使用してください。credentialを含むURLは拒否されます。
- `is_official: true`は製品・組織自身が発行する一次情報だけに付けます。
- 追加前にRSS／Atom／RDFとして解析できること、MIME type、redirect先、更新頻度、転載・商用利用条件、robots.txt、運営者の利用規約を確認してください。
- feed URLや利用条件は将来変わり得ます。`/brief feeds`とJSONログを定期確認し、失効したfeedは`enabled: false`にしてください。

ITmediaは技術的に取得可能でも、[ITmedia RSSフィード利用条件](https://corp.itmedia.co.jp/media/rss_condition/)がアプリケーションへの組込みや抜粋利用を制限しているため、初期設定から除外しています。利用条件を回避するために別URLやscrapingを追加しないでください。他媒体も、技術的に取得できることと利用許諾があることを混同しないでください。

## 収集と安全性

全live HTTPは1つの再利用Transportを通ります。

- connect timeout 5秒、response header timeout 10秒、request全体20秒
- redirect最大5回。各redirect先を再検証
- 同時実行は全体4、hostごと1、同一hostへの開始間隔は最低1秒
- feed 2 MiB、記事は`MAX_ARTICLE_BYTES`、robots.txt 512 KiB。圧縮展開後に超過を検出
- RSS／Atom／RDFのXML rootとMIME、記事HTML／XHTMLのMIMEを検証
- 宣言された文字コードを`golang.org/x/net/html/charset`でUTF-8へ変換
- `http`／`https`以外、userinfo、localhost、private／loopback／link-local／multicast／unspecified／予約IPを拒否
- DNS結果をURL検証時と接続直前に確認し、DNS rebindingやpublic/private混在回答を拒否
- `HTTP_PROXY`、`HTTPS_PROXY`などの環境proxyを意図的に継承しない

記事ページ取得前にoriginごとの`robots.txt`を確認し、結果を1時間cacheします。4xxは「規則なし」として許可し、5xx、通信失敗、不正なrobots、Disallowは記事HTMLを取得しません。`Crawl-delay`が5秒を超える場合も取得せず、feed本文へfallbackします。許可されたdelayはhostの最短間隔へ反映します。

feedごとの失敗はDBとwarningログへ残し、他feedの処理は継続します。有効feedがすべて失敗した場合だけrun全体を失敗にします。

## 記事処理、重複排除、ランキング

### 解析と正規化

feedからGUID、title、URL、description／content、published／updatedを取得します。本文が160 grapheme未満の候補について、事前ランキング上位20件まで記事HTMLを補完します。readability本文に加え、canonical、Open Graph、JSON-LD `Article`／`NewsArticle`、著者、site名、画像、日時、関連linkを解析し、失敗時はfeed本文へ戻ります。

URLはscheme／host小文字化、default port・fragment・末尾slashの除去、queryの安定sort、`utm_*`と`ref`の除去を行います。title比較keyはNFKC、小文字化、空白・句読点整理、既知媒体suffix除去で作ります。表示用の原文は別に保持します。

完全重複はGUID、canonical URL、正規化URL、正規化titleのいずれかで統合し、公式性、feed weight、日時、URL、sourceの順で代表を決めます。近似重複はrune bigramのSørensen–Dice係数を使い、同一domainかつ72時間以内なら`0.78`以上、別domainかつ48時間以内なら`0.90`以上を統合します。異なるCVEやversion識別子を含むtitleは別記事として保護し、ranking上位側を残します。

### 候補期間とスコア

公開日時、なければ更新日時を使います。日時なし、現在より15分を超えて未来の記事は選外です。まず48時間で選び、5件に満たない場合は72、120、168時間まで広げます。それでも不足する場合は取得できた件数だけ投稿します。

スコアは次の固定式です。

| 要素 | 点数 |
|---|---:|
| feed | `round(weight × 10)` |
| freshness | `max(0, 28 - floor(ageHours / 6))` |
| official | `+12` |
| 主要技術分野 | 一致groupごと`+2`、最大`+16` |
| security、release、breaking/API変更、EOL、compiler/runtime | 一致groupごと`+6`、最大`+24` |
| 国内・AtCoder・学生・contest関連 | `+8` |
| 本文・日時・URLの充足度 | 最大`+8` |
| 求人 | `-30` |
| 広告／affiliate | `-25` |
| 宣伝だけのseminar | `-18` |
| 煽り見出し | `-12` |
| 本文不足 | `-15` |
| 弱い技術関連性 | `-25` |

日本語以外に設定されたfeedの記事は、主要技術group、security、または日本・学生関連語のいずれかに一致した場合だけ候補になります。同点はscore、日時、official、正規化title、正規化URL、sourceの順で常に同じ結果になります。内訳と一致語はSQLiteとdebugログへ保存しますが、本文は保存・debug出力しません。

## 抽出的要約

本文を句点、疑問符、感嘆符、改行などで文分割し、15〜220 graphemeの文を候補にします。titleとのbigram一致、技術語、CVE／version／APIなどの識別子、冒頭位置で順位付けし、広告定型句、URLだけの文、重複文を除きます。

- 「要点」は上位1〜2文を元の順序へ戻して表示
- 「技術的背景」と「開発者・学生への影響」は専用の固定語彙で別の文を優先
- 「次に確認すべき点」は公式document、release note、repository、migration、changelog、issue、PR、advisoryへのlinkを固定優先順位で選択
- 適切な文やlinkがない場合は、事実を生成せず定型文を表示
- 1記事の表示textは合計600 grapheme以下。要点280、背景130、影響130、次の確認60以下

これは意味理解やfact checkを行う要約器ではありません。原文の品質、feedのdescription、HTML抽出精度に左右され、文脈を十分に表せない場合があります。重要な判断では必ず元記事と一次資料を確認してください。

## Discord投稿と再投稿規則

各記事を1 Embed、最後に「今日の最優先トピック」を1 Embedとして作ります。Discordの10 embeds／message、合計6000 UTF-16 units、title 256、description 4096、field 25件・各1024などを一箇所で検証し、grapheme境界を壊さず複数messageへ分割します。画像はpublic HTTPS URLだけ、記事linkはpublic HTTP(S)だけを使い、`allowed_mentions`を空にします。

各message batchは送信前にSQLiteへ予約し、24文字の決定論的nonceとDiscordの`enforce_nonce`を使用します。送信成功時だけmessage IDとdeliveryを`sent`にします。timeoutや5xxなど送信成否が不明な場合は`unknown`にして自動再送せず、重複投稿を避けます。Channelを目視確認してから手動対応してください。

再投稿抑止はchannel単位です。

- 定期便: 同じchannelへ過去に`sent`、`sending`、`unknown`となった記事を再投稿しない
- `/brief now`と投稿型`--once`: 同じchannel・同じ`APP_TIMEZONE`日付のdeliveryだけを除外。前日以前の記事は7日窓の範囲で再選定可能
- `/brief preview`とdry-run: delivery履歴を作らない。ただしChannel IDがあるpreviewは当日の既存deliveryを候補から除外
- 定期便: `daily_briefs(channel, local_date)`により1暦日1回だけclaim。再起動や別processから同日二重実行しない

## SQLiteと監査情報

`DATABASE_PATH`の親directoryを作成し、埋込みmigrationをchecksum検証付きtransactionで適用します。SQLiteはWAL、foreign keys、busy timeout 5秒、接続数1で使用します。

保存するtable:

- `articles`: GUID、title、URL、日時、source、scoreと内訳。記事本文は保存しない
- `runs`: 種別、開始・終了、結果、取得／選定／投稿件数、error
- `feed_fetches`: feedごとの取得結果、件数、error
- `delivery_batches`: nonce、送信状態、Discord message ID
- `deliveries`: channel・記事・日付ごとの投稿状態
- `daily_briefs`: 定期便の日次排他状態
- `schema_migrations`: 適用済みmigrationとchecksum

時刻はDB内ではUTC、日次keyは`APP_TIMEZONE`の現地日付です。単一host・単一常駐processを推奨します。DBは監査・重複防止に必要なので、削除すると投稿履歴も失われます。

## Windows 11での常駐運用

### 起動scriptとログ

同梱の`scripts/run-bot.ps1`はrepository rootをworking directoryに設定し、`bin/discord-news-bot.exe`を起動します。秘密値は読まず、設定せず、呼出processの環境だけを継承します。標準出力と標準errorは`logs/bot-YYYY-MM-DD.log`へ追記します。

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\run-bot.ps1
```

1回の確認にscriptを使う場合:

```powershell
.\scripts\run-bot.ps1 -BotArguments @("--once", "--dry-run")
```

ログ確認:

```powershell
Get-Content .\logs\bot-$(Get-Date -Format yyyy-MM-dd).log -Wait
```

停止はconsole実行なら`Ctrl+C`、Task Schedulerなら「終了」を使います。processを強制終了せず、SIGINT／SIGTERM相当の終了を待つことでscheduler、実行中run、Discord、SQLiteを順に閉じます。1 runの上限は5分です。

PCがsleep中はcronは動かず、復帰後のcatch-upも行いません。08:00投稿を保証するには、その時刻にPCを起動しておき、Windowsの電源設定とTask Schedulerの「スリープを解除して実行」を運用要件に応じて設定してください。

### Windows Defender Firewall

通常の「outbound許可／inbound拒否」構成なら追加ruleは不要です。このBotは受信portをlistenしません。outboundを既定拒否にしている端末だけ、管理者PowerShellでbuild済みbinaryのTCP 443を許可します。

```powershell
$exe = (Resolve-Path .\bin\discord-news-bot.exe).Path
New-NetFirewallRule -DisplayName "Discord Computer News Bot HTTPS" `
  -Direction Outbound -Program $exe -Action Allow -Protocol TCP -RemotePort 443 `
  -Profile Domain,Private,Public
```

inbound ruleは作成しないでください。Botは環境proxyを使用しないため、proxy必須networkでは直接接続を許可するか、運用network設計を見直す必要があります。

### Task Schedulerで起動・再起動

1. Bot専用の通常権限Windows accountを用意し、そのaccountのUser環境変数へDiscord設定を登録します。TokenをscriptやTask引数へ書かないでください。
2. 環境変数の変更後は、対象accountでsign out／sign inして新しいprocessへ反映します。
3. 管理者PowerShellで次を実行し、起動時trigger、1分間隔の自動再起動、repository rootのworking directoryを設定します。credentialはpromptから渡され、scriptには保存されません。

```powershell
$repo = (Resolve-Path C:\Code\Discord_ComputerNewsBot).Path
$powershell = "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe"
$arguments = "-NoProfile -ExecutionPolicy Bypass -File `"$repo\scripts\run-bot.ps1`""
$action = New-ScheduledTaskAction -Execute $powershell -Argument $arguments -WorkingDirectory $repo
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet `
  -RestartCount 10 -RestartInterval (New-TimeSpan -Minutes 1) `
  -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew `
  -StartWhenAvailable -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
$credential = Get-Credential -Message "Botを実行するWindows account"
Register-ScheduledTask -TaskName "DiscordComputerNewsBot" `
  -Action $action -Trigger $trigger -Settings $settings `
  -Description "Japanese programming news Discord bot" `
  -User $credential.UserName -Password $credential.GetNetworkCredential().Password
Remove-Variable credential
```

Task SchedulerのHistory、`Last Run Result`、および`logs`を確認してください。binaryを更新したらTaskを終了し、build、fixture smoke test、Task開始の順で行います。Taskを「ユーザーがログオンしているときのみ実行」に変更すると、startup時に未ログオンでは起動しません。

## Linux systemd

`deploy/discord-news-bot.service`を環境に合わせて配置できます。例:

```bash
sudo useradd --system --home /opt/discord-news-bot --shell /usr/sbin/nologin discord-news-bot
sudo install -d -o root -g root /opt/discord-news-bot/{bin,config}
sudo install -m 0755 bin/discord-news-bot /opt/discord-news-bot/bin/discord-news-bot
sudo install -m 0644 config/feeds.yaml /opt/discord-news-bot/config/feeds.yaml
sudo install -m 0644 deploy/discord-news-bot.service /etc/systemd/system/discord-news-bot.service
sudoedit /etc/discord-news-bot.env
sudo chmod 600 /etc/discord-news-bot.env
sudo systemctl daemon-reload
sudo systemctl enable --now discord-news-bot.service
```

`/etc/discord-news-bot.env`は`export`を付けずに記載します。

```dotenv
DISCORD_BOT_TOKEN=replace-in-secret-file
DISCORD_APPLICATION_ID=123456789012345678
DISCORD_GUILD_ID=123456789012345678
DISCORD_CHANNEL_ID=123456789012345678
APP_TIMEZONE=Asia/Tokyo
BRIEF_CRON=0 8 * * *
DATABASE_PATH=/var/lib/discord-news-bot/bot.db
```

運用command:

```bash
sudo systemctl status discord-news-bot.service
sudo journalctl -u discord-news-bot.service -f
sudo systemctl restart discord-news-bot.service
sudo systemctl stop discord-news-bot.service
```

unitは専用User、`NoNewPrivileges`、read-only system領域、専用state directoryを使用し、失敗時だけ再起動します。停止timeoutは実行中runの5分上限より長く設定しています。

## 開発と検証

```powershell
$env:GOCACHE = Join-Path $PWD ".cache\go-build"
$goFiles = @(git ls-files --cached --others --exclude-standard "*.go")
gofmt -w $goFiles
go mod tidy
go mod verify
go test ./...
go vet ./...
go test -race ./...
$env:CGO_ENABLED = "0"
go test ./...
go build -trimpath -o .\bin\discord-news-bot.exe .\cmd\bot
.\bin\discord-news-bot.exe --once --dry-run --fixture .\testdata\feeds
git diff --check
```

Windowsの`-race`はC compilerがない環境では失敗します。GitHub ActionsのUbuntu jobでrace testを実行します。testは`httptest`とfixtureを使用し、実サイトやDiscordへ接続してはいけません。

主要な直接依存:

| 依存 | 用途 |
|---|---|
| `discordgo v0.29.0` | Gateway、Slash Command、interaction、Discord型 |
| `gofeed v1.3.0` | RSS／Atom／RDF parser。URL取得APIは使用しない |
| `go-readability/v2 v2.1.2` | Reader入力から記事本文を抽出 |
| `modernc.org/sqlite v1.53.0` | pure Go SQLite。CGO不要 |
| `gocron/v2 v2.22.0` | 5フィールドcron、timezone、singleton job |
| `go.yaml.in/yaml/v3` | strict YAML decode |
| `golang.org/x/net`, `golang.org/x/text` | charset処理とUnicode NFKC |
| `clipperhouse/uax29/v2` | grapheme cluster境界 |

robots.txt parser、migration、CLI、logging、JSON、HTTP policy、nonce生成には標準libraryとrepository内コードを使用しています。新しい依存を追加する場合は、必要性、license、保守状況、CGO／Windowsへの影響を説明してください。

## トラブルシューティング

### `configuration error`

- 常駐はToken、Application ID、Channel ID、投稿型`--once`はTokenとChannel IDが必要です。
- IDは空白なしの数字です。許可Userはカンマ区切りです。
- `BRIEF_CRON`は5フィールドです。秒fieldを追加しないでください。
- `.env`は自動読込されません。実行processに環境変数が見えるか確認してください。

### `/brief`が表示されない

- invite scopeに`applications.commands`が含まれるか確認します。
- `DISCORD_APPLICATION_ID`とBot Tokenが同じApplicationのものか確認します。
- 開発中は正しい`DISCORD_GUILD_ID`を設定して再起動します。global commandは反映が遅れる場合があります。
- Gateway接続とcommand登録のerrorをJSONログで確認します。

### commandは見えるが実行できない

- `DISCORD_ALLOWED_USER_IDS`設定時はAdministratorでも一覧外なら拒否されます。
- 未設定時の`now`／`preview`は実行者にAdministratorが必要です。
- `status`／`feeds`を一般公開する場合は`COMMAND_STATUS_PUBLIC=true`です。
- commandはguild context専用で、DMでは使用できません。

### 投稿されない／件数が少ない

- `/brief feeds`、`/brief status`、warningログを確認します。
- feedのMIME、XML root、DNS、利用停止、Firewall、robots.txtを確認します。
- 日時なし、15分超未来、最大7日より古い、技術関連性がない、重複、すでに投稿済みの記事は除外されます。
- 定期便は同じchannelの過去記事を再投稿しません。同日定期便も1回だけです。
- `ARTICLE_FETCH_ENABLED=false`、robots拒否、本文取得失敗でもfeed本文で継続しますが、要約品質は下がることがあります。

### Discord timeout後にchannelへ投稿済みか不明

DB上は`unknown`となり、自動再送しません。まず対象Channelを確認してください。確認せずDBを編集したり即時再実行すると重複する可能性があります。

### SQLiteを開けない／lockedになる

- `DATABASE_PATH`の親directoryと実行accountの権限を確認します。
- 同じDBを多数processから共有しないでください。通常は常駐processを1つだけ起動します。
- network shareではなくlocal diskを使います。
- backupや移動はBotをgraceful stopしてから行い、DBと残存する`-wal`／`-shm`を一体として扱ってください。

### corporate proxy環境で取得できない

SSRF境界を一貫させるため環境proxyを無効にしています。`HTTP_PROXY`を設定しても使用しません。直接HTTPS接続を許可できない環境では、そのままではlive収集できません。fixtureは利用できます。

## 制約と運用上の注意

- feedや記事の正確性、可用性、利用条件を保証しません。媒体側の規約変更を継続確認してください。
- 抽出的要約は生成AIではなく、誤った原文を訂正せず、翻訳もしません。
- HTML構造変更、短いfeed本文、robots制限により定型fallbackが増える場合があります。
- schedulerはprocessが起動中のときだけ動作し、sleep中のmissed runを補完しません。
- SQLiteに自動retentionはなく、長期運用では監査DBとlogの容量を監視してください。
- 複数hostにまたがるdistributed schedulerは想定していません。
- Discord、feed、記事ページ以外のAPI、LLM、課金serviceへのfallbackはありません。

## ライセンスと利用条件

repositoryのlicense、および各依存libraryのlicenseに従ってください。ニュース本文・画像・feedの権利は各提供者に帰属します。本Botは短い抽出文と元記事linkを扱いますが、それだけで任意の利用方法が許諾されるとは限りません。公開・組込み・商用運用前に、運用者自身が各情報源の最新条件を確認してください。
