#!/bin/sh
# Copyright 2026 Abhinav Gautam
# SPDX-License-Identifier: Apache-2.0
#
# serial101.sh puts an IEC 101 peer on a pty that socat bridges to TCP, in
# place of a serial line and a serial-to-IP converter. Options after the
# link address (bal=1 la= cot= ca= ioa=) are passed to the peer.
#
#   serial101.sh slave <link> [options]               the peer is a secondary; TCP listens on 2410
#   serial101.sh master <host> <port> <link> [options] the peer polls; TCP connects to host:port
set -e
case "$1" in
slave)
	socat pty,link=/tmp/tty0,raw,echo=0 tcp-listen:2410,reuseaddr &
	link=$2
	mode=slave101
	shift 2
	;;
master)
	socat pty,link=/tmp/tty0,raw,echo=0 "tcp:$2:$3" &
	link=$4
	mode=master101
	shift 4
	;;
*)
	echo "usage: serial101.sh slave <link> [options] | master <host> <port> <link> [options]" >&2
	exit 2
	;;
esac
while [ ! -e /tmp/tty0 ]; do sleep 0.05; done
exec peer "$mode" /tmp/tty0 "$link" "$@"
