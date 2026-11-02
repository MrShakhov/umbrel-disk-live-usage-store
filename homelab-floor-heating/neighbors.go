package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// These thermostats stop answering ARP requests after a while, yet keep working
// for anyone who already knows their MAC address. So a small container on the
// host network pins each thermostat's address as a permanent neighbor entry.
// It reads neighbors.json, which holds no keys, and can't read the rest of the
// data folder: it runs without CAP_DAC_OVERRIDE.

const (
	neighborsFile = "neighbors.json"
	pinInterval   = 30 * time.Second

	rtmNewNeigh  = 28
	rtmDelNeigh  = 29
	nudPermanent = 0x80
	ndaDst       = 1
	ndaLLAddr    = 2
)

type neighbor struct {
	IP  string `json:"ip"`
	MAC string `json:"mac"`
}

func pinNeighbors(dir string) {
	log.Printf("pinning thermostat addresses from %s every %s", neighborsFile, pinInterval)
	pinned := map[string]string{}
	for {
		want, err := readNeighbors(filepath.Join(dir, neighborsFile))
		if err != nil {
			log.Printf("read %s: %v", neighborsFile, err)
		} else {
			syncNeighbors(want, pinned, setNeighbor, delNeighbor)
		}
		time.Sleep(pinInterval)
	}
}

func readNeighbors(path string) ([]neighbor, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []neighbor
	return out, json.Unmarshal(b, &out)
}

// syncNeighbors makes the pinned set match want. Entries are set again every
// time, because the kernel drops them when the link goes down.
func syncNeighbors(want []neighbor, pinned map[string]string, set func(neighbor) error, del func(string) error) {
	wanted := map[string]bool{}
	for _, n := range want {
		wanted[n.IP] = true
		if err := set(n); err != nil {
			log.Printf("pin %s: %v", n.IP, err)
			continue
		}
		if pinned[n.IP] != n.MAC {
			log.Printf("pinned %s to %s", n.IP, n.MAC)
			pinned[n.IP] = n.MAC
		}
	}
	for ip := range pinned {
		if wanted[ip] {
			continue
		}
		if err := del(ip); err != nil {
			log.Printf("unpin %s: %v", ip, err)
			continue
		}
		log.Printf("unpinned %s", ip)
		delete(pinned, ip)
	}
}

func setNeighbor(n neighbor) error {
	mac, err := net.ParseMAC(n.MAC)
	if err != nil {
		return err
	}
	flags := syscall.NLM_F_REQUEST | syscall.NLM_F_ACK | syscall.NLM_F_CREATE | syscall.NLM_F_REPLACE
	return neighborRequest(rtmNewNeigh, flags, n.IP, mac)
}

func delNeighbor(ip string) error {
	return neighborRequest(rtmDelNeigh, syscall.NLM_F_REQUEST|syscall.NLM_F_ACK, ip, nil)
}

// interfaceFor finds the interface on the same subnet as ip.
func interfaceFor(ip net.IP) (*net.Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.Contains(ip) {
				return &ifaces[i], nil
			}
		}
	}
	return nil, errors.New("no interface on this subnet")
}

func neighborRequest(msgType, flags int, addr string, mac net.HardwareAddr) error {
	ip := net.ParseIP(addr).To4()
	if ip == nil {
		return fmt.Errorf("not an IPv4 address: %q", addr)
	}
	iface, err := interfaceFor(ip)
	if err != nil {
		return err
	}
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	req := encodeNeighbor(msgType, flags, iface.Index, ip, mac)
	if err := syscall.Sendto(fd, req, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	reply := make([]byte, 4096)
	n, _, err := syscall.Recvfrom(fd, reply, 0)
	if err != nil {
		return err
	}
	return netlinkAck(reply[:n])
}

// encodeNeighbor builds an nlmsghdr, an ndmsg and the address attributes. Netlink
// uses native byte order, which is little-endian on arm64 and amd64.
func encodeNeighbor(msgType, flags, ifindex int, ip net.IP, mac net.HardwareAddr) []byte {
	b := make([]byte, 28, 48)
	binary.LittleEndian.PutUint16(b[4:], uint16(msgType))
	binary.LittleEndian.PutUint16(b[6:], uint16(flags))
	binary.LittleEndian.PutUint32(b[8:], 1) // sequence number
	b[16] = syscall.AF_INET
	binary.LittleEndian.PutUint32(b[20:], uint32(ifindex))
	binary.LittleEndian.PutUint16(b[24:], nudPermanent)
	b = appendAttr(b, ndaDst, ip)
	if mac != nil {
		b = appendAttr(b, ndaLLAddr, mac)
	}
	binary.LittleEndian.PutUint32(b, uint32(len(b)))
	return b
}

func appendAttr(b []byte, kind uint16, value []byte) []byte {
	head := make([]byte, 4)
	binary.LittleEndian.PutUint16(head, uint16(len(head)+len(value)))
	binary.LittleEndian.PutUint16(head[2:], kind)
	b = append(append(b, head...), value...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// netlinkAck reads the error code of an NLMSG_ERROR reply; 0 means success.
func netlinkAck(reply []byte) error {
	if len(reply) < 20 || binary.LittleEndian.Uint16(reply[4:]) != syscall.NLMSG_ERROR {
		return errors.New("unexpected netlink reply")
	}
	if code := int32(binary.LittleEndian.Uint32(reply[16:])); code != 0 {
		return syscall.Errno(-code)
	}
	return nil
}
