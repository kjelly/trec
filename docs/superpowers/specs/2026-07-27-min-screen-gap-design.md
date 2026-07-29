# Minimum Screen Gap Playback Design

## Goal

讓 `trec play` 能以毫秒為單位設定相鄰畫面輸出事件的最小間隔，預設為 500ms，讓快速連續的畫面變化更容易閱讀。

## Scope

本功能只影響 `trec play` 的播放時間軸，不修改原始 `.cast` 檔案。畫面事件定義為 asciicast output (`o`) 事件；input (`i`)、marker (`m`) 與 resize (`r`) 不會自行觸發畫面間隔。

## Interface

新增旗標：

```text
trec play --min-screen-gap-ms N recording.cast
```

`N` 預設為 `500`。`N=0` 停用限制；負值由命令層拒絕。播放速度會照既有 `--speed` 規則套用到調整後時間軸。

## Timing behavior

播放事件先套用既有 idle cap 與 smart pacing，再套用 screen gap，最後套用 input gap。screen gap 會尋找相鄰的 `o` 事件；若兩者間隔小於設定值，就把不足的差值加入該輸出事件及後續事件。事件順序、事件資料與原始時間戳不會被改寫到檔案中。

## Implementation

- `play.go`：新增 screen-gap 時間軸調整函式，並在 `play` 命令註冊／讀取 `--min-screen-gap-ms`。
- `play_test.go`：測試連續 output 事件、非 output 事件穿插、停用值，以及預設旗標值。

## Error handling

負的 `--min-screen-gap-ms` 會在開始播放前報錯並結束；0 與正值可正常執行。

## Verification

執行 `go test ./...`、`go vet ./...`、`go build -o trec .`；本次不涉及 PTY 或錄製並行邏輯，因此不需額外以 race detector 作為必要驗證。
