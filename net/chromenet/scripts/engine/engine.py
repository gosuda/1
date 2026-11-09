#!/usr/bin/env python3
"""Builds chrome_net_server from a pinned official Chromium release.

Every input is pinned in third_party/chromium/: VERSION names the Chromium release, SHA256 holds
the digest of the source tarball and of every toolchain archive the release itself pins (clang,
Rust, GN, ninja, the Windows clang runtime), and WINSDK names the MSVC CRT and Windows SDK that
xwin fetches from Microsoft. A download that does not match its pin stops the build.

  engine.py fetch     --work DIR
      Downloads and verifies the source tarball into DIR/cache and unpacks it to DIR/src.
  engine.py toolchain --work DIR --targets T[,T...]
      Installs the pinned clang, Rust, GN and ninja into DIR/src (and DIR/bin), the Linux
      sysroots and the Windows SDK the targets need.
  engine.py build     --work DIR --target T --hqq HQQ_C_DIR --out FILE
      Copies engine/ and the HQQ C runtime into //third_party/chromenet and builds the target.
  engine.py pin       --version V
      Rewrites VERSION and SHA256 for Chromium release V (used by the upstream tracker).

Targets: linux-amd64, linux-arm64, windows-amd64, windows-arm64 (Linux x86-64 host) and
darwin-arm64, darwin-amd64 (macOS arm64 host).
"""

import argparse
import base64
import hashlib
import json
import os
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import zipfile

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), '..', '..'))
PIN_DIR = os.path.join(ROOT, 'third_party', 'chromium')
ENGINE_DIR = os.path.join(ROOT, 'engine')

CHROMIUM_URL = 'https://commondatastorage.googleapis.com/chromium-browser-official'
CDS_URL = 'https://commondatastorage.googleapis.com/chromium-browser-clang'
CIPD_URL = 'https://chrome-infra-packages.appspot.com/dl'
XWIN_URL = 'https://github.com/Jake-Shadle/xwin/releases/download'
DASH_URL = 'https://chromiumdash.appspot.com/fetch_releases'

LINUX_HOST = 'Linux_x64'
MAC_HOST = 'Mac_arm64'
CIPD_PLATFORM = {LINUX_HOST: 'linux-amd64', MAC_HOST: 'mac-arm64'}
GN_DIR = {LINUX_HOST: 'linux64', MAC_HOST: 'mac'}

TARGETS = {
    # target: (host, target_os, target_cpu, binary suffix)
    'linux-amd64': (LINUX_HOST, 'linux', 'x64', ''),
    'linux-arm64': (LINUX_HOST, 'linux', 'arm64', ''),
    'windows-amd64': (LINUX_HOST, 'win', 'x64', '.exe'),
    'windows-arm64': (LINUX_HOST, 'win', 'arm64', '.exe'),
    'darwin-arm64': (MAC_HOST, 'mac', 'arm64', ''),
    'darwin-amd64': (MAC_HOST, 'mac', 'x64', ''),
}

# GN arguments shared by every target. Release, no symbols, no PGO profile, the ICU data linked
# into the binary, and no file:// support: the engine only speaks HTTP(S).
COMMON_ARGS = {
    'is_debug': False,
    'is_component_build': False,
    'is_official_build': False,
    'dcheck_always_on': False,
    'symbol_level': 0,
    'treat_warnings_as_errors': False,
    'use_remoteexec': False,
    'use_siso': False,
    'chrome_pgo_phase': 0,
    'icu_use_data_file': False,
    'disable_file_support': True,
    'use_kerberos': False,
}

# Linux: no GLib and no NSS, so the binary needs only libc, libm, libdl, libpthread and
# libgcc_s. Server certificates are verified against the Chrome Root Store built into //net.
# Remoting and GTK are off only because their build files assert use_glib when gn loads them.
LINUX_ARGS = {
    'use_glib': False,
    'use_nss_client_certs': False,
    'use_nss_server_certs': False,
    'enable_remoting': False,
    'use_gtk': False,
}


