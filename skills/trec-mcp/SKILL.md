---
name: trec-mcp
description: 使用內建 `trec mcp` 操作 trec 的錄影、播放、轉錄、註解、HTML/HTTP 服務及持續終端 session。當 agent 的 shell 工具每次呼叫都是獨立行程、無法安全保留 `trec drive --interactive` stdin，或需要以 MCP 操作任何 trec 功能時必須使用。
---

# Trec MCP

啟動 `trec mcp` 作為 stdio MCP server。它的 stdout 是 JSON-RPC 專用；不要直接對 server 的 stdin/stdout 寫入非 MCP 資料。

## 選工具

- `run`：一次性操作。用於 `transcript`、`annotate`、`html`、短暫 `serve`、非互動 `play`、`record`，以及 `drive --script`。設定 timeout 時會先送 SIGTERM、保留 2 秒收尾，再視需要 SIGKILL；結果包含 `timed_out` 與 `forced_kill`。
- `cast_verify`：批次驗證 cast/result 完整性、exit status 與 secret scan；接受 cast 路徑或目錄。
- `terminal_start`：啟動需要持續 stdin 的命令；回傳 `session_id`。
- `terminal_write`：以同一 `session_id` 寫入按鍵或一行 `drive --interactive` DSL。
- `terminal_read`：讀取累積的 stdout、stderr、是否仍在執行與 exit code。每次讀取會取走目前累積輸出。
- `terminal_close`：明確結束 session；工作完成後必須呼叫，避免遺留子程序。
- `session_list`：盤點尚未關閉的 session。

兩個啟動工具都接受 `working_directory`。`terminal_start` 另接受 `cols`、`rows`，預設
120×40；需要重現固定版面或避免 TUI 折行時明確指定，兩者皆須介於 1–1000。錄影檔以
trec 命令的 `-o <path>` 指定；相對路徑以 `working_directory` 為準，絕對路徑則直接寫到
指定位置。每次錄影都明確傳 `-o`，不要依賴預設的時間戳檔名。

每次由 `record`、`drive` 或帶 `record_file` 的 `terminal_start` 產生的 cast 都有同名 `.result.json`。錄影開始時 status 是 `in_progress`，完整收尾後才改成最終狀態。錄影程序結束後呼叫 `cast_verify`；不要只因 MCP process 已停止就宣告工作成功。digest 不符、檔案大小不符、摘要不存在／仍在進行，或 `scan.safe_to_share=false` 時，視為不可用的錄影。使用 `record` 時，trec 會傳遞子程序的非零 exit code。

## Stateful TUI

對 wizard、`trec drive --interactive`、`trec record`、`trec play --tui` 與長駐 `serve`，一律使用 `terminal_start`，禁止 FIFO、背景 shell、heredoc 或每頁重開程序。

對 `trec drive --interactive`：

1. 用 `terminal_start` 啟動命令並保留 `session_id`。
2. 用 `terminal_write` 送一條 DSL 指令，加上換行。
3. 用 `terminal_read` 取得其 `OK|ERR`、`CURSOR` 與固定行數的 `SCREEN` 回覆，再決定下一步。
4. 同一個有狀態流程必須重用同一 session 與同一 cast。

長跑工作不以 `EXPECT_QUIET` 判定完成。腳本模式先執行 `trec drive lint --strict`，用 `ENTER_IF`／`CHOOSE` 綁定畫面條件，再以明確上限的 `WAIT_CHILD_EXIT@<ms>`、`ASSERT_EXIT 0` 收尾。`run.timeout_seconds` 必須大於 drive 內層最長 timeout 加 2 秒；互動模式持續以 `terminal_read` 檢查 session 的 `running` 與 `exit_code`。

MCP stdio transport 結束時，server 會關閉所有尚存 session，將 result 寫成 `aborted` 並加入 `SESSION_END`；不得把 transport 關閉當作成功。result 的 script provenance、last_step 與 updated_at 可用來診斷中斷位置。

## 安全

