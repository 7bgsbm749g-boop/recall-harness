#!/usr/bin/env python3
"""Install a verified Recall build and its optional per-user memory worker."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys


def run(command):
    subprocess.run(command, check=True)


def ps_quote(value):
    return "'" + str(value).replace("'", "''") + "'"


def stop_windows_task(name):
    task = ps_quote(name)
    return (
        f"if(Get-ScheduledTask -TaskName {task} -ErrorAction SilentlyContinue){{"
        f"Stop-ScheduledTask -TaskName {task};"
        "$recallStopUntil=(Get-Date).AddSeconds(15);"
        "do{Start-Sleep -Milliseconds 250;"
        f"$recallStopping=Get-ScheduledTask -TaskName {task};"
        "}while($recallStopping.State -in @('Running','Queued') -and (Get-Date) -lt $recallStopUntil);"
        "if($recallStopping.State -in @('Running','Queued')){throw 'Recall task did not stop before update'};"
        "};"
    )


def start_windows_task(name):
    task = ps_quote(name)
    return (
        f"Start-ScheduledTask -TaskName {task};"
        "$recallStartUntil=(Get-Date).AddSeconds(15);"
        "do{Start-Sleep -Milliseconds 250;"
        f"$recallStarting=Get-ScheduledTask -TaskName {task};"
        "}while($recallStarting.State -ne 'Running' -and (Get-Date) -lt $recallStartUntil);"
        "if($recallStarting.State -ne 'Running'){throw 'Recall task did not remain running; inspect Task Scheduler'};"
        "$recallStarting | Select-Object TaskName,State | ConvertTo-Json -Compress"
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--sha256", help="expected release checksum")
    parser.add_argument("--register", action="store_true", help="register the local T3 provider")
    parser.add_argument("--worker", action="store_true", help="enable the per-user background worker")
    parser.add_argument("--sync-peer", choices=("fleet-vps", "fleet-laptop"), help="enable private fleet memory sync")
    args = parser.parse_args()
    source = args.binary.resolve(strict=True)
    checksum = hashlib.sha256(source.read_bytes()).hexdigest()
    if args.sha256 and checksum != args.sha256.lower():
        parser.error("Recall checksum does not match")
    output = subprocess.check_output([str(source), "version"], text=True).strip()
    if not re.fullmatch(r"Recall [0-9A-Za-z.+-]+", output):
        parser.error("The supplied executable did not identify itself as Recall")
    version = output.split()[1]
    filename = "recall.exe" if os.name == "nt" else "recall"
    release = Path.home() / ".local" / "lib" / "recall" / version / checksum[:16]
    release.mkdir(parents=True, exist_ok=True)
    installed = release / filename
    if installed.exists():
        if hashlib.sha256(installed.read_bytes()).hexdigest() != checksum:
            raise RuntimeError("An installed immutable build has changed")
    else:
        temporary = installed.with_suffix(installed.suffix + ".tmp")
        shutil.copyfile(source, temporary)
        temporary.chmod(0o755)
        os.replace(temporary, installed)
    bindir = Path.home() / ".local" / "bin"
    bindir.mkdir(parents=True, exist_ok=True)
    if os.name == "nt":
        launcher = bindir / "recall.cmd"
        launcher.write_text(f'@echo off\r\n"{installed}" %*\r\n', encoding="utf-8")
        command = installed
    else:
        launcher = bindir / "recall"
        if launcher.exists() and not launcher.is_symlink():
            raise RuntimeError("The Recall command path contains another executable; preserve it before installing")
        temporary = bindir / ".recall-new"
        temporary.unlink(missing_ok=True)
        temporary.symlink_to(installed)
        os.replace(temporary, launcher)
        command = launcher
    shared = Path.home() / ".local" / "share" / "recall"
    shared.mkdir(parents=True, exist_ok=True)
    for name in ("sync-endpoint.py", "sync-memory.py"):
        shutil.copyfile(Path(__file__).with_name(name), shared / name)
    manifest = shared / "install.json"
    temporary = manifest.with_suffix(".tmp")
    temporary.write_text(json.dumps({"version": version, "binary": str(installed), "sha256": checksum}), encoding="utf-8")
    temporary.chmod(0o600)
    os.replace(temporary, manifest)
    if args.register:
        run([sys.executable, str(Path(__file__).with_name("register-t3.py")), "--binary", str(command)])
    if args.worker:
        if os.name == "nt":
            script = (
                "$ErrorActionPreference='Stop';"
                "$action=New-ScheduledTaskAction -Execute " + ps_quote(installed)
                + " -Argument 'memory worker --interval 20s';"
                "$trigger=New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME;"
                "$principal=New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Limited;"
                "$settings=New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1);"
                + stop_windows_task("RecallMemory") +
                "Register-ScheduledTask -TaskName RecallMemory -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null;"
                + start_windows_task("RecallMemory")
            )
            run(["powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script])
        else:
            unitdir = Path.home() / ".config" / "systemd" / "user"
            unitdir.mkdir(parents=True, exist_ok=True)
            unit = unitdir / "recall-memory.service"
            unit.write_text(
                "[Unit]\nDescription=Recall background memory worker\n\n[Service]\nType=simple\n"
                f"ExecStart={json.dumps(str(command))} memory worker --interval 20s\n"
                "Restart=on-failure\nRestartSec=10\nTimeoutStopSec=20\nUMask=0077\n\n[Install]\nWantedBy=default.target\n",
                encoding="utf-8",
            )
            run(["systemctl", "--user", "daemon-reload"])
            run(["systemctl", "--user", "enable", "recall-memory.service"])
            run(["systemctl", "--user", "restart", "recall-memory.service"])
            run(["systemctl", "--user", "is-active", "recall-memory.service"])
    if args.sync_peer:
        sync_script = shared / "sync-memory.py"
        arguments = [str(sync_script), "--peer", args.sync_peer, "--watch"]
        if os.name == "nt":
            script = (
                "$ErrorActionPreference='Stop';"
                "$action=New-ScheduledTaskAction -Execute " + ps_quote(sys.executable)
                + " -Argument " + ps_quote(subprocess.list2cmdline(arguments)) + ";"
                "$trigger=New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME;"
                "$principal=New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Limited;"
                "$settings=New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1);"
                + stop_windows_task("RecallMemorySync") +
                "Register-ScheduledTask -TaskName RecallMemorySync -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null;"
                + start_windows_task("RecallMemorySync")
            )
            run(["powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script])
        else:
            unitdir = Path.home() / ".config" / "systemd" / "user"
            unitdir.mkdir(parents=True, exist_ok=True)
            executable = " ".join(json.dumps(arg) for arg in [sys.executable] + arguments)
            (unitdir / "recall-memory-sync.service").write_text(
                "[Unit]\nDescription=Recall private fleet memory sync\n\n[Service]\nType=simple\n"
                f"ExecStart={executable}\nRestart=on-failure\nRestartSec=30\nTimeoutStopSec=20\nUMask=0077\n\n[Install]\nWantedBy=default.target\n",
                encoding="utf-8",
            )
            run(["systemctl", "--user", "daemon-reload"])
            run(["systemctl", "--user", "enable", "--now", "recall-memory-sync.service"])
    print(json.dumps({"name": "Recall", "version": version, "binary": str(installed), "provider_command": str(command), "sha256": checksum}))


if __name__ == "__main__":
    main()
