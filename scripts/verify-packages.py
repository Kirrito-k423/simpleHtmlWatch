"""核验发布能力、架构、文档、校验值与敏感文件排除。"""
import hashlib
import json
from pathlib import Path, PurePosixPath
import struct
import sys
import tarfile
import zipfile

version = sys.argv[1]
dist = Path(__file__).resolve().parents[1] / "dist"
capabilities = ("accelerator-utilization-v1", "accelerator-occupancy-v1", "queue-residue-cleanup-v1", "forwarded-resource-identity-v1", "bounded-storage-v1", "task-result-stream-v1", "ssh-terminal-v1", "completed-task-retention-v1", "idle-machine-ranking-v1")
forbidden = {"config.enc", "vault.key", "service.json", "app.lock", "known_hosts", "__pycache__", ".DS_Store", "storage-budget.json", "job.json", "completed.json.gz", "reservations.json", "continuations.json", "result.tar.gz", ".cleanup", "recent.json.gz", "idle-ranking.json"}
checksums = {name: sha for sha, name in (line.split("  ", 1) for line in (dist / "SHA256SUMS").read_text().splitlines())}
report = {"version": version, "capabilities": list(capabilities), "packages": []}
for target in ("windows_amd64", "windows_arm64", "darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"):
    root = f"simpleHtmlWatch_{version}_{target}"
    path = dist / (root + (".zip" if target.startswith("windows") else ".tar.gz"))
    sha = hashlib.sha256(path.read_bytes()).hexdigest()
    assert checksums.get(path.name) == sha, "发布包校验值不匹配"
    if path.suffix == ".zip":
        with zipfile.ZipFile(path) as archive:
            content = {name: archive.read(name) for name in archive.namelist() if not name.endswith("/")}
    else:
        with tarfile.open(path) as archive:
            members = archive.getmembers()
            assert all(member.isfile() or member.isdir() for member in members), "发布包包含链接或特殊文件"
            content = {member.name: archive.extractfile(member).read() for member in members if member.isfile()}
    for name in content:
        parts = PurePosixPath(name).parts
        assert not name.startswith("/") and ".." not in parts and parts[0] == root
        assert not forbidden.intersection(parts) and not name.endswith(".pyc"), "发布包包含敏感文件或缓存"
    required = ["README.md", "THIRD-PARTY-NOTICES.md", "AI-START-HERE.md", "AI-GLOBAL-PROMPT.md", "docs/utilization.md", "docs/idle-ranking.md", "docs/storage.md", "docs/experiment-continuations.md", "docs/atomic-group-reservations.md", "docs/service-lifecycle.md", "scripts/storage_audit.py", "skills/cluster-task-controller/SKILL.md", "skills/cluster-task-controller/scripts/taskctl.py", "skills/simplehtmlwatch-netdata/SKILL.md", "skills/simplehtmlwatch-netdata/scripts/metricsctl.py"]
    exe = "simpleHtmlWatch.exe" if target.startswith("windows") else "simpleHtmlWatch"
    required.append(exe)
    if target.startswith("windows"):
        required += ["start.bat", "upgrade.bat", "WINDOWS-INSTALL.md"]
    assert all(root + "/" + name in content for name in required), "发布包缺少必要文件"
    binary = content[root + "/" + exe]
    assert all(cap.encode() in binary for cap in capabilities), "程序缺少发布要求的能力"
    assert b"SSH terminal" in binary or "SSH 终端".encode() in binary
    assert b"__shwTerminalStyle" in binary and b"watch-style-nonce" in binary
    assert b"archiveMode" in content[root + "/skills/cluster-task-controller/scripts/taskctl.py"]
    assert b"growthLimitBytes" in binary and b"prune-completed" in binary
    assert b"history-btn" not in binary, "removed history recorder still embedded"
    assert b"cleanup-policy" in content[root + "/skills/cluster-task-controller/scripts/taskctl.py"]
    assert b"forwarded-resource-identity-v1" in content[root + "/skills/cluster-task-controller/SKILL.md"]
    assert b"--reservation-grant" in content[root + "/skills/cluster-task-controller/scripts/taskctl.py"]
    assert "物理资源 ID".encode() in binary, "界面缺少物理资源配置入口"
    assert b"accelerator-occupancy-v1" in content[root + "/skills/simplehtmlwatch-netdata/scripts/service_client.py"]
    assert "整机占用时长比例".encode() in binary and "诊断与清理日志".encode() in binary, "界面未集成"
    if target.startswith("windows"):
        offset = struct.unpack_from("<I", binary, 0x3c)[0]
        assert binary[:2] == b"MZ" and binary[offset:offset+4] == b"PE\0\0"
        arch = struct.unpack_from("<H", binary, offset+4)[0]
        assert arch == (0x8664 if target.endswith("amd64") else 0xaa64)
        assert b"-adopt-build" in content[root + "/upgrade.bat"]
    elif target.startswith("linux"):
        assert binary[:4] == b"\x7fELF"
        assert struct.unpack_from("<H", binary, 18)[0] == (62 if target.endswith("amd64") else 183)
    else:
        assert struct.unpack_from("<I", binary)[0] == 0xfeedfacf
        assert struct.unpack_from("<I", binary, 4)[0] == (0x1000007 if target.endswith("amd64") else 0x100000c)
    report["packages"].append({"file": path.name, "bytes": path.stat().st_size, "sha256": sha, "architectureVerified": True, "bothFeaturesEmbedded": True, "credentialAndCacheAudit": True})
output = dist / f"package-verification_{version}.json"
output.write_text(json.dumps(report,ensure_ascii=False,indent=2) + "\n")
print(json.dumps({"verifiedPackages":len(report["packages"]),"report":str(output)},ensure_ascii=False))
