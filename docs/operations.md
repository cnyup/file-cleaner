# 运维手册

## 部署

推荐 systemd（模板见 scripts/filecleaner.service）：

```
/opt/filecleaner/filecleaner          # 二进制
/etc/filecleaner/config.yaml          # 配置
/var/lib/filecleaner/                 # state_dir（锁、审计 JSONL、SQLite 投影）
```

生产建议以 daemon 模式运行（主清理 + purge retire + sweeper 三条调度线自动运转）：
`filecleaner daemon --config /etc/filecleaner/config.yaml`

## NFS 属性缓存调优

重核验依赖 stat 读取真实属性；NFS 客户端默认 `acregmax=60s` 可能读到缓存旧值。
建议挂载参数（代价是 RPC 增多）：

```
mount -t nfs -o acregmin=5,acregmax=10,acdirmin=5,acdirmax=10 ...
```

注意方向：上面是**调小**缓存窗口以换取属性新鲜度，代价是 RPC 增多、扫描变慢。
若目标相反（追求扫描吞吐），需要评估下面的 scan 优化章节——但注意那里的实测结论。

## Scan 耗时优化：三个候选方案与实测结论

全树 scan 是长轮次的主要耗时（生产 video 策略实测 `scan_ms` 占 run 总时长 70-77%，
`drain_ms` 仅 0.8%）。**期限条件（mtime/atime/ctime）不影响 scan 成本**——每个文件
都必须先 lstat 拿到元数据才能判断是否命中，所以放宽 retention 只减少 move 量，
不减少 lstat 次数。

以下是三个常见优化方向，以及 2026-11-13 在测试 NAS（`nas-test-ali-bj-translate-saas-cn-02`，
4,000 文件测试树）上的实测结论：

### 方案 1：调大 NFS 缓存窗口（`acdirmax` / `acregmax`）

**实测结论：收益有限，不建议为此改动挂载参数。**

测试方法：drop_caches 造冷态，遍历 4,000 文件树；空闲 70 秒（超过默认 `acdirmax=60`）
后再遍历。

| 场景 | 耗时 |
|---|---|
| 冷启动（drop_caches 后） | 0.26 s |
| 空闲 70s 后（超 acdirmax） | 0.08 s（仍快 3.25×） |
| 紧接着再遍历 | 0.04 s |

结论：空闲超过 `acdirmax` 后缓存**依然有效**——NFS 客户端的 dentry/inode 缓存走
Linux VFS 的 LRU 回收，与 `acdirmax`（只控制属性重验证间隔）是两套机制。因此调大
`acdirmax` 对「重复遍历同一棵树」的收益远小于预期；而首次冷遍历的成本无法通过
客户端参数规避。

### 方案 2：scan 前预热

**实测结论：无效，不建议实现。**

| 方式 | 总耗时 |
|---|---|
| 预热 + 扫描 | 337 ms（预热 289 + 扫描 47） |
| 直接扫描（无预热） | 347 ms |

预热本身要付出完整的一次冷遍历成本，省下的 10ms（3%）在噪声范围内。除非预热结果
能跨多次 scan 复用，否则净收益为零或负。

### 方案 3：错峰调度（推荐，但取决于业务）

**原理：利用其他业务的访问把 NFS 客户端缓存预热。** 生产实测支持这一点：同一棵树
（约 1,360 万文件）的 `scan_ms` 在不同时段差异达 4.7 倍——

| run | 起跑时刻 | files | scan_ms | 吞吐 |
|---|---|---|---|---|
| #1 定时轮 | 02:00 | 14,352,137 | 4,153,576（69 min） | 3,455 f/s |
| #2 补跑 | 11:38 | 13,639,279 | 1,326,657（22 min） | 10,281 f/s |
| #3 补跑 | 13:04 | 13,751,780 | 844,108（14 min） | 16,291 f/s |

差异不是文件数导致的（三者量级相同），而是缓存温度：#2/#3 跑在 #1 之后，元数据已在
客户端缓存中。

**建议**：把大策略的 `schedule` 挪到业务访问较活跃的时段之后（如上午而非凌晨），
让 scan 搭上缓存的便车。代价是需要了解业务的访问规律，且收益依赖业务侧行为、
不保证稳定。这是**运维侧决策**，工具本身不做假设。

