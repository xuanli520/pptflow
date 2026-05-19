# Docker 服务重构、构建镜像源与清理机制实施计划

## 背景

本轮讨论聚焦三个问题：

1. TUI 管理的 p2r-qa 项目只做了当前 run 或同 task 历史 run 的局部 Docker 清理，缺少定时清理全局 Docker 缓存、无用容器、镜像、网络和卷的机制，磁盘占用会快速膨胀，也可能造成环境冲突。
2. Stage B / Stage C 运行时阶段存在不稳定失败，典型现象是 Docker 镜像拉取或构建阶段报 `Error response...` 一类错误。源码复盘后，Stage B 的 required `docker compose pull --ignore-buildable` 是高风险点之一。
3. 质检机是专用机器，可以接受 p2r 修改 Docker daemon 配置；但 `daemon.json` 只能影响基础镜像拉取，不能影响 Dockerfile 内部的 `RUN pip install`、`RUN npm install`、`RUN apt-get install gcc` 等命令。后者发生在构建容器内部，必须通过临时 Dockerfile 或构建代理进入构建环境。

本计划只描述实现方案，不在本文档落地代码。

---

## 源码复盘结论

### 当前 Docker 包边界过窄

`internal/docker/manager.go` 当前只包含：

- `ComposeProjectName()`
- `CleanupComposeProject()`
- `CleanupComposeArgs()`
- `CommandLine()`

也就是说，`internal/docker` 目前更像 cleanup helper，不是完整 Docker 服务模块。

实际 Docker 编排仍集中在 `internal/pipeline/stage_b.go`：

- compose 文件发现：`findCompose()`
- README 中 compose 命令解析：`readmeComposeCommand()`
- `pull/build/up/ps/config/port` 命令拼接
- `docker compose ps --format json` 解析
- `docker port` fallback
- 基础 HTTP probe
- `port_map.json` 写入

这些 helper 位于 `internal/pipeline/compose.go`，但语义上属于 Docker/Compose 工具层。

### Stage B 高风险点

当前 Stage B 对 repo 内 compose 文件固定执行：

```text
docker compose -f <compose> -p <project> pull --ignore-buildable
docker compose -f <compose> -p <project> build
docker compose -f <compose> -p <project> up -d
```

其中 pull 是 required：pull 失败会直接让 Stage B 失败。

风险：

- 私有镜像、临时 registry、Docker Hub 网络波动都会提前失败。
- 有些 compose 服务虽然声明 `image`，但实际期望本地 `build` 产出；`pull --ignore-buildable` 仍可能对非 buildable 服务失败。
- 镜像源配置未建立前，pull 失败会被误归因成项目无法运行。

本轮修复应把 pull 策略降级为可配置、可回退、可记录，而不是固定 required。

### Stage C 语义需要统一

当前 `internal/pipeline/stage_c.go` 在宿主机执行：

```text
bash run_tests.sh
```

并通过 Stage B runtime evidence 注入：

- `<SERVICE>_URL`
- `COMPOSE_PROJECT_NAME`
- `COMPOSE_FILE`

注意：这不是“在容器或 Compose 网络内执行测试”。旧设计文档 `docs/p2r_cli_设计.md` 写的是 C 依赖 B，并在容器或 Compose 网络内执行；但较新的迭代计划中又要求移除 B -> C 的硬阻断，让 C 在缺少 port mappings 时给出自己的失败原因。

落地前需要明确产品语义。本计划建议：

- 不在 Docker 重构中顺手大改 C 的终态语义。
- 先修 Stage B 构建稳定性和 runtime evidence 可靠性。
- Stage C 保留“缺少 runtime evidence 时给出清晰失败”的现状，另开小步评估是否恢复容器内/Compose 网络内执行。

### cleanup 分散在 pipeline

`internal/pipeline/cleanup.go` 同时负责：

- task run lock
- stale run cleanup
- current runtime cleanup
- cleanup summary 写入
- cleanup finding 生成
- cleanup 时机判断

