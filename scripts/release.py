#!/usr/bin/env python3
"""Build, inspect and publish complete CC AutoMux binary distributions."""

import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import socket
import struct
import subprocess
import sys
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parent.parent
REPO = "Siriusrry/cc-automux"
TARGETS = [(system, arch) for system in ("darwin", "linux") for arch in ("amd64", "arm64")]
STABLE = re.compile(r"v([1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\Z")


def run(*args, **kwargs):
    return subprocess.check_output(args, **kwargs)


def version():
    return (ROOT / "internal/version/VERSION").read_text().strip()


def check_tag(tag):
    if not STABLE.fullmatch(tag) or tag != version():
        raise ValueError("tag must be a stable vX.Y.Z matching internal/version/VERSION")


def asset_name(tag, system, arch):
    return f"cc-automux_{tag}_{system}_{arch}.tar.gz"


def package_files(system):
    files = ["LICENSE", "README.md", "README.zh-CN.md", "docs/usage.md", "docs/usage.zh-CN.md"]
    files += [str(p.relative_to(ROOT)) for p in sorted((ROOT / "scripts").glob("*.sh"))]
    files += ["scripts/README.md", "scripts/README.zh-CN.md"]
    platform = "macos" if system == "darwin" else "linux"
    files += [str(p.relative_to(ROOT)) for p in sorted((ROOT / "packaging" / platform).glob("*.template"))]
    return sorted(files)


def inspect_binary(data, system, arch):
    if system == "linux":
        if data[:6] != b"\x7fELF\x02\x01" or struct.unpack_from("<H", data, 18)[0] != {"amd64": 62, "arm64": 183}[arch]:
            raise ValueError("ELF platform/architecture mismatch")
    elif data[:4] != b"\xcf\xfa\xed\xfe" or struct.unpack_from("<I", data, 4)[0] != {"amd64": 0x1000007, "arm64": 0x100000c}[arch]:
        raise ValueError("Mach-O platform/architecture mismatch")
    if b".debug_info" in data or b"__debug_info" in data:
        raise ValueError("binary contains debug information")
    with tempfile.NamedTemporaryFile() as binary:
        binary.write(data)
        binary.flush()
        metadata = run("go", "version", "-m", binary.name).decode()
    if "-trimpath=true" not in metadata or "vcs." in metadata:
        raise ValueError("binary lacks trimmed paths or contains VCS metadata")
    if f"GOOS={system}" not in metadata or f"GOARCH={arch}" not in metadata:
        raise ValueError("Go build target mismatch")
    if version().encode() not in data:
        raise ValueError("binary does not contain the expected product version")


def inspect_privacy(data):
    # Check actual build locations as well as recognizable absolute user paths.
    forbidden = {str(ROOT), str(Path.home()), tempfile.gettempdir(), socket.gethostname()}
    forbidden.update(os.environ.get(k, "") for k in ("GITHUB_WORKSPACE", "RUNNER_TEMP", "GOROOT", "GOCACHE"))
    source_repo = os.environ.get("GITHUB_REPOSITORY", "")
    if source_repo != REPO:
        forbidden.add(source_repo)
    origin = subprocess.run(["git", "remote", "get-url", "origin"], cwd=ROOT, capture_output=True, text=True)
    if origin.returncode == 0 and REPO + ".git" not in origin.stdout and not origin.stdout.strip().endswith(REPO):
        forbidden.add(origin.stdout.strip())
    forbidden.update(value for key, value in os.environ.items() if key.endswith(("_TOKEN", "_SECRET", "_KEY")) and len(value) >= 12)
    if any(len(value) > 4 and value.encode() in data for value in forbidden):
        raise ValueError("package contains a build-machine identifier or absolute build path")
    if re.search(rb"/(?:Users|home)/[A-Za-z0-9_.-]+/", data):
        raise ValueError("package contains an absolute user path")


def inspect_package(path, system, arch):
    expected = {"dist/cc-automux", *package_files(system)}
    with tarfile.open(path, "r:gz") as archive:
        members = archive.getmembers()
        if len(members) != len(expected) or {m.name for m in members} != expected:
            raise ValueError(f"unexpected or missing package members: {path.name}")
        for member in members:
            if not member.isfile() or member.uid or member.gid or member.uname or member.gname or member.pax_headers:
                raise ValueError("package contains links, special files or machine metadata")
            data = archive.extractfile(member).read()
            inspect_privacy(data)
            if member.name == "dist/cc-automux":
                if member.mode != 0o755:
                    raise ValueError("binary must be executable")
                inspect_binary(data, system, arch)
            elif data != (ROOT / member.name).read_bytes():
                raise ValueError(f"packaged file differs from source tag: {member.name}")


def build(system, arch, output):
    if (system, arch) not in TARGETS:
        raise ValueError("unsupported build target")
    tag = version()
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-dev)?", tag):
        raise ValueError("invalid product version")
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="cc-automux-build-") as work:
        binary = Path(work) / "cc-automux"
        env = dict(os.environ, GOOS=system, GOARCH=arch, CGO_ENABLED="0")
        subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", str(binary), "./cmd/cc-automux"], cwd=ROOT, env=env, check=True)
        path = output / asset_name(tag, system, arch)
        with path.open("wb") as raw, gzip.GzipFile(fileobj=raw, mode="wb", filename="", mtime=0) as compressed, tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
            for name in ["dist/cc-automux", *package_files(system)]:
                source = binary if name == "dist/cc-automux" else ROOT / name
                if source.is_symlink() or not source.is_file():
                    raise ValueError(f"not a regular package input: {name}")
                data = source.read_bytes()
                member = tarfile.TarInfo(name)
                member.size = len(data)
                member.mode = 0o755 if name == "dist/cc-automux" or name.endswith(".sh") else 0o644
                archive.addfile(member, io.BytesIO(data))
        inspect_package(path, system, arch)
        print(path)