def die(msg):
    sys.exit('engine.py: ' + msg)


def note(msg):
    print('engine.py: ' + msg, file=sys.stderr, flush=True)


# Pins ------------------------------------------------------------------------------------------


def read_keyvals(path):
    out = {}
    with open(path, encoding='utf-8') as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith('#'):
                continue
            key, sep, value = line.partition('=')
            if not sep:
                die('malformed line in %s: %s' % (path, line))
            out[key.strip()] = value.strip()
    return out


def read_pins():
    pins = {}
    with open(os.path.join(PIN_DIR, 'SHA256'), encoding='utf-8') as f:
        for line in f:
            if not line.strip() or line.startswith('#'):
                continue
            digest, name = line.split(None, 1)
            if not re.fullmatch(r'[0-9a-f]{64}', digest):
                die('bad digest for %s in SHA256' % name.strip())
            pins[name.strip()] = digest
    return pins


def version_pins():
    v = read_keyvals(os.path.join(PIN_DIR, 'VERSION'))
    for key in ('version', 'archive', 'url'):
        if not v.get(key):
            die('third_party/chromium/VERSION has no %s' % key)
    if not v['url'].startswith('https://'):
        die('url must be https: ' + v['url'])
    return v


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for block in iter(lambda: f.read(1 << 20), b''):
            h.update(block)
    return h.hexdigest()


def download(url, dest, attempts=10):
    """Downloads url to dest. Server errors and dropped connections are retried with backoff;
    the caller verifies the content against its pin."""
    tmp = dest + '.part'
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    req = urllib.request.Request(url, headers={'User-Agent': 'chromenet-engine-build'})
    for attempt in range(1, attempts + 1):
        note('downloading ' + url)
        try:
            with urllib.request.urlopen(req, timeout=300) as resp, open(tmp, 'wb') as out:
                shutil.copyfileobj(resp, out, 1 << 20)
            os.replace(tmp, dest)
            return
        except urllib.error.HTTPError as e:
            if e.code < 500 or attempt == attempts:
                raise
            err = e
        except (urllib.error.URLError, OSError) as e:
            if attempt == attempts:
                raise
            err = e
        delay = 15 * attempt
        note('download failed (%s); retrying in %d s' % (err, delay))
        time.sleep(delay)


def fetch_pinned(name, url, dest, pins):
    """Downloads url to dest unless dest already matches the pin for name."""
    want = pins.get(name)
    if not want:
        die('no pin for %s in third_party/chromium/SHA256 (run engine.py pin)' % name)
    if not (os.path.exists(dest) and sha256_file(dest) == want):
        download(url, dest)
    got = sha256_file(dest)
    if got != want:
        os.remove(dest)
        die('%s: sha256 %s does not match pin %s' % (name, got, want))
    return dest


def run(cmd, **kw):
    note('$ ' + ' '.join(cmd))
    subprocess.run(cmd, check=True, **kw)


# Upstream pins read from the unpacked source -----------------------------------------------------


def source_revisions(src):
    def grab(path, name):
        text = open(os.path.join(src, path), encoding='utf-8').read()
        m = re.search(r"^%s = '?([^'\n]+)'?$" % name, text, re.M)
        if not m:
            die('%s not found in %s' % (name, path))
        return m.group(1).strip()

    clang = '%s-%s' % (
        grab('tools/clang/scripts/update.py', 'CLANG_REVISION'),
        grab('tools/clang/scripts/update.py', 'CLANG_SUB_REVISION'),
    )
    rust = '%s-%s-%s' % (
        grab('tools/rust/update_rust.py', 'RUST_REVISION'),
        grab('tools/rust/update_rust.py', 'RUST_SUB_REVISION'),
        grab('tools/clang/scripts/update.py', 'CLANG_REVISION'),
    )
    deps = open(os.path.join(src, 'DEPS'), encoding='utf-8').read()
    gn = re.search(r"'gn_version': '([^']+)'", deps).group(1)
    ninja = re.search(r"'ninja_version': '([^']+)'", deps).group(1)
    return {'clang': clang, 'rust': rust, 'gn': gn, 'ninja': ninja}


