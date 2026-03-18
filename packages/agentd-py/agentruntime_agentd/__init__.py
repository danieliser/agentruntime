"""agentruntime-agentd: Pre-built agentd binary distribution."""

import os
import stat
import subprocess
import sys


def get_binary_path() -> str:
    """Return the absolute path to the bundled agentd binary.

    Ensures the binary has executable permissions on Unix systems.
    """
    binary_name = "agentd.exe" if sys.platform == "win32" else "agentd"
    binary_path = os.path.join(os.path.dirname(__file__), "bin", binary_name)

    if not os.path.exists(binary_path):
        raise FileNotFoundError(
            f"agentd binary not found at {binary_path}. "
            "This may indicate a broken installation."
        )

    if sys.platform != "win32":
        current_mode = os.stat(binary_path).st_mode
        executable_mode = current_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH
        if current_mode != executable_mode:
            os.chmod(binary_path, executable_mode)

    return binary_path


def main() -> None:
    """Console script entry point: exec the bundled agentd binary."""
    binary_path = get_binary_path()

    if sys.platform == "win32":
        sys.exit(subprocess.call([binary_path] + sys.argv[1:]))
    else:
        os.execvp(binary_path, [os.path.basename(binary_path)] + sys.argv[1:])
