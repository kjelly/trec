---
name: trec-tui-drive
description: 用 trec drive 驅動並錄製互動式 TUI（wizard、選單、表單）的必守流程，把盲打按鍵造成的錄影失敗降到最低。任何要以腳本或 agent 自動操作 TUI 並產出 asciicast 錄影的任務都必須使用；遇到「按錯格」「停在未存檔畫面」「按鍵跑在畫面前面」等症狀時也必須回來對照本清單。
---

# TREC TUI Drive — 閉環驅動 TUI

核心原則：**每一步都「等畫面證實 → 才動作 → 動作後驗證」**。失敗的錄影幾乎都來自開環盲打：憑想像數格數（`DOWN 2`）、憑手感猜時間（`WAIT 800`）。trec drive 內建 VT 螢幕模擬，腳本可以直接對「渲染後的畫面」等待、斷言、找選項——用它。

## 指令速查

```text
TEXT <text>          逐字打字（不含 Enter）
TEXT_ENV <ENV>       送出 ENV 的值，但 cast 的輸入事件只記 <redacted:ENV>
TEXT_FILE <path>     送出檔案的原始文字內容，並自動在 cast 遮罩該值
TEXT_AND_ENTER <text> 輸入文字後以同一步驟送 Enter；ENV/FILE 另有對應的 *_AND_ENTER
REPLACE_TEXT <text>  先送 Ctrl-U 清空目前欄位，再逐字輸入新值
REPLACE_TEXT_ENV <ENV>  清空欄位後送出 ENV 值，cast 僅記 redacted placeholder
REPLACE_TEXT_FILE <path> 清空欄位後送出檔案內容，cast 僅記 redacted placeholder
REPLACE_TEXT_AND_ENTER <text> 覆寫欄位後以同一步驟送 Enter；ENV/FILE 另有對應的 *_AND_ENTER
ENTER / SPACE / TAB / CTRLC
ENTER_IF <text>     畫面包含指定文字才送 Enter；不符就 fail
CHOOSE <label>      以 SELECT 找到唯一選項後立即送 Enter
TOGGLE <label>      以 SELECT 找到 checklist 項目後立即送 Space
DOWN [n] / UP [n]    ⚠ 僅限無法以 label 辨識的非選單場景；n 必須為正整數
BACKSPACE [n]        送 DEL，清除 prompt 預填值
CLEAR_LINE / CTRLU   送 Ctrl-U，一次清除游標前的整行；覆寫欄位優先用 REPLACE_TEXT 系列
WAIT <ms>            ⚠ 最後手段；先考慮 EXPECT / EXPECT_QUIET
EXPECT <text>        等到畫面出現文字（預設 timeout 10s）
EXPECT@<ms> <text>   單步覆寫 timeout（慢步驟：建置、網路）
EXPECT_QUIET [ms]    等輸出安靜 N ms（預設 300；總等待使用 --expect-timeout）
EXPECT_QUIET@<timeout-ms> <quiet-ms> 以單步 timeout 等待輸出安靜
ASSERT <text>        當下畫面必須有該文字，否則立刻失敗
WAIT_CHILD_EXIT      僅 script：等待子程序退出；script 預設受 120 秒安全上限約束
WAIT_CHILD_EXIT@<ms> 僅此步覆寫等待上限；長跑工作必須明確給足時間
ASSERT_EXIT <code>   僅 script：子程序已退出時斷言 exit code；不符即寫 FAILED marker 並失敗
SELECT <label>       自動按 ↑/↓ 直到選單指標行含有 label
SNAPSHOT [label]     將渲染畫面保存到 result 的 snapshots，並傾印到 stderr（除錯用）
END_SESSION          不等待子程序，自動終止並以 aborted 保存探索錄影
QUIT                 END_SESSION 的相容別名
```

## 必守規則

