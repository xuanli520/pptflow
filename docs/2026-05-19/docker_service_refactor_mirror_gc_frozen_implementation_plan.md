# Docker 服务重构、镜像源与 GC 冻结实施计划

> 状态：Frozen
> 日期：2026-05-19
> 输入依据：`docs/2026-05-19/docker_service_refactor_mirror_gc_plan.md`
> 当前代码核对范围：`internal/docker`、`internal/pipeline/stage_b.go`、`internal/pipeline/compose.go`、`internal/pipeline/stage_c.go`、`internal/pipeline/cleanup.go`、`internal/pipeline/runtime_state.go`、`internal/config/config.go`、`cmd/*`、`internal/tui/*`、`internal/scheduler/*`、`tests/internal/*`

本文件是冻结版开发实现计划。它不落地业务代码，但冻结模块边界、默认值、artifact schema、fallback 语义、测试门禁和分阶段写集。后续实现应按本计划拆分独立提交，不把行为重构、Dockerfile patch、daemon 配置和 GC 混在同一个提交里。

---

## 1. 冻结结论

### 1.1 必须解决的问题

1. Stage B 当前仍直接拼接并执行 Docker Compose 命令，且 `docker compose pull --ignore-buildable` 是 required，拉取失败会直接导致 Stage B 失败。
2. `internal/docker` 当前只有 compose project name 与 cleanup helper，不是完整 Docker service。
3. runtime evidence 当前只能表示单个 `compose_file`，无法表达原 compose + mirror override 的 compose file set。
4. Docker cleanup 逻辑分散在 pipeline lifecycle、cleanup、recovery 中，无法承载全局维护型 GC。
5. `.p2r.yaml` 的 `docker` 配置缺少 pull policy、daemon mirrors、build mirrors 和 GC 配置。
6. Docker daemon mirror 只能解决 `FROM`、`docker pull`、compose pull/up 的基础镜像拉取；Dockerfile 内部 `RUN pip/npm/apt/apk/yum/go/cargo` 必须通过 build 环境注入，本轮采用 run artifact 下的临时 Dockerfile patch。

### 1.2 本轮最终交付范围

本轮开发完成后应具备：

1. `internal/docker` 成为 Stage B、runtime evidence、Compose、mirror、cleanup 的 Docker service 包。
2. Stage B 调用 Docker service，不再在 stage 代码中直接维护 pull/build/up/ps/port/probe 的命令细节。
3. `docker.pull_policy` 支持 `required | best_effort | skip`，默认 `best_effort`。
4. 支持 `p2r admin docker-mirror status/apply/restore`，以可审计方式管理 daemon registry mirrors。
5. 支持 TUI 图形化配置 daemon registry mirrors，保存目标固定为项目级 `.p2r.yaml`。
6. 支持 artifact-scoped Dockerfile patch 和 compose override，不修改用户交付目录 `repo/`。
7. 支持保守 Docker GC，默认只清 p2r 可归属资源，提供 dry-run、status 和显式 run。
8. Stage C 本轮不改执行模型，仍在宿主机运行 `bash run_tests.sh`；仅增强 runtime evidence summary 和缺失分类。
9. 所有镜像源注入、跳过、fallback、daemon 变更和 GC 动作都有结构化 artifact。
10. `go test ./...` 在没有真实 Docker daemon、甚至没有 `docker` binary 的环境中通过。

### 1.3 明确非目标

1. 不修改用户交付目录 `repo/`。
2. 不引入 Docker SDK，继续通过 Docker CLI 和 `executor.CommandRunner` 调用。
3. 不默认执行 `docker system prune -a --volumes` 之类的无差别全局 prune。
4. 不把 scheduler 改造成 GC 调度器；scheduler 只提供 active job 状态给 maintenance 判断。
5. 不在本轮恢复 Stage C 的容器内或 Compose 网络内执行模型。
6. 不承诺覆盖所有 Dockerfile 场景。MVP 只支持常见 compose + Dockerfile build string/object。
7. 不支持 README 中自由拼接的 compose 启动命令做 Dockerfile patch。README compose 路径继续可启动，但 mirror patch 跳过并记录 warning。

---

## 2. 当前代码锚点

以下是冻结计划基于当前工作区核对后的事实：

| 主题 | 当前状态 | 影响 |
|---|---|---|
| Docker 包 | `internal/docker/manager.go` 只有 `ComposeProjectName`、cleanup args、compose down、shell quote | 需要扩展为 service facade |
| Stage B | `internal/pipeline/stage_b.go` 直接执行 `pull/build/up/ps/config/port/probe` | 需要下沉到 `internal/docker` |
| pull 行为 | `pull --ignore-buildable` 是 required | 改为 `pull_policy` 控制 |
| Compose helper | `internal/pipeline/compose.go` 内含 compose 发现、README 命令解析、ps JSON、probe 类型 | 迁移到 `internal/docker` |
| RuntimeState | `internal/pipeline/runtime_state.go` 只有 `ComposeFile string` | 增加 `ComposeFiles []string`，保留旧字段兼容 |
| Stage C | `internal/pipeline/stage_c.go` 在宿主机执行 `bash run_tests.sh` | 本轮保留，只增强 summary |
| cleanup | `internal/pipeline/cleanup.go` 同时处理 task lock、stale cleanup、current cleanup | 当前 runtime cleanup 保留生命周期语义，Docker 命令实现迁移 |
| config | `internal/config/config.go` 的 DockerConfig 只有 cleanup/runtime 字段 | 新增 pull/mirror/gc raw/default/validate |
| CLI | `cmd/root.go` 没有 `admin` 命令 | 新增 `p2r admin ...` |
| TUI | `internal/tui/app.go` 启动 scheduler 和 stale recovery，无 GC hook，也没有 daemon mirror 图形化配置入口 | 增加 mirror 配置视图和 opportunistic GC hook，不能阻塞 TUI |

---

## 3. 冻结架构

### 3.1 目录边界

