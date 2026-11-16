"""Send access-log datagrams to Vector's loopback UDP source inside its container."""

import struct
import subprocess
import time

# Reads length-prefixed records from stdin and sends one datagram each. Every 16
# records it waits while the receive queue is past half the default 212992-byte
# buffer: the kernel drops UDP silently, so an unpaced sender would test the
# buffer, not the pipeline.
SENDER = r"""
use strict;
use IO::Socket::INET;
my ($port, $key, $pause_every) = @ARGV;
my $s = IO::Socket::INET->new(PeerAddr => "127.0.0.1", PeerPort => $port, Proto => "udp") or die "socket: $!";
sub queued {
  open(my $f, "<", "/proc/net/udp") or die "open: $!";
  while (my $line = <$f>) {
    return hex($1) if $line =~ /^\s*\d+: $key \S+ \S+ [0-9A-F]+:([0-9A-F]+) /;
  }
  die "no UDP socket bound on $key";
}
binmode STDIN;
my $sent = 0;
while (read(STDIN, my $header, 4) == 4) {
  my $n = unpack("N", $header);
  read(STDIN, my $record, $n) == $n or die "short record";
  if ($sent % 16 == 0) {
    select(undef, undef, undef, 0.001) while queued() > 106496;
  }
  select(undef, undef, undef, 0.01) if $pause_every && $sent % $pause_every == 0;
  $sent++;
  defined $s->send($record) or die "send: $!";
}
"""


def socket_key(port):
    return f"0100007F:{port:04X}"


def wait_bound(container, port, budget=15):
    deadline = time.monotonic() + budget
    while True:
        table = subprocess.run(["docker", "exec", container, "cat", "/proc/net/udp"],
                               capture_output=True, text=True).stdout
        if f": {socket_key(port)} " in table:
            return
        if time.monotonic() >= deadline:
            raise AssertionError(f"Vector did not bind its access-log port {port}")
        time.sleep(0.05)


def drops(container, port):
    table = subprocess.run(["docker", "exec", container, "cat", "/proc/net/udp"],
                           check=True, capture_output=True, text=True).stdout
    for line in table.splitlines():
        if f": {socket_key(port)} " in line:
            return int(line.split()[-1])
    raise AssertionError(f"no UDP socket bound on port {port}")


def send(container, port, records, pause_every=0):
    payload = b"".join(struct.pack("!I", len(record)) + record for record in records)
    subprocess.run(["docker", "exec", "-i", container, "perl", "-e", SENDER, str(port), socket_key(port), str(pause_every)],
                   input=payload, check=True)
