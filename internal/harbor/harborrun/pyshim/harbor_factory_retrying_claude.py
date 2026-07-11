import asyncio
import shlex

from harbor.agents.installed.base import NetworkConnectionError
from harbor.agents.installed.claude_code import ClaudeCode


class RetryingClaudeCode(ClaudeCode):
    """Claude Code agent with bounded retries for transient installer failures."""

    _INSTALL_ATTEMPTS = 4

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
