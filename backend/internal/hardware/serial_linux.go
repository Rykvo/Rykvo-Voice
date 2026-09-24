//go:build linux

package hardware

import (
	"errors"
	"golang.org/x/sys/unix"
	"path/filepath"
	"strings"
)

type linuxAT struct{ fd int }

func openAT(path string) (atPort, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.EACCES) {
		return nil, errors.New("PERMISSION_DENIED")
	}
	if errors.Is(err, unix.EBUSY) {
		return nil, errors.New("DEVICE_BUSY")
	}
	if err != nil {
		return nil, err
	}
	fail := func(err error) (atPort, error) { unix.Close(fd); return nil, err }
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(errors.New("DEVICE_BUSY"))
	}
	if !strings.HasPrefix(filepath.Base(path), "wwan") {
		if err = unix.IoctlSetInt(fd, unix.TIOCEXCL, 0); err != nil {
			return fail(errors.New("DEVICE_BUSY"))
		}
		t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			return fail(err)
		}
		t.Iflag = 0
		t.Oflag = 0
		t.Lflag = 0
		t.Cflag = unix.B115200 | unix.CS8 | unix.CREAD | unix.CLOCAL
		t.Ispeed = unix.B115200
		t.Ospeed = unix.B115200
		t.Cc[unix.VMIN] = 0
		t.Cc[unix.VTIME] = 0
		if err = unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
			return fail(err)
		}
		if err = unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH); err != nil {
			return fail(err)
		}
	}
	return &linuxAT{fd}, nil
}
func (p *linuxAT) Read(b []byte) (int, error)  { return p.transfer(b, false) }
func (p *linuxAT) Write(b []byte) (int, error) { return p.transfer(b, true) }
func (p *linuxAT) Close() error                { return unix.Close(p.fd) }
func (p *linuxAT) transfer(b []byte, write bool) (int, error) {
	event := int16(unix.POLLIN)
	if write {
		event = unix.POLLOUT
	}
	fds := []unix.PollFd{{Fd: int32(p.fd), Events: event}}
	n, err := unix.Poll(fds, 100)
	if errors.Is(err, unix.EINTR) {
		return 0, nil
	}
	if err != nil || n == 0 {
		return 0, err
	}
	if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
		return 0, errors.New("DEVICE_UNAVAILABLE")
	}
	if write {
		n, err = unix.Write(p.fd, b)
	} else {
		n, err = unix.Read(p.fd, b)
	}
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
		return 0, nil
	}
	return n, err
}