### 不可行的方向（已排除）

- **放宽 retention 来提速**：不影响 lstat 次数，只减少 move 量（而 move 只占 0.8%）
- **单纯提高 `dir_read_workers`**：并发过高可能撞 NAS 服务端限流，需 benchmark 验证
- **目录 mtime 剪枝**（父目录 mtime 未变则跳过子树）：理论收益最大，但 NFS 属性缓存
  可能导致读到旧 mtime，安全性需要额外验证；且 empty_dirs 已需要独立遍历，两者合并
  属于较大重构。**目前未实现。**

## 单机清理约束

锁在执行机本地盘，只防本机重叠。其他机器**不得**运行本工具清理同一 NFS——
两台执行器会产生重复移动与审计分裂。

## 回收站容量规划（分桶方案）

保留期内空间不释放，且 retire 后到 sweep 完成前还有一段释放延迟：

```text
空间占用峰值 ≈ 日清理量 × (保留期 + sweep 滞后)
sweep 滞后 ≈ 桶内文件数 ÷ 110/s（NAS 物理删除速率）
```

例：每日 200GB（约 40 万小文件）× 30 天保留 = 6TB 峰值；retire 后每桶
再需 ~1 小时后台释放。磁盘紧张时调低保留期或加密 sweep 巡检。

## 日常操作

```bash
# 查看到期桶 retire（毫秒级，只做逻辑删除）
filecleaner purge --config /etc/filecleaner/config.yaml --policy <id>

# 手动触发物理删除（daemon 正常时不需要；SweepLoop 每小时自动跑）
filecleaner sweep --config /etc/filecleaner/config.yaml

# 恢复文件（仅活跃桶；retire 后拒绝）
filecleaner restore --config /etc/filecleaner/config.yaml --policy <id> --path <rel>
```

## state_dir 磁盘满

审计写失败（含磁盘满）→ 清理暂停（fail-closed）。处理：

1. 扩容或清理 state_dir 所在盘
2. `filecleaner validate` 确认配置
3. 重新触发；未闭环 intent 由启动核对逻辑列出（audit.jsonl 中 intent 无 result 的行）

## 审计恢复

```bash
# 投影重建（JSONL 是事实源，SQLite 可随时重建）
filecleaner audit rebuild --config /etc/filecleaner/config.yaml

# 崩溃后核对未闭环 intent（有 intent 无 result 的悬案）
filecleaner audit rebuild --config /etc/filecleaner/config.yaml --check-open
# 输出每条悬案的 src/trash 路径；人工核对文件实际位置后，
# 向 audit.jsonl 追加一条 result 事件闭环，再跑 --check-open 确认为空
```

## 崩溃恢复语义

审计（audit.jsonl）任意时刻崩溃只有三种合法状态：
- intent 无 result：文件可能已移动——按 intent 列表核对实际位置后人工闭环
- intent + result 齐全：完整事务
- 尾行截断：该行被跳过（FindOpenIntents 容忍）

回收站三区由 rename 原子分隔，崩溃后自愈：
- 活跃桶：不动，等下次 retire 判定
- `.retired`：不动，等 sweep 领取
- `.purging`：sweeper 重启发现即幂等续删（该桶本来就要删，续删安全）

## 回滚

软删除天然支持回滚：保留期内 `restore` 把文件移回原路径。
注意：桶进 `.retired` 后 restore 拒绝——保留期是恢复窗口的硬边界。
二进制回滚：保留旧版本二进制 + 同一 config 直接替换（分桶布局对旧版
二进制不可读，回滚前先 sweep 干净 `.retired`/`.purging`）。

## 已知限制

- atime 语义依赖挂载（noatime/relatime），作必要条件时需确认服务端语义
- 无业务封存信号：post-rename inode 核验消除"移错文件"，但扫描到移动
  之间被业务替换的文件会被回滚并跳过（下轮重评）
- **trash.retention 最短有效值 = 1 天**（桶按日期粒度判到期）；更短的
  配置表现为"当天桶永不 retire"
- retire 依赖桶名日期与时钟；时钟回拨会延后到期判定（保守方向）
- 物理 unlink 速率受 NAS 元数据写墙限制（实测 ~110/s），大批量删除的
  空间释放存在小时级滞后——这是存储层物理属性，客户端无法消除
