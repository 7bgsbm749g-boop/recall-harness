#!/usr/bin/env python3
"""Restricted, binary-safe memory transfer endpoint for private fleet SSH."""
import base64
import json
import os
from pathlib import Path
import sys


def command(payload, binary):
    operation = payload.get("operation")
    if operation in ("projects", "import"):
        return [str(binary), "memory", operation]
    if operation not in ("inventory", "export"):
        raise ValueError("Unsupported memory transfer operation")
    key = payload.get("project_key")
    if not isinstance(key, str) or not key or len(key) > 4096 or "\0" in key:
        raise ValueError("Invalid memory project identity")
    result = [str(binary), "memory", operation, "--project-key", key]
    if operation == "export":
        result += ["--gzip", "--known-stdin"]
    return result


def main():
    if len(sys.argv) != 2 or len(sys.argv[1]) > 16384:
        raise ValueError("Expected one encoded memory transfer request")
    payload = json.loads(base64.b64decode(sys.argv[1], validate=True))
    manifest = Path.home() / ".local" / "share" / "recall" / "install.json"
    installed = json.loads(manifest.read_text(encoding="utf-8"))
    binary = Path(installed["binary"])
    if not binary.is_file():
        raise ValueError("The installed Recall binary is missing")
    arguments = command(payload, binary)
    os.execv(str(binary), arguments)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(f"Recall memory endpoint: {error}", file=sys.stderr)
        sys.exit(1)
