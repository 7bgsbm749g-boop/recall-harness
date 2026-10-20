#!/usr/bin/env python3
"""Run real-model Recall memory checks in a separate synthetic workspace."""
import argparse
import json
import os
from pathlib import Path
import queue
import subprocess
import tempfile
import threading
import time


class ACP:
    def __init__(self, binary, env):
        self.process = subprocess.Popen([str(binary), "acp"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                        stderr=subprocess.PIPE, text=True, bufsize=1, env=env)
        self.messages = queue.Queue()
        self.counter = 0
        self.text = []
        def read():
            for line in self.process.stdout:
                try:
                    self.messages.put(json.loads(line))
                except ValueError:
                    self.messages.put({"error": {"message": "Invalid ACP output"}})
            self.messages.put(None)
        threading.Thread(target=read, daemon=True).start()

    def call(self, method, params):
        self.counter += 1
        request_id = self.counter
        self.process.stdin.write(json.dumps({"jsonrpc": "2.0", "id": request_id, "method": method, "params": params}) + "\n")
        self.process.stdin.flush()
        self.text = []
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            message = self.messages.get(timeout=max(0.1, deadline - time.monotonic()))
            if message is None:
                raise RuntimeError("Recall exited before replying")
            if message.get("method") == "session/request_permission":
                # This test authorizes no filesystem or external actions.
                self.process.stdin.write(json.dumps({"jsonrpc": "2.0", "id": message["id"], "result": {"outcome": {"outcome": "selected", "optionId": "reject_once"}}}) + "\n")
                self.process.stdin.flush()
            elif message.get("method") == "session/update":
                update = message["params"]["update"]
                if update.get("sessionUpdate") == "agent_message_chunk":
                    self.text.append(update.get("content", {}).get("text", ""))
            elif message.get("id") == request_id:
                if "error" in message:
                    raise RuntimeError(message["error"]["message"])
                return message["result"], "".join(self.text).strip()
        raise RuntimeError("Recall smoke request timed out")

    def prompt(self, session, text):
        return self.call("session/prompt", {"sessionId": session, "prompt": [{"type": "text", "text": text}]})

    def close(self):
        if self.process.poll() is None:
            self.process.stdin.close()
            try:
                self.process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--evidence", type=Path, required=True)
    args = parser.parse_args()
    args.evidence.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="recall-live-") as temporary:
        root = Path(temporary)
        workspace = root / "workspace"
        workspace.mkdir()
        env = dict(os.environ, RECALL_DATA_DIR=str(root / "state"), RECALL_REASONING_EFFORT="low",
                   RECALL_SCAN_TOKENS="1024", RECALL_COMPACT_TOKENS="4096", RECALL_CONTEXT_TOKENS="32768",
                   RECALL_RESERVE_TOKENS="8192", RECALL_RETAIN_TOKENS="1024", RECALL_SUMMARY_BYTES="8000",
                   RECALL_LAYER_FANOUT="2")
        peer = ACP(args.binary, env)
        results = {}
        def flush_memory():
            completed = subprocess.run([str(args.binary), "memory", "worker", "--once"], env=env,
                                       text=True, capture_output=True, timeout=180)
            if completed.returncode:
                raise RuntimeError("Memory worker failed: " + completed.stderr[:1000])
        try:
            peer.call("initialize", {"protocolVersion": 2, "clientCapabilities": {}, "clientInfo": {"name": "Recall live test", "version": "1"}})
            setup, _ = peer.call("session/new", {"cwd": str(workspace), "mcpServers": []})
            session = setup["sessionId"]
            filler = "\n".join(f"Closed historical item {i}: verified complete, no current action." for i in range(600))
            result, text = peer.prompt(session, "This is a synthetic memory test. The initial synthetic export format is JSON. "
                                      "The following closed items need no retention. Do not use tools. Reply exactly SAVED_JSON.\n" + filler)
            if text != "SAVED_JSON":
                raise RuntimeError("Initial live response did not match: " + text[:300])
            results["initial_response"] = True
            flush_memory()
            result, text = peer.prompt(session, "Correction for the synthetic project: Use CSV for all future synthetic exports. "
                                      "This replaces the earlier JSON format choice. Do not use tools. Reply exactly SAVED_CSV.")
            if text != "SAVED_CSV":
                raise RuntimeError("Correction response did not match: " + text[:300])
            stats = result.get("_meta", {}).get("recall", {})
            if stats.get("compactions", 0) < 1:
                raise RuntimeError("Live test did not activate prepared compaction")
            results["prepared_compaction"] = stats
            flush_memory()
            _, text = peer.prompt(session, "What format now applies to synthetic exports? Reply with only its name. Do not use tools.")
            if text != "CSV":
                raise RuntimeError("Corrected fact was lost after compaction: " + text[:300])
            results["correction_after_compaction"] = True
            flush_memory()
            setup, _ = peer.call("session/new", {"cwd": str(workspace), "mcpServers": []})
            _, text = peer.prompt(setup["sessionId"], "What format now applies to synthetic exports in this project? Reply with only its name. Do not use tools.")
            if text != "CSV":
                raise RuntimeError("New thread did not receive corrected project memory: " + text[:300])
            results["new_thread_project_memory"] = True
            flush_memory()
            verified = subprocess.run([str(args.binary), "memory", "verify", "--workspace", str(workspace)],
                                      env=env, capture_output=True, text=True, check=True)
            results["integrity"] = json.loads(verified.stdout)
            (args.evidence / "live-acp-memory.json").write_text(json.dumps(results, indent=2), encoding="utf-8")
            print(json.dumps(results), flush=True)
        finally:
            peer.close()


if __name__ == "__main__":
    main()
