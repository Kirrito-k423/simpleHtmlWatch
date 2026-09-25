#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
version="${1:-dev}"
mkdir -p dist
for target in windows/amd64 windows/arm64 darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  target_os="${target%/*}"
  target_arch="${target#*/}"
  package="simpleHtmlWatch_${version}_${target_os}_${target_arch}"
  folder="dist/${package}"
  mkdir -p "$folder"
  executable="simpleHtmlWatch"
  if [[ "$target_os" == windows ]]; then executable="simpleHtmlWatch.exe"; fi
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -trimpath -ldflags "-s -w -X main.version=${version}" -o "$folder/$executable" .
  cp README.md LICENSE AI-START-HERE.md "$folder/"
  mkdir -p "$folder/docs"
  cp docs/dashboard.png docs/troubleshooting.md docs/task-controller.md "$folder/docs/"
  mkdir -p "$folder/skills/cluster-task-controller"
  cp -R skills/cluster-task-controller/. "$folder/skills/cluster-task-controller/"
  if [[ "$target_os" == windows ]]; then
    cp scripts/start.bat "$folder/start.bat"
    (cd dist && zip -qr "${package}.zip" "$package")
  else
    if [[ "$target_os" == darwin ]]; then
      cp scripts/start.command "$folder/start.command"
      chmod +x "$folder/start.command"
    else
      cp scripts/start.sh "$folder/start.sh"
      chmod +x "$folder/start.sh"
    fi
    tar -czf "dist/${package}.tar.gz" -C dist "$package"
  fi
done
(cd skills && zip -qr "../dist/cluster-task-controller_${version}.zip" cluster-task-controller)
python3 - "$version" <<'PY'
import hashlib, pathlib, sys
files = sorted(p for p in pathlib.Path('dist').iterdir() if p.is_file() and (p.name.startswith('simpleHtmlWatch_' + sys.argv[1] + '_') or p.name == 'cluster-task-controller_' + sys.argv[1] + '.zip'))
pathlib.Path('dist/SHA256SUMS').write_text(''.join(f'{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n' for p in files))
PY
