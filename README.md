# filecleaner

面向 TB 级 NFS 的定时文件清理工具：全量扫描、按目录策略、软删除可恢复、审计可溯源。

## 范围

- Linux 执行机 + NFS 挂载（单机运行清理；其他机器可同时读写 NFS）
- 每目录一条策略：cron 调度、mtime/ctime/atime AND/OR 条件、目录与 glob/正则排除
- 软删除：同文件系统 rename 进日期分桶回收站，保留原目录结构
- 删除分两阶段：到期桶整桶 retire（O(1)，毫秒级）+ 后台物理删除（异步，不阻塞）
- 恢复不覆盖已有文件；桶 retire 后不可恢复（保留期语义）
- 审计：本地 JSONL（预提交事务）+ SQLite 投影查询

## 回收站布局（日期分桶）

```text
<root>/.filecleaner-trash/
  2026-09-29.run-9c45a6/   ← 活跃桶：每轮 once 一个桶，原相对路径结构
  .retired/                 ← 已到期隔离区（retire 目标，等待物理删除）
  .purging/                 ← 物理删除中（崩溃标记，重启续删）
```

```mermaid
flowchart LR
    A[源目录<br/>命中策略] -->|rename 逐文件| B[活跃桶<br/>保留期内可 restore]
    B -->|到期 retire<br/>O&#40;1&#41; rename ~34ms| C[.retired]
    C -->|sweep 领取| D[.purging]
    D -->|后台逐文件 unlink<br/>~110/s 异步| E[空间释放]
    B -->|restore 反 rename| A
```

到期判定按桶日期粒度（日期 23:59:59 早于 cutoff 才 retire），**最短有效 trash.retention = 1 天**。

## 快速开始

```bash
CGO_ENABLED=0 go build -o filecleaner ./cmd/filecleaner

cp config.example.yaml config.yaml   # 编辑策略
./filecleaner validate --config config.yaml
./filecleaner once --config config.yaml --policy video-data-daily --dry-run
./filecleaner once --config config.yaml --policy video-data-daily
```

## 子命令

```
filecleaner validate --config <yaml>
filecleaner once    --config <yaml> --policy <id> [--dry-run]
filecleaner restore --config <yaml> --policy <id> --path <rel>
filecleaner purge   --config <yaml> --policy <id>   # retire 到期桶，毫秒级返回
filecleaner sweep   --config <yaml> [--policy <id>] # 手动物理删除 .retired/.purging
filecleaner audit query --config <yaml> [--policy <id>]
filecleaner audit rebuild --config <yaml> [--check-open]  # 重建投影 / 列出未闭环 intent
filecleaner bench scan --root <dir> [--stat-workers 1,4,8,16,32,64]
filecleaner daemon  --config <yaml>                 # 主清理 + purge + sweeper 巡检
```

daemon 内含三条调度线：各策略 `schedule`（主清理）、`trash.purge_schedule`（retire）、SweepLoop 每小时物理删除。

## 安全不变量

1. 每批 rename 前 intent 已 fsync 到 JSONL；审计不可写 → 该批零文件操作
2. trash 与 root 不同 st_dev → 策略禁用；EXDEV 记错，绝不 copy
3. 排除优先；符号链接不跟随；回收站目录永不进入扫描
4. **sweep 只删进过 `.retired` 的桶；活跃桶与未知文件永不删**（fail-closed）
5. 同策略锁（flock）：上次未完成 → 本次跳过；retire 持锁毫秒级
6. scan/drain/total 分开计时，禁止用总时长冒充扫描时长
7. 崩溃安全：活跃桶/`.retired`/`.purging` 三区由 rename 原子分隔，重启幂等续删

## 性能口径

见 docs/performance.md。关键实测（阿里云容量型 NAS，600 万文件）：

- 扫描：23,067 files/s（旧工具 7,895，快 2.9 倍，超 50% 目标）
- retire（逻辑删除）：34ms/桶，与桶内文件数无关（旧逐文件 purge 同规模 46s）
- 物理删除：后台 ~110/s（NAS 元数据写物理墙，异步不阻塞前台）
- 软删除入桶：~120/s（同一物理墙；建议 move_workers=16 并配业务低峰窗口）

## 运维

见 docs/operations.md。
