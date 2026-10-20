#!/usr/bin/env python3
"""Verify real image input and process restart with an independent synthetic fixture."""
import argparse
import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--image", type=Path, required=True)
    parser.add_argument("--expected", required=True, help="code visible only in the image pixels")
    parser.add_argument("--evidence", type=Path, required=True)
    args = parser.parse_args()
    args.evidence.mkdir(parents=True, exist_ok=True)
    spec = importlib.util.spec_from_file_location("smoke_acp", Path(__file__).with_name("smoke-acp.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    image = args.image.read_bytes()
    results = {"image_sha256": hashlib.sha256(image).hexdigest()}
    with tempfile.TemporaryDirectory(prefix="recall-image-") as temporary:
        root = Path(temporary)
        workspace = root / "workspace"
        workspace.mkdir()
        env = dict(os.environ, RECALL_DATA_DIR=str(root / "state"), RECALL_REASONING_EFFORT="low")
        peer = module.ACP(args.binary, env)
        def initialize():
            result, _ = peer.call("initialize", {"protocolVersion": 2, "clientCapabilities": {},
                                                 "clientInfo": {"name": "Recall image smoke", "version": "1"}})
            if not result["agentCapabilities"]["promptCapabilities"].get("image"):
                raise RuntimeError("Image prompt capability is missing")
        try:
            initialize()
            setup, _ = peer.call("session/new", {"cwd": str(workspace), "mcpServers": []})
            session = setup["sessionId"]
            _, text = peer.call("session/prompt", {"sessionId": session, "prompt": [
                {"type": "text", "text": "Read the code in the image. Reply with only that code. Do not use tools."},
                {"type": "image", "mimeType": "image/png", "data": base64.b64encode(image).decode("ascii")}]})
            if text != args.expected:
                raise RuntimeError("Image recognition failed: " + text[:300])
            results["native_image_input"] = True
            peer.close()
            peer = module.ACP(args.binary, env)
            initialize()
            peer.call("session/load", {"sessionId": session, "cwd": str(workspace), "mcpServers": []})
            _, text = peer.prompt(session, "Read the code from the earlier image again. Reply with only that code. Do not use tools.")
            if text != args.expected:
                raise RuntimeError("Image history did not survive restart: " + text[:300])
            results["process_restart"] = True
            verified = subprocess.run([str(args.binary), "memory", "verify", "--workspace", str(workspace)],
                                      env=env, capture_output=True, text=True, check=True, timeout=60)
            results["integrity"] = json.loads(verified.stdout)
        finally:
            peer.close()
    (args.evidence / "live-images.json").write_text(json.dumps(results, indent=2), encoding="utf-8")
    print(json.dumps(results), flush=True)


if __name__ == "__main__":
    main()
