# ipa — 指令流水线离线分析工具

`ipa` 把一段指令执行追踪（trace）还原为流水线时间线，识别数据/控制/结构冒险，
建模指令缓存与数据缓存，并统计缓存命中率与各类停顿周期。全程单机单进程，
不依赖任何外部服务；同一输入与配置必得到逐项一致的结果，可复现历史结论。

## 构建与测试

```sh
go build ./...
go test ./...
```

## 追踪格式

每个非空、非注释行描述一条已执行指令，字段为 `key=value`，以空白分隔：

```
# 注释以 # 开头；空行被忽略
addr=0x1000 op=add w=r1 r=r2,r3
addr=0x1004 op=lw  w=r5 r=r2 mem=0x2000 width=4
addr=0x1008 op=sw  r=r1,r2 mem=0x2004 width=4
addr=0x100c op=beq r=r1,r2
```

| 字段    | 含义                                   | 必填           |
|---------|----------------------------------------|----------------|
| `addr`  | 指令地址（十六进制 `0x` 或十进制）     | 是             |
| `op`    | 助记符（见下表）                       | 是             |
| `w`     | 被写寄存器，逗号分隔；`-` 或空表示无   | 否             |
| `r`     | 被读寄存器，逗号分隔                   | 否             |
| `mem`   | 访存地址                               | load/store 必填 |
| `width` | 访存宽度（字节）                       | load/store 必填 |

已知助记符：
- **ALU**：`add addi sub and andi or ori xor xori sll srl sra mul div rem mv not neg slt li nop`
- **加载**：`lw lh lb ld lbu lhu load`
- **存储**：`sw sh sb sd store`
- **分支/跳转**：`beq bne blt bge beqz bnez j jal jalr ret call`

解析逐行流式进行。**任何一行无法解析（未知助记符、字段缺失、数值非法、未知字段）
都会产生一条带行号、原因可区分的错误**（写入 `-instr-timeline` 运行时的 stderr，
并计入 `parse errors`），该行被跳过，分析继续，不会崩溃也不会静默丢弃。
寄存器 `x0`/`zero` 不参与依赖分析。

## 流水线模型与时间线语义

流水线为按序、锁步推进的 `depth` 级结构（`depth >= 5`）：

```
IF  ID  EX1 .. EXk  MEM  WB      (k = depth - 4)
```

- **IF** 取指（访问 I-cache）；**ID** 译码/读寄存器堆；**EX\*** 执行；
  **MEM** 数据访存（访问 D-cache）；**WB** 写回。
- 发射宽度 `width`：每级每周期最多容纳 `width` 条指令，级内严格按序。
- 时间线中 `IF=1-2` 表示半开区间 `[1,2)`：该指令在第 1 周期占据 IF 级。
- 逐指令视图（`-instr-timeline`）在指令离开 WB 时输出；逐周期视图
  （`-cycle-timeline`）逐周期列出每级 occupants。两者均为流式输出，
  大追踪可不开启（默认关闭），此时只保留汇总所需的中间状态，内存有界。
- 分支在 `branch-stage`（默认 EX1，即第 3 级）末解析：解析完成前取指重定向
  （暂停取指），惩罚周期数等于解析级深度。

## 冒险识别与停顿归因

每条停顿周期都归因到一条指令和一个可区分的原因，同时计入 `Stalls` 统计；
冒险事件本身在 `Hazards` 中按指令计一次：

| 停顿原因          | 含义                                                     |
|-------------------|----------------------------------------------------------|
| `load_use`        | 加载后立即使用：转发无法化解，互锁等待加载数据越过 MEM   |
| `data_bypass`     | 有转发时 ALU 依赖仍需等待（深 EX 链下结果尚未就绪）      |
| `data_no_forward` | 关闭转发时的 RAW 停顿，等待生产者到达 WB                 |
| `control`         | 分支跳转引起的取指重定向                                 |
| `icache_miss`     | 取指未命中，占据 IF 额外 `penalty` 周期                  |
| `dcache_miss`     | 访存未命中，占据 MEM 额外 `penalty` 周期                 |
| `struct_mem_port` | 内存端口不足：MEM 级访存与取指同周期冲突（MEM 优先）     |
| `struct_ex_unit`  | 执行单元不足：EX 级占用数达到 `ex-units` 上限            |

