# p2r_tui Bugfix 迭代最终实施计划

> 版本: 2026-05-27  
> 状态: 已按项目结构、关键源码和多智能体交叉审阅修订  
> 范围: 3 个 bug，按实际代码边界拆为 13 个实施点  
> 优先级: Bug 3 Docker/Stage C > Bug 2 default_stages > Bug 1 文档计数与 QA 门禁

---

## 交叉核对后修正

| 原方案点 | 修正 |
|---|---|
| 3e 写在 `internal/pipeline/stage_a.go` | `run_tests.sh` 实际由 `internal/pipeline/stage_c.go` 执行，所有 Stage C 改动必须落在 Stage C 链路 |
| 端口重写文件替换原 compose | 必须改成最小 override，并用 `原 compose + override` 叠加验证，避免 YAML anchors、extends、注释、多文档结构被 marshal 往返破坏 |
| 只把 runner 放进 compose 网络即可兼容硬编码 `localhost:8080` | 不成立。必须给 runner 提供同一网络命名空间内的 localhost 端口代理，才能把 `localhost:<原端口>` 转发到对应 compose service |
| 代理变量安全由 `runtimeEnvSensitive()` 保证 | 不准确。该函数只看 key，不看 value。代理 URL 若带账号密码会被传给 docker 子进程，但当前不会写入 artifact |
| QA 门禁只加 `StartInspection()` | 不完整。`SubmitInspection()`、`RetryGitSync()` 和运行配置提交都会绕过；最终门禁应放在 pipeline 导入 dropbox 后、创建 run 前，TUI 只做前置提示 |
| `default_stages` 只在 `normalizeRunOptions()` 注入即可 | 还要处理 `static_only` 与显式阶段选择的冲突，否则 `selectedStages()` 对 `Stages` 会绕过 runtime stage 过滤 |

---

## 已确认的代码事实

| 子问题 | 现有代码证据 |
|---|---|
| 文档计数缓存 | `internal/tui/keymap.go:642` 使用 `m.detailVM.DocsSummary.Count` |
| 托管附件计数 | `internal/tui/runconfig.go:223` 已用 `taskdocs.Count()` 在添加附件后刷新 |
| QA 提交通道 | `internal/tui/shared.go:531` 的 `submitInspection()` 是 TUI inspection/sync retry 的统一提交点 |
| 阶段选择 | `internal/pipeline/stage.go:123` 的 `selectedStages()` 负责 Stage A-F 选择 |
| Run 入口 | `internal/pipeline/run_lifecycle.go:141` 调用 `normalizeRunOptions()` |
| dropbox 导入 | `internal/pipeline/run_lifecycle.go` 在 `prepareRun()` 中调用 `taskdocs.ImportDropbox()` 后读取 manifest |
| Recovery | `internal/pipeline/recovery.go:175` 使用 manifest 中的 stage/from/stages，不应重新套用当前配置 |
| Docker compose 查找 | `internal/docker/compose.go:29` 当前只查 repo 顶层 |
| 端口重写 | `internal/docker/ports.go:30` 当前读取完整 compose 后 marshal 写回 |
| Compose 文件层叠 | `internal/docker/build.go:171` 当前用重写文件替换 base runtime files |
| Stage C 执行 | `internal/pipeline/stage_c.go:35` 查找 `repo/run_tests.sh`，`stage_c.go:109` 在宿主机执行 |
| Stage C URL 注入 | `internal/pipeline/runtime_evidence.go:71` 生成 host URL 环境变量 |

---

## Docker 官方依据

参考:
- https://docs.docker.com/compose/how-tos/networking/
- https://docs.docker.com/engine/network/drivers/bridge/
- https://docs.docker.com/reference/compose-file/services/#ports
- https://docs.docker.com/reference/compose-file/services/#network_mode
- https://docs.docker.com/reference/cli/docker/compose/run/