`internal/pipeline/run_lifecycle.go` 在 preflight 后调用 stale cleanup，在 B/C 后或 run 结束前调用 runtime cleanup。取消和 crash 路径又在 `internal/pipeline/lifecycle.go` 用 background context 再清理一次。

这说明 Docker 资源生命周期已经跨多个 pipeline 文件，不适合继续承载全局 GC 和构建镜像源逻辑。

### 配置缺口

`internal/config/config.go` 的 `DockerConfig` 目前只有：

- `managed_label`
- `compose_project_prefix`
- `keep_failed_containers_minutes`
- `health_check_timeout_seconds`
- `cleanup_policy`
- `cleanup_images`
- `cleanup_volumes`
- `cleanup_build_cache`
- `build_cache_prune_until`
- `keep_runtime`

缺少：

- daemon registry mirror 配置
- Dockerfile patch mirror 配置
- npm/pip/apt/apk/yum/go/cargo 镜像源配置
- GC 调度配置
- GC 状态文件和锁配置
- pull 策略配置

---

## 总体目标

1. 将 Docker/Compose 编排、构建镜像源、runtime evidence、cleanup、GC 从 pipeline 中拆出到明确的 Docker 服务模块。
2. Stage B 使用新的 Docker 服务，不再在 stage 代码中直接拼大段 Docker 命令。
3. 支持质检机级别的 Docker daemon registry mirror 配置，用于基础镜像拉取。
4. 支持在 TUI 中图形化配置 daemon registry mirrors，并将期望配置保存到项目级 `.p2r.yaml`。
5. 支持 run 级临时 Dockerfile patch，不污染 `repo/`，用于 Dockerfile 内部依赖安装换源。
6. 支持保守、可审计、可回退的定时 Docker GC。
7. 所有镜像源注入、fallback、跳过项和清理动作都生成 artifact，方便质检员排查。

---

## 非目标

1. 不修改用户交付目录 `repo/`。
2. 不强行覆盖所有 Dockerfile 场景。MVP 优先支持常见 compose + Dockerfile 项目。
3. 不默认执行危险的无差别全局 prune，例如 `docker system prune -a --volumes`。
4. 不在本轮引入 Docker SDK。继续使用 Docker CLI，符合现有设计。
5. 不把 scheduler 变成 Docker GC 调度器。scheduler 仍只管理 p2r pipeline job。

---

## 建议模块边界

扩展 `internal/docker` 为完整 Docker 服务包：

```text
internal/docker/
  manager.go              # 对外 Manager/Service facade
  command.go              # Docker CLI 调用、环境、超时、日志摘要
  compose.go              # compose 文件发现、compose args、config 解析
  runtime.go              # RuntimeState、port mapping、probe result
  build.go                # pull/build/up/ps/port/probe 编排
  mirrors.go              # mirror profile、daemon mirror、build mirror summary
  dockerfile_patch.go     # 临时 Dockerfile patch
  cleanup.go              # compose project cleanup、stale cleanup
  gc.go                   # p2r 安全 GC
  lock.go                 # Docker maintenance lock
```

pipeline 保留：

- stage 状态转换
- artifact required/best-effort 持久化
- DB stage/finding/run 持久化
- RunProgress 事件
- abort/crash/finalize 语义

pipeline 不再直接关心 Docker 命令细节。

---

## 配置设计

建议扩展 `.p2r.yaml`：

