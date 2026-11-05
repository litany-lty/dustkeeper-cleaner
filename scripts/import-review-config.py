#!/usr/bin/env python3
"""Import explicit dual-review settings without contacting either provider."""

import argparse
from contextlib import suppress
import json
import os
from pathlib import Path
import re
import stat
import sys
from urllib.parse import urlsplit


JEV_URL = "https://api.typesafe.ai/v1/systemone"
KNOWN_MINIMAX_ADAPTER = "@ai-sdk/anthropic"
MINIMAX_API_HOSTS = {"api.minimaxi.com", "api.minimax.cn"}
MINIMAX_CLIENT_GATEWAYS = {"agent.minimax.cn", "agent.minimax.io"}
MAX_CONFIG_BYTES = 1 << 20
MODEL_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:-]{0,127}\Z")
PLACEHOLDER_KEYS = {
    "api_key",
    "changeme",
    "change-me",
    "change_me",
    "placeholder",
    "replace-me",
    "replace_me",
    "your-api-key",
    "your_api_key",
    "xxx",
}


class ImportErrorSafe(Exception):
    """An error whose message is safe to show without leaking configuration."""


def read_bounded_regular_file(path, label):
    descriptor = None
    try:
        descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        if not stat.S_ISREG(os.fstat(descriptor).st_mode):
            raise ImportErrorSafe(f"{label} must be a regular file")
        with os.fdopen(descriptor, "rb") as stream:
            descriptor = None
            raw = stream.read(MAX_CONFIG_BYTES + 1)
    except ImportErrorSafe:
        raise
    except OSError:
        raise ImportErrorSafe(f"{label} could not be read") from None
    finally:
        if descriptor is not None:
            os.close(descriptor)
    if len(raw) > MAX_CONFIG_BYTES:
        raise ImportErrorSafe(f"{label} is too large")
    try:
        return raw.decode("utf-8")
    except UnicodeError:
        raise ImportErrorSafe(f"{label} is not UTF-8") from None


def parse_args():
    parser = argparse.ArgumentParser(
        description="Create a new offline Dustkeeper dual-review configuration directory."
    )
    parser.add_argument(
        "--minimax-config",
        default="~/.minimax/config.yaml",
        help="MiniMax YAML configuration (default: ~/.minimax/config.yaml)",
    )
    parser.add_argument(
        "--minimax-mcp-config",
        help="JSON file containing mcpServers.MiniMax.env API credentials",
    )
    parser.add_argument("--jev-proxy-url", default="http://127.0.0.1:7890", help="Explicit JEV HTTP(S) proxy; no direct fallback")
    parser.add_argument("--output-dir", required=True, help="New output directory")
    parser.add_argument(
        "--jev-model",
        default=os.environ.get("STEWARD_DECISION_TYPESAFE_MODEL", "jev-latest"),
        help="JEV model name",
    )
    return parser.parse_args()


def require_mapping(value, label):
    if not isinstance(value, dict):
        raise ImportErrorSafe(f"invalid MiniMax configuration: {label} must be a mapping")
    return value


def require_key(value, label):
    if not isinstance(value, str) or not value.strip():
        raise ImportErrorSafe(f"{label} is missing")
    key = value.strip()
    normalized = key.lower()
    if (
        any(character.isspace() or ord(character) < 0x20 or ord(character) == 0x7F for character in key)
        or normalized in PLACEHOLDER_KEYS
        or "placeholder" in normalized
        or (normalized.startswith("replace") and "key" in normalized)
        or normalized.startswith("your_")
        or normalized.startswith("your-")
        or (key.startswith("<") and key.endswith(">"))
    ):
        raise ImportErrorSafe(f"{label} is a placeholder")
    return key


def require_model(value, label, provider_prefix=None):
    if not isinstance(value, str):
        raise ImportErrorSafe(f"invalid {label}")
    model = value.strip()
    if provider_prefix is not None:
        prefix = provider_prefix + "/"
        if not model.startswith(prefix):
            raise ImportErrorSafe(f"invalid {label}")
        model = model[len(prefix) :]
    if not MODEL_RE.fullmatch(model):
        raise ImportErrorSafe(f"invalid {label}")
    return model