| 依据 | 对本方案的影响 |
|---|---|
| Compose 默认为每个 project 创建 `<project-name>_default` 网络，服务可用 service name 互访 | p2r 已生成唯一 compose project name，可把每个 QA run 的服务隔离到自己的 compose 网络 |
| user-defined bridge network 提供容器间 DNS 与网络隔离 | runner/proxy 必须进入该 run 的 compose 网络，而不是 default bridge |
| `docker compose run` 默认不发布 service ports | Stage C runner 可以作为一次性容器运行，不再占用宿主机端口 |
| `network_mode: service:<name>` 可让容器加入另一个 service 的网络命名空间 | runner 可共享 proxy service 的 localhost，从而让硬编码 `localhost:8080` 命中代理 |
| Compose ports 可只写 container port 或 published range，由运行时分配 host port | Stage B 可以避免固定宿主端口冲突，同时保留给 host evidence 的随机端口 |

## 最终修改范围

### 生产代码

| 文件 | 目的 |
|---|---|
| `internal/docker/compose.go` | 确定性递归查找 compose 文件 |
| `internal/docker/ports.go` | 条件端口重写、最小 override、端口占用判断 |
| `internal/docker/build.go` | 使用 `original + ports override + mirror override + label override` 叠加链路 |
| `internal/docker/dockerfile_patch.go` | mirror Dockerfile path 做可验证修正，禁止 artifactRoot 到 build context 的无效相对路径 |
| `internal/config/config.go` | 增加 Stage C isolated runner/proxy 配置 |
| `internal/pipeline/runtime_env.go` | 放行 Docker 代理环境变量 |
| `internal/pipeline/runtime_evidence.go` | 输出 Stage C 可消费的服务 URL 与 ports env 内容 |
| `internal/pipeline/stage_c.go` | 按配置选择 host 或 isolated 执行，并写 Stage C summary |
| `internal/pipeline/stage_c_isolated.go` | 生成 runner/proxy override，执行隔离 Stage C，处理取消与清理 |
| `internal/pipeline/run_lifecycle.go` | 导入 dropbox 后、创建 run 前执行 initial QA 文档门禁 |
| `internal/pipeline/preparation.go` | 注入 `default_stages` 并保留显式选择优先级 |
| `internal/pipeline/stage.go` | 统一 static-only 对显式/默认阶段的约束 |
| `internal/taskdocs/docs.go` | 增加可用文档计数，覆盖 managed manifest 与 dropbox 待导入文件 |
| `internal/tui/keymap.go` | 运行配置打开时读取实时文档数 |
| `internal/tui/runconfig.go` | 提交前提前提示无文档初检 |
| `internal/tui/shared.go` | TUI inspection 前置门禁与错误分类 |

### 聚焦验证

| 测试区域 | 覆盖点 |
|---|---|
| `tests/internal/docker/service_test.go` | compose 递归、端口 override 叠加、anchors 保留、mirror 与 label 叠加 |
| `tests/internal/pipeline/runtime_evidence_test.go` | proxy env、Stage C URL/env artifact、localhost proxy 映射 |
| `tests/internal/pipeline/stage_c_isolated_test.go` | isolated runner/proxy override、取消清理、硬编码 localhost |
| `tests/internal/pipeline/pipeline_test.go` | `default_stages` 注入、static-only 冲突、manifest stages、导入 dropbox 后的文档门禁 |
| `tests/internal/tui/stage_plan_test.go` | 实时文档计数、运行配置门禁 |
| `tests/internal/tui/taskaction_test.go` | TUI 前置门禁不会调度 job、不会写 git sync error |
| `tests/internal/taskdocs/docs_test.go` | managed + dropbox 可用文档计数 |

---

## Bug 3: Docker 与 Stage C 稳定性

### 3a - Docker 代理环境变量

- 文件: `internal/pipeline/runtime_env.go`
- 修改 `runtimeEnvAllowed(key, docker)`:
  - docker=true 时允许 `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY` 及小写变体。
  - 仍然按 key 过滤 `TOKEN`、`SECRET`、`PASSWORD`、`KEY` 等敏感变量。