1. **會提交的選單優先用 `CHOOSE <label>`；只移動指標時才用 `SELECT <label>`。** `SELECT` 不會送 Enter。禁止用 `DOWN n` 猜格數。label 選畫面上該行獨有的子字串。指標不是 `❯`/`>` 系列時用 `--pointer` 自訂 regexp。
1a. **多選/checklist 使用 `TOGGLE <label>`。** `TOGGLE` 會讓 `SELECT` 捲動尋找 label，再送 Space；不要以 `DOWN n` 硬編項目索引。
2. **文字欄位優先使用 `TEXT_AND_ENTER`／`REPLACE_TEXT_AND_ENTER` 原子提交。** 需要先驗證目前提示才提交時，使用一般 TEXT 後接 `ENTER_IF <目前提示的唯一文字>`；轉場後、下一個動作前仍須 `EXPECT <新畫面才有的文字>`。`EXPECT_QUIET` 只證明沒有輸出，不是畫面狀態證據。
3. **關鍵動作（存檔、送出、刪除）後立刻 `ASSERT` 結果文字。** 迴圈處理多個項目時每一輪結尾都要 ASSERT。
4. **超過一個轉場的流程，先探勘再寫腳本。** 不確定畫面時，用 `--interactive` 走一遍，或在草稿腳本加入 `SNAPSHOT`；禁止憑想像一次寫完長腳本。
5. **EXPECT/ASSERT 是逐列比對**：跨列折行的長字串不會命中。選短而唯一的關鍵字；寬度不夠導致折行時加大 `-W`。
6. **固定終端尺寸**（預設 220×50），重跑才可重現；別依賴外層終端大小。
7. **秘密不落地**：密碼、token 絕不 `TEXT` 或 `REPLACE_TEXT` 進錄影。wizard 要求輸入秘密時，用 `TEXT_ENV` / `TEXT_FILE`；覆寫預填秘密用 `REPLACE_TEXT_ENV` / `REPLACE_TEXT_FILE`。以 `--secret-env <ENV>` 或 `--secret-file NAME=path` 宣告可能出現在 command、子程序 output、marker 或診斷的秘密。完整 command 預設不寫入 header；需要辨識時用 `--command-label`。只有確實需要時才開啟 `--record-command`，且仍必須搭配 `--secret-env`。
8. **長跑 apply 不得以 `EXPECT_QUIET` 判定完成。** 最後一次送出 apply 後，腳本必須以足夠上限的 `WAIT_CHILD_EXIT@<ms>` 再以 `ASSERT_EXIT 0` 判定完成。
9. **inline comment 以空白後的 `#` 開始。** `issue#123` 的 hash 仍是文字；要輸入以 `#` 開頭或含有 ` #` 的 literal，使用 JSON step。JSON step 必須獨占整行。
10. **opcode 與參數間的額外空白會被忽略。** 若 `TEXT` 必須傳送前導空白，使用 JSON step（例如 `{"kind":"text","text":" hello"}`）。
11. **同一個有狀態的多步驟流程，探勘必須使用同一個錄影與 stdin session。** 前一頁輸入會影響下一頁、可返回、或最後才提交的流程，禁止為每個畫面另開探勘錄影。
12. **執行前先 lint。** Agent 腳本使用 `trec drive lint --strict steps.txt`；ERROR 必須修正，WARNING 必須閱讀。
13. **腳本必須明確收尾。** 可完成的工作使用 `WAIT_CHILD_EXIT@<ms>` + `ASSERT_EXIT <code>`；只為探勘畫面、不把錄影當完成證據時使用 `END_SESSION`。否則 strict lint 會拒絕腳本，避免步驟跑完後空等 120 秒。

## 腳本範本

```text
# 設定 client-vm 這台主機
EXPECT Choose host:
CHOOSE client-vm
EXPECT name>
TEXT_AND_ENTER client-vm
EXPECT ip>
TEXT_AND_ENTER 192.168.122.2
EXPECT configured client-vm        # 這一輪的結果驗證
EXPECT Choose host:                # 確認回到主選單
CHOOSE save & exit
ASSERT SAVED                       # 存檔證據，沒有就 fail
WAIT_CHILD_EXIT@30000
ASSERT_EXIT 0
```

長跑工作在最後一次確認送出後，改用 child lifecycle 指令收尾：

```text
ENTER_IF 確定套用                  # 送出 apply
WAIT_CHILD_EXIT@1200000            # 最多等 20 分鐘，不受 EXPECT timeout 影響
ASSERT_EXIT 0                      # 非零時保留最後畫面與 FAILED marker
```

執行：

