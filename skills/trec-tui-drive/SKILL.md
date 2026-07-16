---
name: trec-tui-drive
description: 用 trec drive 驅動並錄製互動式 TUI（wizard、選單、表單）的必守流程，把盲打按鍵造成的錄影失敗降到最低。任何要以腳本或 agent 自動操作 TUI 並產出 asciicast 錄影的任務都必須使用；遇到「按錯格」「停在未存檔畫面」「按鍵跑在畫面前面」等症狀時也必須回來對照本清單。
---

# TREC TUI Drive — 閉環驅動 TUI

核心原則：**每一步都「等畫面證實 → 才動作 → 動作後驗證」**。失敗的錄影幾乎都來自
開環盲打：憑想像數格數（`DOWN 2`）、憑手感猜時間（`WAIT 800`）。trec drive 內建
VT 螢幕模擬，腳本可以直接對「渲染後的畫面」等待、斷言、找選項——用它。

## 指令速查

```text
TEXT <text>          逐字打字（不含 Enter）
TEXT_ENV <ENV>       送出 ENV 的值，但 cast 的輸入事件只記 <redacted:ENV>
TEXT_FILE <path>     送出檔案的原始文字內容，並自動在 cast 遮罩該值
ENTER / SPACE / TAB / CTRLC
DOWN [n] / UP [n]    ⚠ 僅限非選單場景；選單一律用 SELECT
BACKSPACE [n]        送 DEL，清除 prompt 預填值
WAIT <ms>            ⚠ 最後手段；先考慮 EXPECT / EXPECT_QUIET
EXPECT <text>        等到畫面出現文字（預設 timeout 10s）
EXPECT@<ms> <text>   單步覆寫 timeout（慢步驟：建置、網路）
EXPECT_QUIET [ms]    等輸出安靜 N ms（預設 300）
ASSERT <text>        當下畫面必須有該文字，否則立刻失敗
WAIT_CHILD_EXIT      僅 script：純等待被錄製的子程序自然退出，不看畫面或安靜時間
ASSERT_EXIT <code>   僅 script：子程序已退出時斷言 exit code；不符即寫 FAILED marker 並失敗
SELECT <label>       自動按 ↑/↓ 直到選單指標行含有 label
SNAPSHOT             傾印渲染畫面到 stderr（除錯用）
QUIT                 提前結束
```

## 必守規則

1. **選單一律 `SELECT <label>`，禁止 `DOWN n` 數格數。** 選項多一項、順序變了，
   SELECT 仍然正確；DOWN n 會整串錯位。label 選畫面上該行獨有的子字串。
   指標不是 `❯`/`>` 系列時用 `--pointer` 自訂 regexp。
2. **每個 ENTER 轉場後、下一個動作前，必加 `EXPECT <轉場後才有的文字>`。**
   這取代「猜 settle 時間」，是消除 race 的關鍵。
3. **關鍵動作（存檔、送出、刪除）後立刻 `ASSERT` 結果文字。** 迴圈處理多個項目
   時每一輪結尾都要 ASSERT——錯誤要停在發生的那一輪，不是跑完才發現。
4. **超過一個轉場的流程，先探勘再寫腳本。** 不確定畫面長怎樣時，用
   `--interactive` 走一遍（見下），或在草稿腳本裡加 `SNAPSHOT`。禁止憑想像
   一次寫完長腳本。
5. **EXPECT/ASSERT 是逐列比對**：跨列折行的長字串不會命中。選短而唯一的關鍵字；
   寬度不夠導致折行時加大 `-W`。
6. **固定終端尺寸**（預設 220×50），重跑才可重現；別依賴外層終端大小。
7. **秘密不落地**：密碼、token 絕不 `TEXT` 進錄影。wizard 要求輸入秘密時，用
   `TEXT_ENV <ENV>` 或 `TEXT_FILE <path>`。以 `--secret-env <ENV>` 或
   `--secret-file NAME=path` 宣告任何可能出現在 command、子程序 output、marker 或診斷的
   秘密（可重複）。兩種 TEXT 指令都會真正送值到 PTY，但 cast 只留下 `<redacted:...>`；
   script 中環境變數／檔案無法讀取、為空或不是 UTF-8 時會在啟動子程序前失敗，interactive
   模式則在該操作送出前回覆錯誤。`TEXT_FILE` 不會移除結尾換行，檔案內容必須正好是要輸入
   的文字。不要仰賴 `password=` 或
   `sshpass -p` 的猜測式警告：未宣告的明文秘密不保證能遮住。
   完整 command 預設不寫入 header；需要辨識時用 `--command-label`。只有確實需要時才
   開啟 `--record-command`，且仍必須搭配 `--secret-env`。
8. **長跑 apply 不得以 `EXPECT_QUIET` 判定完成。** `EXPECT_QUIET` 只適合短暫、可預期
   會停止的畫面轉場；Ansible 等持續輸出的工作會讓它一直等到輸出停止。最後一次送出
   apply 後，腳本必須以 `WAIT_CHILD_EXIT` 再以 `ASSERT_EXIT 0` 判定完成；不要用 per-step
   `EXPECT@` 或 quiet timeout 當整個 deploy 的存活／完成判斷。
