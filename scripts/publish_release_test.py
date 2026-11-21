import hashlib
import io
import json
import subprocess
import tempfile
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest.mock import patch

from publish_release import ARCHIVES, CHECKSUMS, publish


class ReleaseTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.out = Path(self.temp.name)
        self.remote = {}
        self.commands = []
        for name in ARCHIVES:
            (self.out / name).write_bytes(name.encode())
        self.draft = False
        self.exists = True
        patcher = patch("publish_release.gh", self.fake_gh)
        patcher.start()
        self.addCleanup(patcher.stop)

    def fake_gh(self, *args):
        self.commands.append(args)
        action = args[1]
        if action == "view":
            if not self.exists:
                raise subprocess.CalledProcessError(1, args, stderr="release not found")
            return json.dumps({
                "isDraft": self.draft,
                "assets": [{"name": k, "size": len(v)} for k, v in self.remote.items()],
            })
        if action == "create":
            self.assertIn("--verify-tag", args)
            self.assertIn("--draft", args)
            self.exists = True
            self.draft = True
        elif action == "download":
            name = args[args.index("--pattern") + 1]
            directory = Path(args[args.index("--dir") + 1])
            (directory / name).write_bytes(self.remote[name])
        elif action == "upload":
            for filename in args[3:]:
                path = Path(filename)
                self.assertNotIn(path.name, self.remote)
                self.remote[path.name] = path.read_bytes()
        elif action == "edit":
            self.draft = False
        else:
            self.fail(f"unexpected gh command: {args}")
        return ""

    def run_publish(self):
        with redirect_stdout(io.StringIO()):
            publish("v0.0.2", self.out)

    def test_precreated_release_gets_all_artifacts(self):
        self.run_publish()
        self.assertEqual(set(self.remote), {*ARCHIVES, CHECKSUMS})
        self.assertEqual(
            self.remote[CHECKSUMS].decode(),
            "".join(f"{hashlib.sha256(self.remote[name]).hexdigest()}  {name}\n" for name in ARCHIVES),
        )

    def test_partial_release_keeps_original_zip_and_hashes_it(self):
        name = ARCHIVES[0]
        self.remote[name] = b"archive already published"
        self.run_publish()
        self.assertEqual(self.remote[name], b"archive already published")
        self.assertIn(hashlib.sha256(self.remote[name]).hexdigest().encode(), self.remote[CHECKSUMS])

    def test_conflicting_manifest_prevents_upload(self):
        self.remote[CHECKSUMS] = b"wrong checksum"
        with self.assertRaisesRegex(RuntimeError, "existing SHA256SUMS"):
            self.run_publish()
        self.assertEqual(set(self.remote), {CHECKSUMS})

    def test_new_release_is_created_as_draft_and_published_after_upload(self):
        self.exists = False
        self.run_publish()
        self.assertEqual([call[1] for call in self.commands],
                         ["view", "create", "upload", "upload", "view", "edit"])
        self.assertEqual(set(self.remote), {*ARCHIVES, CHECKSUMS})
        self.assertFalse(self.draft)

    def test_complete_release_is_unchanged_on_retry(self):
        self.run_publish()
        self.commands.clear()
        self.run_publish()
        self.assertEqual([call[1] for call in self.commands], ["view"])


if __name__ == "__main__":
    unittest.main()