```bash
trec drive lint --strict steps.txt
trec drive --script steps.txt -o run.cast -- ./wizard
```

腳本穩定後可加速：`--key-delay 30 --settle-delay 200`。

## 失敗了怎麼辦

fail-fast 已內建：任一步失敗會立刻停止（後續按鍵不會打出去）、stderr 印出**失敗行號 + 指令 + 原因 + 當下畫面傾印**、cast 裡留下 `STEP_FAILED` marker、exit 1。每個成功步驟也會留下 `STEP_START` 與 `STEP_OK`，後者附帶耗時。

1. 先讀 stderr 的畫面傾印，多數情況可直接看出實際畫面與預期差異。
2. `trec play run.cast`：`n`/`N` 跳 step marker（每行腳本一個 ⚑），`←`/`→` 逐格、`space` 暫停、`↑`/`↓` 變速。
3. `trec markers --regex '^STEP_FAILED' run.cast`：先找出失敗 step 的時間與 index；可用 `--from`、`--to` 縮小範圍，給程式使用時加 `--output-format jsonl`。
4. `trec render --marker-regex '^STEP_FAILED' --output-format jsonl run.cast`：讀取失敗 marker 當下的 rendered screen；已知 marker 的篩選後 index 時，用 `--marker-index N` 只輸出一個畫面。
5. `trec transcript run.cast`：⚑ marker 與輸出對齊的純文字版，適合 agent 閱讀。
6. **只重跑失敗段**：從最近一個可 EXPECT 的穩定畫面另寫小腳本接手，不要重做已成功的部分；注意重跑對外部狀態的副作用。

錄影結束後，以 `verify` subcommand 或 MCP `cast_verify` 一次檢查 result status、exit code、SHA-256、byte size、event count、`SESSION_END` 與 secret scan。`verify` 也會確認唯一且位於最後的 `SESSION_END` status/exit code 與 result 一致。result 會保存模式、timeout、終止原因、script SHA-256、去敏 normalized steps、最後步驟及更新時間。若 result 不存在、仍是 `in_progress`、存在未配對的 `STEP_START`、digest 不符或 scan 有 finding，不要以其中的畫面或 marker 當作完成或可分享證據。

## 互動模式（邊看邊按）

流程分支多、畫面不可預測、或第一次探勘時，改用互動模式：一次送一條指令，看到回覆的畫面再決定下一步。`--interactive` 可獨立使用，不需要 `--script`；若兩者同時提供，會先跑完腳本再讀取 stdin 的互動操作。

**Agent 前提：必須保留同一個 stdin session。** 以可持續寫入 stdin 的 PTY/session 啟動 `trec drive --interactive`，每次收到 `SCREEN` 回覆後，以同一個 session 送下一條操作。不要使用會在命令送出後關閉 stdin 的 heredoc、pipe 或一次性 exec；這種 agent 應改用短 `--script` 搭配 `EXPECT`、`ASSERT` 與 `SNAPSHOT` 探測畫面，再依結果啟動下一個短腳本。

```bash
trec drive --interactive -o run.cast -- ./wizard
```

stdin 每行一條指令（語法同上），每條指令執行後 stdout 回一組：

```text
OK | ERR <原因>
CURSOR <row> <col>
SCREEN <rows> <cols>
<rows> 行渲染後的畫面>
```

- 子程序 raw output 不會混進 stdout，回覆可直接程式化解析（SCREEN 行數固定）。
- `ERR` 不會結束 session，由呼叫方決定補救；`END_SESSION`／`QUIT` 會立即終止子程序並把 result 標成 `aborted`。關閉 stdin 只表示不再送操作，trec 仍會等子程序自然結束，最長至 `--timeout`。
- 子程序自行結束時，trec 會立即收尾錄影，即使 stdin 仍開著。
- 長跑工作可安全讓 stdin EOF。interactive 未指定 `--timeout` 時等待子程序自然結束；script-only 未指定時保留 120 秒安全期限。
- `SNAPSHOT` 在互動模式和任何操作一樣，回傳一組當前 SCREEN；它不是 stdout flush 指令。

## 參數調校