def toolchain_files(revs, hosts, with_win):
    """Returns (pin name, url) for every toolchain archive the hosts need."""
    files = []
    for host in hosts:
        files.append(('cds/%s/clang-%s.tar.xz' % (host, revs['clang']),
                      '%s/%s/clang-%s.tar.xz' % (CDS_URL, host, revs['clang'])))
        files.append(('cds/%s/rust-toolchain-%s.tar.xz' % (host, revs['rust']),
                      '%s/%s/rust-toolchain-%s.tar.xz' % (CDS_URL, host, revs['rust'])))
        plat = CIPD_PLATFORM[host]
        files.append(('cipd/gn/gn/%s/%s.zip' % (plat, revs['gn']),
                      '%s/gn/gn/%s/+/%s' % (CIPD_URL, plat, revs['gn'])))
        files.append(('cipd/infra/3pp/tools/ninja/%s/%s.zip' % (plat, revs['ninja']),
                      '%s/infra/3pp/tools/ninja/%s/+/%s' % (CIPD_URL, plat, revs['ninja'])))
    if with_win:
        files.append(('cds/Win/clang-win-runtime-library-%s.tar.xz' % revs['clang'],
                      '%s/Win/clang-win-runtime-library-%s.tar.xz' % (CDS_URL, revs['clang'])))
    return files


def xwin_file():
    w = read_keyvals(os.path.join(PIN_DIR, 'WINSDK'))
    name = 'xwin-%s-x86_64-unknown-linux-musl.tar.gz' % w['xwin']
    return w, 'xwin/' + name, '%s/%s/%s' % (XWIN_URL, w['xwin'], name)


def host_of():
    system, machine = platform.system(), platform.machine().lower()
    if system == 'Linux' and machine in ('x86_64', 'amd64'):
        return LINUX_HOST
    if system == 'Darwin' and machine == 'arm64':
        return MAC_HOST
    die('unsupported build host %s/%s' % (system, machine))


# fetch -----------------------------------------------------------------------------------------


def patch_files():
    d = os.path.join(PIN_DIR, 'patches')
    return sorted(os.path.join(d, p) for p in os.listdir(d) if p.endswith('.patch')) \
        if os.path.isdir(d) else []


def cmd_fetch(args):
    v = version_pins()
    pins = read_pins()
    work = os.path.abspath(args.work)
    tarball = fetch_pinned(v['archive'], v['url'], os.path.join(work, 'cache', v['archive']), pins)
    src = os.path.join(work, 'src')
    stamp = os.path.join(src, '.chromenet-source')
    # The stamp covers the archive and the patches applied to it.
    want = '\n'.join([pins[v['archive']]] + [sha256_file(p) for p in patch_files()])
    if os.path.exists(stamp) and open(stamp).read().strip() == want:
        note('source already unpacked: ' + src)
        print(src)
        return
    # src/out may be a mount point (build.sh puts it on a tmpfs): empty src around it.
    os.makedirs(src, exist_ok=True)
    for entry in os.listdir(src):
        path = os.path.join(src, entry)
        if os.path.ismount(path):
            continue
        if os.path.isdir(path) and not os.path.islink(path):
            shutil.rmtree(path)
        else:
            os.remove(path)
    staging = os.path.join(work, 'unpack')
    if os.path.exists(staging):
        shutil.rmtree(staging)
    os.makedirs(staging)
    note('unpacking %s' % v['archive'])
    if shutil.which('tar') and shutil.which('xz'):
        run(['tar', '-xJf', tarball, '-C', staging])
    else:
        with tarfile.open(tarball, 'r:xz') as tf:
            tf.extractall(staging, filter='tar')
    top = os.listdir(staging)
    if len(top) != 1:
        die('unexpected tarball layout: %s' % top)
    tree = os.path.join(staging, top[0])
    for entry in os.listdir(tree):
        dest = os.path.join(src, entry)
        if os.path.ismount(dest):
            die('the source archive has %s, a mount point here' % entry)
        os.replace(os.path.join(tree, entry), dest)
    shutil.rmtree(staging)
    for p in patch_files():
        run(['patch', '-p1', '--forward', '-i', p], cwd=src)
    with open(stamp, 'w') as f:
        f.write(want + '\n')
    print(src)