```text
internal/docker/
  manager.go              # Service facade、公共 request/result、兼容旧 helper
  command.go              # Docker CLI 调用、streaming、timeout、env、日志摘要
  compose.go              # compose 发现、README command 解析、compose file set、config 解析
  runtime.go              # RuntimeState、port mapping、probe、ps/port fallback
  build.go                # StartRuntime 编排：config/pull/build/up/ps/probe
  mirrors.go              # mirror profile、summary、daemon mirror 纯函数
  dockerfile_patch.go     # Dockerfile parser-guided patch 与 override 生成
  cleanup.go              # compose project cleanup，支持多个 compose files
  gc.go                   # Docker resource discovery、dry-run/run plan
  lock.go                 # maintenance lock

internal/maintenance/
  docker_gc.go            # GC status、state file、trigger policy、TUI/CLI orchestration

cmd/
  admin.go                # p2r admin root
  admin_docker_mirror.go  # docker-mirror status/apply/restore
  admin_docker_gc.go      # docker-gc status/run
```

### 3.2 pipeline 保留职责

`internal/pipeline` 保留：

1. stage 状态转换和 `StageRecord`。
2. artifact required/best-effort 持久化。
3. DB stage/finding/run 持久化。
4. `RunProgress` 事件。
5. abort/crash/finalize 语义。
6. Stage B/C/F 等业务 stage 的 findings 和错误摘要。

`internal/pipeline` 不再维护 Docker 命令细节。

### 3.3 Docker service facade

新增 facade 形态：

```go
type Service struct {
    Exec executor.CommandRunner
    Config config.DockerConfig
}

type StartRuntimeRequest struct {
    ProjectPath  string
    RepoPath     string
    ArtifactRoot string
    TaskID       string
    RunID        string
    Progress     func(docker.ProgressEvent)
    Timeouts     RuntimeTimeouts
}

type StartRuntimeResult struct {
    Runtime        RuntimeState
    MirrorSummary  BuildMirrorSummary
    RuntimeSummary DockerRuntimeSummary
    EffectiveConfigPath string
    LogHints       []string
    Warnings       []string
}
```

pipeline 的 Stage B wrapper 只负责：

1. 打开 `logs/B_docker.log` 并将 stream 转发到 progress。
2. 调用 `docker.Service.StartRuntime(...)`。
3. 写 `port_map.json`、`docker_mirror_summary.json`、`docker_runtime_summary.json`、`docker_compose_effective_config.yml`。
4. 根据 Docker service result 生成 `StageRecord`、findings 和 cleanup target。

---

## 4. 配置冻结

### 4.1 `.p2r.yaml` schema

```yaml
docker:
  managed_label: "managed_by=p2rqa"
  compose_project_prefix: "p2rqa"
  keep_runtime: false

  pull_policy: "best_effort"   # required | best_effort | skip

  daemon_mirrors:
    enabled: false
    daemon_json: "/etc/docker/daemon.json"
    backup_dir: "./projects-qa/.qa-control/docker-daemon-backups"
    registry_mirrors: []
    require_manual_apply: true

  build_mirrors:
    enabled: true
    mode: "patch_dockerfile"   # off | env_only | patch_dockerfile
    fallback_to_original: true
    verify_override: true
    profile: "cn"
    apt_mirror: ""
    ubuntu_mirror: ""
    apk_mirror: ""
    yum_mirror: ""
    npm_registry: "https://registry.npmmirror.com"
    pip_index_url: "https://pypi.tuna.tsinghua.edu.cn/simple"
    go_proxy: "https://goproxy.cn,direct"
    cargo_registry: ""

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
    prune_builder_cache: false
    builder_cache_until: "72h"
```

### 4.2 默认值决策

| 配置 | 冻结默认值 | 原因 |
|---|---:|---|
| `pull_policy` | `best_effort` | 解决 pull 网络波动误伤 Stage B |
| `daemon_mirrors.enabled` | `false` | 系统级配置不默认启用 |
| `daemon_mirrors.require_manual_apply` | `true` | 禁止静默修改 `/etc/docker/daemon.json` |
| `build_mirrors.enabled` | `true` | 质检机默认需要构建换源能力 |
| `build_mirrors.mode` | `patch_dockerfile` | Dockerfile 内 RUN 依赖安装只有 patch 才可靠 |
| `build_mirrors.fallback_to_original` | `true` | patch 失败不应掩盖原项目可构建性 |
| `build_mirrors.verify_override` | `true` | override 必须先通过 compose config |
| `gc.enabled` | `true` | 需要自动维护机制 |
| `gc.run_on_tui_start` | `true` | TUI 是长期运行入口，适合 opportunistic GC |
| `gc.run_before_cli_run` | `false` | CLI run 不默认被维护任务延迟 |
| `gc.p2r_only` | `true` | 默认只清 p2r 可归属资源 |
| `gc.prune_volumes` | `false` | volume 误删风险高 |
| `gc.prune_images` | `false` | image 误删和重拉成本高 |
| `gc.prune_builder_cache` | `false` | builder cache 无可靠 p2r label 归属，默认关闭 |

`gc.prune_builder_cache=true` 是显式 opt-in。启用后 summary 必须标记 `scope: "global_builder_cache_by_age"`，并记录它不是严格 p2r-only 资源。

### 4.3 validation 规则

1. `pull_policy` 只能是 `required`、`best_effort`、`skip`。
2. `build_mirrors.mode` 只能是 `off`、`env_only`、`patch_dockerfile`。
3. `gc.interval`、`gc.builder_cache_until`、`build_cache_prune_until` 必须是 Go duration 字符串，至少支持 `24h`、`72h`。
4. `daemon_mirrors.daemon_json` 为空时 validation fail。
5. `daemon_mirrors.backup_dir` 为空时 validation fail。
6. `gc.p2r_only=false` 允许解析，但默认命令和 TUI startup 不使用非 p2r-only GC；只有 admin `docker-gc run --yes --allow-global` 可执行全局范围动作。
7. `build_mirrors.enabled=true` 且 `mode=patch_dockerfile` 时，所有 mirror URL 可为空；为空表示对应包管理器不注入，只记录 warning。

### 4.4 TUI 配置持久化

daemon mirror 的 TUI 配置保存目标固定为项目级 `.p2r.yaml`：