def require_https_base_url(value):
    if not isinstance(value, str) or not value.strip():
        raise ImportErrorSafe("MiniMax baseURL is missing")
    base_url = value.strip().rstrip("/")
    try:
        parsed = urlsplit(base_url)
        parsed.port
    except ValueError:
        raise ImportErrorSafe("invalid MiniMax baseURL") from None
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
    ):
        raise ImportErrorSafe("invalid MiniMax baseURL")
    if parsed.hostname.casefold() in MINIMAX_CLIENT_GATEWAYS:
        raise ImportErrorSafe(
            "MiniMax client gateway cannot be used for API review; "
            "provide --minimax-mcp-config with API credentials"
        )
    suffix = "/messages" if parsed.path.endswith("/v1") else "/v1/messages"
    return base_url + suffix


def load_minimax_config(path, use_mcp_credentials=False):
    try:
        import yaml
    except ImportError:
        raise ImportErrorSafe("PyYAML is required and was not installed automatically") from None

    try:
        config = yaml.safe_load(read_bounded_regular_file(path, "MiniMax configuration"))
    except yaml.YAMLError:
        raise ImportErrorSafe("MiniMax configuration is invalid YAML") from None

    config = require_mapping(config, "root")
    providers = require_mapping(config.get("provider"), "provider")
    minimax = require_mapping(providers.get("minimax"), "provider.minimax")
    if minimax.get("npm") != KNOWN_MINIMAX_ADAPTER:
        raise ImportErrorSafe("unsupported MiniMax adapter")
    options = require_mapping(minimax.get("options"), "provider.minimax.options")
    result = {
        "model": require_model(
            config.get("defaultModel"), "MiniMax defaultModel", provider_prefix="minimax"
        ),
    }
    if not use_mcp_credentials:
        result.update(
            {
                "api_key": require_key(options.get("apiKey"), "MiniMax apiKey"),
                "url": require_https_base_url(options.get("baseURL")),
            }
        )
    return result


def require_minimax_api_host(value):
    if not isinstance(value, str) or not value.strip():
        raise ImportErrorSafe("MINIMAX_API_HOST is missing")
    try:
        parsed = urlsplit(value.strip())
        port = parsed.port
    except ValueError:
        raise ImportErrorSafe("invalid MINIMAX_API_HOST") from None
    if (
        parsed.scheme != "https"
        or parsed.hostname is None
        or parsed.hostname.casefold() not in MINIMAX_API_HOSTS
        or port is not None
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path not in ("", "/")
        or parsed.query
        or parsed.fragment
    ):
        raise ImportErrorSafe("invalid MINIMAX_API_HOST")
    return f"https://{parsed.hostname.casefold()}/anthropic/v1/messages"


def load_minimax_mcp_config(path):
    try:
        config = json.loads(read_bounded_regular_file(path, "MiniMax MCP configuration"))
    except json.JSONDecodeError:
        raise ImportErrorSafe("MiniMax MCP configuration is invalid JSON") from None

    config = require_mapping(config, "MCP root")
    servers = require_mapping(config.get("mcpServers"), "mcpServers")
    minimax = require_mapping(servers.get("MiniMax"), "mcpServers.MiniMax")
    env = require_mapping(minimax.get("env"), "mcpServers.MiniMax.env")
    return {
        "api_key": require_key(env.get("MINIMAX_API_KEY"), "MINIMAX_API_KEY"),
        "url": require_minimax_api_host(env.get("MINIMAX_API_HOST")),
    }


def validate_output_dir(path):
    expanded = path.expanduser()
    if expanded.is_symlink():
        raise ImportErrorSafe("output directory must not be a symbolic link")
    output = expanded.resolve(strict=False)
    if output.exists():
        raise ImportErrorSafe("output directory already exists")
    protected_parts = {part.casefold() for part in output.parts}
    if "dustkeeper-managed" in protected_parts or "launchagents" in protected_parts:
        raise ImportErrorSafe("output directory is inside a protected location")
    return output