# toolchain -------------------------------------------------------------------------------------


def unzip(path, dest):
    with zipfile.ZipFile(path) as z:
        for info in z.infolist():
            out = z.extract(info, dest)
            mode = (info.external_attr >> 16) & 0o777
            if mode:
                os.chmod(out, mode)


def cmd_toolchain(args):
    work = os.path.abspath(args.work)
    src = os.path.join(work, 'src')
    targets = parse_targets(args.targets)
    host = host_of()
    for t in targets:
        if TARGETS[t][0] != host:
            die('%s builds on a %s host, not on %s' % (t, TARGETS[t][0], host))
    with_win = any(TARGETS[t][1] == 'win' for t in targets)
    pins = read_pins()
    revs = source_revisions(src)

    # clang and Rust: verified archives in a local mirror of the clang bucket, installed by the
    # release's own update scripts so their stamp files match what GN expects.
    mirror = os.path.join(work, 'cache', 'cds-mirror')
    for name, url in toolchain_files(revs, [host], with_win):
        dest = os.path.join(work, 'cache', name)
        fetch_pinned(name, url, dest, pins)
        if name.startswith('cds/'):
            link = os.path.join(mirror, name[len('cds/'):])
            os.makedirs(os.path.dirname(link), exist_ok=True)
            shutil.copyfile(dest, link)
    with open(os.path.join(work, '.gclient'), 'w') as f:
        f.write('target_os = %r\n' % (['win'] if with_win else []))
    env = dict(os.environ, CDS_CLANG_BUCKET_OVERRIDE='file://' + mirror)
    run([sys.executable, os.path.join(src, 'tools/clang/scripts/update.py')], env=env)
    run([sys.executable, os.path.join(src, 'tools/rust/update_rust.py')], env=env)

    plat = CIPD_PLATFORM[host]
    gn_dir = os.path.join(src, 'buildtools', GN_DIR[host])
    os.makedirs(gn_dir, exist_ok=True)
    unzip(os.path.join(work, 'cache', 'cipd/gn/gn/%s/%s.zip' % (plat, revs['gn'])), gn_dir)
    bin_dir = os.path.join(work, 'bin')
    os.makedirs(bin_dir, exist_ok=True)
    unzip(os.path.join(work, 'cache',
                       'cipd/infra/3pp/tools/ninja/%s/%s.zip' % (plat, revs['ninja'])), bin_dir)

    if host == LINUX_HOST:
        # The host toolchain (build tools such as protoc) always links against the amd64 sysroot.
        arches = {'amd64'} | {'arm64' for t in targets if TARGETS[t][1:3] == ('linux', 'arm64')}
        for arch in sorted(arches):
            run([sys.executable, os.path.join(src, 'build/linux/sysroot_scripts/install-sysroot.py'),
                 '--arch=' + arch])
    if with_win:
        install_winsdk(work, src, pins, targets)


def install_winsdk(work, src, pins, targets):
    w, name, url = xwin_file()
    archive = fetch_pinned(name, url, os.path.join(work, 'cache', name), pins)
    xdir = os.path.join(work, 'xwin')
    if os.path.exists(xdir):
        shutil.rmtree(xdir)
    os.makedirs(xdir)
    with tarfile.open(archive, 'r:gz') as tf:
        tf.extractall(xdir, filter='tar')
    xwin = os.path.join(xdir, name[len('xwin/'):-len('.tar.gz')], 'xwin')
    root = os.path.join(work, 'winsdk')
    if os.path.exists(root):
        shutil.rmtree(root)
    arches = sorted({{'x64': 'x86_64', 'arm64': 'aarch64'}[TARGETS[t][2]]
                     for t in targets if TARGETS[t][1] == 'win'})
    run([xwin, '--accept-license', '--cache-dir', os.path.join(work, 'cache', 'xwin-cache'),
         '--manifest-version', w['manifest'], '--sdk-version', w['sdk'],
         '--crt-version', w['crt'], '--arch', ','.join(arches), '--include-atl',
         '--http-retry', '8',
         'splat', '--use-winsysroot-style', '--preserve-ms-arch-notation', '--output', root])
    write_win_toolchain(src, root)


