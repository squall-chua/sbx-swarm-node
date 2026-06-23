#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
npm run generate            # nuxi generate -> .output/public
rm -rf dist && mkdir -p dist
cp -r .output/public/* dist/
echo "built web/dist"