```yaml
docker:
  managed_label: "managed_by=p2rqa"
  compose_project_prefix: "p2rqa"
  keep_runtime: false

  pull_policy: "best_effort"   # required | best_effort | skip

  daemon_mirrors:
    enabled: true
    daemon_json: "/etc/docker/daemon.json"
    backup_dir: "./projects-qa/.qa-control/docker-daemon-backups"
    registry_mirrors:
      - "https://example-dockerhub-mirror"
    require_manual_apply: true

  build_mirrors:
    enabled: true
    mode: "patch_dockerfile"   # off | env_only | patch_dockerfile
    fallback_to_original: true
    verify_override: true
    profile: "cn"
    apt_mirror: "https://example/debian"
    ubuntu_mirror: "https://example/ubuntu"
    apk_mirror: "https://example/alpine"
    yum_mirror: "https://example/centos"
    npm_registry: "https://registry.npmmirror.com"
    pip_index_url: "https://pypi.tuna.tsinghua.edu.cn/simple"
    go_proxy: "https://goproxy.cn,direct"
    cargo_registry: "sparse+https://example/crates.io-index/"

  cleanup_policy: "always"
  cleanup_images: true
  cleanup_volumes: true
  cleanup_build_cache: false
  build_cache_prune_until: "24h"

  gc:
    enabled: true
    run_on_tui_start: true
    run_before_cli_run: false
    interval: "24h"
    p2r_only: true
    prune_exited_containers: true
    prune_networks: true
    prune_volumes: false
    prune_images: false
    prune_builder_cache: true
    builder_cache_until: "72h"
```

默认策略建议：

- `daemon_mirrors.enabled` 可以 true，但 `require_manual_apply` 默认 true，避免静默修改系统文件。
- `build_mirrors.enabled` 默认 true，`fallback_to_original` 默认 true。
- `pull_policy` 默认 `best_effort`。
- `gc.p2r_only` 必须默认 true。
- 全局 image/volume prune 默认 false。

---

## daemon.json 方案

用户已确认质检机是专用机器，因此允许提供修改 Docker daemon 配置的能力。

建议新增 admin 命令：

```text
p2r admin docker-mirror status
p2r admin docker-mirror apply --yes
p2r admin docker-mirror restore --backup <path> --yes
```

行为：

1. 读取 `/etc/docker/daemon.json`。
2. 保留已有字段，只合并或更新 `registry-mirrors`。
3. 写入前备份到 `.qa-control/docker-daemon-backups/`。
4. 写入后提示需要重启 Docker，或在 Linux 上可选执行 `systemctl restart docker`。
5. `status` 显示当前 daemon mirror 是否和 p2r 配置一致。

### TUI 图形化配置

daemon mirror 需要同时支持 TUI 配置入口。冻结要求：

1. TUI 提供 Docker mirror 配置面板，可编辑：
   - `docker.daemon_mirrors.enabled`
   - `docker.daemon_mirrors.daemon_json`
   - `docker.daemon_mirrors.backup_dir`
   - `docker.daemon_mirrors.registry_mirrors`
   - `docker.daemon_mirrors.require_manual_apply`
2. TUI 的保存动作只写项目级 `.p2r.yaml`，不直接修改 `/etc/docker/daemon.json`。
3. 如果项目级 `.p2r.yaml` 不存在，TUI 可以创建；如果当前配置来自用户级配置或 `P2R_CONFIG`，TUI 仍明确提示并写项目级 `.p2r.yaml`。
4. TUI 的 `Status` 操作只读，复用 CLI/backend 的 status 逻辑显示 current mirrors、desired mirrors、diff 和一致性状态。
5. TUI 的 `Apply` 操作必须显示将写入的 daemon path、registry-mirrors diff、backup path 和 Docker 重启提示，并要求强确认。
6. TUI 的 `Restore` 操作必须从 backup 列表选择目标，并要求强确认。
7. TUI 不内置 sudo 密码输入；权限不足时显示错误和等价 CLI 命令，例如 `sudo p2r admin docker-mirror apply --yes`。
8. CLI 与 TUI 必须复用同一套 daemon mirror service，避免两套实现产生审计差异。

注意：

- daemon mirror 只影响 `FROM`、`docker pull`、compose pull/up 拉基础镜像。
- daemon mirror 不影响 `RUN pip install`、`RUN npm install`、`RUN apt-get install`。

---

## 临时 Dockerfile patch 方案

### 为什么必须 patch

Docker 构建里的依赖安装发生在构建容器内部：

```dockerfile
FROM python:3.12-slim
RUN pip install requests
RUN apt-get update && apt-get install -y gcc
```