def write_win_toolchain(src, root):
    """Describes the xwin tree the way build/vs_toolchain.py reads a packaged toolchain."""
    msvc = os.path.join(root, 'VC', 'Tools', 'MSVC')
    crt = sorted(os.listdir(msvc))[-1]
    kits = os.path.join(root, 'Windows Kits', '10')
    sdk = sorted(os.listdir(os.path.join(kits, 'Include')))[-1]
    sdk_version = re.search(r"^SDK_VERSION = '([^']+)'",
                            open(os.path.join(src, 'build/vs_toolchain.py')).read(), re.M).group(1)
    # xwin names the SDK directories without the trailing ".0" that an installed SDK (and
    # build/vs_toolchain.py) uses.
    if sdk + '.0' == sdk_version:
        for sub in ('Include', 'Lib'):
            os.symlink(sdk, os.path.join(kits, sub, sdk_version))
        sdk = sdk_version
    if sdk != sdk_version:
        die('xwin produced Windows SDK %s, Chromium expects %s (update WINSDK)' % (sdk, sdk_version))
    vs_year = re.search(r"\('(\d{4})', '[\d.]+'\),\s+# The VS version in our packaged toolchain",
                        open(os.path.join(src, 'build/vs_toolchain.py')).read()).group(1)
    # setup_toolchain.py looks up cl.exe on PATH to locate the VC bin directory; clang-cl and
    # lld-link do the work, so an empty marker file is enough.
    for cpu in ('x64', 'arm64'):
        bin_dir = os.path.join(msvc, crt, 'bin', 'Hostx64', cpu)
        os.makedirs(bin_dir, exist_ok=True)
        open(os.path.join(bin_dir, 'cl.exe'), 'a').close()
    sdk_bin = os.path.join(kits, 'bin')
    os.makedirs(sdk_bin, exist_ok=True)
    for cpu in ('x64', 'arm64', 'x86'):
        env = {
            'VSINSTALLDIR': [['.\\']],
            'VCINSTALLDIR': [['VC\\']],
            'INCLUDE': [
                ['VC', 'Tools', 'MSVC', crt, 'include'],
                ['VC', 'Tools', 'MSVC', crt, 'atlmfc', 'include'],
                ['Windows Kits', '10', 'Include', sdk, 'ucrt'],
                ['Windows Kits', '10', 'Include', sdk, 'um'],
                ['Windows Kits', '10', 'Include', sdk, 'shared'],
                ['Windows Kits', '10', 'Include', sdk, 'winrt'],
                ['Windows Kits', '10', 'Include', sdk, 'cppwinrt'],
            ],
            'LIB': [
                ['VC', 'Tools', 'MSVC', crt, 'lib', cpu],
                ['VC', 'Tools', 'MSVC', crt, 'atlmfc', 'lib', cpu],
                ['Windows Kits', '10', 'Lib', sdk, 'ucrt', cpu],
                ['Windows Kits', '10', 'Lib', sdk, 'um', cpu],
            ],
            'PATH': [['VC', 'Tools', 'MSVC', crt, 'bin', 'Hostx64', cpu if cpu != 'x86' else 'x64']],
        }
        # setup_toolchain.py runs for x86, x64 and arm64 whatever the target, and requires every
        # INCLUDE and LIB entry to exist. Headers xwin did not produce are left out; library
        # directories of architectures xwin did not fetch are created empty (nothing links
        # against them).
        env['INCLUDE'] = [p for p in env['INCLUDE'] if os.path.isdir(os.path.join(root, *p))]
        for p in env['LIB']:
            os.makedirs(os.path.join(root, *p), exist_ok=True)
        with open(os.path.join(sdk_bin, 'SetEnv.%s.json' % cpu), 'w') as f:
            json.dump({'env': env}, f, indent=1)
    data = {
        'path': root,
        'version': vs_year,
        'win_sdk': kits,
        'wdk': os.path.join(kits, 'wdk'),
        # chrome_net_server links the CRT statically (/MT); with no runtime directories
        # vs_toolchain.py copy_dlls has no MSVC redistributable DLLs to copy.
        'runtime_dirs': [],
    }
    with open(os.path.join(src, 'build', 'win_toolchain.json'), 'w') as f:
        json.dump(data, f, indent=1)
    add_case_aliases(src, [
        os.path.join(msvc, crt, 'include'),
        os.path.join(msvc, crt, 'atlmfc', 'include'),
    ] + [os.path.join(kits, 'Include', sdk, d) for d in ('ucrt', 'um', 'shared', 'winrt', 'cppwinrt')])


