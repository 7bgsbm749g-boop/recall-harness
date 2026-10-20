#!/usr/bin/env python3
"""Replicate Recall memory through private SSH, without model credentials or tool state."""
import argparse
import base64
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import time


def encoded_request(payload):
    return base64.b64encode(json.dumps(payload, separators=(",", ":")).encode()).decode("ascii")


class Sync:
    def __init__(self, peer, ssh=None):
        root = Path.home() / ".local" / "share" / "recall"
        self.binary = json.loads((root / "install.json").read_text(encoding="utf-8"))["binary"]
        self.cache_path = root / f"sync-{peer}.json"
        self.peer = peer
        if ssh is None:
            config_path = Path.home() / ".codex" / "fleet-adapter" / "config.json"
            config = json.loads(config_path.read_text(encoding="utf-8")) if config_path.exists() else {}
            ssh = config.get("sshExecutable", "ssh")
        self.ssh = ssh

    def remote(self, operation, key=None):
        payload = {"operation": operation}
        if key is not None:
            payload["project_key"] = key
        encoded = encoded_request(payload)
        if self.peer == "fleet-vps":
            receiver = shlex.join(["python3", "/home/ubuntu/.local/share/recall/sync-endpoint.py", encoded])
        else:
            receiver = f'python "C:/Users/jacks/.local/share/recall/sync-endpoint.py" {encoded}'
        return [self.ssh, "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", self.peer, receiver]

    @staticmethod
    def output(command):
        completed = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=30)
        if completed.returncode:
            raise RuntimeError(f"Memory transport command failed with exit code {completed.returncode}")
        return completed.stdout

    def local(self, operation, key=None):
        command = [self.binary, "memory", operation]
        if key is not None:
            command += ["--project-key", key]
        return command

    @staticmethod
    def transfer(export_command, import_command, known):
        exporter = subprocess.Popen(export_command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        importer = None
        try:
            exporter.stdin.write(known)
            exporter.stdin.close()
            importer = subprocess.Popen(import_command, stdin=exporter.stdout, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            exporter.stdout.close()
            result, _ = importer.communicate(timeout=300)
            exporter.wait(timeout=30)
            if importer.returncode or exporter.returncode:
                raise RuntimeError("Memory transfer failed; current transcript heads were retained")
            return json.loads(result)
        finally:
            for process in (exporter, importer):
                if process is not None and process.poll() is None:
                    process.kill()
                    process.wait()
                if process is not None:
                    for stream in (process.stdin, process.stdout, process.stderr):
                        if stream is not None and not stream.closed:
                            stream.close()

    def once(self):
        local = {p["key"]: p for p in json.loads(self.output(self.local("projects")))}
        remote = {p["key"]: p for p in json.loads(self.output(self.remote("projects")))}
        cache = json.loads(self.cache_path.read_text(encoding="utf-8")) if self.cache_path.exists() else {}
        before = {key: [local.get(key, {}).get("fingerprint"), remote.get(key, {}).get("fingerprint")]
                  for key in local.keys() | remote.keys()}
        changed = 0
        for key in sorted(local.keys() | remote.keys()):
            signature = [local.get(key, {}).get("fingerprint"), remote.get(key, {}).get("fingerprint")]
            if cache.get(key) == signature:
                continue
            if key in remote:
                known = self.output(self.local("inventory", key))
                changed += self.transfer(self.remote("export", key), self.local("import"), known)["changed_threads"]
            if key in local or key in remote:
                known = self.output(self.remote("inventory", key))
                changed += self.transfer(self.local("export", key) + ["--gzip", "--known-stdin"], self.remote("import"), known)["changed_threads"]
        local = {p["key"]: p for p in json.loads(self.output(self.local("projects")))}
        remote = {p["key"]: p for p in json.loads(self.output(self.remote("projects")))}
        # A foreground turn may append evidence while a snapshot is exported.
        # Cache only pairs that stayed stable throughout this exchange. Otherwise
        # the next pass must transfer the newly observed version too.
        state = {}
        for key in local.keys() | remote.keys():
            signature = [local.get(key, {}).get("fingerprint"), remote.get(key, {}).get("fingerprint")]
            if signature == before.get(key):
                state[key] = signature
        temporary = self.cache_path.with_suffix(".tmp")
        temporary.write_text(json.dumps(state), encoding="utf-8")
        temporary.chmod(0o600)
        os.replace(temporary, self.cache_path)
        return changed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--peer", choices=("fleet-vps", "fleet-laptop"), required=True)
    parser.add_argument("--ssh-executable")
    parser.add_argument("--watch", action="store_true")
    parser.add_argument("--interval", type=int, default=30)
    args = parser.parse_args()
    if args.interval < 10:
        parser.error("The sync interval must be at least ten seconds")
    sync = Sync(args.peer, args.ssh_executable)
    while True:
        try:
            count = sync.once()
            if count or not args.watch:
                print(json.dumps({"peer": args.peer, "changed_threads": count}), flush=True)
        except Exception as error:
            print(f"Recall memory sync: {error}", file=sys.stderr, flush=True)
            if not args.watch:
                return 1
        if not args.watch:
            return 0
        time.sleep(args.interval)


if __name__ == "__main__":
    sys.exit(main())
