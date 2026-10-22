# 回收站日期分桶方案

> 状态：待实现 · 探针已验证核心前提

## 问题

容量型 NAS 元数据写物理墙 ~100-170 ops/s（实测）：purge 600 万文件需 ~15 小时，且 purge 持策略锁期间主清理被跳过。**不是快慢问题，是“逻辑完成”被物理删除绑架**。

## 核心思路（探针验证过的事实）

探针数据（阿里云容量型 NAS，2026-09-29）：

| 操作 | 耗时 |
|---|---:|
| 目录 rename（5000 文件整桶，O(1)） | **3.8 ms** |
| rmdir 空目录 | 5.4 ms |
| unlink 单文件 | 12.1 ms |

整桶 rename 与桶内文件数无关。**把“逻辑删除”与“物理删除”分离**：到期桶 rename 进隔离区（毫秒级、O(1)），物理 unlink 转后台异步，不再阻塞任何东西。

## 布局设计

```mermaid
flowchart LR
    A[扫描命中<br/>mtime=3天前] --> B[trash/2026-09-29/<br/>run-8f3a2c/<br/>原相对路径]
    B -- 到期保留期 --> C[trash/.retired/<br/>2026-09-29.run-8f3a2c<br/>rename O(1)]
    C -- 后台 unlink 巡检 --> D[trash/.purging/...<br/>逐文件删+清空目录]
    D --> E[空间真正释放]
    B -- 恢复请求 --> F[原路径 rename 回<br/>不跨桶]
```

```text
<root>/.filecleaner-trash/
  2026-09-29/            ← 活跃桶（当天所有 run 共用，按日期）
    run-<id>/            ← 按运行分桶（同一天多轮清理隔离）
      a/b/file.txt       ← 原相对路径结构（恢复用）
  .retired/              ← 已到期隔离区（purge 语义=rename 进这里，O(1)）
    2026-09-29.run-<id>/ ← 整桶原样搬入
  .purging/              ← 物理删除中（崩溃恢复标记）
```

## 关键决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 桶粒度 | 日期 + run-id | 恢复按 run/path 查审计即可定位桶；同名冲突天然隔离（不同 run 不同桶） |
| purge 语义 | rename 进 `.retired` | O(1) 毫秒级完成，锁持有时间从小时级降到毫秒级 |
| 物理删除 | 后台巡检 `.retired` → rename 进 `.purging` → 逐文件 unlink → 自底向上清空目录 | `.purging` 是崩溃标记：启动时发现即恢复删除（幂等） |
| `.retired` 可见性 | 进入 `.retired` 即视为“已删除”（restore 拒绝） | 语义简单：保留期 = 活跃桶中的时间；跨过 `.retired` 线即不可恢复——与现行为“到期即删”一致 |
| 审计不变量 | 逐文件 moved/restored 事件照旧；purge 改为桶级 `purge_intent`/`purge_result`（记桶路径） | fail-closed 保留：桶内文件清单由审计可重建 |
| 恢复 | 从活跃桶反 rename 回原路径 | `.retired` 里的不再恢复（已越过保留期） |

## 与现状的行为差异（明示）

1. **purge 完成时间**：从“小时级逐文件”变为“毫秒级整桶 rename + 后台物理删除”——用户可见的 purge 耗时直接归零；
2. **空间释放时间**：从 purge 时刻起改为后台巡检节奏（可配置，默认每小时）——空间回收延后但不再阻塞；
3. **restore 边界变化**：现行为“到期 purge 前可恢复”；新行为“桶进 `.retired` 前可恢复”——语义等价（保留期到即 retire）；
4. 同名二次回收：无需时间戳后缀（不同 run 天然分桶），实现简化。

## 实现波次

### Wave 1：分桶布局 + retire 路径（核心）
- Layout 重构：TrashPath(rel) → TrashPath(rel, runID)（日期/run 桶内路径）
- purge → retire（rename O(1)，锁内毫秒级）
- 审计：桶级事件；SQLite 投影：桶状态
- E2E：move → restore（活跃桶）→ retire → restore 拒绝

### Wave 2：后台物理删除巡检
- daemon 增加 purge-worker 循环：`.purging` 恢复 + `.retired` 逐桶搬入删除
- unlink 限速（复用 moveSem）；空目录自底向上清理
- E2E：崩溃恢复（中途 kill 后重启续删）、幂等

### Wave 3：迁移 + 文档
- 现有扁平回收站 → 分桶布局的一次性迁移（audit rebuild 兼容）
- performance.md/operations.md 更新；README 布局图

## 验收（可观察）

1. 600 万文件桶 retire：**秒级完成**（rename O(1)，实测探针 3.8ms/桶）
2. purge 期间策略锁持有：毫秒级（不再阻塞主清理）
3. 空间在后台巡检周期内释放；`.retired` 不积压
4. restore 语义：保留期内可恢复、retire 后拒绝——E2E 断言
5. 崩溃恢复：`.purging` 中断后重启续删，审计可对账