1. TUI 可以创建或更新项目级 `.p2r.yaml` 的 `docker.daemon_mirrors` 节点。
2. 保存动作只更新 p2r 期望配置，不修改 `/etc/docker/daemon.json`。
3. 如果当前运行配置来自用户级配置或 `P2R_CONFIG`，TUI 仍写项目级 `.p2r.yaml`，并在保存前展示目标文件路径。
4. 保存后提示用户重新加载配置或由 TUI 触发内部 config reload。
5. 写 YAML 必须保留与 `config.Load` 兼容的字段名；不得写 `.qa-control` 私有 override 作为 mirror 配置真源。

---

## 5. RuntimeState 与 artifact 兼容

### 5.1 RuntimeState 升级

新增字段，保留旧字段：

```go
type RuntimeState struct {
    ComposeProject string `json:"compose_project"`
    ComposeFile    string `json:"compose_file,omitempty"`
    ComposeFiles   []string `json:"compose_files,omitempty"`
    WorkDir        string `json:"work_dir"`
    Services       []string `json:"services"`
    Mappings       map[string][]PortMapping `json:"mappings"`
    Probes         []ProbeResult `json:"probes"`
    Mirror         RuntimeMirrorState `json:"mirror,omitempty"`
}
```

兼容规则：

1. 读取旧 artifact 时，如果 `compose_files` 为空且 `compose_file` 非空，则 `ComposeFiles = []string{ComposeFile}`。
2. 写新 artifact 时同时写 `compose_file` 和 `compose_files`。`compose_file` 固定为最终有效 compose file set 的第一个文件，通常是原始 compose。
3. cleanup、Stage C env、recovery 全部使用 `ComposeFiles`。旧 `ComposeFile` 只用于兼容显示和旧 artifact。
4. Stage C 继续注入 `COMPOSE_FILE`，值为 `ComposeFiles` 按平台 path list separator 拼接后的字符串；同时在 `test_runtime_summary.json` 写数组字段 `compose_files`。

### 5.2 Stage B required artifacts

Stage B 执行后必须写：

```text
logs/B_docker.log
port_map.json
docker_runtime_summary.json
docker_mirror_summary.json
docker_compose_effective_config.yml
docker_startup.png
```

失败路径也应尽量写：

1. `logs/B_docker.log`
2. `port_map.json`，包含 cleanup target 和失败 reason。
3. `docker_runtime_summary.json`
4. `docker_mirror_summary.json`

如果 required artifact 写失败，Stage B 仍失败，但 in-memory `RuntimeState` 必须返回 cleanup target，延续现有 `port_map.json` 写失败仍可 cleanup 的测试语义。

### 5.3 `docker_runtime_summary.json`

冻结 schema：

```json
{
  "ok": true,
  "run_id": "run-...",
  "task_id": "TASK-...",
  "compose_project": "p2rqa_task_run_hash",
  "compose_file": "/abs/repo/docker-compose.yml",
  "compose_files": ["/abs/repo/docker-compose.yml", "/abs/run/docker_mirror/compose.mirror.override.yml"],
  "work_dir": "/abs/repo",
  "pull_policy": "best_effort",
  "pull": {
    "status": "warning",
    "required": false,
    "exit_code": 1,
    "error": "..."
  },
  "build": {
    "status": "ok",
    "fallback_used": false,
    "using_mirror_override": true
  },
  "up": {
    "status": "ok"
  },
  "port_collection": {
    "status": "ok",
    "method": "compose_ps_json"
  },
  "warnings": []
}
```

### 5.4 `port_map.json`

保留现有消费者需要的字段，并扩展：

```json
{
  "run_id": "run-...",
  "compose_project": "p2rqa_task_run_hash",
  "compose_file": "/abs/repo/docker-compose.yml",
  "compose_files": ["/abs/repo/docker-compose.yml"],
  "work_dir": "/abs/repo",
  "services": ["web"],
  "mappings": {},
  "probes": [],
  "labels": {
    "managed_by": "p2rqa",
    "p2r.task_id": "TASK-...",
    "p2r.run_id": "run-..."
  },
  "cleanup_command": "docker compose -f ... -p ... down --remove-orphans",
  "runtime_summary": "docker_runtime_summary.json",
  "docker_mirror_summary": "docker_mirror_summary.json"
}
```

### 5.5 既有 cleanup artifact

以下既有 artifact 继续保留，并在多 compose file 后兼容 `compose_files`：

1. `pre_run_cleanup_summary.json`：preflight 后清理同 task 历史 run 时写入。
2. `cleanup_summary.json`：normal、abort、crash、recovery cleanup 都必须尽力写入。
3. `run_manifest.json`：继续合并 `pre_run_cleanup` 与 `cleanup`，并新增 `docker_runtime`、`docker_mirror`、`docker_gc` 摘要引用。

---

## 6. Stage B 冻结行为

### 6.1 执行流程

Stage B 使用以下顺序：

1. 查找 repo 内 compose file。
2. 如果没有 compose file，则读取 README compose command。README 路径保留原启动能力，但跳过 Dockerfile patch。
3. `docker compose -f <compose> -p <project> config` 验证原始 compose。
4. 准备 build mirror：
   - `build_mirrors.enabled=false` 或 `mode=off`：不生成 override。
   - `mode=env_only`：仅生成 build args override，记录 coverage 限制。
   - `mode=patch_dockerfile`：生成 patched Dockerfile 和 override。
5. 如果生成 override，则执行 `docker compose -f <compose> -f <override> -p <project> config` 验证。
6. override 验证失败时，回退原 compose file set，并记录 fallback。
7. 按 `pull_policy` 执行 pull。
8. build 使用当前有效 compose file set。
9. patched build 失败且 `fallback_to_original=true` 时，使用原 compose file set 再 build 一次。
10. up 使用最终成功 build 对应的 compose file set。
11. `ps --format json` 和 `docker port` fallback 使用最终 compose file set。
12. 写 runtime evidence 和 summaries。

### 6.2 pull policy

| policy | 是否执行 pull | pull 失败行为 |
|---|---:|---|
| `required` | 是 | Stage B failed |
| `best_effort` | 是 | 记录 warning，继续 build/up |
| `skip` | 否 | 记录 skipped，继续 build/up |

pull 命令仍使用：

```text
docker compose -f <file...> -p <project> pull --ignore-buildable
```

如果 README command 路径无法可靠构造 pull/build，pull/build 阶段按当前行为跳过或降级，并在 summary 中写 `readme_command_mode=true` 和 warning。

### 6.3 fallback 状态机

