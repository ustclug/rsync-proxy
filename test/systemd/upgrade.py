#!/usr/bin/env python3
"""Exercise MAINPID handoff using an isolated user-level systemd service."""

import argparse
import contextlib
import http.client
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=40)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def control(path, method, endpoint):
    with contextlib.closing(UnixHTTP(path)) as client:
        client.request(method, endpoint)
        response = client.getresponse()
        body = response.read()
        if response.status != 200:
            raise RuntimeError(f"{endpoint}: {response.status}: {body!r}")
        return body


def echo(connection):
    with connection, connection.makefile("rb") as reader:
        if not reader.readline():
            return
        connection.sendall(b"@RSYNCD: 32.0 sha512 sha256 sha1 md5 md4\n")
        if not reader.readline():
            return
        connection.sendall(b"READY\n")
        while line := reader.readline():
            connection.sendall(line)


def accept(listener):
    while True:
        try:
            connection, _ = listener.accept()
        except OSError:
            return
        threading.Thread(target=echo, args=(connection,), daemon=True).start()


def property_value(unit, name):
    return subprocess.check_output(
        ["systemctl", "--user", "show", unit, f"--property={name}", "--value"],
        text=True,
    ).strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    args = parser.parse_args()
    source = args.binary.resolve(strict=True)
    unit = f"rsync-proxy-upgrade-test-{os.getpid()}.service"
    with tempfile.TemporaryDirectory(prefix="rp-systemd-") as temporary:
        directory = Path(temporary)
        binary = directory / "proxy"
        shutil.copy2(source, binary)
        control_socket = directory / "http.sock"
        # A Unix rsync listener also verifies that no retired process unlinks it.
        rsync_socket = directory / "rsync.sock"
        config = directory / "config.toml"
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen()
        port = listener.getsockname()[1]
        threading.Thread(target=accept, args=(listener,), daemon=True).start()
        config.write_text(
            f'[proxy]\nlisten="{rsync_socket}"\nlisten_http="{control_socket}"\n'
            f'state_dir="{directory}"\n[upstreams.u]\n'
            f'address="127.0.0.1:{port}"\nmodules=["foo"]\n'
            'max_active_connections=3\n'
        )
        connections = []
        readers = []
        try:
            subprocess.run(
                ["systemd-run", "--user", f"--unit={unit}", "--collect",
                 "--property=Type=notify", "--property=NotifyAccess=all",
                 "--property=KillMode=control-group", "--property=TimeoutStopSec=5s",
                 str(binary), "--config", str(config)],
                check=True,
            )
            pids = []
            for generation in range(3):
                if generation:
                    shutil.copy2(source, directory / "proxy.next")
                    os.replace(directory / "proxy.next", binary)
                    control(control_socket, "POST", "/upgrade?timeout=30s")
                assert property_value(unit, "ActiveState") == "active"
                pid = property_value(unit, "MainPID")
                assert pid != "0" and pid not in pids, pids
                pids.append(pid)
                connection = socket.socket(socket.AF_UNIX)
                connection.settimeout(5)
                connection.connect(str(rsync_socket))
                connections.append(connection)
                reader = connection.makefile("rb")
                readers.append(reader)
                connection.sendall(b"@RSYNCD: 32.0\n")
                assert reader.readline().startswith(b"@RSYNCD:")
                connection.sendall(b"foo\n")
                assert reader.readline() == b"READY\n"
                for client, stream in zip(connections, readers):
                    client.sendall(b"ping\n")
                    assert stream.readline() == b"ping\n"
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                status = json.loads(control(control_socket, "GET", "/status"))
                if status["count"] == 3:
                    break
                time.sleep(0.1)
            assert status["count"] == 3, status
            for reader in readers:
                reader.close()
            for connection in connections:
                connection.close()
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline:
                status = json.loads(control(control_socket, "GET", "/status"))
                if all(g["state"] == "exited" for g in status["generations"][:-1]):
                    break
                time.sleep(0.1)
            assert all(g["state"] == "exited" for g in status["generations"][:-1]), status
            assert property_value(unit, "MainPID") == pids[-1]
            assert property_value(unit, "ActiveState") == "active"
            assert rsync_socket.exists() and control_socket.exists()
            print(f"PASS: MAINPID {' -> '.join(pids)}; all streams survived; old generations exited")
        finally:
            for reader in readers:
                reader.close()
            for connection in connections:
                connection.close()
            subprocess.run(["systemctl", "--user", "stop", unit], check=False)
            listener.close()


if __name__ == "__main__":
    main()
