# basic-serice streaming 部署方案（2026-11-18 定稿）

## 现状（实测）

- 生产 `/opt/filecleaner/filecleaner`：11-12 版（md5 前 8 位 40ad185f），不含 streaming 代码
- `config.prod.yaml`：basic 策略 root=整 NAS 根（94 万顶层目录），schedule 已注释（11-16 止血）
- streaming 引擎语义：只处理 root 的**直接子目录**——所以 temp 必须独立策略（root 直指 temp）
- 10.92.0.126 有 go1.26.5；项目唯一外部依赖 gopkg.in/yaml.v3 → vendor 源码本地构建
- 二进制传输三通道全断 → 源码 tar（30MB，含 vendor）分块经跳板传

## 部署设计：三分层

### 1. 配置（config.prod.yaml 加独立策略，daemon 不动）

```yaml
  # temp 巨目录专用（2026-11-18）：streaming 引擎 + 夜间窗口 + checkpoint
  # 独立于 basic-serice-video-daily——streaming 只处理 root 直接子目录，
  # root 直指 temp 才能对准 2615 万直接子目录。daemon 永不调度它
  # （不配 schedule），由 cron 在窗口内触发 CLI。
  - id: basic-temp-streaming
    root: /nas/prod/nas-prod-ali-bj-basic-serice-cn/temp
    retention:
      match: AND
      conditions: {mtime_older_than: 30d}
    empty_dirs:
      enabled: true
      retention: 30d
      mode: streaming
      window: "01:00-07:00"
      checkpoint_dir: /var/lib/filecleaner/emptydirs-ck
```

注意：daemon 读同一 config，但此策略无 schedule → daemon 忽略之；CLI `empty-dirs --policy basic-temp-streaming` 单独触发。原 basic 策略保持暂停不动。

### 2. 触发（cron，与 fc-night-probe 同风格）

```
# /etc/cron.d/fc-emptydirs-streaming
5 1 * * * root /opt/filecleaner/filecleaner empty-dirs --config /opt/filecleaner/config.prod.yaml --policy basic-temp-streaming >> /var/log/fc-emptydirs.log 2>&1
```

窗口外启动被 CLI 熔断拒绝（自保险，cron 早于窗口不会误跑）。每窗口一进程，到 07:00 优雅停 + checkpoint，次日续。

### 3. 二进制升级（源码本地构建）

源码 tar → 10.92.0.126 `/tmp/fc-build/` → `go build -mod=vendor -o /tmp/filecleaner.new ./cmd/filecleaner`
→ 备份现二进制 → 覆盖（daemon 下次重启生效；running 进程不受影响，video/ocr 零扰动）
→ `validate --config config.prod.yaml` 过 → test NAS 1M 夹具 dry-run 验证 streaming。

### 回滚

二进制：`mv filecleaner.bak-<date> filecleaner` + restart。
配置：删策略段。cron：删 /etc/cron.d 文件。三层各自独立可回滚。

## 顺序

1. 传源码 → 构建 → validate
2. test NAS 1M 夹具 dry-run + kill 续扫实测
3. prod 配置加策略 + validate
4. 挂 cron → 明晨 01:05 首窗口
5. 次日核 journal/audit：stopped/resumed/removed + temp links 下降