- 安全边界:
  - 不把 docker env 写入 `run_manifest.json`、`docker_runtime_summary.json` 或日志。
  - 不声称代理 value 会被敏感过滤。若代理 URL 带账号密码，它只进入 docker 子进程环境。

### 3b - Dockerfile patch 路径

- 文件: `internal/docker/dockerfile_patch.go`
- 当前 mirror override 写入绝对 `patch.Path`。
- 禁止简单使用 `filepath.Rel(definition.Context, patch.Path)`:
  - `patch.Path` 位于 artifactRoot，通常在 build context 外。
  - 写成 `../.../docker_mirror/*.Dockerfile.p2r` 不是有效修复，很多 builder 会拒绝 context 外路径。
- 最终实现:
  - 先保留当前 artifactRoot patched Dockerfile + absolute `build.dockerfile` 策略，并用 `docker compose config` 与 `docker compose build` 测试覆盖。
  - 若当前 Compose/Builder 接受 absolute Dockerfile，只补充 warning 和测试，不改成无效相对路径。
  - 若检测到 Dockerfile path 被拒绝，回退到 base compose，并在 `docker_mirror_summary.json` 记录 `fallback_reason=dockerfile_path_outside_context`。
  - 本轮不复制整个 build context 到 artifactRoot；这会放大 IO、破坏 `.dockerignore` 语义，超出 bugfix 边界。

### 3c - Compose 文件确定性递归查找

- 文件: `internal/docker/compose.go`
- 查找顺序:
  1. 先按现有优先级检查 repo 顶层: `docker-compose.yml`、`docker-compose.yaml`、`compose.yml`、`compose.yaml`。
  2. 顶层没有时递归查找，最大深度 5。
  3. 跳过 `.git`、`node_modules`、`vendor`、`.venv`、`venv`、`.qa-control`、`dist`、`build`。
  4. 候选按深度、文件名优先级、路径字典序排序，返回第一个。
- README compose command fallback 保持不变，只在没有 compose 文件时触发。

### 3d - 端口重写改为最小 override

- 文件: `internal/docker/ports.go`、`internal/docker/build.go`
- `prepareRuntimePortRewrite()` 不再 marshal 完整 compose。
- 新行为:
  - 只生成包含 `services.<service>.ports` 的最小 override 文件。
  - 原始 compose 永远作为第一层传给 Docker Compose。
  - 验证命令使用 `originalFiles + portsOverride`。
  - `baseRuntimeFiles` 改为 `originalFiles + portsOverride`，后续 mirror override、label override 继续追加。
- 端口策略:
  - host 执行模式下，固定 host port 未被占用时可保留原始发布端口，兼容旧行为。
  - isolated 执行模式下，固定 host port 一律从业务服务发布中移除或改成 target-only，让 Docker 分配随机 host port，避免并发 QA run 冲突。
  - hardcoded `localhost:<原始端口>` 的兼容由 Stage C proxy 在 runner 网络命名空间内提供，不再依赖宿主机固定端口。
  - 如果 `docker compose up` 因端口占用失败，允许一次带 override 的重试，并在 summary 标记 `port_rewrite_retry=true`。
- 产物要求:
  - `docker_runtime_summary.json` 记录 kept fixed ports、rewritten ports、conflict reason、override file。
  - `docker_compose_effective_config.yml` 来自叠加后的 `docker compose config` 输出。

### 3e - Stage C 隔离执行环境

- 文件: `internal/config/config.go`、`internal/pipeline/stage_c.go`、`internal/pipeline/stage_c_isolated.go`、`internal/pipeline/runtime_evidence.go`
- 目标:
  - 每个 QA run 使用独立 compose project 与独立 compose network。
  - 不再要求业务服务占用原始固定宿主机端口。
  - `run_tests.sh` 中硬编码的 `localhost:<原始端口>` 在隔离 runner 内仍可用。

