//go:build linux

package hardware

import (
	"bytes"
	"context"
	"golang.org/x/sys/unix"
)

func Watch(ctx context.Context) <-chan struct{} {
	events := make(chan struct{}, 1)
	go func() {
		defer close(events)
		fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.NETLINK_KOBJECT_UEVENT)
		if err != nil {
			return
		}
		defer unix.Close(fd)
		if unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 1}) != nil {
			return
		}
		buf := make([]byte, 8192)
		for ctx.Err() == nil {
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			n, e := unix.Poll(fds, 250)
			if e != nil || n == 0 {
				continue
			}
			n, from, e := unix.Recvfrom(fd, buf, 0)
			sender, ok := from.(*unix.SockaddrNetlink)
			if e != nil || !ok || sender.Pid != 0 {
				continue
			}
			if bytes.Contains(buf[:n], []byte("SUBSYSTEM=usb")) || bytes.Contains(buf[:n], []byte("SUBSYSTEM=tty")) || bytes.Contains(buf[:n], []byte("SUBSYSTEM=wwan")) {
				select {
				case events <- struct{}{}:
				default:
				}
			}
		}
	}()
	return events
}