这里的 `pip` 和 `apt-get` 来自 `python:3.12-slim` 镜像，不来自宿主机。宿主机的 pip/npm/apt 配置不会自动生效。

因此，非 pull 类依赖安装要么靠代理，要么必须通过 Dockerfile/构建环境注入镜像源。本计划采用 run 级临时 Dockerfile patch，不改 `repo/`。

### 保守 MVP 流程

1. Stage B 发现 compose 文件后，先执行：

   ```text
   docker compose -f <compose> config
   ```

   用于验证原始 compose 可解析。

2. 解析 compose 中有 `build` 的服务。

   支持：

   ```yaml
   services:
     web:
       build: ./web
   ```

   和：

   ```yaml
   services:
     web:
       build:
         context: ./web
         dockerfile: Dockerfile
         target: production
   ```

   首版跳过并记录：

   - `dockerfile_inline`
   - 无明确 context 的复杂动态写法
   - Dockerfile 不存在

3. 在 run artifact 下生成：

   ```text
   docker_mirror/
     web.Dockerfile.p2r
     api.Dockerfile.p2r
     compose.mirror.override.yml
     docker_mirror_summary.json
   ```

4. 生成 compose override。

   示例：

   ```yaml
   services:
     web:
       build:
         context: /abs/path/to/project/repo/web
         dockerfile: /abs/path/to/run/docker_mirror/web.Dockerfile.p2r
   ```

   需要保留原 build 的 `args`、`target`、`platforms` 等字段。

5. 使用原 compose + override 做验证：

   ```text
   docker compose -f <compose> -f <override> config
   ```

   验证失败则回退原始 compose，并在 `docker_mirror_summary.json` 记录。

6. 后续 build/up/ps/down 统一使用同一组 compose 文件。

   这要求 RuntimeState 从单个 `ComposeFile string` 升级为：

   ```go
   ComposeFiles []string `json:"compose_files"`
   ```

   为兼容旧 artifact，可暂时保留 `ComposeFile string`。

### patch 规则

原则：

- 不做全文件正则替换。
- 使用 Dockerfile parser 保留语义。
- 保留顶部 parser directive，例如 `# syntax=docker/dockerfile:1`。
- 多阶段构建按 stage 独立处理。
- 不对 `scratch`、distroless、无 shell stage 盲目插 `RUN`。
- 只在检测到相关命令时注入相关镜像源配置。

系统包管理器：

- 检测 `RUN apt-get` 或 `RUN apt `：注入 apt/ubuntu/debian mirror 配置。
- 检测 `RUN apk add`：注入 Alpine repository 配置。
- 检测 `RUN yum install` 或 `RUN dnf install`：注入 yum/dnf repo 配置。
- `gcc`、`build-essential`、`make` 等构建工具依赖由系统包管理器 mirror 覆盖。

语言包管理器：

- 检测 pip：插入 `ENV PIP_INDEX_URL=...`，必要时配置 trusted host。
- 检测 npm/yarn/pnpm：插入 `ENV NPM_CONFIG_REGISTRY=...`，必要时写 `.npmrc`。
- 检测 Go：插入 `ENV GOPROXY=...`。
- 检测 Cargo：在首次 cargo 命令前写 `$CARGO_HOME/config.toml` 或 `.cargo/config.toml`。

无法可靠覆盖的场景：

- `RUN ./install.sh` 内部再执行 pip/npm/apt。
- 写死 URL 的 `curl https://...`。
- 私有源、私有 registry、认证源。
- 项目主动覆盖镜像源配置。

这些场景只记录 warning，并交给构建代理或人工修复。

### fallback 策略

必须支持两层 fallback：

1. override 验证失败：直接用原 compose 构建。
2. patched build 失败：如果 `fallback_to_original=true`，再用原 compose build 一次。

每次 fallback 都必须写入：

- `docker_mirror_summary.json`
- `B_docker.log`
- `run_manifest.json`