#### 架构

```text
host p2r process
  |
  | docker compose -p p2rqa_<task>_<run> -f original -f p2r overrides
  v
compose project network: p2rqa_<task>_<run>_default
  |
  +-- app services
  |
  +-- p2r_stage_c_proxy
  |     listens inside its namespace:
  |       127.0.0.1:8080 -> web:80
  |       127.0.0.1:5432 -> db:5432
  |
  +-- p2r_stage_c_runner
        network_mode: service:p2r_stage_c_proxy
        /workspace -> repo
        runs: bash run_tests.sh
```

- proxy service 加入当前 run 的 compose network，能用 service name 访问 `web:80`、`api:8000`、`db:5432`。
- runner service 使用 `network_mode: service:p2r_stage_c_proxy`，因此 runner 里的 `localhost:8080` 就是 proxy service 监听的 `localhost:8080`。
- runner 不发布任何宿主机端口，避免并发 QA run 的 host port 冲突。

#### 配置

- 新增配置:
  - `pipeline.stage_c.execution`: `host` 或 `isolated`，默认先保持 `host`，本迭代目标配置为 `isolated`。
  - `pipeline.stage_c.runner_image`: 执行 `run_tests.sh` 的镜像。isolated 模式下必须显式配置，除非后续实现 runner service 自动推断。
  - `pipeline.stage_c.proxy_image`: p2r 控制的轻量 TCP proxy 镜像，默认可由 p2r 管理或构建。
  - `pipeline.stage_c.fail_on_unmapped_localhost`: 默认 true。脚本硬编码了未声明的 localhost 端口时直接失败。
- 不把 Docker socket 挂进 runner。
- repo 挂载到 `/workspace`，artifact root 挂载到 `/p2r-artifacts`。

#### 端口映射来源

- 从原始 compose/effective config 解析 service ports:
  - short syntax: `"8080:80"` 映射为 `listen 8080 -> service:80`。
  - long syntax: `published: 8080, target: 80` 映射为 `listen 8080 -> service:80`。
  - 只有 target、没有 published 的端口，只生成 `SERVICE_URL`，不生成硬编码 localhost proxy。
- Stage B 对业务服务的宿主机端口改为 target-only 或随机 published，用于 host evidence，不再保留原始 fixed host port。
- Stage C proxy 单独在 runner/proxy 网络命名空间中监听原始 fixed host port，用于兼容硬编码脚本。

#### Stage C 执行流程

1. Stage B 写入 `port_map.json`、`docker_runtime_summary.json`、`p2r_stage_c_proxy.json`。
2. Stage C 读取 `p2r_stage_c_proxy.json`，生成 `stage_c.runner.override.yml`。
3. 执行 `docker compose --profile p2r-stage-c up -d p2r_stage_c_proxy`。
4. 执行 `docker compose --profile p2r-stage-c run --rm --no-deps --name <deterministic-runner-name> p2r_stage_c_runner`。
5. runner 共享 proxy 网络命名空间，在 `/workspace` 中执行 `bash run_tests.sh`。
6. 正常结束、失败、超时、取消都执行 `docker rm -f <runner>` 与 `docker compose rm -sf p2r_stage_c_proxy`。

#### 失败与降级

- isolated 模式下没有 `runner_image`: Stage C failed，提示配置 runner image。
- `run_tests.sh` 硬编码了 compose 中不存在的 localhost 端口: Stage C failed，给出具体端口和修复建议。
- 多个 service 争用同一个 published 端口: Stage C failed，要求配置显式映射或修复 compose。
- compose 使用 `network_mode: host`、`container_name`、external/named network 导致 run 间隔离不可保证: strict isolated 模式 failed，非 strict 模式 warning。
- proxy service 启动失败: Stage C failed，保留 proxy log artifact。

#### 兼容性边界