| 參數 | 何時調 |
|---|---|
| `--key-delay` / `--settle-delay` | 腳本全面採用 EXPECT 後可降到 30 / 200 加速；不穩定就回預設 300 / 700 |
| `--expect-timeout` | 全域預設 10s；個別慢步驟用 `EXPECT@<ms>` 放寬，不要拉高全域 |
| `--timeout` | script 的 `WAIT_CHILD_EXIT` 與 script 收尾共用的秒數上限；單步可用 `WAIT_CHILD_EXIT@<ms>` 覆寫。interactive 未指定時仍等待子程序自然結束 |

## 實作驗證紀錄（2026-07-18）

以下輸出為公開版本；Go warning 中的本機 home path 已以 `<redacted-home>` 遮蔽，原始輸出保留於本次工作 session。

本次 follow-up 的 fact snapshot：

```text
$ git rev-parse --short HEAD
5be1cc5
$ go version
warning: both GOPATH and GOROOT are the same directory (<redacted-home>/go); see https://go.dev/wiki/InstallTroubleshooting
go version go1.26.2 linux/amd64
```

新增 DSL 的 strict lint 以同一份腳本連續執行兩次，結果一致：

```text
$ ./trec drive lint --strict <tmp>/trec-doc-semantic.drive
PASS <tmp>/trec-doc-semantic.drive: no findings
$ ./trec drive lint --strict <tmp>/trec-doc-semantic.drive
PASS <tmp>/trec-doc-semantic.drive: no findings
```

狀態一致性、原子輸入、明確收尾、lint 與 `SESSION_END` 交叉驗證的實際測試輸出：

```text
$ go test ./... -count=1 -run '^(TestParseDriveLifecycleSteps|TestDriveSemanticInputOperations|TestLintDriveStepsRequiresSubmissionAndExplicitDisposition|TestDriveOutcomeConsistencyAndExplicitEndSession|TestVerifyCastRejectsSessionEndMismatchAndNonFinalMarker)$' -v
warning: both GOPATH and GOROOT are the same directory (<redacted-home>/go); see https://go.dev/wiki/InstallTroubleshooting
=== RUN   TestParseDriveLifecycleSteps
--- PASS: TestParseDriveLifecycleSteps (0.00s)
=== RUN   TestDriveSemanticInputOperations
--- PASS: TestDriveSemanticInputOperations (0.00s)
=== RUN   TestLintDriveStepsRequiresSubmissionAndExplicitDisposition
--- PASS: TestLintDriveStepsRequiresSubmissionAndExplicitDisposition (0.00s)
=== RUN   TestDriveOutcomeConsistencyAndExplicitEndSession
=== RUN   TestDriveOutcomeConsistencyAndExplicitEndSession/interactive_nonzero_child_is_failed_everywhere
=== RUN   TestDriveOutcomeConsistencyAndExplicitEndSession/step_failure_preserves_child_exit_code
=== RUN   TestDriveOutcomeConsistencyAndExplicitEndSession/END_SESSION_terminates_immediately_with_provenance
--- PASS: TestDriveOutcomeConsistencyAndExplicitEndSession (2.17s)
    --- PASS: TestDriveOutcomeConsistencyAndExplicitEndSession/interactive_nonzero_child_is_failed_everywhere (0.53s)
    --- PASS: TestDriveOutcomeConsistencyAndExplicitEndSession/step_failure_preserves_child_exit_code (0.55s)
    --- PASS: TestDriveOutcomeConsistencyAndExplicitEndSession/END_SESSION_terminates_immediately_with_provenance (0.53s)
=== RUN   TestVerifyCastRejectsSessionEndMismatchAndNonFinalMarker
--- PASS: TestVerifyCastRejectsSessionEndMismatchAndNonFinalMarker (0.03s)
PASS
ok  	github.com/kjelly/trec	2.209s
```