def write_private_json(path, value, directory_fd=None):
    data = (json.dumps(value, indent=2, sort_keys=True) + "\n").encode("utf-8")
    descriptor = os.open(path.name if directory_fd is not None else path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory_fd)
    os.fchmod(descriptor, 0o600)
    try:
        with os.fdopen(descriptor, "wb") as stream:
            stream.write(data)
    except Exception:
        try:
            os.close(descriptor)
        except OSError:
            pass
        raise


def import_config(args):
    mcp_credentials = None
    if args.minimax_mcp_config:
        mcp_credentials = load_minimax_mcp_config(
            Path(args.minimax_mcp_config).expanduser()
        )
    minimax = load_minimax_config(
        Path(args.minimax_config).expanduser(),
        use_mcp_credentials=mcp_credentials is not None,
    )
    if mcp_credentials is not None:
        minimax.update(mcp_credentials)
    jev_key = require_key(os.environ.get("TYPESAFE_API_KEY"), "TYPESAFE_API_KEY")
    jev_model = require_model(args.jev_model, "JEV model")
    output = validate_output_dir(Path(args.output_dir))

    credentials_path = output / "review-credentials.json"
    evidence_path = output / "evidence.json"
    settings_path = output / "review-settings.json"
    try:
        proxy = urlsplit(args.jev_proxy_url)
        if proxy.port is not None and not 1 <= proxy.port <= 65535:
            raise ValueError()
    except ValueError:
        raise ImportErrorSafe("invalid JEV proxy port") from None
    if proxy.scheme not in ("http", "https") or not proxy.hostname or proxy.username is not None or proxy.password is not None or proxy.query or proxy.fragment or proxy.path not in ("", "/"):
        raise ImportErrorSafe("invalid JEV proxy URL")
    settings = {
        "jev_proxy_url": args.jev_proxy_url,
        "evidence_file": str(evidence_path),
        "credentials_file": str(credentials_path),
        "jev_url": JEV_URL,
        "jev_model": jev_model,
        "minimax_url": minimax["url"],
        "minimax_model": minimax["model"],
        "max_candidates": 10,
        "max_read_bytes": 134217728,
    }

    created_output = False
    directory_fd = None
    try:
        try:
            output.mkdir(mode=0o700, parents=True)
            created_output = True
            directory_fd = os.open(output, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            os.fchmod(directory_fd, 0o700)
            write_private_json(credentials_path, {"typesafe_api_key": jev_key, "minimax_api_key": minimax["api_key"]}, directory_fd)
            write_private_json(evidence_path, [], directory_fd)
            write_private_json(settings_path, settings, directory_fd)
            if not os.path.samestat(os.fstat(directory_fd), os.stat(output, follow_symlinks=False)):
                raise ImportErrorSafe("output directory changed during import")
        except Exception:
            same_output = False
            if directory_fd is not None:
                with suppress(OSError):
                    same_output = os.path.samestat(
                        os.fstat(directory_fd), os.stat(output, follow_symlinks=False)
                    )
                for name in (credentials_path.name, evidence_path.name, settings_path.name):
                    with suppress(OSError):
                        os.unlink(name, dir_fd=directory_fd)
            if created_output and same_output:
                with suppress(OSError):
                    output.rmdir()
            raise
    except FileExistsError:
        raise ImportErrorSafe("output path appeared during import; nothing was overwritten") from None
    except OSError:
        raise ImportErrorSafe("private configuration files could not be written") from None
    finally:
        if directory_fd is not None:
            os.close(directory_fd)


def main():
    try:
        import_config(parse_args())
    except ImportErrorSafe as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    except Exception:
        print("error: configuration import failed", file=sys.stderr)
        return 1
    print("Offline review configuration created.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
