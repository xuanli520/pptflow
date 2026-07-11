import asyncio
import shlex

from harbor.agents.installed.base import NetworkConnectionError
from harbor.agents.installed.claude_code import ClaudeCode


_PROTECTED_ENV_PATH = "/tmp/harbor-factory-agent-env"
_SENSITIVE_MARKERS = ("TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL", "AUTH")


def _is_sensitive_env(name):
    upper = name.strip().upper()
    if not upper or upper.endswith(("_TOKENS", "_KEYS")):
        return False
    return any(marker in upper for marker in _SENSITIVE_MARKERS)


def _valid_env_name(name):
    return (
        bool(name)
        and not name[0].isdigit()
        and name.replace("_", "a").isalnum()
    )


class _ProtectedEnvironment:
    """Keep agent credentials out of docker compose exec argv."""

    def __init__(self, environment, protected):
        self._environment = environment
        self._protected = dict(protected)
        self._installed_payload = None

    def __getattr__(self, name):
        return getattr(self._environment, name)

    async def exec(self, command, cwd=None, env=None, timeout_sec=None, user=None):
        clean_env = {}
        protected = {
            name: item
            for name, item in self._protected.items()
            if _valid_env_name(name)
        }
        for name, item in (env or {}).items():
            if _is_sensitive_env(name) and _valid_env_name(name):
                protected[name] = item
            else:
                clean_env[name] = item
        if protected:
            await self._install_protected_env(protected)
            command = f"set -a; . {shlex.quote(_PROTECTED_ENV_PATH)}; set +a; {command}"
        return await self._environment.exec(
            command=command,
            cwd=cwd,
            env=clean_env or None,
            timeout_sec=timeout_sec,
            user=user,
        )

    async def _install_protected_env(self, protected):
        payload = "".join(
            f"export {name}={shlex.quote(item)}\n"
            for name, item in sorted(protected.items())
        ).encode()
        if not payload or payload == self._installed_payload:
            return
        compose = getattr(self._environment, "_run_docker_compose_command", None)
        if compose is None:
            raise RuntimeError(
                "Harbor environment does not support secret-safe stdin injection"
            )
        result = await compose(
            [
                "exec",
                "-T",
                "main",
                "sh",
                "-c",
                f"umask 077; cat > {shlex.quote(_PROTECTED_ENV_PATH)}",
            ],
            check=False,
            stdin_data=payload,
        )
        if result.return_code != 0:
            raise RuntimeError("failed to inject Claude Code credentials over stdin")
        self._installed_payload = payload


class RetryingClaudeCode(ClaudeCode):
    """Claude Code agent with bounded retries for transient installer failures."""

    _INSTALL_ATTEMPTS = 4

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._factory_protected_env = {
            name: item
            for name, item in self._extra_env.items()
            if _is_sensitive_env(name)
        }
        for name in self._factory_protected_env:
            self._extra_env.pop(name, None)

    async def run(self, instruction, environment, context):
        environment = _ProtectedEnvironment(
            environment, self._factory_protected_env
        )
        await super().run(instruction, environment, context)

    async def install(self, environment):
        if await self._installed_claude_satisfies_version(environment):
            return
        if await self._install_from_cache(environment):
            return
        for attempt in range(1, self._INSTALL_ATTEMPTS + 1):
            try:
                await self._install_from_npm(environment)
                return
            except NetworkConnectionError:
                if attempt == self._INSTALL_ATTEMPTS:
                    raise
                delay = min(2 ** attempt, 8)
                self.logger.warning(
                    "Claude Code installation hit a transient network error; retrying",
                    extra={"attempt": attempt, "next_attempt": attempt + 1, "delay_seconds": delay},
                )
                await asyncio.sleep(delay)

    async def _install_from_cache(self, environment):
        result = await environment.exec(
            command=(
                "set -eu; "
                "case \"$(uname -m)\" in "
                "x86_64) arch=x64 ;; "
                "aarch64|arm64) arch=arm64 ;; "
                "*) exit 44 ;; "
                "esac; "
                "suffix=''; "
                "if [ -e /etc/alpine-release ]; then suffix='-musl'; fi; "
                "source=/opt/harbor-factory/claude-code-cache/node_modules/@anthropic-ai/claude-code-linux-${arch}${suffix}/claude; "
                "test -x \"$source\"; "
                "mkdir -p \"$HOME/.local/bin\"; "
                "ln -sfn \"$source\" \"$HOME/.local/bin/claude\"; "
                "\"$HOME/.local/bin/claude\" --version"
            ),
        )
        if result.return_code == 0:
            self.logger.info("Claude Code installation satisfied from read-only Factory cache")
            return True
        return False

    async def _install_from_npm(self, environment):
        await self.exec_as_root(
            environment,
            command=(
                "if command -v apk &> /dev/null; then"
                "  apk add --no-cache bash curl nodejs npm procps;"
                " elif command -v apt-get &> /dev/null; then"
                "  apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y curl nodejs npm procps;"
                " elif command -v yum &> /dev/null; then"
                "  yum install -y curl nodejs npm procps-ng;"
                " else"
                "  command -v npm >/dev/null 2>&1;"
                " fi"
            ),
        )
        package = "@anthropic-ai/claude-code"
        if self._version is not None:
            package += "@" + self._version
        await self.exec_as_agent(
            environment,
            command=(
                "set -euo pipefail; "
                "npm_config_fetch_retries=5 "
                "npm_config_fetch_retry_mintimeout=2000 "
                "npm_config_fetch_retry_maxtimeout=20000 "
                f"npm install -g {shlex.quote(package)} && "
                "echo 'export PATH=\"$HOME/.local/bin:$PATH\"' >> ~/.bashrc && "
                'export PATH="$HOME/.local/bin:$PATH" && '
                "claude --version"
            ),
        )
