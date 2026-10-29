#!/usr/bin/env python3
# SPDX-License-Identifier: MIT

"""Detects encoding, separator and decimal mark of a CSV and prints pd.read_csv arguments plus a preview.

Usage: python3 csv_check.py inbox/file.csv
"""
import csv
import re
import sys

import pandas as pd


def detect(path: str) -> dict:
    raw = open(path, "rb").read(200_000)
    # latin-1 decodes every byte string, so the loop always ends with a result
    for encoding in ("utf-8-sig", "cp1252", "latin-1"):
        try:
            text = raw.decode(encoding)
            break
        except UnicodeDecodeError:
            continue
    try:
        sep = csv.Sniffer().sniff(text[:20_000], delimiters=";,\t|").delimiter
    except csv.Error:
        sep = ";" if text.count(";") > text.count(",") else ","
    args = {"sep": sep, "encoding": encoding}
    # decimal comma only possible when the comma is not the separator
    if sep != "," and re.search(r"\d,\d", text):
        args["decimal"] = ","
        if re.search(r"\d\.\d{3}(?!\d)", text):
            args["thousands"] = "."
    return args


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: csv_check.py <file.csv>")
    args = detect(sys.argv[1])
    print("pd.read_csv(path, " + ", ".join(f"{k}={v!r}" for k, v in args.items()) + ")")
    df = pd.read_csv(sys.argv[1], **args)
    print(f"{len(df)} Zeilen, {len(df.columns)} Spalten")
    print(df.dtypes.to_string())
    print(df.head(5).to_string())