9. **同一個有狀態的多步驟流程，探勘必須使用同一個錄影與 stdin session。** 前一頁的
   輸入會影響下一頁、可返回、或最後才提交的流程，禁止為每個畫面另開探勘錄影；逐頁
   重啟會遺失狀態，也無法驗證真正的轉場。以同一個 `--interactive` session 逐步讀取
   `SCREEN`，直到流程完成或找到可重現的分岔。每次啟動都能直接進入同一畫面且沒有
   跨頁狀態的程式，則可分開探勘。

## 腳本範本

```text
# 設定 client-vm 這台主機
EXPECT Choose host:
SELECT client-vm
ENTER
EXPECT name>
TEXT client-vm
ENTER
EXPECT ip>
TEXT 192.168.122.2
ENTER
EXPECT configured client-vm        # 這一輪的結果驗證
EXPECT Choose host:                # 確認回到主選單
SELECT save & exit
ENTER
ASSERT SAVED                       # 存檔證據，沒有就 fail
```

長跑工作在最後一次確認送出後，改用 child lifecycle 指令收尾：

```text
ENTER                              # 送出 apply
WAIT_CHILD_EXIT                    # 不受 EXPECT / EXPECT_QUIET timeout 影響
ASSERT_EXIT 0                      # 非零時保留最後畫面與 FAILED marker
```

執行：

```bash
trec drive --script steps.txt -o run.cast -- ./wizard
# 腳本穩定後可加速：--key-delay 30 --settle-delay 200
```

## 失敗了怎麼辦

fail-fast 已內建：任一步失敗會立刻停止（後續按鍵不會打出去）、stderr 印出
**失敗行號 + 指令 + 原因 + 當下畫面傾印**、cast 裡留下 `FAILED` marker、exit 1。

1. 先讀 stderr 的畫面傾印——多數情況直接看出實際畫面與預期差在哪。
2. `trec play run.cast`：`n`/`N` 跳 step marker（每行腳本一個 ⚑），`←/→` 逐格、
   `space` 暫停、`↑/↓` 變速，逐格看分岔點。
3. `trec transcript run.cast`：⚑ marker 與輸出對齊的純文字版，適合 agent 讀。
4. **只重跑失敗段**：從最近一個可 EXPECT 的穩定畫面另寫小腳本接手，
   不要重做已成功的部分（注意重跑對外部狀態的副作用）。

## 互動模式（邊看邊按）

流程分支多、畫面不可預測、或第一次探勘時，改用互動模式——一次送一條指令，
看到回覆的畫面再決定下一步，從根本上不存在「腳本與畫面不同步」：

`--interactive` 可獨立使用，**不需要** `--script`；若兩者同時提供，會先跑完腳本
再讀取 stdin 的互動操作。

**Agent 前提：必須保留同一個 stdin session。** 互動模式不是「每一輪新開一次 shell」
的協定。Agent 必須以可持續寫入 stdin 的 PTY/session 啟動 `trec drive --interactive`，
並在每次收到 `SCREEN` 回覆後，以同一個 session 送下一條操作（例如使用 `write_stdin`）。
不要用會在命令送出後關閉 stdin 的 heredoc、pipe 或一次性 exec；那種 agent 應改用短
`--script` 搭配 `EXPECT`、`ASSERT` 與 `SNAPSHOT` 探測畫面，再依結果啟動下一個短腳本。

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

- 子程序的 raw output 不會混進 stdout，回覆可直接程式化解析（SCREEN 行數固定）。
- `ERR` 不會結束 session，由呼叫方決定補救；只有 `QUIT` 會明確要求 trec
  盡快結束子程序。**關閉 stdin 只表示不再送操作**，trec 仍會等子程序自然結束，
  最長至 `--timeout`；這讓 heredoc 或 pipe 在送完最後一條操作後可以安全關閉。
- 子程序自行結束時，trec 會立即收尾錄影，即使 stdin 仍開著、控制端正等待送出
  下一條操作。
- **長跑工作（例如 Ansible deploy）可安全讓 stdin EOF。** 未明確指定 `--timeout` 的
  interactive 模式會等子程序自然結束，不會沿用 script 模式的 120 秒安全期限。只有要
  強制設上限時才傳入 `--timeout`；不要把每個短步驟的 `EXPECT@` timeout 當作整個 deploy
  的存活時間。
- `SNAPSHOT` 在互動模式和任何操作一樣，回傳一組當前 SCREEN；它不是 stdout 的
  flush 指令。若 `EXPECT_QUIET` 正在等待，持續輸出（例如 Ansible）會讓該操作直到
  輸出停止或 timeout 才回覆。
- 整個過程照常錄進 cast，事後同樣可 play / transcript。

## 參數調校

| 參數 | 何時調 |
|---|---|
| `--key-delay` / `--settle-delay` | 腳本全面採用 EXPECT 後可降到 30 / 200 加速；不穩定就回預設 300 / 700 |
| `--expect-timeout` | 全域預設 10s；個別慢步驟用 `EXPECT@<ms>` 放寬，不要拉高全域 |
| `--timeout` | interactive 未指定時等待子程序自然結束；只有需要強制上限時才傳入。script-only 未指定時保留 120 秒安全期限 |
| `--pointer` | 選單指標非 `❯ ▸ › → » >` 開頭時自訂 |
| `--step-markers=false` | 僅在 marker 干擾 transcript 判讀時關閉 |
