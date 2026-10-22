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
