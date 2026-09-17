#!/usr/bin/env python3
"""Persist the optional local client scheduler; preserve other profile rows."""
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parent.parent


def main():
    # Use Harness's own YAML dialect, cross-process writer lock and atomic writer.
    subprocess.run(['node', str(ROOT/'integrations/deepseek-harness/install.mjs')], check=True)


if __name__ == '__main__':
    main()