```text
$ go test ./... -count=1 -run '^(TestParseDriveInlineCommentsAndGuardedActions|TestLintDriveStepsRejectsBlindTransitionsAndChecksExitPair|TestDriveSignalFinalizesAbortedRecording|TestMCPRunTimeoutIsExplicitAndGraceful|TestMCPCloseAllSessionsFinalizesAbortedRecording|TestVerifyDiagnosesUnfinishedStepWithoutPendingHashNoise)$' -v
warning: both GOPATH and GOROOT are the same directory (<redacted-home>/go); see https://go.dev/wiki/InstallTroubleshooting
=== RUN   TestParseDriveInlineCommentsAndGuardedActions
--- PASS: TestParseDriveInlineCommentsAndGuardedActions (0.00s)
=== RUN   TestLintDriveStepsRejectsBlindTransitionsAndChecksExitPair
--- PASS: TestLintDriveStepsRejectsBlindTransitionsAndChecksExitPair (0.00s)
=== RUN   TestDriveSignalFinalizesAbortedRecording
--- PASS: TestDriveSignalFinalizesAbortedRecording (1.42s)
=== RUN   TestMCPRunTimeoutIsExplicitAndGraceful
--- PASS: TestMCPRunTimeoutIsExplicitAndGraceful (1.00s)
=== RUN   TestMCPCloseAllSessionsFinalizesAbortedRecording
--- PASS: TestMCPCloseAllSessionsFinalizesAbortedRecording (0.01s)
=== RUN   TestVerifyDiagnosesUnfinishedStepWithoutPendingHashNoise
--- PASS: TestVerifyDiagnosesUnfinishedStepWithoutPendingHashNoise (0.01s)
PASS
ok  	github.com/kjelly/trec	2.462s
```

以下黑箱流程也以剛建置的 binary 實際執行；公開版本將暫存路徑統一遮蔽為 `<tmp>`：

```text
$ ./trec drive lint --strict <tmp>/trec-doc-guarded.drive
PASS <tmp>/trec-doc-guarded.drive: no findings

$ ./trec drive --strict-agent --force --script <tmp>/trec-doc-guarded.drive -o <tmp>/trec-doc-guarded.cast -- sh -c "printf 'Confirm deployment'; IFS= read -r answer"
Confirm deployment
trec drive: process exited 0
trec drive: recorded to <tmp>/trec-doc-guarded.cast — replay with: trec play <tmp>/trec-doc-guarded.cast

$ ./trec verify <tmp>/trec-doc-guarded.cast
PASS <tmp>/trec-doc-guarded.cast
checked=1 passed=1 failed=0
```

```text
$ go test ./... -count=1 -run '^(TestReplaceTextClearsLineAndRedactsSecretInput|TestStrictAgentRejectsLiteralSecretScreenAndMarkersHideText|TestDriveWaitChildExitAndAssertExit)$' -v
warning: both GOPATH and GOROOT are the same directory (<redacted-home>/go); see https://go.dev/wiki/InstallTroubleshooting
=== RUN   TestReplaceTextClearsLineAndRedactsSecretInput
--- PASS: TestReplaceTextClearsLineAndRedactsSecretInput (0.00s)
=== RUN   TestStrictAgentRejectsLiteralSecretScreenAndMarkersHideText
--- PASS: TestStrictAgentRejectsLiteralSecretScreenAndMarkersHideText (0.00s)
=== RUN   TestDriveWaitChildExitAndAssertExit
=== RUN   TestDriveWaitChildExitAndAssertExit/per-step_timeout_permits_an_explicitly_long_wait
=== RUN   TestDriveWaitChildExitAndAssertExit/global_timeout_bounds_wait
=== RUN   TestDriveWaitChildExitAndAssertExit/nonzero_exit_writes_failure_marker_and_reports_final_screen
=== RUN   TestDriveWaitChildExitAndAssertExit/explicit_nonzero_assertion_succeeds
--- PASS: TestDriveWaitChildExitAndAssertExit (5.18s)
    --- PASS: TestDriveWaitChildExitAndAssertExit/per-step_timeout_permits_an_explicitly_long_wait (2.04s)
    --- PASS: TestDriveWaitChildExitAndAssertExit/global_timeout_bounds_wait (1.58s)
    --- PASS: TestDriveWaitChildExitAndAssertExit/nonzero_exit_writes_failure_marker_and_reports_final_screen (0.54s)
    --- PASS: TestDriveWaitChildExitAndAssertExit/explicit_nonzero_assertion_succeeds (0.53s)
PASS
ok  	github.com/kjelly/trec	5.188s
```
| `--pointer` | 選單指標非 `❯ ▸ › → » >` 開頭時自訂 |
| `--step-markers=false` | 僅在 marker 干擾 transcript 判讀時關閉 |
