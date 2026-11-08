#!/usr/bin/env bash
set -euo pipefail

# Multi-Platform Build and Cross-Compilation Driver for chromenet Server
# Target architectures supported:
#   - linux/amd64   (x86_64-linux-gnu)
#   - linux/arm64   (aarch64-linux-gnu)
#   - darwin/amd64  (x86_64-apple-darwin)
#   - darwin/arm64  (arm64-apple-darwin)
#   - windows/amd64 (x86_64-w64-mingw32)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHROME_NET_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
SERVER_DIR="${CHROME_NET_DIR}/server"
COMPAT_DARWIN_DIR="${SCRIPT_DIR}/compat/darwin"
HQQ_C_DIR="${CHROME_NET_DIR}/../../../ipc/hqq/HQQ/c"
HQQ_C_DIR="$(cd "${HQQ_C_DIR}" && pwd)"

PLATFORM="linux/amd64"
BUILD_DIR="${SERVER_DIR}/build"
BUILD_TYPE="Release"
RUN_TESTS=false

for arg in "$@"; do
    case "$arg" in
        --platform=*)
            PLATFORM="${arg#*=}"
            ;;
        --build-dir=*)
            BUILD_DIR="${arg#*=}"
            ;;
        --release)
            BUILD_TYPE="Release"
            ;;
        --debug)
            BUILD_TYPE="Debug"
            ;;
        --test)
            RUN_TESTS=true
            ;;
        --help|-h)
            echo "Usage: $0 [options]"
            echo "Options:"
            echo "  --platform=<linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|windows/amd64|all>"
            echo "  --build-dir=<path>    Output build directory (default: server/build)"
            echo "  --release             Build in release mode (default)"
            echo "  --debug               Build in debug mode"
            echo "  --test                Run test suite on host"
            exit 0
            ;;
        *)
            echo "Unknown argument: $arg" >&2
            exit 1
            ;;
    esac
done

echo "=========================================================="
echo " chromenet Native Server Build & Cross-Compilation Driver"
echo " Platform target: ${PLATFORM}"
echo " Build directory: ${BUILD_DIR}"
echo " Build type:      ${BUILD_TYPE}"
echo "=========================================================="

build_host() {
    local target_dir="${BUILD_DIR}/host"
    echo "==> [host: linux/amd64] Building native binaries via CMake/Ninja..."
    mkdir -p "${target_dir}"
    cmake -S "${SERVER_DIR}" -B "${target_dir}" -G Ninja -DCMAKE_BUILD_TYPE="${BUILD_TYPE}"
    cmake --build "${target_dir}" -- -j"$(nproc 2>/dev/null || echo 4)"
    echo "==> [host: linux/amd64] Built successfully:"
    ls -l "${target_dir}/chrome_net_server" "${target_dir}/signatures_test"

    echo "==> [host: linux/amd64] Executing test suite..."
    "${target_dir}/signatures_test"
}

