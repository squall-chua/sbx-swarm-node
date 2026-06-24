#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
npm run generate            # nuxi generate -> .output/public
rm -rf dist && mkdir -p dist
cp -r .output/public/* dist/
touch dist/.gitkeep          # recreate the tracked placeholder rm -rf wiped, so builds leave git clean
echo "built web/dist"
