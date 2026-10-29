# SPDX-License-Identifier: MIT

"""Copy durable project documents without depending on the GUI event loop."""
import os
from pathlib import Path
import shutil
import stat
import time
import uuid


def recover(root):
    root = Path(root)
    if not root.is_dir() or root.resolve() != root:
        raise ValueError("Project directory missing or redirected")
    copies = []
    for directory, dirs, files in os.walk(root, followlinks=False):
        dirs[:] = [d for d in dirs if not Path(directory, d).is_symlink()]
        for name in files:
            source = Path(directory, name)
            if source.is_symlink() or ".recovery-" in name or source.suffix.lower() not in (".fcstd", ".fcstd1"):
                continue
            target = source.with_name(source.stem + f".recovery-{time.time_ns()}-{uuid.uuid4().hex[:8]}.FCStd")
            temporary = target.with_suffix(".tmp")
            fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            try:
                with os.fdopen(fd, "rb") as original, temporary.open("xb") as copy:
                    if not stat.S_ISREG(os.fstat(original.fileno()).st_mode):
                        raise ValueError("Recovery source is not a regular file")
                    shutil.copyfileobj(original, copy, 1 << 20)
                    copy.flush()
                    os.fsync(copy.fileno())
                temporary.rename(target)
            finally:
                temporary.unlink(missing_ok=True)
            copies.append(str(target.relative_to(root)))
    return copies


if __name__ == "__main__":
    import json
    print(json.dumps({"recovery_files": recover(os.environ["WORKSPACE_FREECAD_PROJECT"])}))