这样后续判断失败原因时能区分：

- 镜像源 patch 本身失败
- 原项目构建失败
- Docker daemon 拉基础镜像失败

---

## Stage B 调整

建议把 Stage B 改为调用 `internal/docker` 的 build runtime service：

```go
runtime, mirrorSummary, err := dockerService.StartRuntime(ctx, docker.StartRuntimeRequest{
    ProjectPath:  project.Path,
    ArtifactRoot: run.ArtifactRoot,
    TaskID:       project.TaskID,
    RunID:        run.RunID,
    Progress:     progress,
})
```

Stage B 自身只负责：

- 把 Docker service 的结果写为 stage record。
- 写 required artifact。
- 将 `RuntimeState` 返回给 Stage C 和 cleanup。

命令策略：

1. `docker compose config` 验证原始 compose。
2. 准备 mirror override。
3. `pull` 变成 `best_effort`，失败记录 warning，不立即失败。
4. `build` 使用 mirror override；失败后按配置 fallback 原 build。
5. `up -d` 使用最终选定的 compose file set。
6. `ps --format json` 和 `docker port` fallback 使用最终 compose file set。

Stage B 新增 artifact：

- `docker_mirror_summary.json`
- `docker_compose_effective_config.yml` 或 `docker_compose_effective_config.json`
- `docker_runtime_summary.json` 可选，或扩展现有 `port_map.json`

---

## Stage C 调整

短期建议：

- 继续消费 Stage B 返回的 in-memory `RuntimeState`。
- `test_runtime_summary.json` 记录 build mirror 是否启用、compose project、compose files。
- 对缺失 runtime evidence 的错误文案加清晰分类：`missing_runtime_evidence`、`missing_port_mapping`、`stage_b_failed`。

中期评估：

- 是否恢复旧设计中的容器内/Compose 网络内测试。
- 如果恢复，可由 Docker service 提供：

  ```text
  docker compose exec <service> ...
  docker compose run --rm <service> ...
  ```

  但必须明确如何选择测试 service，以及如何处理只提供 host run_tests.sh 的项目。

本轮不建议同时做 Stage C 执行模型大改，避免和 Stage B 构建稳定性混在一起。

---

## Docker GC 方案

### 安全原则

默认只清理 p2r 管理的资源。

可以识别的资源：

- compose project name 前缀：`p2rqa_`
- compose label：`com.docker.compose.project=<project>`
- p2r label：`managed_by=p2rqa`
- artifact 中记录的 compose project

默认不清：

- 非 p2r 容器
- 非 p2r volume
- 非 p2r image
- 全局 `docker system prune -a --volumes`

### GC 调度

新增 maintenance service，而不是放到 scheduler：

```text
internal/maintenance/docker_gc.go
```

触发点：

- TUI 启动时，如果超过 `docker.gc.interval`，尝试运行一次。
- CLI `p2r run` 前默认不跑，除非配置 `run_before_cli_run=true`。
- 提供手动命令：

  ```text
  p2r admin docker-gc run --dry-run
  p2r admin docker-gc run --yes
  p2r admin docker-gc status
  ```

状态文件：

```text
<scan_path>/.qa-control/docker_maintenance_state.json
```

锁文件：

```text
<scan_path>/.qa-control/locks/docker-maintenance.lock
```

运行条件：

- 如果 TUI scheduler 有 running job，则跳过 GC。
- 如果 task run lock 存在但不是当前任务，保守跳过。
- GC 命令必须有 timeout。
- GC summary 必须记录 dry-run、实际删除、失败、跳过原因。

### GC 动作

MVP 默认动作：

```text
docker container prune --force --filter label=managed_by=p2rqa
docker network prune --force --filter label=managed_by=p2rqa
docker builder prune --force --filter until=<duration>
```

谨慎动作，默认关闭：

```text
docker volume prune --force --filter label=managed_by=p2rqa
docker image prune --force --filter label=managed_by=p2rqa
```

如果 Docker CLI 对某些 prune filter 支持不稳定，则改为：