冻结状态：

| 状态 | 后续 compose file set |
|---|---|
| 原始 compose config 失败 | Stage B failed |
| mirror 准备无可 patch 服务 | 原 compose |
| override 生成成功且 config 验证成功 | 原 compose + override |
| override config 失败 | 原 compose |
| patched build 成功 | 原 compose + override |
| patched build 失败，fallback 成功 | 原 compose |
| patched build 失败，fallback 失败 | Stage B failed |

最终 `RuntimeState.ComposeFiles` 必须等于 up/ps/down/cleanup 使用的 compose file set。

每一次 fallback 都必须同时落三类证据：

1. `logs/B_docker.log` 追加 fallback 起因、失败命令摘要和最终选中的 compose file set。
2. `docker_mirror_summary.json` 或 `docker_runtime_summary.json` 写 `fallback_used=true`、`fallback_reason`、`fallback_from`、`fallback_to`。
3. `run_manifest.json` 合并 `docker_runtime` 摘要，至少包含 pull policy、mirror mode、fallback 状态和最终 compose files。

### 6.4 Stage B findings

Stage B 失败 findings 的 evidence 必须区分：

1. `compose_config_failed`
2. `pull_failed_required`
3. `build_failed`
4. `patched_build_failed_and_fallback_failed`
5. `up_failed`
6. `port_inspection_failed`
7. `no_published_ports`
8. `artifact_write_failed`

pull best-effort 失败不生成 High finding，只写 warning。

---

## 7. Dockerfile patch 冻结设计

### 7.1 parser 选择

Phase 4 新增 Dockerfile parser 依赖：

```text
github.com/moby/buildkit/frontend/dockerfile/parser
github.com/moby/buildkit/frontend/dockerfile/instructions
```

该依赖只用于 Dockerfile 语法解析和 stage/RUN 定位，不引入 Docker SDK，不连接 Docker daemon。

实现不得对整个 Dockerfile 做无上下文正则替换。允许使用 parser 定位 stage 与 instruction 后，对原始行数组执行受控插入，以保留原文件主体内容和顶部 directive。

### 7.2 compose build 支持范围

MVP 支持：

```yaml
services:
  web:
    build: ./web
```

```yaml
services:
  web:
    build:
      context: ./web
      dockerfile: Dockerfile
      target: production
      args:
        NODE_ENV: production
```

必须保留 build object 中除 `context`、`dockerfile` 以外的字段，包括但不限于：

```text
args, target, platforms, ssh, secrets, cache_from, cache_to, labels,
network, extra_hosts, no_cache, pull
```

首版跳过并记录：

1. `dockerfile_inline`
2. remote git/http context
3. context 为空或无法解析
4. Dockerfile 不存在
5. README compose command
6. `extends` 或多文件 compose 中无法归属的 build definition
7. build context 在 repo 外部

### 7.3 artifact 输出

```text
<artifactRoot>/docker_mirror/
  web.Dockerfile.p2r
  api.Dockerfile.p2r
  compose.mirror.override.yml
docker_mirror_summary.json
```

override 示例：

```yaml
services:
  web:
    build:
      context: /abs/project/repo/web
      dockerfile: /abs/artifacts/docker_mirror/web.Dockerfile.p2r
      target: production
      args:
        NODE_ENV: production
```

### 7.4 `docker_mirror_summary.json`

冻结 schema：

```json
{
  "enabled": true,
  "mode": "patch_dockerfile",
  "profile": "cn",
  "repo_modified": false,
  "compose_file": "/abs/repo/docker-compose.yml",
  "compose_files": ["/abs/repo/docker-compose.yml", "/abs/artifacts/docker_mirror/compose.mirror.override.yml"],
  "override_file": "/abs/artifacts/docker_mirror/compose.mirror.override.yml",
  "override_generated": true,
  "override_verified": true,
  "fallback_used": false,
  "fallback_reason": "",
  "services": [
    {
      "service": "web",
      "context": "/abs/repo/web",
      "original_dockerfile": "/abs/repo/web/Dockerfile",
      "patched_dockerfile": "/abs/artifacts/docker_mirror/web.Dockerfile.p2r",
      "patched": true,
      "skipped_reason": "",
      "injected": ["apt", "npm", "pip"],
      "warnings": []
    }
  ],
  "warnings": []
}
```

### 7.5 注入规则

通用原则：