build_cross_target() {
    local plat="$1"
    local triple="$2"
    local plat_flat="${plat//\//_}"
    local out_dir="${BUILD_DIR}/${plat}"
    local cross_server_dir="${CHROME_NET_DIR}/build/cross/${plat_flat}/server"
    echo "==> [cross: ${plat}] Compiling complete engine objects for ${triple}..."
    mkdir -p "${out_dir}" "${cross_server_dir}"

    local c_flags=("-target" "${triple}" "-I${SERVER_DIR}" "-I${HQQ_C_DIR}/include" "-O2")
    local cxx_flags=("-target" "${triple}" "-I${SERVER_DIR}" "-I${HQQ_C_DIR}/include" "-std=c++17" "-O2")

    case "${plat}" in
        "linux/amd64")
            # Uses host toolchain sysroot directly
            ;;
        "linux/arm64")
            c_flags+=("-isystem" "/usr/aarch64-linux-gnu/include")
            cxx_flags+=(
                "-isystem" "/usr/aarch64-linux-gnu/include/c++/12"
                "-isystem" "/usr/aarch64-linux-gnu/include/c++/12/aarch64-linux-gnu"
                "-isystem" "/usr/aarch64-linux-gnu/include"
            )
            ;;
        "darwin/amd64")
            c_flags+=(
                "-mmacosx-version-min=11.0"
                "-isystem" "/usr/include/x86_64-linux-gnu"
                "-U__nonnull" "-D__nonnull(x)=" "-D__THROW=" "-D__attribute_pure__=" "-D__attr_access(x)="
                "-Wno-macro-redefined"
            )
            cxx_flags+=(
                "-mmacosx-version-min=11.0"
                "-stdlib=libc++"
                "-isystem" "/usr/lib/llvm-14/include/c++/v1"
                "-isystem" "${COMPAT_DARWIN_DIR}"
                "-isystem" "/usr/include/x86_64-linux-gnu"
                "-D_DARWIN_C_SOURCE" "-D_GNU_SOURCE"
                "-U__nonnull" "-D__nonnull(x)=" "-D__THROW=" "-D__attribute_pure__=" "-D__attr_access(x)="
                "-Wno-macro-redefined"
            )
            ;;
        "darwin/arm64")
            c_flags+=(
                "-mmacosx-version-min=11.0"
                "-isystem" "/usr/aarch64-linux-gnu/include"
                "-U__nonnull" "-D__nonnull(x)=" "-D__THROW=" "-D__attribute_pure__=" "-D__attr_access(x)="
                "-Wno-macro-redefined"
            )
            cxx_flags+=(
                "-mmacosx-version-min=11.0"
                "-stdlib=libc++"
                "-isystem" "/usr/lib/llvm-14/include/c++/v1"
                "-isystem" "${COMPAT_DARWIN_DIR}"
                "-isystem" "/usr/aarch64-linux-gnu/include"
                "-D_DARWIN_C_SOURCE" "-D_GNU_SOURCE"
                "-U__nonnull" "-D__nonnull(x)=" "-D__THROW=" "-D__attribute_pure__=" "-D__attr_access(x)="
                "-Wno-macro-redefined"
            )
            ;;
        "windows/amd64")
            c_flags+=("-isystem" "/usr/x86_64-w64-mingw32/include")
            cxx_flags+=(
                "-isystem" "/usr/lib/gcc/x86_64-w64-mingw32/12-posix/include/c++"
                "-isystem" "/usr/lib/gcc/x86_64-w64-mingw32/12-posix/include/c++/x86_64-w64-mingw32"
                "-isystem" "/usr/x86_64-w64-mingw32/include"
            )
            ;;
    esac

    # 1. Compile C sources (Dispatcher & Daemon Entrypoint)
    clang "${c_flags[@]}" -c "${SERVER_DIR}/chrome_net_server.c" -o "${out_dir}/chrome_net_server.o"
    clang "${c_flags[@]}" -c "${SERVER_DIR}/main.c" -o "${out_dir}/main.o"

    # 2. Compile C++ sources (Engine Core, Signatures, Adapters)
    clang++ "${cxx_flags[@]}" -c "${SERVER_DIR}/signatures.cc" -o "${out_dir}/signatures.o"
    clang++ "${cxx_flags[@]}" -c "${SERVER_DIR}/engine_core.cc" -o "${out_dir}/engine_core.o"
    clang++ "${cxx_flags[@]}" -c "${SERVER_DIR}/webtransport_adapter.cc" -o "${out_dir}/webtransport_adapter.o"
    clang++ "${cxx_flags[@]}" -c "${SERVER_DIR}/websocket_adapter.cc" -o "${out_dir}/websocket_adapter.o"

    # Copy objects to cross distribution directory
    cp "${out_dir}/"*.o "${cross_server_dir}/"

    # Create target static archive containing all engine core and server symbols
    llvm-ar rcs "${out_dir}/libchrome_net_server.a" \
        "${out_dir}/chrome_net_server.o" \
        "${out_dir}/main.o" \
        "${out_dir}/signatures.o" \
        "${out_dir}/engine_core.o" \
        "${out_dir}/webtransport_adapter.o" \
        "${out_dir}/websocket_adapter.o"
    cp "${out_dir}/libchrome_net_server.a" "${cross_server_dir}/libchrome_net_server.a"

    echo "==> [cross: ${plat}] Successfully built complete target objects and archive:"
    file "${out_dir}/chrome_net_server.o" "${out_dir}/signatures.o" "${out_dir}/libchrome_net_server.a"
}

case "${PLATFORM}" in
    "linux/amd64"|"linux_amd64")
        build_host
        build_cross_target "linux/amd64" "x86_64-linux-gnu"
        ;;
    "linux/arm64"|"linux_arm64")
        build_cross_target "linux/arm64" "aarch64-linux-gnu"
        ;;
    "darwin/amd64"|"darwin_amd64")
        build_cross_target "darwin/amd64" "x86_64-apple-darwin"
        ;;
    "darwin/arm64"|"darwin_arm64")
        build_cross_target "darwin/arm64" "arm64-apple-darwin"
        ;;
    "windows/amd64"|"windows_amd64")
        build_cross_target "windows/amd64" "x86_64-w64-mingw32"
        ;;
    "all")
        echo "Building all 5 target platforms..."
        build_host
        build_cross_target "linux/amd64" "x86_64-linux-gnu"
        build_cross_target "linux/arm64" "aarch64-linux-gnu"
        build_cross_target "darwin/amd64" "x86_64-apple-darwin"
        build_cross_target "darwin/arm64" "arm64-apple-darwin"
        build_cross_target "windows/amd64" "x86_64-w64-mingw32"
        ;;
    *)
        echo "Unsupported platform: ${PLATFORM}" >&2
        exit 1
        ;;
esac

if [ "${RUN_TESTS}" = true ] && [ "${PLATFORM}" != "linux/amd64" ] && [ "${PLATFORM}" != "all" ]; then
    echo "Running host test suite..."
    build_host
fi

echo "=========================================================="
echo " BUILD AND VERIFICATION FINISHED SUCCESSFULLY!"
echo "=========================================================="