冒险分类（`Hazards`）：`data_bypassed`（转发化解的 RAW）、`data_stall`
（必须停顿的 RAW）、`load_use`、`control`、`structural`。

转发语义：消费者在其首个 EX 周期开始时需要操作数；ALU 结果在最后一个 EX
级末就绪，加载数据在 MEM 级末就绪。因此紧邻的 load-use 对停顿 1 周期，
紧邻的 ALU 对经旁路零停顿。无转发时寄存器堆前半周期写、后半周期读，
消费者可与其生产者的 WB 同周期离开 ID。

结构冒险不会导致同一流水级被重复占用：每级每周期 occupant 数恒不超过
`width`，EX 级总数恒不超过 `ex-units`（有测试守护）。

## 缓存模型

I-cache 与 D-cache 独立配置：容量（字节）、行大小、相联度、替换策略
（`lru`/`fifo`）与未命中惩罚周期。容量必须整除 `行大小×相联度`。
逐次访存分类命中/未命中并汇总命中率；未命中停顿直接体现在时间线上
（指令在 IF/MEM 级多占 `penalty` 个周期），统计与时间线严格一致。

## 配置项（CLI 标志）

```
-depth n            流水线深度（>=5，默认 5）
-width n            发射宽度（>=1，默认 1）
-forwarding         是否启用旁路转发（默认 true）
-branch-stage n     分支解析级（1 起，0=默认 EX1）
-mem-ports n        IF/MEM 共享内存端口数（0=不限）
-ex-units n         执行单元数（0=不限）
-icache  C:L:A:P:K  I-cache：容量:行:相联度:lru|fifo:惩罚
-dcache  C:L:A:P:K  D-cache，同上
-concurrency n      多追踪并发度（默认 4）
-timeout d          整体超时（如 500ms，0=不限）
-instr-timeline F   逐指令时间线输出到文件（"-" 为 stdout）
-cycle-timeline F   逐周期时间线输出到文件（"-" 为 stdout）
```

输入为若干追踪文件；无文件参数时读标准输入。时间线输出仅支持单输入。

## 多追踪并发分析

`AnalyzeAll`（库）/ 多文件参数（CLI）对每份追踪独立推进，模拟器之间不共享
任何状态，因此任意并发度下的汇总结果与串行逐项一致（有测试守护）。单份追踪
失败（读取错误等）只影响自身结果。`context` 取消或 `-timeout` 到期时：
读取被中断、未完成的追踪标记为 `canceled`，进程不会卡死，也不会留下
半截输出（时间线按指令退休顺序流式写出，已写出的行均完整）。

## 库用法

```go
cfg := ipa.DefaultConfig()
cfg.DCache = ipa.CacheConfig{Enabled: true, Capacity: 4096, LineSize: 64,
    Assoc: 4, Replacement: "lru", MissPenalty: 10}

sim, _ := ipa.NewSim(cfg, ipa.SimOptions{InstrTimelineW: os.Stdout})
sim.Run(ipa.NewParser(r))   // r 为 io.Reader，流式推进
stats := sim.Stats()
```

## 测试

```sh
go test ./...        # 含 -race 亦通过
```

覆盖：解析异常（未知助记符/字段缺失/行号归因/错误后继续）、旁路化解与必须
停顿的数据依赖、load-use 互锁、分支重定向、缓存命中/未命中/替换策略差异、
结构冒险（端口与执行单元）、流水级占用不变量、大体积流式输入、配置变化引起
时间线确定性变化、多次运行结果逐项一致、并发与串行等价、失败隔离、
取消与超时恢复。
