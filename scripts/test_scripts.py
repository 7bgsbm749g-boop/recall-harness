import base64
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest


def load(name):
    path = Path(__file__).with_name(name + ".py")
    spec = importlib.util.spec_from_file_location("recall_" + name.replace("-", "_"), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


endpoint = load("sync-endpoint")
sync = load("sync-memory")


class MemoryTransportTests(unittest.TestCase):
    def test_concurrent_append_is_not_cached_as_transferred(self):
        with tempfile.TemporaryDirectory(prefix="recall-sync-test-") as root:
            peer = object.__new__(sync.Sync)
            peer.cache_path = Path(root) / "cache.json"
            peer.local = lambda operation, key=None: ["local", operation]
            peer.remote = lambda operation, key=None: ["remote", operation]
            scans = iter(["old", "old", "new", "old"])
            transfers = []
            def output(command):
                if command[1] == "projects":
                    return json.dumps([{"key":"fixture","fingerprint":next(scans)}]).encode()
                return b"[]"
            peer.output = output
            peer.transfer = lambda *args: (transfers.append(args) or {"changed_threads":0})
            peer.once()
            self.assertNotIn("fixture", json.loads(peer.cache_path.read_text()))
            scans = iter(["new", "old", "new", "old"])
            peer.once()
            self.assertEqual(len(transfers), 4, "new evidence was skipped after a concurrent append")
            scans = iter(["new", "old", "new", "old"])
            peer.once()
            self.assertEqual(len(transfers), 4, "stable completed exchange was not cached")

    def test_endpoint_restricts_operations(self):
        with self.assertRaises(ValueError):
            endpoint.command({"operation": "run", "command": "arbitrary shell"}, "/fixture/recall")
        with self.assertRaises(ValueError):
            endpoint.command({"operation": "export", "project_key": "bad\0key"}, "/fixture/recall")

    def test_project_identity_remains_one_literal_argument(self):
        key = "directory:C:/project with spaces/$(untrusted);'\""
        payload = {"operation": "export", "project_key": key}
        encoded = sync.encoded_request(payload)
        self.assertEqual(json.loads(base64.b64decode(encoded)), payload)
        command = endpoint.command(payload, "C:/Program Files/Recall/recall.exe")
        self.assertEqual(command[-3], key)
        self.assertEqual(command[-2:], ["--gzip", "--known-stdin"])

    def test_binary_pipeline_preserves_bytes(self):
        exporter = [sys.executable, "-c", "import sys; assert sys.stdin.buffer.read()==b'[]'; sys.stdout.buffer.write(bytes(range(256))*2048)"]
        importer = [sys.executable, "-c", "import sys,json; data=sys.stdin.buffer.read(); assert data==bytes(range(256))*2048; print(json.dumps({'changed_threads':1}))"]
        self.assertEqual(sync.Sync.transfer(exporter, importer, b"[]"), {"changed_threads": 1})

    def test_failed_pipeline_is_reported(self):
        exporter = [sys.executable, "-c", "import sys; sys.stdin.buffer.read(); sys.stdout.buffer.write(b'partial'); sys.exit(2)"]
        importer = [sys.executable, "-c", "import sys; sys.stdin.buffer.read(); print('{}')"]
        with self.assertRaisesRegex(RuntimeError, "retained"):
            sync.Sync.transfer(exporter, importer, b"[]")


if __name__ == "__main__":
    unittest.main()
