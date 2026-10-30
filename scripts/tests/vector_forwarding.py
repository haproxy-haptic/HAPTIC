"""Send real datagrams and check Vector's stdout record framing."""

import json
import os
import pathlib
import socket
import sys
import time

mode, directory = sys.argv[1:]
work = pathlib.Path(directory)
records = [
    b'{"status":200,"path":"/newline"}',
    b'{"status":201,"path":"/no-newline"}',
    b'{"status":202,"path":"/crlf"}',
    b'[NOTICE] worker started',
    b'{"status":200',
    b'{"status":203,"path":"/escaped\\nnewline"}',
    b'{"status":204,"path":"/framing-complete"}',
]
if mode == "send":
    deadline = time.monotonic() + 10
    while not (work / "haproxy.sock").exists():
        if time.monotonic() >= deadline:
            sys.exit("Vector did not create its log socket")
        time.sleep(0.05)
    endings = [b"\n", b"", b"\r\n", b"\n", b"\n", b"\n", b"\n"]
    os.chdir(work)
    with socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM) as sender:
        for record, ending in zip(records, endings):
            sender.sendto(record + ending, "haproxy.sock")
elif mode == "check":
    actual = (work / "stdout").read_bytes().splitlines()
    if actual != records:
        sys.exit(f"stdout records differ: expected {records!r}, got {actual!r}")
    for index in (0, 1, 2, 5, 6):
        json.loads(actual[index])
    print(f"Forwarded {len(actual)} datagrams as {len(actual)} nonempty stdout records")
else:
    sys.exit(f"unknown mode: {mode}")
