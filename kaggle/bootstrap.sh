#!/usr/bin/env bash
# Bootstrap a Kaggle session: restore the machine, install the engine, start it.
# The notebook runs this and then waits. All session logic lives here, in the
# repo, so the notebook itself stays minimal.
set -e

RAW_BASE="${RAW_BASE:-https://raw.githubusercontent.com/Tulipskun/aixodia-engine/main}"
PREFIX="/usr/local/bin"

# Make sure the snapshot script is present (fetched from this repo).
if [ ! -x "$PREFIX/kgat_sync.py" ]; then
  mkdir -p "$PREFIX"
  curl -fsSL "$RAW_BASE/kaggle/kgat_sync.py" -o "$PREFIX/kgat_sync.py"
  chmod +x "$PREFIX/kgat_sync.py"
fi

# Restore the machine from the last snapshot.
python3 "$PREFIX/kgat_sync.py" restore

# Install (or upgrade) the engine binary and launcher.
curl -fsSL "$RAW_BASE/install.sh" | sudo bash

# Start the engine (daemonizes; the notebook keeps the session alive).
aixodia