INCLUDE_RE = re.compile(rb'^\s*#\s*include\s*[<"]([^>"]+)[>"]', re.M)
SOURCE_SUFFIXES = ('.h', '.hh', '.hpp', '.inc', '.c', '.cc', '.cpp', '.cxx')
# Directories of the source tree that the engine's dependency graph compiles from.
ALIAS_SCAN_DIRS = ('base', 'build', 'buildtools', 'components', 'crypto', 'net', 'testing',
                   'third_party', 'url')


def add_case_aliases(src, include_dirs):
    """The Windows SDK lives on a case-sensitive file system here, but Chromium includes SDK
    headers with whatever case Windows accepts (<Winsock2.h> for WinSock2.h). xwin adds aliases for
    the spellings the SDK itself uses; this adds a symlink for every other spelling the engine's
    sources use."""
    index = {}
    for root in include_dirs:
        if not os.path.isdir(root):
            continue
        for dirpath, _, files in os.walk(root):
            for name in files:
                rel = os.path.relpath(os.path.join(dirpath, name), root)
                index.setdefault(rel.lower(), (root, rel))
    spellings = set()
    for top in ALIAS_SCAN_DIRS:
        for dirpath, dirs, files in os.walk(os.path.join(src, top)):
            dirs[:] = [d for d in dirs if d not in ('.git', 'test', 'tests', 'testdata', 'out')]
            for name in files:
                if not name.endswith(SOURCE_SUFFIXES):
                    continue
                try:
                    with open(os.path.join(dirpath, name), 'rb') as f:
                        text = f.read()
                except OSError:
                    continue
                for m in INCLUDE_RE.finditer(text):
                    spellings.add(m.group(1).decode('utf-8', 'replace'))
    added = 0
    for spelling in spellings:
        hit = index.get(spelling.replace('\\', '/').lower())
        if not hit:
            continue
        root, rel = hit
        alias = os.path.join(root, spelling.replace('\\', '/'))
        if os.path.lexists(alias):
            continue
        os.makedirs(os.path.dirname(alias), exist_ok=True)
        os.symlink(os.path.relpath(os.path.join(root, rel), os.path.dirname(alias)), alias)
        added += 1
    note('added %d case aliases for Windows SDK headers' % added)


# build -----------------------------------------------------------------------------------------


def gn_value(v):
    if isinstance(v, bool):
        return 'true' if v else 'false'
    if isinstance(v, int):
        return str(v)
    return '"%s"' % v