1. `docker ps -a --filter label=... --format json`
2. 明确列出 IDs
3. `docker rm` / `docker network rm` / `docker volume rm`

这样更可审计，也更容易写测试。

---

## artifact 与 TUI 展示

新增或扩展 artifact：

```text
docker_mirror_summary.json
docker_gc_summary.json
daemon_mirror_summary.json
pre_run_cleanup_summary.json
cleanup_summary.json
port_map.json
```

`docker_mirror_summary.json` 建议字段：

```json
{
  "enabled": true,
  "mode": "patch_dockerfile",
  "profile": "cn",
  "services": [
    {
      "service": "web",
      "context": "/abs/repo/web",
      "original_dockerfile": "/abs/repo/web/Dockerfile",
      "patched_dockerfile": "/abs/artifacts/docker_mirror/web.Dockerfile.p2r",
      "patched": true,
      "skipped_reason": "",
      "injected": ["apt", "npm", "pip"]
    }
  ],
  "override_file": "/abs/artifacts/docker_mirror/compose.mirror.override.yml",
  "override_verified": true,
  "fallback_used": false,
  "warnings": []
}
```

TUI 详情页可在 cleanup 区域附近增加 Docker runtime 区块：

- daemon mirror 状态
- daemon mirror 期望配置来源：项目级 `.p2r.yaml`
- build mirror 状态
- fallback 是否发生
- GC 最近一次运行状态
- cleanup 状态

TUI 可增加 Docker maintenance / mirror 配置视图：

- 编辑 daemon mirror 期望配置，并保存到项目级 `.p2r.yaml`
- 刷新只读 status
- 执行带强确认的 apply
- 从 backup 列表执行带强确认的 restore

总览页暂时不新增列，避免表格过宽。

---

## 测试计划

### `tests/internal/config`

- 解析 `docker.daemon_mirrors`
- 解析 `docker.build_mirrors`
- 解析 `docker.gc`
- 校验 interval/duration、pull_policy、mode 枚举

### `tests/internal/docker`

新增覆盖：

- compose build string/object 解析
- dockerfile path 相对 context / compose file 的解析
- `docker compose -f original -f override` 参数构造
- pull policy：required/best_effort/skip
- cleanup 支持多个 compose files
- GC dry-run 列表生成
- daemon.json merge/backup/restore

### `tests/internal/tui`

- daemon mirror 配置面板将期望配置保存到项目级 `.p2r.yaml`
- status 操作只读，不写 daemon.json
- apply/restore 必须强确认
- 权限失败时展示等价 CLI 命令

Dockerfile patch 测试：