- 该方案兼容硬编码 `localhost:<原始端口>`，前提是端口能从 compose ports 推断。
- 如果脚本访问宿主机本身的服务，而不是 QA run 的 compose service，需要显式配置额外 host gateway 映射。
- runner image 必须包含测试所需工具链。p2r 只负责网络与端口隔离，不自动安装项目依赖。
- Windows/macOS 主机通过 Docker Desktop 跑 Linux runner；repo 挂载路径必须使用 Docker 可接受的绝对路径。

---

## Bug 2: `default_stages` 全入口生效

### 2a - 在 Run 统一入口注入默认阶段

- 文件: `internal/pipeline/preparation.go`
- 位置: `normalizeRunOptions()`，mode 默认化之后、mode 分支校验之前。
- 条件:
  - `opts.Stage == ""`
  - `opts.From == ""`
  - `len(opts.Stages) == 0`
  - `cfg.Pipeline.DefaultStages[opts.Mode]` 非空
- 行为:
  - 注入默认阶段到 `opts.Stages`。
  - 显式 `--stage`、`--from`、运行配置多选永远优先。
  - `writeRunManifest()` 写入注入后的 stages，Recovery 继续以原 run manifest 为准。

### 2b - static-only 阶段约束

- 文件: `internal/pipeline/preparation.go`、`internal/pipeline/stage.go`
- effective static-only = `opts.StaticOnly || cfg.Pipeline.StaticOnly`。
- 默认注入产生的 B/C 在 static-only 下过滤掉。
- 用户显式选择 B/C 且 static-only 生效时返回清晰错误，不静默执行 runtime stage。
- `selectedStages()` 保持最终防线，不能让显式 `Stages` 绕过 static-only。

---

## Bug 1: 文档计数与 QA 门禁

### 1a - 可用文档计数统一 helper

- 文件: `internal/taskdocs/docs.go`
- 新增可用文档计数能力:
  - managed manifest 中已有文档计数。
  - `scanPath/task-docs/<taskID>/` dropbox 中待导入的普通文件计数。
  - 相同 SHA 的 dropbox 文件不重复计数。
- 原 `Count()` 可保留 managed-only 语义；TUI 和门禁使用新 helper，避免 dropbox 文档尚未导入时被误拦截。

### 1b - Pipeline 最终门禁

- 文件: `internal/pipeline/run_lifecycle.go`
- 位置: `prepareRun()` 中 `ImportDropbox()` 和 `ReadManifest()` 完成之后、`CreateRun()` 之前。
- 行为:
  - effective mode 为空时视为 initial。
  - initial run 的 `len(docsManifest.Docs) < 1` 时返回错误。
  - 不创建 run，不写 stage，不污染 run history。
  - dropbox 文件会先被导入 managed manifest，因此不会被误拦截。
- 适用入口:
  - CLI `p2r run`。
  - TUI inspection。
  - scheduler pipeline job。
  - 任何直接调用 `Runner.Run()` 的内部入口。
- recheck 不受该 initial 门禁影响。

### 1c - 运行配置打开时实时读取

- 文件: `internal/tui/keymap.go`
- `openRunConfigForTask()` 不再使用 `m.detailVM.DocsSummary.Count`。
- 改用可用文档计数 helper。
- 新增 import `internal/taskdocs`。

### 1d - UI 提交前提示

- 文件: `internal/tui/runconfig.go`
- `submitRunConfig()` 在 `plan.blockedReason` 之后、`toRunOptions()` 之前检查:
  - action 是 inspection。
  - effective mode 是 initial。
  - 可用文档数小于 1。
- 失败时保留 run config，不提交 job，并显示 `至少需要一个补充文档才能开始质检`。
- 成功时刷新 `attachedCount`，避免提交前显示旧值。

### 1e - TUI 提交层防御