def cmd_build(args):
    work = os.path.abspath(args.work)
    src = os.path.join(work, 'src')
    target = parse_targets(args.target)[0]
    host, target_os, cpu, suffix = TARGETS[target]
    if host != host_of():
        die('%s builds on a %s host' % (target, host))

    overlay = os.path.join(src, 'third_party', 'chromenet')
    if os.path.exists(overlay):
        shutil.rmtree(overlay)
    shutil.copytree(ENGINE_DIR, overlay)
    hqq = os.path.abspath(args.hqq)
    for sub in ('include', 'src'):
        shutil.copytree(os.path.join(hqq, sub), os.path.join(overlay, 'hqq', sub))
    # licenses.py credits a GN target's directory once: the engine's notice carries both the
    # chromenet and the HQQ runtime license.
    with open(os.path.join(overlay, 'LICENSE'), 'w') as f:
        f.write(open(os.path.join(ROOT, 'LICENSE')).read().rstrip() + '\n\n'
                'The HQQ runtime (https://github.com/gosuda/HQQ, hqq/):\n\n'
                + open(os.path.join(hqq, 'LICENSE')).read())

    out = os.path.join(src, 'out', target)
    os.makedirs(out, exist_ok=True)
    gn_args = dict(COMMON_ARGS, target_os=target_os, target_cpu=cpu)
    if target_os == 'linux':
        gn_args.update(LINUX_ARGS)
    if target_os == 'win':
        # The system module map that Chromium generates from the Windows SDK does not build
        # against xwin's tree (the SAL macros go missing inside the module); plain headers do.
        gn_args['use_clang_modules'] = False
        # xwin ships the static CRT without its PDBs, so lld-link warns LNK4099 for every libcmt
        # object; Chromium links with /WX by default.
        gn_args['fatal_linker_warnings'] = False
    with open(os.path.join(out, 'args.gn'), 'w') as f:
        for k in sorted(gn_args):
            f.write('%s = %s\n' % (k, gn_value(gn_args[k])))
    gn = os.path.join(src, 'buildtools', GN_DIR[host], 'gn')
    env = dict(os.environ)
    env['PATH'] = os.path.join(work, 'bin') + os.pathsep + env['PATH']
    if target_os == 'win':
        env['DEPOT_TOOLS_WIN_TOOLCHAIN'] = '1'
    # Only the engine and its dependencies are defined: faster, and unrelated targets (UI, tests)
    # cannot trip gn's checks.
    run([gn, 'gen', out, '--root-target=//third_party/chromenet:all',
         '--root-pattern=//third_party/chromenet:*',
         '--script-executable=' + sys.executable], cwd=src, env=env)
    jobs = args.jobs or os.cpu_count() or 4
    run([os.path.join(work, 'bin', 'ninja'), '-C', out, '-j', str(jobs),
         'third_party/chromenet:chrome_net_server'], cwd=src, env=env)

    built = os.path.join(out, 'chrome_net_server' + suffix)
    dest = os.path.abspath(args.out)
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    shutil.copyfile(built, dest)
    os.chmod(dest, 0o755)
    llvm = os.path.join(src, 'third_party', 'llvm-build', 'Release+Asserts', 'bin')
    if target_os == 'linux':
        run([os.path.join(llvm, 'llvm-strip'), '--strip-all', dest])
    elif target_os == 'mac':
        run(['strip', '-x', dest])
        run(['codesign', '--force', '--sign', '-', dest])
    write_notice(work, src, out, target_os, gn, env, args.notice)
    note('built %s (%d bytes, sha256 %s)' % (dest, os.path.getsize(dest), sha256_file(dest)))


def write_notice(work, src, out, target_os, gn, env, dest):
    """Writes the license texts of everything linked into chrome_net_server, as Chromium's own
    tools/licenses/licenses.py finds them from the GN dependency graph."""
    # licenses.py runs `gn desc` on its own; this shim adds the root target and pattern the
    # engine build was generated with.
    shim = os.path.join(work, 'gn-for-licenses.py')
    with open(shim, 'w') as f:
        f.write('import os, sys\n'
                'gn = %r\n'
                'os.execv(gn, [gn] + sys.argv[1:] + ["--root-target=//third_party/chromenet:all",\n'
                '    "--root-pattern=//third_party/chromenet:*", "--script-executable=%s"])\n'
                % (gn, sys.executable))
    env = dict(env, LICENSES_GN_PATH=shim)
    dest = os.path.abspath(dest)
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    run([sys.executable, os.path.join(src, 'tools/licenses/licenses.py'), 'license_file',
         '--format', 'txt', '--gn-out-dir', out, '--gn-target',
         '//third_party/chromenet:chrome_net_server', '--target-os', target_os, dest],
        cwd=src, env=env)


