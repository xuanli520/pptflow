# Harbor Factory Harbor Flow 生产包

此目录是一个不可拆分的本地生产部署单元。运行时会从实际
`harbor-factory` 可执行文件所在目录读取冻结的 `deployments/`，并校验其
catalog、lock 和受管文件。因此，可执行文件和完整部署目录必须始终同级、
同版本、一起移动和一起替换。

生产包根目录包含：

```text
README.md
harbor-factory
deployments/
  standard-authoring/
  codeedge-phase1/
  codeedge-evaluator-child/
SHA256SUMS
harbor-factory-harbor-flow-production.tar.gz
```

`SHA256SUMS` 覆盖 `README.md`、可执行文件、所有生产部署文件和归档本身，
但不包含自身。归档包含运行所需的 `README.md`、`harbor-factory` 和完整
`deployments/` 树。

## 安装

先在构建产物目录中校验所有受控文件：

```text
cd /path/to/harbor-flow-production
sha256sum -c SHA256SUMS
```

然后将归档解压到一个新的、专用的部署根目录。下面的示例还保留校验清单，
便于后续人工审计：

```text
release_root=/opt/harbor-factory
install -d -m 0755 "$release_root"
tar -xzf harbor-factory-harbor-flow-production.tar.gz -C "$release_root"
install -m 0644 SHA256SUMS "$release_root/SHA256SUMS"
cd "$release_root"
./harbor-factory --root /var/lib/harbor-factory tui
```

`--root` 指向可变的本地控制面数据目录；它不是部署目录，不能用它替代
`deployments/`。首次使用时，程序会在该受管根目录创建本地数据库、备份和
运行证据。

## 不可拆分规则

禁止 binary-only 安装。不得只复制 `harbor-factory`，也不得从源仓库、另一
版本的安装或网络挂载位置借用 `deployments/`。以下做法均不受支持：

- 将包根中的 `harbor-factory` 设为符号链接，或用外部符号链接替代完整包的
  部署方式。
- 将 `deployments/`、任一阶段目录或其中受管文件设为符号链接。
- 将新二进制与旧 `deployments/` 混用，或只替换其中一部分阶段材料。
- 通过复制、绑定挂载或符号链接让多个版本共享同一部署树。

升级时，请先退出使用旧版本的进程，在独立目录中完成新包校验，再将新包作为
完整单元部署。保留旧完整目录用于回退；不要跨版本复用二进制或部署文件。

生产预检故意采用 fail-closed 行为：实际包根缺少完整真实的 `deployments/`、
部署文件不是普通文件、部署目录不是实际目录，或任何 catalog/lock 绑定不一致
时，TUI 和 worker 都不会启动。这是为了避免未受控的模型、provider 或执行
契约进入运行。

## 本地使用

从部署根目录启动 TUI：

```text
./harbor-factory --root /var/lib/harbor-factory tui
```

创建标准题目时，在 Task Hub 输入 `t` 后输入 `s`，审阅计划并按确认表单填写
HTTPS/SSH Git 仓库 URL、完整 commit、slug、标题、可选 metadata 与原因。已入队的
Run 会由本地受控 outbox 激活器立即交接给 child worker；退出 TUI 的交接面板仅用于
仍在运行的 Run 的受控交接或恢复。外部 provider 的凭据只应通过受批准的环境变量和
secret reference 提供，不能写入此生产包或其部署文件。

Standard 创题的 HTTPS 拉取始终无凭据、非交互。SSH 只允许连接到
`deployments/standard-authoring/ssh/known_hosts` 中已有精确 host key 的主机；该
文件、OpenSSH client 与 wrapper shell 都由 Standard lock 固定。程序不会读取
`~/.ssh`、普通 `SSH_AUTH_SOCK`、用户 SSH config，也不会接受 `accept-new`。
任何需要 SSH 认证的仓库都必须在启动进程时显式提供一个受管 Unix socket：

```text
HARBOR_FACTORY_STANDARD_AUTHORING_SSH_AUTH_SOCK=/absolute/path/to/agent.sock
```

该值仅在 SSH 拉取瞬间使用，不进入数据库、Run、日志契约或发布包。新增或轮换
SSH host key 必须更新受管 `known_hosts`、重新生成 Standard lock，并重新构建完整
生产包。
