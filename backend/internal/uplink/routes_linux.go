package uplink

import (
	"encoding/binary"
	"errors"
	"slices"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Read kernel state without spawning ip processes for every worker poll.
func routeDump(kind uint16) ([]syscall.NetlinkMessage, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	addr := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	if err = unix.Bind(fd, addr); err != nil {
		return nil, err
	}
	if err = unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Sec: 1}); err != nil {
		return nil, err
	}
	request := make([]byte, unix.NLMSG_HDRLEN+unix.SizeofRtMsg)
	binary.NativeEndian.PutUint32(request, uint32(len(request)))
	binary.NativeEndian.PutUint16(request[4:], kind)
	binary.NativeEndian.PutUint16(request[6:], unix.NLM_F_REQUEST|unix.NLM_F_DUMP)
	binary.NativeEndian.PutUint32(request[8:], 1)
	if err = unix.Sendto(fd, request, 0, addr); err != nil {
		return nil, err
	}
	var result []syscall.NetlinkMessage
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		buffer := make([]byte, 64*1024)
		n, _, flags, from, err := unix.Recvmsg(fd, buffer, nil, 0)
		if err != nil {
			return nil, err
		}
		peer, ok := from.(*unix.SockaddrNetlink)
		if !ok || peer.Pid != 0 || flags&unix.MSG_TRUNC != 0 {
			return nil, unix.EINVAL
		}
		messages, err := syscall.ParseNetlinkMessage(buffer[:n])
		if err != nil || len(messages) == 0 {
			return nil, unix.EINVAL
		}
		for _, msg := range messages {
			if msg.Header.Seq != 1 || msg.Header.Flags&unix.NLM_F_DUMP_INTR != 0 {
				return nil, unix.EINTR
			}
			switch msg.Header.Type {
			case unix.NLMSG_DONE:
				if len(msg.Data) >= 4 && binary.NativeEndian.Uint32(msg.Data) != 0 {
					return nil, unix.EIO
				}
				return result, nil
			case unix.NLMSG_ERROR:
				return nil, unix.EIO
			default:
				result = append(result, msg)
			}
		}
	}
	return nil, unix.ETIMEDOUT
}

// Both rule and route messages use a 12-byte header and a 32-bit table attribute.
func ownedRouteKey(msg syscall.NetlinkMessage, table uint32) (string, bool, error) {
	if len(msg.Data) < unix.SizeofRtMsg {
		return "", false, unix.EINVAL
	}
	owner := uint32(msg.Data[4])
	parts := []string{string(msg.Data[:unix.SizeofRtMsg])}
	for data := msg.Data[unix.SizeofRtMsg:]; len(data) > 0; {
		if len(data) < 4 {
			return "", false, unix.EINVAL
		}
		n := int(binary.NativeEndian.Uint16(data))
		kind := binary.NativeEndian.Uint16(data[2:])
		aligned := (n + 3) &^ 3
		if n < 4 || aligned > len(data) {
			return "", false, unix.EINVAL
		}
		if kind == unix.RTA_TABLE { // FRA_TABLE has the same value.
			if n != 8 {
				return "", false, unix.EINVAL
			}
			owner = binary.NativeEndian.Uint32(data[4:])
		}
		// IPv6 cache usage changes without a route configuration change.
		if msg.Header.Type != unix.RTM_NEWROUTE || kind != unix.RTA_CACHEINFO {
			parts = append(parts, string(data[:n]))
		}
		data = data[aligned:]
	}
	slices.Sort(parts[1:])
	return strconv.Itoa(int(msg.Header.Type)) + ":" + encodeParts(parts), owner == table, nil
}

func encodeParts(parts []string) string {
	var result string
	for _, part := range parts {
		result += strconv.Itoa(len(part)) + ":" + part
	}
	return result
}

func (l *Lease) routeState() ([]string, int, int, error) {
	table, err := strconv.ParseUint(l.table, 10, 32)
	if err != nil || table>>24 != 0x52 {
		return nil, 0, 0, errors.New("invalid lease table")
	}
	var state []string
	var rules, routes int
	for _, kind := range []uint16{unix.RTM_GETRULE, unix.RTM_GETROUTE} {
		messages, err := routeDump(kind)
		if err != nil {
			return nil, 0, 0, err
		}
		for _, msg := range messages {
			key, owned, err := ownedRouteKey(msg, uint32(table))
			if err != nil {
				return nil, 0, 0, err
			}
			if owned {
				state = append(state, key)
				if kind == unix.RTM_GETRULE {
					rules++
				} else {
					routes++
				}
			}
		}
	}
	slices.Sort(state)
	return state, rules, routes, nil
}