1. 保留 `# syntax=`、`# escape=` 等顶部 parser directive。
2. 多阶段构建按 stage 独立检测和注入。
3. 只在检测到相关 package manager 命令时注入对应配置。
4. `FROM scratch` 或 image 名包含 `distroless` 的 stage 不插入新的 `RUN`。
5. 如果 stage 内没有任何 `RUN`，不插入 package manager `RUN`。
6. `RUN --mount=type=cache ...` 的 flags 必须保留。
7. 多行 `RUN \` 不能被拆坏。
8. 对 `RUN ./install.sh`、写死 URL、私有源、认证源、项目主动覆盖源配置，只记录 warning，不尝试深度改写。

包管理器注入：

| 检测 | 注入 |
|---|---|
| `RUN apt-get` 或 `RUN apt ` | 在首次 apt RUN 前插入 `RUN`，备份并替换 `/etc/apt/sources.list`、`/etc/apt/sources.list.d/*.sources` 中已知 Debian/Ubuntu 官方源 URL |
| `RUN apk add` | 在首次 apk RUN 前插入 `RUN sed -i` 替换 `/etc/apk/repositories` 的官方 Alpine 源 |
| `RUN yum install` 或 `RUN dnf install` | 在首次 yum/dnf RUN 前插入保守 repo 替换块；无法识别 repo 文件时 warning |
| `pip install`、`python -m pip` | 在首次 pip RUN 前插入 `ENV PIP_INDEX_URL=...`；HTTP 源额外生成 `PIP_TRUSTED_HOST` |
| `npm install`、`npm ci`、`yarn`、`pnpm` | 在首次 npm/yarn/pnpm RUN 前插入 `ENV NPM_CONFIG_REGISTRY=...` |
| `go mod download`、`go build`、`go test` | 在首次 go RUN 前插入 `ENV GOPROXY=...` |
| `cargo build`、`cargo test`、`cargo fetch` | 在首次 cargo RUN 前插入 `RUN mkdir -p "${CARGO_HOME:-/usr/local/cargo}" ... config.toml` |

apt mirror 决策：

1. 替换 Debian 源时优先用 `apt_mirror`。
2. 替换 Ubuntu 源时优先用 `ubuntu_mirror`。
3. 对无法识别发行版或 mirror 为空的情况，只记录 warning，不合成 sources.list。

### 7.6 `env_only` 语义

`mode=env_only` 不改 Dockerfile，只生成 compose override 中的 `build.args`，并记录：

```json
{
  "mode": "env_only",
  "coverage": "requires Dockerfile ARG usage; does not affect arbitrary RUN environment"
}
```

该模式主要用于项目 Dockerfile 已显式声明并使用 `ARG PIP_INDEX_URL`、`ARG NPM_CONFIG_REGISTRY` 等变量的场景。它不承诺覆盖 apt/apk/yum，也不作为默认推荐模式。

---

## 8. daemon mirror 冻结设计

### 8.1 命令

```text
p2r admin docker-mirror status
p2r admin docker-mirror apply --yes
p2r admin docker-mirror restore --backup <path> --yes
```

### 8.2 行为

1. `status` 只读，不写任何文件。
2. `apply` 必须要求 `--yes`。如果 `require_manual_apply=true` 且没有 `--yes`，直接失败并打印将要写入的 path 和 mirrors。
3. `apply` 读取 `daemon_json`，保留未知字段，只合并或替换 `registry-mirrors`。
4. 写入前必须备份到 `backup_dir`。
5. backup 文件名包含 UTC 时间和原文件 sha256 前缀。
6. `restore` 必须要求 `--backup` 和 `--yes`。
7. CLI 不自动 sudo，不自动重启 Docker。权限不足时返回清晰错误，并提示使用有权限的 shell 运行。
8. 写入后提示操作者执行 `sudo systemctl restart docker` 或按本机 Docker 管理方式重启。

### 8.3 TUI 图形化管理

TUI 必须复用 daemon mirror service，不允许另写一套 daemon.json 读写逻辑。

配置面板能力：

1. 编辑 `enabled`、`daemon_json`、`backup_dir`、`registry_mirrors`、`require_manual_apply`。
2. `Save Config` 写项目级 `.p2r.yaml`，不写 daemon.json。
3. `Refresh Status` 只读，显示 current mirrors、desired mirrors、diff、backup dir、daemon path、一致性状态。
4. `Apply` 先展示 daemon path、registry mirror diff、backup path、restart notice，再要求强确认。
5. `Restore` 从 backup 列表选择文件，展示 before/after 信息后要求强确认。
6. 权限不足时展示错误和等价 CLI 命令，例如 `sudo p2r admin docker-mirror apply --yes`。
7. 每次 apply/restore 都写同一份 `daemon_mirror_summary.json`。

TUI 不内置 sudo 密码输入，不自动重启 Docker。

### 8.4 `daemon_mirror_summary.json`

admin 命令将 summary 写到：

```text
<scan_path>/.qa-control/daemon_mirror_summary.json
```

冻结 schema：

```json
{
  "operation": "apply",
  "ok": true,
  "daemon_json": "/etc/docker/daemon.json",
  "backup_path": "/abs/projects-qa/.qa-control/docker-daemon-backups/daemon-20260519T120000Z-a1b2c3d4.json",
  "before_sha256": "a1b2...",
  "after_sha256": "c3d4...",
  "current_registry_mirrors": ["https://mirror.example"],
  "desired_registry_mirrors": ["https://mirror.example"],
  "changed": true,
  "diff": {
    "registry_mirrors_added": ["https://mirror.example"],
    "registry_mirrors_removed": []
  },
  "restart_required": true,
  "restart_command": "sudo systemctl restart docker",
  "recorded_at": "2026-05-19T12:00:00Z",
  "warnings": []
}
```

无效 JSON 时：

1. 不覆盖原文件。
2. summary `ok=false`。
3. error 中包含 parse failure。
4. 如果 restore 可用，仍提示 restore 命令。

---

## 9. Docker GC 冻结设计

### 9.1 命令

```text
p2r admin docker-gc status
p2r admin docker-gc run --dry-run
p2r admin docker-gc run --yes
```

`run --yes` 和 `run --dry-run` 互斥。默认不执行删除。

如果配置 `gc.p2r_only=false`，admin run 仍需要额外显式 flag：

```text
p2r admin docker-gc run --yes --allow-global
```

TUI startup 和 CLI pre-run hook 永远不传 `--allow-global`。

### 9.2 state 与 lock

状态文件：

```text
<scan_path>/.qa-control/docker_maintenance_state.json
```

锁文件：

```text
<scan_path>/.qa-control/locks/docker-maintenance.lock
```

锁内容必须包含：

```text
pid=<pid>
created_at=<UTC RFC3339>
operation=docker-gc
```

stale lock 判定复用 pipeline task lock 的 pid 存活逻辑。

### 9.3 自动触发

TUI 启动：

1. `docker.gc.enabled=true`
2. `docker.gc.run_on_tui_start=true`
3. 上次成功或跳过检查距今超过 `docker.gc.interval`
4. scheduler 当前没有 running 或 queued job
5. `.qa-control/locks` 下没有 task run lock
6. maintenance lock 可获取

满足后后台触发一次 GC run。TUI 不等待 GC 完成，不阻塞首屏。

CLI `p2r run` 前：

1. 默认不跑。
2. 只有 `docker.gc.run_before_cli_run=true` 时尝试。
3. 如果当前 task 或其他 task 有 run lock，跳过。

### 9.4 资源归属

默认只处理 p2r 可归属资源：

1. label 等于 `managed_by=p2rqa`，其中 key/value 从 `docker.managed_label` 解析。
2. label `com.docker.compose.project` 以 `compose_project_prefix + "_"` 开头。
3. artifact 记录过的 compose project。

非 p2r 资源不能进入 delete plan。dry-run summary 可记录 skipped 计数，但不要求列出所有非 p2r 资源。

### 9.5 动作

MVP 优先使用枚举删除，而不是依赖 prune filter：

```text
docker ps -a --filter label=<managed_label> --format json
docker rm <container-id>

docker network ls --filter label=<managed_label> --format json
docker network rm <network-id>

docker volume ls --filter label=<managed_label> --format json
docker volume rm <volume-name>

docker image ls --filter label=<managed_label> --format json
docker image rm <image-id>
```

默认开启：

1. exited container cleanup
2. p2r network cleanup

默认关闭：

1. p2r volume cleanup
2. p2r image cleanup
3. builder cache cleanup

如果显式启用 builder cache：

```text
docker builder prune --force --filter until=<duration>
```

summary 必须声明这是 age-based global builder cache cleanup，不是严格 p2r-only。

### 9.6 `docker_gc_summary.json`

admin run 和自动触发都写：

```text
<scan_path>/.qa-control/docker_gc_summary.json
```

如果 GC 是某次 pipeline run 触发的，也复制到该 run artifact root。

冻结 schema：

```json
{
  "ok": true,
  "dry_run": true,
  "trigger": "tui_start",
  "started_at": "2026-05-19T12:00:00Z",
  "finished_at": "2026-05-19T12:00:02Z",
  "duration_ms": 2000,
  "lock_path": "/abs/projects-qa/.qa-control/locks/docker-maintenance.lock",
  "skipped": false,
  "skip_reason": "",
  "p2r_only": true,
  "managed_label": "managed_by=p2rqa",
  "compose_project_prefix": "p2rqa",
  "actions": [
    {
      "kind": "container",
      "enabled": true,
      "dry_run": true,
      "candidates": [
        {
          "id": "abc123",
          "name": "p2rqa_web_1",
          "labels": {
            "managed_by": "p2rqa"
          },
          "reason": "managed_label"
        }
      ],
      "deleted": [],
      "failed": []
    }
  ],
  "commands": [
    "docker ps -a --filter label=managed_by=p2rqa --format json"
  ],
  "warnings": []
}
```

---

## 10. Stage C 冻结调整

本轮只做小改：

1. Stage C 继续在宿主机执行 `bash run_tests.sh`。
2. Stage C 继续从 Stage B in-memory `RuntimeState` 获取 service URLs。
3. `test_runtime_summary.json` 增加：
   - `compose_files`
   - `build_mirror_enabled`
   - `build_mirror_mode`
   - `build_mirror_fallback_used`
   - `runtime_evidence_classification`
4. 缺失 runtime evidence 分类：
   - `stage_b_failed`
   - `missing_runtime_evidence`
   - `missing_port_mapping`
5. 不新增 `docker compose exec` 或 `docker compose run --rm`。

Stage C 执行模型评估另开文档和任务，不作为本轮实现验收项。

---

## 11. TUI 展示冻结

### 11.1 Docker mirror 配置视图

新增 Docker maintenance / mirror 配置视图，面向质检员维护项目级 mirror 期望配置：

```text
Docker Mirror

Desired config
  Source:         <project>/.p2r.yaml
  Enabled:        [x]
  daemon.json:    /etc/docker/daemon.json
  backup dir:     <scan_path>/.qa-control/docker-daemon-backups
  mirrors:
    1. https://...

Current daemon
  Status:         consistent / drift / unreadable
  Current mirrors:
    1. https://...

Actions
  [Refresh Status] [Save Config] [Apply to daemon] [Restore Backup]
```

交互要求：

1. `Save Config` 只写项目级 `.p2r.yaml`。
2. `Refresh Status` 不写任何文件。
3. `Apply to daemon` 和 `Restore Backup` 必须进入确认页。
4. 确认页必须展示 diff、backup path、daemon path 和 Docker restart notice。

### 11.2 详情页

在现有 cleanup 区域附近增加 Docker runtime 信息块，数据来自 artifact 文件：

1. `docker_runtime_summary.json`
2. `docker_mirror_summary.json`
3. `.qa-control/docker_gc_summary.json`
4. `.qa-control/daemon_mirror_summary.json`
5. `cleanup_summary.json`

展示字段：

```text
Docker runtime:
  Compose: <project> <compose file count>
  Pull: <policy> <status>
  Mirror config: <project>/.p2r.yaml
  Build mirror: <enabled/mode> fallback=<true/false>
  Daemon mirror: <disabled/consistent/drift/error>
  GC: <last status> <last recorded_at>
  Cleanup: <existing cleanup status>
```

### 11.3 总览页

总览页不新增列，避免表格过宽。

### 11.4 TUI startup GC

TUI 只触发 maintenance service，不直接运行 Docker 命令。GC 完成后写 `.qa-control/docker_gc_summary.json`，详情页下次刷新时读取。

---

## 12. 分阶段实施计划

### Phase 0：保护网与基线

目标：先建立可回归测试，不改行为。

写集：

1. `tests/internal/pipeline/runtime_evidence_test.go`
2. `tests/internal/docker/manager_test.go`
3. `tests/internal/config/config_test.go`
4. 新增必要 test fake helper

任务：

1. 增加 Stage B current behavior 测试，锁定当前 pull/build/up/ps 顺序。
2. 增加 cleanup 读取旧 `compose_file` artifact 的兼容测试。
3. 增加 no-Docker 环境说明，确保 tests 不调用真实 Docker。

门禁：

```text
go test ./...
```

### Phase 1：Docker 包边界重构，不改行为

目标：迁移代码边界，但保持 Stage B 行为不变。

写集：

1. `internal/docker/command.go`
2. `internal/docker/compose.go`
3. `internal/docker/runtime.go`
4. `internal/docker/cleanup.go`
5. `internal/docker/manager.go`
6. `internal/pipeline/stage_b.go`
7. `internal/pipeline/compose.go`
8. `internal/pipeline/runtime_state.go`
9. `internal/pipeline/runtime_evidence.go`
10. `internal/pipeline/cleanup.go`
11. `tests/internal/docker/*`
12. `tests/internal/pipeline/*`

任务：

1. 将 compose discovery、README compose command、compose args、ps JSON parse、port parse、probe 迁到 `internal/docker`。
2. 将 `RuntimeState`、`PortMapping`、`ProbeResult` 移到 `internal/docker` 或定义 alias 兼容 pipeline。
3. cleanup 支持 `ComposeFiles []string`，但旧 `ComposeFile` 仍可工作。
4. Stage B wrapper 调用 Docker service，但仍按 required pull、build、up 的旧顺序执行。

验收：

1. `go test ./...` 通过。
2. `stage_b.go` 不再包含大段 Docker 命令拼装。
3. 行为保持：pull 失败仍失败 Stage B，作为 Phase 1 基线。

### Phase 2：pull policy 与 Stage B summary

目标：解除 required pull 误伤。

写集：

1. `internal/config/config.go`
2. `internal/docker/build.go`
3. `internal/pipeline/stage_b.go`
4. `internal/pipeline/lifecycle_persist.go`
5. `tests/internal/config/config_test.go`
6. `tests/internal/docker/*`
7. `tests/internal/pipeline/runtime_evidence_test.go`

任务：

1. 增加 `docker.pull_policy` config/default/validate。
2. Docker service 支持 `required | best_effort | skip`。
3. 写 `docker_runtime_summary.json`。
4. `run_manifest.json` 的 docker policy 区域加入 `pull_policy`。
5. Stage B pull best-effort 失败时继续 build/up，并在 summary/log/manifest 中记录 warning。

验收：

1. pull 失败但 build/up/ps 成功时 Stage B done。
2. `pull_policy=required` 时 pull 失败仍 Stage B failed。
3. `pull_policy=skip` 不执行 pull。
4. `go test ./...` 不依赖真实 Docker。

### Phase 3：daemon mirror admin

目标：提供可审计 daemon mirror 管理能力。

写集：

1. `internal/config/config.go`
2. `internal/docker/mirrors.go`
3. `cmd/root.go`
4. `cmd/admin.go`
5. `cmd/admin_docker_mirror.go`
6. `internal/tui/app.go`
7. `internal/tui/viewmodel.go`
8. `internal/tui/runconfig.go`
9. `tests/internal/docker/*`
10. `tests/internal/tui/*`
11. `tests/cmd/*`

任务：

1. 增加 `daemon_mirrors` config/default/validate。
2. 实现 daemon JSON merge/status/backup/restore 纯函数。
3. 实现 admin 命令。
4. 写 `.qa-control/daemon_mirror_summary.json`。
5. 不自动重启 Docker。
6. 实现 TUI daemon mirror 配置视图，保存到项目级 `.p2r.yaml`。
7. TUI status/apply/restore 复用 daemon mirror service。

验收：

1. status 不写文件。
2. apply 必须 `--yes`。
3. restore 必须 `--backup` 和 `--yes`。
4. merge 保留未知字段。
5. 无效 JSON 不覆盖原文件。
6. TUI `Save Config` 写项目级 `.p2r.yaml`，不写 daemon.json。
7. TUI `Apply` / `Restore` 必须强确认并写 `daemon_mirror_summary.json`。

### Phase 4：Dockerfile patch MVP

目标：解决 Dockerfile 内 RUN 依赖安装换源问题。

写集：

1. `go.mod`
2. `go.sum`
3. `internal/config/config.go`
4. `internal/docker/dockerfile_patch.go`
5. `internal/docker/mirrors.go`
6. `internal/docker/compose.go`
7. `internal/docker/build.go`
8. `internal/pipeline/stage_b.go`
9. `internal/pipeline/runtime_state.go`
10. `tests/internal/docker/*`
11. `tests/internal/pipeline/*`

任务：

1. 增加 `build_mirrors` config/default/validate。
2. 支持 compose build string/object 解析。
3. 生成 patched Dockerfile 和 compose override。
4. 执行 override `docker compose config` 验证。
5. 支持 apt/apk/yum/npm/pip/go/cargo 注入规则。
6. 支持 override 验证失败 fallback。
7. 支持 patched build 失败 fallback 原 build。
8. 写 `docker_mirror_summary.json` 和 effective config。

验收：

1. artifact 下存在 patched Dockerfile、override、summary。
2. `repo/` 无改动，可通过测试比较原 repo 文件 sha256。
3. override 验证失败自动 fallback 原 compose。
4. patched build 失败且 fallback 成功时 Stage B done。
5. Dockerfile patch 单测覆盖 parser directive、多阶段、scratch、多行 RUN、RUN mount。

### Phase 5：Docker GC

目标：提供保守、可审计、可跳过的维护型 GC。

写集：

1. `internal/docker/gc.go`
2. `internal/docker/lock.go`
3. `internal/maintenance/docker_gc.go`
4. `internal/config/config.go`
5. `cmd/admin_docker_gc.go`
6. `internal/tui/app.go`
7. `internal/tui/viewmodel.go`
8. `tests/internal/docker/*`
9. `tests/internal/tui/*`
10. `tests/cmd/*`

任务：

1. 增加 `docker.gc` config/default/validate。
2. 实现 state file 与 maintenance lock。
3. 实现 dry-run plan。
4. 实现 explicit run 删除。
5. 实现 active job/task lock skip。
6. TUI startup opportunistic GC 后台触发。
7. 写 `.qa-control/docker_gc_summary.json`。

验收：

1. dry-run 不删除任何资源。
2. run 只删除 p2r 可归属资源。
3. active job 存在时 TUI startup GC skip。
4. task run lock 存在时 GC skip。
5. 不生成 `docker system prune -a --volumes`。
6. `go test ./...` 不依赖真实 Docker。

### Phase 6：Stage C 证据增强，不改执行模型

目标：让 Stage C 缺 runtime evidence 的失败更可诊断。

写集：

1. `internal/pipeline/stage_c.go`
2. `internal/pipeline/runtime_evidence.go`
3. `tests/internal/pipeline/runtime_evidence_test.go`

任务：

1. `test_runtime_summary.json` 写 compose files 和 mirror 状态。
2. 缺 runtime evidence 时写分类。
3. 保持 host `bash run_tests.sh`。

验收：

1. Stage C 缺 Stage B evidence 时 summary 为 `stage_b_failed` 或 `missing_runtime_evidence`。
2. Stage C 有 evidence 但无 ports 时 summary 为 `missing_port_mapping`。
3. 不新增容器内执行逻辑。

---

## 13. 测试矩阵

| 层级 | 必测内容 | 方法 | 门禁 |
|---|---|---|---|
| config | pull/mirror/gc 解析、默认值、枚举、duration | YAML fixture | 错误配置 fail |
| docker command | args、timeout、env、streaming summary | fake runner | 不调用真实 Docker |
| compose | build string/object、多 compose args、override args | YAML fixture | 保留 build 字段 |
| Stage B | pull 三态、fallback、artifact 写失败 cleanup target | fake runner | 无 Docker 通过 |
| Dockerfile patch | directive、多阶段、apt/apk/yum/npm/pip/go/cargo、scratch、多行 RUN、RUN mount | fixture + golden | 不改 repo |
| daemon mirror | merge、backup、restore、status diff、无效 JSON | 临时目录 | status 只读 |
| GC | dry-run、label/project 过滤、lock skip、timeout、失败 summary | fake Docker output | 不生成危险命令 |
| lifecycle | normal/abort/crash/recovery cleanup summary | fake runner | 失败路径写 summary |
| TUI | daemon mirror 配置写项目级 `.p2r.yaml`、status 只读、apply/restore 强确认、startup GC skip、runtime block 展示 | test hooks | 不阻塞 TUI，不静默改系统 |
| admin CLI | docker-mirror、docker-gc 参数与确认 | cobra command tests | apply/run 需确认 |

普通门禁：

```text
go test ./...
```

真实 Docker 集成测试为 opt-in：

```text
P2R_DOCKER_INTEGRATION=1 go test -tags docker_integration ./...
```

真实 Docker 样例至少覆盖：

1. Debian/apt
2. Node/npm
3. Python/pip
4. Go/GOPROXY
5. Cargo
6. 多阶段 Dockerfile

---

## 14. 回滚策略

| 能力 | 回滚方式 |
|---|---|
| pull policy | 配置改回 `required` 或 `skip` |
| build mirror | `docker.build_mirrors.enabled=false` 或 `mode=off` |
| patched build | 自动 fallback 原 compose；必要时关闭 build mirror |
| daemon mirror | `p2r admin docker-mirror restore --backup <path> --yes` |
| TUI mirror config | 重新保存项目级 `.p2r.yaml` 或恢复该文件的版本控制副本 |
| GC | `docker.gc.enabled=false` |
| TUI startup GC | `docker.gc.run_on_tui_start=false` |
| builder cache prune | `docker.gc.prune_builder_cache=false` |
| RuntimeState schema | 保留旧 `compose_file` 读取，旧 run cleanup 不受影响 |

每个 Phase 必须独立提交。禁止把 Phase 4 Dockerfile patch 与 Phase 5 GC 合并到同一个提交。

---

## 15. 人工验收脚本

### 15.1 无 Docker 环境

```text
go test ./...
```

期望：不需要 Docker daemon，也不需要 `docker` binary。

### 15.2 pull best-effort

使用 fake runner 或测试 fixture：

1. pull 返回 exit 1。
2. build/up/ps 返回成功。
3. Stage B done。
4. `docker_runtime_summary.json` 中 pull status 为 warning。

### 15.3 daemon mirror

在临时 daemon path 或专用机上：

```text
p2r admin docker-mirror status
p2r admin docker-mirror apply --yes
p2r admin docker-mirror restore --backup <path> --yes
```

检查：

1. status 不写文件。
2. apply 生成 backup、sha256、diff、summary。
3. restore 可恢复。
4. 提示重启 Docker，但不自动重启。

### 15.4 GC dry-run 与 run

准备 p2r label 与非 p2r label 资源：

```text
p2r admin docker-gc run --dry-run
p2r admin docker-gc run --yes
```

检查：

1. dry-run 不删除。
2. run 删除列表只包含 p2r 可归属资源。
3. 非 p2r 资源仍存在。
4. summary 写入 `.qa-control/docker_gc_summary.json`。

### 15.5 Dockerfile patch

对 apt/npm/pip/go/cargo 样例分别运行 Stage B：

1. artifact 下存在 patched Dockerfile。
2. artifact 下存在 `compose.mirror.override.yml`。
3. `docker_mirror_summary.json` 标记 injected。
4. `repo/` 文件 sha256 未变化。
5. fallback 场景 summary 记录清晰。

---

## 16. 需求覆盖矩阵

| 原需求主题 | 冻结覆盖位置 |
|---|---|
| Docker/Compose 编排从 pipeline 拆出 | 第 3、12.1 节 |
| Stage B 使用 Docker service | 第 3.3、6、12.1 节 |
| daemon registry mirror | 第 4、8、12.3 节 |
| Dockerfile 内依赖安装换源 | 第 7、12.4 节 |
| 不污染 `repo/` | 第 1.3、7.3、12.4、15.5 节 |
| pull 降级为可配置、可回退、可记录 | 第 4、6.2、12.2 节 |
| RuntimeState 支持多个 compose files | 第 5 节 |
| fallback 写 summary/log/manifest | 第 5、6.3、7.4、12.4 节 |
| Stage C 不混入执行模型大改 | 第 10、12.6 节 |
| cleanup 支持多 compose files | 第 5、12.1 节 |
| Docker GC p2r-only | 第 9 节 |
| GC state/lock/skip 条件 | 第 9.2、9.3 节 |
| admin docker-gc 命令 | 第 9.1、12.5 节 |
| admin docker-mirror 命令 | 第 8.1、12.3 节 |
| TUI 图形化配置 daemon mirrors | 第 4.4、8.3、11.1、12.3 节 |
| artifact 与 TUI 展示 | 第 5、7.4、8.4、9.6、11 节 |
| config 扩展与默认值 | 第 4 节 |
| `go test ./...` 不依赖真实 Docker | 第 12、13、15.1 节 |
| 真实 Docker 手工样例 | 第 13、15.5 节 |
| 风险与回滚 | 第 14 节 |

---

## 17. 实施守则

1. 先测后改，每个 Phase 先补当前或目标行为测试。
2. 文件迁移和行为改变分开提交。
3. 所有 Docker 调用都走 `executor.CommandRunner`。
4. 所有系统级副作用必须有 `--yes`、backup、summary 和 restore 路径。
5. 所有删除动作必须先能 dry-run。
6. 任何 fallback 都必须同时写 summary 和 log。
7. 任何 artifact schema 变更都必须有读取旧 artifact 的兼容测试。
8. 如果 Phase 4 parser 无法可靠保留语义，宁可跳过 patch 并记录 warning，不允许退化成全文件正则替换。
