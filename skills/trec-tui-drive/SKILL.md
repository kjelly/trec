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
ENTER / SPACE / TAB / CTRLC
DOWN [n] / UP [n]    ⚠ 僅限非選單場景；選單一律用 SELECT
BACKSPACE [n]        送 DEL，清除 prompt 預填值
WAIT <ms>            ⚠ 最後手段；先考慮 EXPECT / EXPECT_QUIET
EXPECT <text>        等到畫面出現文字（預設 timeout 10s）
EXPECT@<ms> <text>   單步覆寫 timeout（慢步驟：建置、網路）
EXPECT_QUIET [ms]    等輸出安靜 N ms（預設 300）
ASSERT <text>        當下畫面必須有該文字，否則立刻失敗
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
7. **秘密不落地**：密碼、token 絕不 `TEXT` 進錄影——它們會以明文存進 cast 的
   "i" 事件。需要憑證的流程改用環境變數或事先注入的設定檔。

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
- `ERR` 不會結束 session，由呼叫方決定補救；`QUIT` 或關閉 stdin 結束。
- 整個過程照常錄進 cast，事後同樣可 play / transcript。

## 參數調校

| 參數 | 何時調 |
|---|---|
| `--key-delay` / `--settle-delay` | 腳本全面採用 EXPECT 後可降到 30 / 200 加速；不穩定就回預設 300 / 700 |
| `--expect-timeout` | 全域預設 10s；個別慢步驟用 `EXPECT@<ms>` 放寬，不要拉高全域 |
| `--pointer` | 選單指標非 `❯ ▸ › → » >` 開頭時自訂 |
| `--step-markers=false` | 僅在 marker 干擾 transcript 判讀時關閉 |