def digest(path):
    with path.open("rb") as file:
        return hashlib.file_digest(file, "sha256").hexdigest()


def checksums(tag, output):
    if tag != version():
        raise ValueError("asset version differs from source version")
    names = {asset_name(tag, system, arch) for system, arch in TARGETS}
    if {p.name for p in output.iterdir()} - {"SHA256SUMS"} != names:
        raise ValueError("release must contain exactly the four supported target packages")
    for system, arch in TARGETS:
        inspect_package(output / asset_name(tag, system, arch), system, arch)
    sums = "".join(f"{digest(output / name)}  {name}\n" for name in sorted(names))
    (output / "SHA256SUMS").write_text(sums)
    return names | {"SHA256SUMS"}


def api(endpoint, method="GET", body=None):
    args = ["gh", "api", f"repos/{REPO}/{endpoint}", "--method", method]
    if body is not None:
        args += ["--input", "-"]
    result = run(*args, input=json.dumps(body).encode() if body is not None else None)
    return json.loads(result) if result else None


def releases():
    found = []
    page = 1
    while True:
        batch = api(f"releases?per_page=100&page={page}")
        found.extend(batch)
        if len(batch) < 100:
            return found
        page += 1


def verify_uploaded(release, output, names):
    if {a["name"] for a in release["assets"]} != names or len(release["assets"]) != len(names):
        raise ValueError("release assets are incomplete or unexpected")
    with tempfile.TemporaryDirectory(prefix="cc-automux-assets-") as work:
        subprocess.run(["gh", "release", "download", release["tag_name"], "--repo", REPO, "--dir", work], check=True)
        for name in names:
            if digest(Path(work) / name) != digest(output / name):
                raise ValueError(f"existing release asset differs; refusing replacement: {name}")


def publish(tag, output):
    if os.environ.get("GITHUB_REPOSITORY") != REPO or os.environ.get("GITHUB_REF") != f"refs/tags/{tag}":
        raise ValueError("publication requires a version tag event in the product repository")
    check_tag(tag)
    names = checksums(tag, output)
    # Require an existing source tag before creating or resuming its Release.
    # An existing Release must never be retargeted.
    api(f"git/ref/tags/{tag}")
    existing = next((r for r in releases() if r["tag_name"] == tag), None)
    if existing is None:
        # The list endpoint may not immediately show a newly created draft.
        # Its creation response is authoritative and already contains its ID.
        existing = api("releases", "POST", {
            "tag_name": tag,
            "draft": True,
            "name": f"CC AutoMux {tag}",
            "body": "macOS and Linux binaries with the embedded web console. Verify downloads with SHA256SUMS.",
        })
    if existing["draft"]:
        # An interrupted upload can be resumed, but never clobber existing bytes.
        uploaded = {a["name"] for a in existing["assets"]}
        if uploaded - names:
            raise ValueError("draft has unexpected assets")
        if uploaded:
            with tempfile.TemporaryDirectory() as work:
                subprocess.run(["gh", "release", "download", tag, "--repo", REPO, "--dir", work], check=True)
                for name in uploaded:
                    if digest(Path(work) / name) != digest(output / name):
                        raise ValueError(f"draft asset differs; refusing replacement: {name}")
        for name in sorted(names - uploaded):
            subprocess.run(["gh", "release", "upload", tag, str(output / name), "--repo", REPO], check=True)
    existing = api(f"releases/{existing['id']}")
    verify_uploaded(existing, output, names)
    if existing["prerelease"]:
        raise ValueError("refusing to convert an existing prerelease")
    stable = [r for r in releases() if not r["draft"] and not r["prerelease"] and STABLE.fullmatch(r["tag_name"])]
    highest = max([tag, *(r["tag_name"] for r in stable)], key=lambda v: tuple(map(int, STABLE.fullmatch(v).groups())))
    api(f"releases/{existing['id']}", "PATCH", {"draft": False, "make_latest": "true" if tag == highest else "false"})
    # Repair latest even if an older release was manually designated latest.
    if highest != tag:
        latest = next(r for r in stable if r["tag_name"] == highest)
        api(f"releases/{latest['id']}", "PATCH", {"make_latest": "true"})
    print(f"Published complete release {tag}; latest stable version is {highest}")


def main():
    args = sys.argv[1:]
    if len(args) == 2 and args[0] == "tag":
        check_tag(args[1])
    elif len(args) == 4 and args[0] == "build":
        build(args[1], args[2], Path(args[3]).resolve())
    elif len(args) == 3 and args[0] in ("verify", "publish"):
        if args[0] == "publish":
            publish(args[1], Path(args[2]).resolve())
        else:
            checksums(args[1], Path(args[2]).resolve())
    else:
        raise ValueError("usage: release.py tag VERSION | build OS ARCH DIR | verify VERSION DIR | publish VERSION DIR")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        sys.exit(f"release: {error}")