# pin -------------------------------------------------------------------------------------------


def cmd_pin(args):
    version = args.version
    if version == 'latest-stable':
        with urllib.request.urlopen(DASH_URL + '?channel=Stable&platform=Linux&num=1') as r:
            version = json.load(r)[0]['version']
    if not re.fullmatch(r'\d+\.\d+\.\d+\.\d+', version):
        die('bad version ' + version)
    archive = 'chromium-%s-lite.tar.xz' % version
    url = '%s/%s' % (CHROMIUM_URL, archive)
    with urllib.request.urlopen(url + '.hashes') as r:
        hashes = r.read().decode()
    m = re.search(r'^sha256\s+([0-9a-f]{64})\s+%s$' % re.escape(archive), hashes, re.M)
    if not m:
        die('no sha256 for %s in %s.hashes' % (archive, url))
    pins = {archive: m.group(1)}

    with tempfile.TemporaryDirectory() as tmp:
        # The toolchain revisions live in three small files of the release.
        src = os.path.join(tmp, 'src')
        for path in ('tools/clang/scripts/update.py', 'tools/rust/update_rust.py', 'DEPS'):
            dest = os.path.join(src, path)
            os.makedirs(os.path.dirname(dest), exist_ok=True)
            gitiles = 'https://chromium.googlesource.com/chromium/src/+/refs/tags/%s/%s?format=TEXT' % (
                version, path)
            with urllib.request.urlopen(gitiles) as r:
                open(dest, 'wb').write(base64.b64decode(r.read()))
        revs = source_revisions(src)
        files = toolchain_files(revs, [LINUX_HOST, MAC_HOST], with_win=True)
        _, xname, xurl = xwin_file()
        files.append((xname, xurl))
        for name, file_url in files:
            dest = os.path.join(tmp, 'dl')
            download(file_url, dest)
            pins[name] = sha256_file(dest)
            os.remove(dest)

    with open(os.path.join(PIN_DIR, 'VERSION'), 'w') as f:
        f.write(
            '# Chromium release whose //net stack chrome_net_server is built from.\n'
            '# Read by scripts/engine/engine.py; rewritten by `engine.py pin`.\n'
            '# Official source archive, verified against SHA256 in this directory.\n'
            'version=%s\narchive=%s\nurl=%s\n' % (version, archive, url))
    with open(os.path.join(PIN_DIR, 'SHA256'), 'w') as f:
        f.write('# SHA-256 of every download of the engine build. Rewritten by `engine.py pin`.\n')
        for name in sorted(pins):
            f.write('%s  %s\n' % (pins[name], name))
    print(version)


def parse_targets(spec):
    targets = [t for t in spec.split(',') if t]
    for t in targets:
        if t not in TARGETS:
            die('unknown target %s (known: %s)' % (t, ', '.join(TARGETS)))
    if not targets:
        die('no target')
    return targets


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    sub = p.add_subparsers(dest='cmd', required=True)
    f = sub.add_parser('fetch')
    f.add_argument('--work', required=True)
    t = sub.add_parser('toolchain')
    t.add_argument('--work', required=True)
    t.add_argument('--targets', required=True)
    b = sub.add_parser('build')
    b.add_argument('--work', required=True)
    b.add_argument('--target', required=True)
    b.add_argument('--hqq', required=True, help='c/ directory of the pinned gosuda.org/hqq module')
    b.add_argument('--out', required=True)
    b.add_argument('--notice', required=True, help='where to write the license texts')
    b.add_argument('--jobs', type=int, default=0)
    n = sub.add_parser('pin')
    n.add_argument('--version', required=True, help='a Chromium version or latest-stable')
    args = p.parse_args()
    {'fetch': cmd_fetch, 'toolchain': cmd_toolchain, 'build': cmd_build, 'pin': cmd_pin}[args.cmd](args)


if __name__ == '__main__':
    main()
