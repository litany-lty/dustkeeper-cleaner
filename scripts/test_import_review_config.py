import json
import importlib.util
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest


SCRIPT = Path(__file__).with_name("import-review-config.py")
FAKE_JEV_KEY = "ts-test-key-1234567890"
FAKE_MINIMAX_KEY = "mm-test-key-1234567890"
FAKE_MINIMAX_MCP_KEY = "mm-api-test-key-0987654321"


class ImportReviewConfigTest(unittest.TestCase):
    def run_import(
        self, config_text, output_name="review", mcp_config=None, **env_overrides
    ):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        config = root / "config.yaml"
        output = root / output_name
        config.write_text(config_text, encoding="utf-8")
        env = os.environ.copy()
        env["TYPESAFE_API_KEY"] = FAKE_JEV_KEY
        env.update(env_overrides)
        command = [
            sys.executable,
            str(SCRIPT),
            "--minimax-config",
            str(config),
            "--output-dir",
            str(output),
        ]
        if mcp_config is not None:
            mcp_path = root / "mcp.json"
            mcp_path.write_text(json.dumps(mcp_config), encoding="utf-8")
            command.extend(["--minimax-mcp-config", str(mcp_path)])
        result = subprocess.run(
            command,
            capture_output=True,
            text=True,
            env=env,
            check=False,
        )
        return result, output

    @staticmethod
    def valid_config(api_key=FAKE_MINIMAX_KEY, base_url="https://api.minimax.io/anthropic"):
        return f"""
provider:
  minimax:
    npm: '@ai-sdk/anthropic'
    options:
      baseURL: '{base_url}'
      apiKey: '{api_key}'
defaultModel: 'minimax/MiniMax-M3.1-Flash'
"""

    def test_import_writes_private_complete_configuration(self):
        result, output = self.run_import(self.valid_config())

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o700)
        files = {path.name for path in output.iterdir()}
        self.assertEqual(
            files,
            {"review-credentials.json", "evidence.json", "review-settings.json"},
        )
        for path in output.iterdir():
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)

        credentials = json.loads((output / "review-credentials.json").read_text())
        self.assertEqual(
            credentials,
            {
                "typesafe_api_key": FAKE_JEV_KEY,
                "minimax_api_key": FAKE_MINIMAX_KEY,
            },
        )
        self.assertEqual(json.loads((output / "evidence.json").read_text()), [])
        settings = json.loads((output / "review-settings.json").read_text())
        self.assertEqual(
            settings,
            {
                "evidence_file": str((output / "evidence.json").resolve()),
                "credentials_file": str((output / "review-credentials.json").resolve()),
                "jev_url": "https://api.typesafe.ai/v1/systemone",
                "jev_proxy_url": "http://127.0.0.1:7890",
                "jev_model": "jev-latest",
                "minimax_url": "https://api.minimax.io/anthropic/v1/messages",
                "minimax_model": "MiniMax-M3.1-Flash",
                "max_candidates": 10,
                "max_read_bytes": 134217728,
            },
        )
        combined_output = result.stdout + result.stderr
        self.assertNotIn(FAKE_JEV_KEY, combined_output)
        self.assertNotIn(FAKE_MINIMAX_KEY, combined_output)

    def test_existing_output_directory_is_rejected_without_changes(self):
        result, output = self.run_import(self.valid_config())
        self.assertEqual(result.returncode, 0, result.stderr)
        marker = output / "marker"
        marker.write_text("keep", encoding="utf-8")

        second = subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--minimax-config",
                str(output.parent / "config.yaml"),
                "--output-dir",
                str(output),
            ],
            capture_output=True,
            text=True,
            env={**os.environ, "TYPESAFE_API_KEY": FAKE_JEV_KEY},
            check=False,
        )

        self.assertNotEqual(second.returncode, 0)
        self.assertEqual(marker.read_text(encoding="utf-8"), "keep")

    def test_error_output_never_contains_key(self):
        result, output = self.run_import(
            self.valid_config(base_url="http://example.invalid?token=" + FAKE_MINIMAX_KEY)
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(output.exists())
        self.assertNotIn(FAKE_JEV_KEY, result.stdout + result.stderr)
        self.assertNotIn(FAKE_MINIMAX_KEY, result.stdout + result.stderr)

    def test_oversized_minimax_config_is_rejected(self):
        oversized = "#" + ("x" * (1 << 20)) + "\n" + self.valid_config()
        result, output = self.run_import(oversized)

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(output.exists())

    def test_write_failure_removes_partial_credentials_and_output(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            config = root / "config.yaml"
            output = root / "review"
            config.write_text(self.valid_config(), encoding="utf-8")
            spec = importlib.util.spec_from_file_location("audit_import_config", SCRIPT)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            original = module.write_private_json
            calls = 0

            def fail_second_write(path, value, directory_fd=None):
                nonlocal calls
                calls += 1
                if calls == 2:
                    raise OSError("injected write failure")
                return original(path, value, directory_fd)

            module.write_private_json = fail_second_write
            args = SimpleNamespace(
                minimax_mcp_config=None,
                minimax_config=str(config),
                jev_model="jev-latest",
                jev_proxy_url="http://127.0.0.1:7890",
                output_dir=str(output),
            )
            previous = os.environ.get("TYPESAFE_API_KEY")
            os.environ["TYPESAFE_API_KEY"] = FAKE_JEV_KEY
            try:
                with self.assertRaises(module.ImportErrorSafe):
                    module.import_config(args)
            finally:
                if previous is None:
                    os.environ.pop("TYPESAFE_API_KEY", None)
                else:
                    os.environ["TYPESAFE_API_KEY"] = previous

            self.assertFalse(output.exists())

    def test_placeholder_key_is_rejected(self):
        result, output = self.run_import(self.valid_config(api_key="YOUR_API_KEY"))

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(output.exists())

    def test_replace_instruction_key_is_rejected(self):
        result, output = self.run_import(
            self.valid_config(api_key="REPLACE_WITH_YOUR_API_KEY")
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(output.exists())

    def test_key_with_header_control_character_is_rejected(self):
        result, output = self.run_import(
            self.valid_config(), TYPESAFE_API_KEY="fake-jev\nheader-injection"
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(output.exists())

    def test_mcp_config_selects_cn_api_credentials_instead_of_client_key(self):
        client_config = self.valid_config(
            api_key=FAKE_MINIMAX_KEY,
            base_url="https://agent.minimax.cn/mavis/v1",
        )
        mcp_config = {
            "mcpServers": {
                "MiniMax": {
                    "env": {
                        "MINIMAX_API_HOST": "https://api.minimaxi.com",
                        "MINIMAX_API_KEY": FAKE_MINIMAX_MCP_KEY,
                    }
                }
            }
        }

        result, output = self.run_import(client_config, mcp_config=mcp_config)

        self.assertEqual(result.returncode, 0, result.stderr)
        credentials = json.loads((output / "review-credentials.json").read_text())
        settings = json.loads((output / "review-settings.json").read_text())
        self.assertEqual(credentials["minimax_api_key"], FAKE_MINIMAX_MCP_KEY)
        self.assertNotEqual(credentials["minimax_api_key"], FAKE_MINIMAX_KEY)
        self.assertEqual(
            settings["minimax_url"],
            "https://api.minimaxi.com/anthropic/v1/messages",
        )
        self.assertEqual(settings["minimax_model"], "MiniMax-M3.1-Flash")
        combined_output = result.stdout + result.stderr
        self.assertNotIn(FAKE_MINIMAX_KEY, combined_output)
        self.assertNotIn(FAKE_MINIMAX_MCP_KEY, combined_output)

    def test_client_gateway_without_mcp_api_credentials_is_rejected(self):
        result, output = self.run_import(
            self.valid_config(base_url="https://agent.minimax.io/mavis/v1")
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(output.exists())
        self.assertIn("--minimax-mcp-config", result.stderr)
        self.assertNotIn(FAKE_MINIMAX_KEY, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
