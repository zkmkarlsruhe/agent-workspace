#!/usr/bin/env python3
# SPDX-License-Identifier: MIT

"""Prints the next free file name: file.xlsx, else file_v2.xlsx, file_v3.xlsx ...

Usage: python3 next_name.py results/report.xlsx
If the name is already a version (report_v3.xlsx), counting continues from the base name.
"""
import re
import sys
from pathlib import Path


def next_name(path: str) -> Path:
    p = Path(path)
    stem = re.sub(r"_v\d+$", "", p.stem)
    candidate = p.with_name(stem + p.suffix)
    n = 1
    while candidate.exists():
        n += 1
        candidate = p.with_name(f"{stem}_v{n}{p.suffix}")
    return candidate


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: next_name.py <path/file.ext>")
    print(next_name(sys.argv[1]))
