---
name: trec-mcp
description: 使用內建 `trec mcp` 操作 trec 的錄影、播放、轉錄、註解、HTML/HTTP 服務及持續終端 session。當 agent 的 shell 工具每次呼叫都是獨立行程、無法安全保留 `trec drive --interactive` stdin，或需要以 MCP 操作任何 trec 功能時必須使用。
---

# Trec MCP

啟動 `trec mcp` 作為 stdio MCP server。它的 stdout 是 JSON-RPC 專用；不要直接對 server 的 stdin/stdout 寫入非 MCP 資料。

## 選工具

- `run`：一次性操作。用於 `transcript`、`annotate`、`html`、短暫 `serve`、非互動 `play`、`record`，以及 `drive --script`。
- `terminal_start`：啟動需要持續 stdin 的命令；回傳 `session_id`。
- `terminal_write`：以同一 `session_id` 寫入按鍵或一行 `drive --interactive` DSL。
- `terminal_read`：讀取累積的 stdout、stderr、是否仍在執行與 exit code。每次讀取會取走目前累積輸出。
- `terminal_close`：明確結束 session；工作完成後必須呼叫，避免遺留子程序。
- `session_list`：盤點尚未關閉的 session。

兩個啟動工具都接受 `working_directory`。`terminal_start` 另接受 `cols`、`rows`，預設
120×40；需要重現固定版面或避免 TUI 折行時明確指定，兩者皆須介於 1–1000。錄影檔以
trec 命令的 `-o <path>` 指定；相對路徑以 `working_directory` 為準，絕對路徑則直接寫到
指定位置。每次錄影都明確傳 `-o`，不要依賴預設的時間戳檔名。

## Stateful TUI

對 wizard、`trec drive --interactive`、`trec record`、`trec play --tui` 與長駐 `serve`，一律使用 `terminal_start`，禁止 FIFO、背景 shell、heredoc 或每頁重開程序。

對 `trec drive --interactive`：

1. 用 `terminal_start` 啟動命令並保留 `session_id`。
2. 用 `terminal_write` 送一條 DSL 指令，加上換行。
3. 用 `terminal_read` 取得其 `OK|ERR`、`CURSOR` 與固定行數的 `SCREEN` 回覆，再決定下一步。
4. 同一個有狀態流程必須重用同一 session 與同一 cast。

長跑工作不以 `EXPECT_QUIET` 判定完成。腳本模式用 `WAIT_CHILD_EXIT`、`ASSERT_EXIT 0`；互動模式持續以 `terminal_read` 檢查 session 的 `running` 與 `exit_code`。

## 安全

`run` 與 `terminal_start` 可執行任意本機命令，僅接受使用者已授權的 command。不要把秘密放在 MCP tool arguments 或 command argv。已知秘密必須以 `--secret-env` 或 `--secret-file` 宣告，讓 trec 在寫入 header、output、marker 與 input event 前遮蔽精確值；它不會猜測未宣告的密碼。需要把秘密送進提示式 TUI 時，使用 `drive` 的 `TEXT_ENV` 或 `TEXT_FILE`，不要用手動 `record` 鍵入，因為一個值可能分散在多個 input event。`html` 和 `serve` 的 keystroke overlay 僅呈現已寫入 cast 的 input event，分享前必須檢查錄影沒有未遮蔽的輸入。
