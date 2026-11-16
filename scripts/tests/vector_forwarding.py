"""Send real datagrams and check Vector's stdout record framing."""

import json
import pathlib
import sys

from vector_udp import send, wait_bound

mode, directory = sys.argv[1:3]
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
    container, port = sys.argv[3], int(sys.argv[4])
    wait_bound(container, port)
    endings = [b"\n", b"", b"\r\n", b"\n", b"\n", b"\n", b"\n"]
    send(container, port, [record + ending for record, ending in zip(records, endings)])
elif mode == "check":
    actual = (work / "stdout").read_bytes().splitlines()
    if actual != records:
        sys.exit(f"stdout records differ: expected {records!r}, got {actual!r}")
    for index in (0, 1, 2, 5, 6):
        json.loads(actual[index])
    print(f"Forwarded {len(actual)} datagrams as {len(actual)} nonempty stdout records")
else:
    sys.exit(f"unknown mode: {mode}")
