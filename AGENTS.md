# AGENTS.md

## 目的

このrepositoryは、公開RSS／Atomと許可された記事ページから日本語プログラミングニュースを収集し、既定では決定論的な抽出型briefingをDiscordへ投稿するGo applicationです。明示的に有効化した場合は、選定後の記事をGemini Developer APIで要約できます。

## 絶対条件

- LLM、local LLM、生成AI API、有料API、API key型news serviceを追加しない。例外は、`GEMINI_ENABLED=true`で明示的に有効化したGemini Developer APIによる選定済み記事の要約だけとする。
- 実行時通信は、設定済みfeed、許可された記事ページと`robots.txt`、Discord公式API、および明示的に有効化した場合のGemini公式`generateContent`エンドポイントに限定する。
- secret、Bot Token、credential、実在するprivate IDをsource、fixture、test、log、command引数へ記録しない。
- `.env`を自動読込しない。`.env.example`には空値または安全な既定値だけを置く。
- 記事本文をSQLite、debug log、test snapshotへ保存しない。Geminiへの入力本文とAPIの生レスポンスもログへ出力しない。
- 自動testから実サイト、Discord、DNS、その他のexternal serviceへ接続しない。`httptest`、fake、fixtureを使う。
- SSRF、DNS rebinding、redirect再検証、response size、MIME、robots、rate limitの境界を弱めない。
- Discord Message Content Intentを要求しない。Bot権限はView Channels、Send Messages、Embed Linksに留める。

## 構成

- `cmd/bot`: CLIとprocess lifecycle
- `internal/app`, `internal/brief`: orchestration、scheduler、Discord command
- `internal/httpclient`, `internal/security`: 唯一のoutbound HTTP境界
- `internal/feed`, `internal/htmlparser`: feed／HTML解析
- `internal/article`, `internal/ranking`, `internal/summarizer`: 正規化、重複排除、選定、抽出型要約と任意のGemini要約
- `internal/discord`: Discord render／publish
- `internal/storage`, `migrations`: SQLiteと監査状態
- `testdata`: 完全offline fixture

package間の責務を保ち、HTTP取得やDiscord送信を別経路で直接実装しないでください。

## 実装規約

- Go 1.25を基準にし、標準libraryを優先する。
- Go codeは`gofmt`を適用し、errorをcontext付きでwrapする。library codeでpanicしない。
- wall clock、network、publisher、resolverはtest可能な境界から注入する。
- ranking、tie-breaker、抽出型要約、nonce、fixtureは同じ入力から同じ結果になるよう維持する。fixtureではGeminiを無効にする。
- 表示原文とNFKC済み比較textを混同しない。Unicodeの切詰めはgrapheme境界とDiscordのUTF-16上限を守る。
- partial failureはfeed単位で隔離し、監査可能な状態と安全なfallbackを残す。
- migrationはappend-onlyで追加する。既存migrationを変更せず、transactionとchecksum検証を維持する。
- secretや記事本文を含み得る値をlog fieldへ追加しない。
- feed追加時はHTTPS、利用条件、MIME、format、redirect、robots、公式性、weightを確認する。技術的に取得できるだけで採用しない。

## 依存関係

新規dependencyを追加する前に、標準libraryや既存dependencyで実現できない理由を明示してください。license、release／保守状況、module graph、supply-chain risk、Windows対応、CGO要否、binary sizeを確認します。versionを固定し、`go mod tidy`と`go mod verify`を実行します。

Geminiへの通信は既存の`internal/httpclient`を使います。LLM向けSDK、telemetry SaaS、その他の外部summarization、paid news API、CGO必須SQLiteへの置換は禁止です。

## Test

変更範囲に応じて単体test、境界test、integration test、fixtureを追加します。最低限、完了前に次を実行してください。

PowerShellでは次を実行します。Unix系環境では同等の環境変数設定に読み替えます。

```powershell
$goFiles = @(git ls-files --cached --others --exclude-standard "*.go")
gofmt -w $goFiles
go mod tidy
go mod verify
go test ./...
go vet ./...
go test -race ./...
$env:CGO_ENABLED = "0"
go test ./...
go build ./cmd/bot
go run ./cmd/bot --once --dry-run --fixture ./testdata/feeds
git diff --check
```

Windowsで`-race`に必要なC compilerがない場合は、その理由を報告し、Ubuntu CIのrace jobを確認します。local sandboxでcache権限に問題がある場合は、ignoredの`.cache/go-build`を`GOCACHE`へ指定します。

Testは成功caseだけでなく、timeout、cancel、oversize、MIME不一致、unsafe URL、DNS rebinding、robots拒否、partial feed failure、Discord partial／ambiguous failure、SQLite rollback／busy、同時実行、再投稿抑止、Unicode上限を含めます。

## 完了条件

- requestされたbehaviorとpublic CLI／環境変数がREADMEに一致している。
- format、test、vet、CGOなしtest／build、offline fixtureが成功している。
- repository内にsecret、禁止API、意図しないexternal test通信がない。
- `go list -m all`と`go mod graph`で新規dependencyをreviewしている。
- `git diff --check`と全diff reviewを行い、無関係なuser変更を上書きしていない。
- 未実行の検証、既知の制約、fallback、破壊的変更を最終報告に明記している。
