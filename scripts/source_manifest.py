"""Allowlisted source files shared by archive and public-copy checks."""
from pathlib import Path
import os

ROOT = Path(__file__).resolve().parent.parent
ROOT_FILES = {'README.md', 'LICENSE', 'THIRD_PARTY.md', 'CHANGELOG.md',
              'CONTRIBUTING.md', 'SECURITY.md', 'go.mod', 'go.sum', '.gitignore', '.gitattributes'}
GROUPS = {'cmd', 'internal', 'firmware', 'web', 'native', 'integrations', 'scripts', 'packaging', 'docs', '.github'}
SUFFIXES = {'.go','.py','.ps1','.mjs','.json','.ts','.tsx','.css','.html','.h','.cpp','.c',
            '.cmake','.yml','.yaml','.md','.txt','.nsi','.csv','.lock','.svg'}
NAMES = {'CMakeLists.txt','Kconfig.projbuild','sdkconfig.defaults','sdkconfig.usb-validation','.clang-format'}
EXCLUDE_DIRS = {'node_modules','dist','build','build-usb','managed_components','evidence',
                '__pycache__','.pytest_cache','.local','.tools','.git','test-reports'}


def source_files():
    for current, dirs, files in os.walk(ROOT, followlinks=False):
        current = Path(current)
        for name in list(dirs):
            child = current / name
            if child.is_symlink():
                raise ValueError('Directory links are not allowed in source exports')
            if name in EXCLUDE_DIRS or name.startswith('build-') or (current == ROOT and name not in GROUPS):
                dirs.remove(name)
        for name in sorted(files):
            path = current / name
            rel = path.relative_to(ROOT)
            if path.is_symlink():
                raise ValueError('File links are not allowed in source exports')
            if current == ROOT:
                if name in ROOT_FILES: yield path
                continue
            if name.startswith(('.env', 'pairing.', 'cookies.')) or path.suffix in {'.pem','.key'}:
                raise ValueError('Private material must be outside source directories: ' + str(rel))
            if rel.parts[:3] == ('internal','ui','assets'):
                if rel.as_posix() == 'internal/ui/assets/index.html': yield path
                continue
            if rel.as_posix() == 'packaging/sources/esptool-5.4.0.tar.gz' or 'licenses' in rel.parts:
                yield path
            elif path.suffix in SUFFIXES or name in NAMES:
                yield path