- 文件: `internal/tui/shared.go`
- `dbTaskActionService.submitInspection()` 做 TUI 防御:
  - effective mode 为空时视为 initial。
  - initial inspection 可用文档数小于 1 时直接返回错误。
  - 不调用 scheduler，不创建 pipeline job。
  - 不把该门禁错误写入 git sync error；这是用户输入缺失，不是同步失败。
- `StartInspection()`、`SubmitInspection()`、`RetryGitSync()`、运行配置入口都覆盖到。
- 即使该层被绕过，`prepareRun()` 仍是最终门禁。

---

## 实施顺序

1. Stage B/C runtime 基础修正:
   - 3d 最小端口 override 与条件重写。
   - 3e Stage C isolated runner/proxy 执行环境。
2. Docker 兼容性修正:
   - 3c compose 递归查找。
   - 3b Dockerfile 相对路径。
   - 3a 代理环境变量。
3. 阶段选择修正:
   - 2a default_stages 注入。
   - 2b static-only 约束。
4. 文档门禁修正:
   - 1a 可用文档计数 helper。
   - 1b Pipeline 最终门禁。
   - 1c/1d TUI 实时计数与提前提示。
   - 1e TUI 提交层防御。

---

## 验证清单

### Docker / Stage C

- [ ] compose 在 `repo/docker/compose.yml` 时能被找到。
- [ ] repo 顶层和子目录同时存在 compose 时优先使用顶层。
- [ ] 原 compose 含 YAML anchors 时，端口 override 后 `docker compose config` 成功。
- [ ] host 模式下固定端口空闲时可保留旧行为。
- [ ] 固定端口被占用时业务服务仍能启动，Stage C runner 内 `localhost:<原端口>` 通过 proxy 访问对应 service。
- [ ] 两个 QA run 同时使用 `8080:80` 的项目时，宿主机端口不冲突，两个 Stage C 都能访问各自 runner 内的 `localhost:8080`。
- [ ] mirror override 与 ports override 同时存在时，Compose 文件顺序为 original -> ports -> mirror -> labels。
- [ ] build mirror patch 不写无效 `../artifactRoot` 相对路径；路径被拒绝时回退并记录原因。
- [ ] `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` 能传给 Docker 命令，且不出现在 runtime summary。
- [ ] Stage C 生成 `p2r_ports.env` artifact。
- [ ] Stage C isolated 模式下生成 runner/proxy override 与 `p2r_stage_c_proxy.json`。
- [ ] Stage C 对无法映射的硬编码 localhost 端口给出 High finding。
- [ ] 取消 Stage C 时 runner/proxy 容器被清理。

### default_stages

- [ ] CLI `p2r run TASK-ID` 使用 `pipeline.default_stages.initial`。
- [ ] TUI 直接开始质检使用 `pipeline.default_stages.initial`。
- [ ] 运行配置首次打开时预选 default_stages；用户手动修改后，以用户选择为准，不被 normalizeRunOptions 重新覆盖。
- [ ] recheck 使用 `pipeline.default_stages.recheck`。
- [ ] Recovery 使用原 run manifest，不使用当前 default_stages。
- [ ] static-only 下默认 B/C 被过滤，显式 B/C 返回错误。

### 文档计数与门禁

- [ ] 无 managed 文档、无 dropbox 文件时，运行配置显示 0。
- [ ] detailVM 缓存为 1 但当前任务无文档时，运行配置显示 0。
- [ ] dropbox 中有待导入文件时，可用文档计数大于 0，初检允许提交。
- [ ] 无文档 initial inspection 被 `submitRunConfig()` 拦截。
- [ ] 绕过 UI 直接调用 `Runner.Run()` 时，导入 dropbox 后仍无文档则不创建 run。
- [ ] 绕过运行配置直接调用 `SubmitInspection()` 时，TUI 提交层不调度 job。
- [ ] recheck 不被 initial 文档门禁误拦。

---

## 建议命令

```powershell
go test ./tests/internal/docker ./tests/internal/pipeline ./tests/internal/tui ./tests/internal/taskdocs
go test ./...
```
