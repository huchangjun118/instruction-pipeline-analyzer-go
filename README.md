# instruction-pipeline-analyzer-go

离线指令追踪分析工具：把一段指令执行追踪还原成流水线时间线，识别数据 /
控制 / 结构冒险，建模指令缓存与数据缓存，并给出停顿归因统计。全程在单
机进程内完成，不依赖任何外部服务。

## 构建与运行

```sh
go build ./cmd/pipeanalyze
./pipeanalyze [flags] [trace-file ...]
```

不带文件名（或使用 `-`）时从标准输入读取。可同时传入多份追踪，彼此独
立分析。

```sh
./pipeanalyze -depth 5 -width 1 -mem-ports 1 \
    -icache 1024:4:32:lru:10 -dcache 2048:4:32:lru:12 \
    -concurrency 4 -timeline trace1.txt trace2.txt
```

## 追踪格式

每行一条记录，空行与 `#` 开头的注释行被忽略：

```
<addr> <mnemonic> [reads=r1,r2] [writes=r3] [mem=0xADDR:WIDTH] [taken]
```

- `addr`：指令地址，十六进制（`0x` 前缀）或十进制。
- `mnemonic`：助记符，必须属于内置集合（见下表）。
- `reads` / `writes`：逗号分隔的寄存器名；`-` 或省略表示空。
- `mem=ADDR:WIDTH`：访存地址与宽度（字节），load/store 必须携带。
- `taken`：标记发生跳转的分支（无条件跳转指令视为恒 taken）。

助记符分类：

| 类别 | 助记符 |
|------|--------|
| ALU  | add sub mul div and or xor sll srl nop |
| Load | lw ld lb lh |
| Store| sw sd sb sh |
| 分支 | beq bne blt bge |
| 跳转 | j jal jr ret |

解析容错：无法识别的助记符、字段缺失（如 load 缺 `mem=`）、字段格式错
误都会产生**带行号且原因可区分**的错误（`unknown mnemonic` /
`missing field` / `bad field`），该记录被跳过，其余记录继续分析，整段
分析不会崩溃或静默丢弃。错误总数与逐条错误（最多保留前 1024 条）都会
出现在结果中。

## 时间线语义

深度 `D`（≥4）、发射宽度 `W`（≥1）的按序流水线：

```
stage 0      IF    取指（访问 icache）
stage 1      ID    译码、读寄存器、冒险检测
stage 2      EX1   执行；分支在离开本级时解析
stages 3..D-2 EXn  （仅 D>4 时存在）
stage D-2    MEM   访存（load/store 访问 dcache）
stage D-1    WB    写回
```

`D=4` 时 EX 与 MEM 合并在 stage 2。时间线逐周期给出每条指令占据的流水
级；一条指令在某级停留超过一个周期即为停顿，并标注归因。同一周期同一
流水级的占用数永不超过发射宽度。

`-timeline` 输出逐周期 × 逐级的占用视图，例如：

```
     8 | #4 beq (load-use)  | #3 sub (load-use)  |  -  | #2 lw (dcache-miss) | #1 add |
```

## 停顿归因

每种停顿周期都计入统计且与时间线一致：

| 归因 | 含义 |
|------|------|
| `load-use` | load 的结果在到达 WB 前被后续指令使用，消费者在 ID 停顿（含等待 load 自身缓存未命中完成的周期） |
| `data-raw` | 关闭旁路（`-no-forwarding`）后，ALU 生产者未到达 WB 导致的 RAW 停顿 |
| `control` | taken 分支 / 无条件跳转引起取指重定向，解析完成前取指暂停（取指气泡） |
| `icache-miss` | 取指未命中，指令在 IF 停留 MissPenalty 个额外周期 |
| `dcache-miss` | load/store 未命中，指令在 MEM 停留 MissPenalty 个额外周期 |
| `structural` | 存储端口或执行单元数量不足，竞争失败方等待（取指让位于 MEM） |

可被旁路化解的 RAW 依赖**不停顿**，单独计入 `forwarded` 计数，与必须停
顿的依赖明确区分。上游指令因下游停顿被反压阻塞时，归因沿流水线向下传
递到根因。缓存未命中造成的停顿周期数与时间线中对应流水级上的标注逐条
一致。

## 配置项

| 标志 | 默认 | 说明 |
|------|------|------|
| `-depth` | 5 | 流水线深度，≥4 |
| `-width` | 1 | 发射宽度，≥1 |
| `-mem-ports` | 0 | IF 与 MEM 共享的存储端口数，0 = 不限 |
| `-alu-units` | 0 | 每周期可用执行单元数，0 = 不限 |
| `-no-forwarding` | false | 关闭结果旁路 |
| `-icache` / `-dcache` | off | `size:assoc:line:repl:penalty`，repl ∈ {lru, fifo}；`off` 表示全命中 |
| `-concurrency` | NumCPU | 多追踪并发度 |
| `-timeout` | 0 | 整体超时，0 = 不限 |
| `-format` | text | `text` 或 `json` |
| `-timeline` | false | 输出逐周期时间线 |

配置变化（深度、宽度、缓存参数、旁路开关等）会确定性地改变时间线与指
标；同一输入与配置反复运行得到逐项一致的结果。

## 流式与并发保证

- 解析与还原均为流式：内存占用只随流水线与缓存配置增长，与追踪长度无
  关（除非显式开启 `-timeline` 保留全量时间线）。
- 多追踪分析时每份追踪使用独立的模拟器实例，任意并发度下的汇总结果与
  串行逐项一致；单份追踪失败（配置错误、读取错误、解析错误过多）不影
  响其余追踪。
- 取消 / 超时会立即终止调度并中断进行中的模拟，每份追踪要么有完整结
  果要么带错误说明，进程不会卡死，也不会留下无解释的半截输出。

## 测试

```sh
go test ./...
```

覆盖：解析异常分类与行号、load-use / RAW / 控制 / 结构冒险、缓存命中
与未命中及 LRU/FIFO 差异、统计与时间线一致性、大体积流式输入、并发与
串行结果一致性、取消与超时恢复、CLI 端到端行为。
