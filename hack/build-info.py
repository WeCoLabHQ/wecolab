#!/usr/bin/env python3
"""Generate shared compile-time product identity from a complete Git checkout."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys


def build_info(root):
    version = (root / 'VERSION').read_text().strip()
    if not re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', version):
        raise ValueError('VERSION must contain a numeric major.minor.patch product version')
    env = os.environ.copy()
    # Explicit --root wins over inherited Git routing; status must not write the index
    # when run by a sandboxed SwiftPM build plugin.
    for key in ('GIT_DIR', 'GIT_WORK_TREE', 'GIT_INDEX_FILE', 'GIT_COMMON_DIR'):
        env.pop(key, None)
    env['GIT_OPTIONAL_LOCKS'] = '0'

    def git(*args):
        return subprocess.check_output(
            ['git', '-C', str(root), *args], text=True,
            stderr=subprocess.PIPE, env=env).strip()

    if git('rev-parse', '--is-shallow-repository') == 'true':
        raise ValueError('shallow Git history cannot identify the build; fetch full history first')
    revision = git('rev-parse', '--verify', 'HEAD')
    number = int(git('rev-list', '--count', 'HEAD'))
    dirty = bool(git('status', '--porcelain', '--untracked-files=normal'))
    return {'productVersion': version, 'buildNumber': number,
            'sourceRevision': revision, 'dirty': dirty}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path(__file__).resolve().parent.parent)
    parser.add_argument('--format', choices=('json', 'go-ldflags', 'swift'), default='json')
    parser.add_argument('--go-package', default='wecolab.io/wecolab/internal/buildinfo')
    args = parser.parse_args()
    try:
        info = build_info(args.root.resolve())
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        detail = error.stderr.strip() if isinstance(error, subprocess.CalledProcessError) else str(error)
        parser.exit(1, f'build-info: {detail}\n')
    if args.format == 'json':
        print(json.dumps(info, separators=(',', ':')))
    elif args.format == 'go-ldflags':
        if not re.fullmatch(r'[A-Za-z0-9_./-]+', args.go_package):
            parser.error('--go-package must be a Go import path')
        print(' '.join(f'-X {args.go_package}.{key}={str(value).lower() if isinstance(value, bool) else value}'
                       for key, value in info.items()))
    else:
        print('public enum BuildInfo {')
        for key, value in info.items():
            swift_type = 'Bool' if isinstance(value, bool) else 'Int' if isinstance(value, int) else 'String'
            print(f'    public static let {key}: {swift_type} = {json.dumps(value)}')
        print('}')


if __name__ == '__main__':
    main()