- 保留 `# syntax=` directive
- 单阶段 apt/npm/pip 注入
- 多阶段每个 stage 独立注入
- `scratch` stage 不插 RUN
- `dockerfile_inline` 跳过并记录
- 多行 `RUN \` 不被破坏
- `RUN --mount=type=cache` 保留参数

### `tests/internal/pipeline`

- Stage B pull 失败但 build/up 成功时，Stage B 不失败，summary 记录 warning。
- mirror override 验证失败时 fallback 原 compose。
- patched build 失败时按配置 fallback 原 build。
- `port_map.json` 保留 runtime cleanup target，即使 artifact 写入失败。
- abort/crash 路径仍写 cleanup summary。
- Stage C 缺少 runtime evidence 时错误分类清晰。

### 可选集成测试

在有 Docker 的 CI 或手工环境中增加：

- 一个 Debian/apt Dockerfile 示例
- 一个 Node/npm Dockerfile 示例
- 一个 Python/pip Dockerfile 示例
- 一个 Go/GOPROXY Dockerfile 示例
- 一个多阶段 Dockerfile 示例

普通 `go test ./...` 不依赖真实 Docker。

---

## 分阶段实施

### Phase 1：Docker 包边界重构，不改行为

目标：

- 把 compose helper、port mapping、probe、runtime state 迁到 `internal/docker`。
- Stage B 仍保持当前命令顺序。
- 测试迁移，确保行为一致。

收益：

- 后续 mirror/GC 不再继续扩大 `stage_b.go`。

### Phase 2：修 Stage B pull/build 策略

目标：

- 增加 `pull_policy`。
- 默认 `best_effort`。
- pull 失败不直接失败 Stage B。
- build/up 日志和错误摘要更清晰。

收益：

- 降低 `[Image #1] Error response...` 这类拉取失败对 Stage B 的误伤。

### Phase 3：daemon mirror admin 能力

目标：

- 增加 daemon mirror 配置。
- 增加 status/apply/restore admin 命令。
- 写备份和 summary。
- 增加 TUI 图形化 daemon mirror 配置入口，保存目标为项目级 `.p2r.yaml`。

收益：

- 解决 `FROM` 和 `docker pull` 层面的基础镜像拉取问题。
- 质检员无需手写 YAML 即可维护 mirror 期望配置，同时保留 daemon 写入审计链路。

### Phase 4：临时 Dockerfile patch MVP

目标：

- 支持 compose build string/object。
- 生成 patched Dockerfile 和 override。
- `docker compose config` 验证 override。
- 支持 apt/apk/yum/npm/pip/go/cargo 常见注入。
- 支持 fallback 原 build。

收益：

- 解决 Dockerfile 内部依赖安装源问题。

### Phase 5：Docker GC

目标：

- 增加 maintenance service。
- 增加 state/lock。
- 增加 TUI startup opportunistic GC。
- 增加 admin docker-gc dry-run/run/status。
- 默认只清 p2r 管理资源。

收益：

- 控制磁盘膨胀，减少环境冲突。

### Phase 6：Stage C 执行模型评估

目标：

- 明确 Stage C 是否需要回到容器内/Compose 网络内执行。
- 如果需要，单独设计测试 service 选择策略和 fallback。

收益：

- 避免把 Stage C 大改混入 Docker 构建/GC 重构，降低风险。

---

## 风险与缓解

| 风险 | 缓解 |
|------|------|
| 临时 Dockerfile patch 破坏复杂 Dockerfile | parser + 保守注入 + override config 验证 + fallback 原 build |
| 修改 daemon.json 影响机器所有 Docker 用户 | 专用质检机假设 + 备份 + status/apply/restore + 默认需要显式确认 |
| GC 误删非 p2r 资源 | 默认 p2r_only + label/project 前缀过滤 + dry-run + summary |
| Compose override 路径语义错误 | 使用绝对路径 + `docker compose config` 预验证 |
| Stage B 失败原因变模糊 | mirror/build/up/pull 各自产出结构化 summary |
| Stage C 语义争议 | 本轮先不大改，文档记录旧设计与新迭代计划冲突 |

---

## 建议验收标准

1. Stage B 普通 compose 项目仍能启动并写出 `port_map.json`。
2. `docker compose pull` 失败但 build/up 成功时，Stage B 成功，summary 记录 pull warning。
3. 启用 daemon mirror 后，`p2r admin docker-mirror status` 能显示配置一致性。
4. TUI 能编辑 daemon mirror 期望配置并保存到项目级 `.p2r.yaml`；保存动作不修改 `/etc/docker/daemon.json`。
5. TUI mirror apply/restore 必须复用 daemon mirror service、要求强确认，并写 `daemon_mirror_summary.json`。
6. 启用 Dockerfile patch 后，artifact 下存在 patched Dockerfile、override 和 summary，repo 目录无改动。
7. patch 验证失败或 patched build 失败时能自动 fallback，并记录清楚。
8. TUI 启动时不会在 active job 运行中触发 GC。
9. GC dry-run 能列出待清理 p2r 资源，不删除任何资源。
10. GC run 只删除 p2r 管理资源，并写 `docker_gc_summary.json`。
11. `go test ./...` 不需要真实 Docker。
12. 真实 Docker 手工样例覆盖 apt/npm/pip/go/cargo 至少各一个。
