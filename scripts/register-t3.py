#!/usr/bin/env python3
"""Register Recall through T3's authenticated settings RPC on a fleet host.

The fleet adapter supplies short-lived authentication and transport only.
This script performs no thread operations and never edits T3's database.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import sys


def register(binary, instance_id="recall"):
    adapter = Path.home() / ".codex" / "fleet-adapter" / "t3_fleet.py"
    if not adapter.is_file():
        raise RuntimeError("The fleet T3 transport is missing. Add Recall as a Local ACP command in T3 Settings → Providers.")
    spec = importlib.util.spec_from_file_location("recall_t3_settings_transport", adapter)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    config = json.loads(adapter.with_name("config.json").read_text(encoding="utf-8"))
    local_cli = Path.home() / ".local" / "bin" / "t3"
    if os.name != "nt" and local_cli.is_file():
        config["t3Command"] = [str(local_cli)]
    expected = {"source": "local", "commandPath": str(binary), "commandArgs": ["acp"]}
    with module.T3(config) as app:
        settings = app.rpc("server.getSettings", {})
        current = settings.get("providerInstances", {}).get(instance_id)
        if current is not None:
            if current.get("driver") != "acpRegistry" or current.get("displayName") != "Recall":
                raise RuntimeError("The selected provider ID belongs to another provider. Choose a different --instance-id.")
            if all(current.get("config", {}).get(k) == v for k, v in expected.items()) and current.get("enabled", True):
                return {"instance_id": instance_id, "registered": True, "changed": False}
            if any(entry.get("valueRedacted") for entry in current.get("environment", [])):
                raise RuntimeError("Recall has private environment overrides. Change its executable in T3 Settings to preserve those values.")
        instance = dict(current or {})
        instance.update(driver="acpRegistry", displayName="Recall", enabled=True)
        instance["config"] = dict(instance.get("config", {}), **expected)
        result = app.rpc("server.updateSettings", {"patch": {}, "providerInstanceMutation": {
            "operation": "upsert" if current else "create", "instanceId": instance_id, "instance": instance}})
        saved = result.get("providerInstances", {}).get(instance_id, {})
        if saved.get("config", {}).get("commandPath") != str(binary):
            raise RuntimeError("T3 did not confirm the Recall provider configuration.")
        return {"instance_id": instance_id, "registered": True, "changed": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--instance-id", default="recall")
    args = parser.parse_args()
    binary = args.binary.absolute()
    if not binary.is_file():
        parser.error("Recall binary is missing")
    print(json.dumps(register(binary, args.instance_id)))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(f"Recall registration: {error}", file=sys.stderr)
        sys.exit(1)