`run` 與 `terminal_start` 可執行任意本機命令，僅接受使用者已授權的 command。不要把秘密放在 MCP tool arguments 或 command argv。已知秘密必須以 `--secret-env` 或 `--secret-file` 宣告，讓 trec 在寫入 header、output、marker 與 input event 前遮蔽精確值；它不會猜測未宣告的密碼。需要把秘密送進提示式 TUI 時，使用 `drive` 的 `TEXT_ENV` 或 `TEXT_FILE`，不要用手動 `record` 鍵入，因為一個值可能分散在多個 input event。`html` 和 `serve` 的 keystroke overlay 僅呈現已寫入 cast 的 input event，分享前必須檢查錄影沒有未遮蔽的輸入。

在把 cast 交給其他人、HTML 或 HTTP server 前，以 trec 的 scan 功能檢查。scan 命中時視為阻擋性安全問題：重新以已宣告的 exact-value redaction 錄製，並保留原檔於受控位置供事件處理；不得將未驗證的錄影公開。

## 實作驗證紀錄（2026-07-18）

以下輸出為公開版本；Go warning 中的本機 home path 已以 `<redacted-home>` 遮蔽，原始輸出保留於本次工作 session。

新版 timeout、transport cleanup 與 verify 診斷的實際測試輸出收錄於配套的 `trec-tui-drive` 技能「實作驗證紀錄」，使用相同的 2026-07-18 focused test command 與結果。

```text
$ go test ./... -count=1 -run '^(TestMCPRecordingFinalizeAndTruncate|TestMCPCastVerifyReturnsStructuredFailure|TestVerify.*|TestRecordRefusesOverwriteUnlessForced|TestBuildMetadataDisplayVersion)$' -v
warning: both GOPATH and GOROOT are the same directory (<redacted-home>/go); see https://go.dev/wiki/InstallTroubleshooting
=== RUN   TestMCPRecordingFinalizeAndTruncate
--- PASS: TestMCPRecordingFinalizeAndTruncate (0.26s)
=== RUN   TestMCPCastVerifyReturnsStructuredFailure
--- PASS: TestMCPCastVerifyReturnsStructuredFailure (0.00s)
=== RUN   TestRecordRefusesOverwriteUnlessForced
--- PASS: TestRecordRefusesOverwriteUnlessForced (0.62s)
=== RUN   TestVerifyPathsAcceptsValidCastAndDirectory
--- PASS: TestVerifyPathsAcceptsValidCastAndDirectory (0.00s)
=== RUN   TestVerifyCastRejectsMissingStaleAndUnsafeResults
=== RUN   TestVerifyCastRejectsMissingStaleAndUnsafeResults/missing_result
=== RUN   TestVerifyCastRejectsMissingStaleAndUnsafeResults/stale_result
=== RUN   TestVerifyCastRejectsMissingStaleAndUnsafeResults/secret_finding
--- PASS: TestVerifyCastRejectsMissingStaleAndUnsafeResults (0.00s)
    --- PASS: TestVerifyCastRejectsMissingStaleAndUnsafeResults/missing_result (0.00s)
    --- PASS: TestVerifyCastRejectsMissingStaleAndUnsafeResults/stale_result (0.00s)
    --- PASS: TestVerifyCastRejectsMissingStaleAndUnsafeResults/secret_finding (0.00s)
=== RUN   TestBuildMetadataDisplayVersion
=== RUN   TestBuildMetadataDisplayVersion/release
=== RUN   TestBuildMetadataDisplayVersion/development
=== RUN   TestBuildMetadataDisplayVersion/revision
=== RUN   TestBuildMetadataDisplayVersion/dirty_revision
--- PASS: TestBuildMetadataDisplayVersion (0.00s)
    --- PASS: TestBuildMetadataDisplayVersion/release (0.00s)
    --- PASS: TestBuildMetadataDisplayVersion/development (0.00s)
    --- PASS: TestBuildMetadataDisplayVersion/revision (0.00s)
    --- PASS: TestBuildMetadataDisplayVersion/dirty_revision (0.00s)
PASS
ok  	github.com/kjelly/trec	0.893s
```
