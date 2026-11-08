#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_DIR="${SCRIPT_DIR}/build"

echo "Configuring with CMake..."
mkdir -p "${BUILD_DIR}"
cmake -S "${SCRIPT_DIR}" -B "${BUILD_DIR}" -G Ninja -DCMAKE_BUILD_TYPE=Release

echo "Building targets..."
cmake --build "${BUILD_DIR}" -- -j"$(nproc 2>/dev/null || echo 4)"

echo "Running signatures_test..."
"${BUILD_DIR}/signatures_test"

echo "Build and test completed successfully."
