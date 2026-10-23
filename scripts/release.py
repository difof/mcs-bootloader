#!/usr/bin/env python3
"""Build standalone release executables. Never tag, push, or publish."""

import argparse
import hashlib
import html
import json
import os
from pathlib import Path, PurePosixPath
import re
import struct
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parent.parent
VERSION = re.compile(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
ARCHITECTURES = {"amd64": 62, "arm64": 183}
ROOT_FILES = frozenset({
    "README.md", "AGENTS.md", "Taskfile.yml", ".env.example",
    ".gitignore", "go.mod", "go.sum", "LICENSE", "LICENSE.md", "assets.go", "assets_test.go",
})
SOURCE_SUFFIXES = {
    "cmd": {".go"}, "internal": {".go"}, "scripts": {".py"},
    "deploy": {".service", ".conf"}, "docs": {".md"},
    ".github": {".yml", ".yaml"},
}
REQUIRED_FILES = frozenset({
    "README.md", "AGENTS.md", "Taskfile.yml", ".env.example", "go.mod", "assets.go",
    "scripts/install.py", "scripts/manage.py", "scripts/env_config.py", "scripts/service_config.py", "scripts/remove.py",
    "deploy/mcs-bootloader.service", "deploy/mcs-bootloader-backend.service",
    "deploy/journald@mcs-bootloader.conf",
})


def checked_version(value):
    if not isinstance(value, str) or VERSION.fullmatch(value) is None:
        raise ValueError("version must be vMAJOR.MINOR.PATCH without leading zeroes")
    return value


def run(argv, *, root=ROOT, env=None):
    return subprocess.run(argv, cwd=root, check=True, capture_output=True,
                          text=True, env=env).stdout.strip()


def package_files(root, tracked):
    """Use only approved, tracked source files; never follow a local symlink."""
    selected = []
    for name in tracked:
        path = PurePosixPath(name)
        if path.is_absolute() or ".." in path.parts or not path.parts:
            raise ValueError("Git contains an unsafe package path")
        if any(part in {".env", ".local", ".git", ".mcs-bootloader-installation.json", "world", "mods", "__pycache__"}
               for part in path.parts) or path.suffix in {".password", ".log"}:
            raise ValueError("private/server data must not be tracked in the release source")
        if name not in ROOT_FILES and path.suffix not in SOURCE_SUFFIXES.get(path.parts[0], set()):
            continue
        source = root / name
        if source.is_symlink() or not source.is_file():
            raise ValueError("release inputs must be regular files, never symlinks")
        selected.append(name)
    missing = REQUIRED_FILES - set(selected)
    if missing:
        raise ValueError("release is missing required installation files: " + ", ".join(sorted(missing)))
    return sorted(selected)


def git_changelog(version, root=ROOT):
    """Read committed subjects since the highest earlier, reachable stable tag."""
    current = tuple(map(int, VERSION.fullmatch(checked_version(version)).groups()))
    if run(["git", "rev-parse", "--is-shallow-repository"], root=root) != "false":
        raise ValueError("release changelog needs full Git history; fetch all history and tags before packaging")
    tags = run(["git", "tag", "--merged", "HEAD", "--list", "v*"], root=root).splitlines()
    previous = None
    previous_number = None
    for tag in tags:
        match = VERSION.fullmatch(tag)
        if match is None:
            continue
        number = tuple(map(int, match.groups()))
        if number < current and (previous_number is None or number > previous_number):
            previous, previous_number = tag, number
    revision_range = f"{previous}..HEAD" if previous else "HEAD"
    log = run(["git", "log", "--reverse", "--format=%H%x00%s", revision_range], root=root)
    commits = []
    for line in log.split("\n") if log else []:
        commit, separator, subject = line.partition("\0")
        if not separator or re.fullmatch(r"[0-9a-f]{40,64}", commit) is None:
            raise ValueError("could not read release commit history")
        commits.append((commit, subject))
    if not commits:
        raise ValueError("no new commits since the previous stable version tag")
    return previous, commits


def markdown_subject(subject):
    """Keep a commit subject as text, never release-page markup or HTML."""
    subject = " ".join(subject.split())
    subject = html.escape(subject, quote=False)
    return re.sub(r"([\\`*_{}\[\]()#+.!|~])", r"\\\1", subject)


def validate_binary(path, architecture):
    with path.open("rb") as stream:
        header = stream.read(20)
    if len(header) < 20 or header[:6] != b"\x7fELF\x02\x01" or struct.unpack("<H", header[18:20])[0] != ARCHITECTURES[architecture]:
        raise ValueError("build did not produce the expected 64-bit Linux executable")


def release_notes(version, commits, metadata, repository, previous=None):
    base = f"https://github.com/{repository}"
    changelog = "\n".join(f"- {markdown_subject(subject)} ([{commit[:12]}]({base}/commit/{commit}))"
                          for commit, subject in commits)
    comparison = (f"[Full diff]({base}/compare/{previous}...{version})" if previous else
                  f"[Full history]({base}/commits/{version})")
    return f"""## Changelog

{changelog}

{comparison}

## Build details

- Version: `{version}`
- Source: [{metadata['commit'][:12]}]({base}/commit/{metadata['commit']})
- Toolchain: `{metadata['go_toolchain']}`
- Targets: Linux amd64 and arm64, `CGO_ENABLED=0`
- Standalone executables with embedded operator scripts and service templates
- Included metadata: `build-info.json`; SHA-256 hashes cover both binaries and metadata.

"""


def package(version, root=ROOT):
    version = checked_version(version)
    if run(["git", "status", "--porcelain", "--untracked-files=all"], root=root):
        raise ValueError("commit or stash source edits before packaging; private .env and dist/ stay ignored")
    tracked = run(["git", "ls-files", "-z"], root=root).split("\0")
    # Validate the tracked tree before compiling: embedded assets must never
    # accidentally include private configuration or server data.
    package_files(root, [name for name in tracked if name])
    commit = run(["git", "rev-parse", "HEAD"], root=root)
    epoch = int(run(["git", "show", "-s", "--format=%ct", "HEAD"], root=root))
    metadata = {
        "project": "mcs-bootloader", "version": version, "commit": commit,
        "commit_timestamp": epoch, "go_toolchain": run(["go", "version"], root=root),
        "targets": ["linux/amd64", "linux/arm64"], "cgo_enabled": False,
        "distribution": "standalone-binary", "operator_assets": "embedded",
    }
    repository = os.environ.get("GITHUB_REPOSITORY", "difof/mcs-bootloader")
    if re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository) is None:
        raise ValueError("invalid GITHUB_REPOSITORY")
    previous, commits = git_changelog(version, root)
    metadata["changelog_base"] = previous
    encoded_metadata = (json.dumps(metadata, indent=2, sort_keys=True) + "\n").encode()
    destination = root / "dist" / version
    if destination.exists() or destination.is_symlink():
        raise ValueError("this version already has local packages in dist/; inspect or move them before rebuilding")
    destination.parent.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".package-", dir=destination.parent) as temporary:
        staged = Path(temporary)
        output = staged / "output"
        output.mkdir()
        assets = []
        for architecture in ARCHITECTURES:
            binary = output / f"mcs-bootloader_{version}_linux_{architecture}"
            run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags",
                 f"-s -w -X main.version={version}", "-o", str(binary), "./cmd/mcs-bootloader"],
                root=root, env={**os.environ, "CGO_ENABLED": "0", "GOOS": "linux",
                                "GOARCH": architecture, "GOFLAGS": "", "GOTOOLCHAIN": "local"})
            validate_binary(binary, architecture)
            binary.chmod(0o755)
            os.utime(binary, (epoch, epoch))
            assets.append(binary)
        info = output / "build-info.json"
        info.write_bytes(encoded_metadata)
        assets.append(info)
        checksums = "".join(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n"
                            for path in sorted(assets))
        (output / "SHA256SUMS").write_text(checksums)
        (output / "RELEASE_NOTES.md").write_text(release_notes(version, commits, metadata, repository, previous))
        output.rename(destination)
    print(f"Standalone binaries ready in {destination}; no tag, push, or release was created.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("check-version", "package"))
    parser.add_argument("--version", required=True)
    args = parser.parse_args()
    checked_version(args.version)
    if args.command == "package":
        package(args.version)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError) as exc:
        print(f"release: {exc}", file=sys.stderr)
        if isinstance(exc, subprocess.CalledProcessError) and exc.stderr:
            print(exc.stderr.strip(), file=sys.stderr)
        raise SystemExit(1)
