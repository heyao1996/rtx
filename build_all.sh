#!/bin/bash
# rtx 全平台构建 + 打包（combined: Phase A + Phase B + bug fixes）
set -e
cd "$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"   # 脚本自身目录（2026-10-03：原为硬编码本机绝对路径，入库会随文件公开泄漏操作者用户名）
rm -rf dist
mkdir -p dist

LDFLAGS="-s -w"

# os,arch 对
PLATFORMS=(
  "darwin arm64"
  "darwin amd64"
  "linux amd64"
  "linux arm64"
  "windows amd64"
  "windows arm64"
)

for pair in "${PLATFORMS[@]}"; do
  os=$(echo "$pair" | cut -d' ' -f1)
  arch=$(echo "$pair" | cut -d' ' -f2)
  dir="dist/rtx-${os}-${arch}"
  mkdir -p "$dir"
  ext=""
  [ "$os" = "windows" ] && ext=".exe"
  echo "=== build ${os}/${arch} ==="
  for bin in agent server rtx; do
    if CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -ldflags="$LDFLAGS" -trimpath -o "$dir/${bin}${ext}" ./cmd/$bin 2>"$dir/.builderr"; then
      echo "  ok: ${bin}${ext} $(ls -lh "$dir/${bin}${ext}" | awk '{print $5}')"
      rm -f "$dir/.builderr"
    else
      echo "  FAIL: $bin"; cat "$dir/.builderr"; exit 1
    fi
  done
  printf 'platform: %s/%s\nbuild: rtx combined (Phase A async-exec + Phase B bg primitives + bug fixes)\nbinaries: agent (deploy to target) / server (control plane) / rtx (operator CLI)\nagent: -c <server:port> -t <token> -i <agent-id> [-q]\nserver: -l :9000 -ctrl :9001 -t <token>\n' "$os" "$arch" > "$dir/PLATFORM.txt"
done

echo "=== 打包 ==="
cd dist
for d in rtx-*; do
  # 2026-10-03 修：原 `${d%%-*-*}` 对 rtx-windows-amd64 取到的是 `rtx` 而非 `windows`
  #   ⇒ Windows 的 zip 分支从未触发（六平台全走 tar.gz，v1.4 发布包也如此）。
  os=${d#rtx-}; os=${os%%-*}
  if [ "$os" = "windows" ]; then
    ( cd "$d" && zip -q -r "../${d}.zip" . )
    echo "  ${d}.zip $(ls -lh "../${d}.zip" | awk '{print $5}')"
  else
    tar -czf "${d}.tar.gz" "$d"
    echo "  ${d}.tar.gz $(ls -lh "${d}.tar.gz" | awk '{print $5}')"
  fi
done

echo "=== 最终产物 ==="
ls -lh dist/ 2>/dev/null || ls -lh .
