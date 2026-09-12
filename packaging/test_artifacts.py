"""Build real distributables; run with python3 packaging/test_artifacts.py."""

from pathlib import Path
import struct
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


class Artifacts(unittest.TestCase):
    def test_windows(self):
        with tempfile.TemporaryDirectory(prefix="screen share ") as directory:
            subprocess.run(
                ["bash", "packaging/build.sh", "windows", directory],
                cwd=ROOT, check=True,
            )
            artifact = Path(directory) / "screen-share-windows-amd64.exe"
            data = artifact.read_bytes()
            self.assertEqual(data[:2], b"MZ")
            pe = struct.unpack_from("<I", data, 0x3C)[0]
            self.assertEqual(data[pe:pe + 4], b"PE\0\0")
            self.assertEqual(struct.unpack_from("<H", data, pe + 4)[0], 0x8664)
            self.assertEqual(struct.unpack_from("<H", data, pe + 24 + 68)[0], 2)
            self.assertIn(b"getDisplayMedia", data)  # Embedded capture UI.
            self.assertEqual(list(Path(directory).iterdir()), [artifact])

    def test_linux(self):
        with tempfile.TemporaryDirectory(prefix="screen share ") as directory:
            subprocess.run(
                ["bash", "packaging/build.sh", "linux", directory],
                cwd=ROOT, check=True,
            )
            artifact = Path(directory) / "screen-share-linux-x86_64.AppImage"
            self.assertEqual(artifact.read_bytes()[8:11], b"AI\x02")
            self.assertTrue(artifact.stat().st_mode & 0o111)
            subprocess.run(
                ["docker", "build", "--target", "smoke", "-t", "screen-share-smoke",
                 "-f", "packaging/Dockerfile", "."], cwd=ROOT, check=True,
            )
            # Only the artifact enters this fresh runtime: no build-host libraries.
            subprocess.run(
                ["docker", "run", "--rm", "--network=none",
                 "-v", f"{artifact}:/tmp/screen share.AppImage:ro",
                 "screen-share-smoke"], check=True,
            )


if __name__ == "__main__":
    unittest.main()
