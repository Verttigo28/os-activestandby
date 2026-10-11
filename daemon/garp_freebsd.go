//go:build freebsd

package main

import (
	"encoding/binary"
	"log"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const biocsetif = 0x8020426c // _IOW('B', 108, struct ifreq)

// announce sends gratuitous ARPs (RFC 5227 announcement form) for every IPv4 address of dev
// through a raw bpf write: immediately, then twice more 100 ms apart in the background.
func announce(dev string) {
	ifc, err := net.InterfaceByName(dev)
	if err != nil || len(ifc.HardwareAddr) != 6 {
		return
	}
	addrs, _ := ifc.Addrs()
	var frames [][]byte
	for _, a := range addrs {
		if ip, _, err := net.ParseCIDR(a.String()); err == nil && ip.To4() != nil {
			frames = append(frames, garpFrame(ifc.HardwareAddr, ip.To4()))
		}
	}
	if len(frames) == 0 {
		return
	}
	f, err := os.OpenFile("/dev/bpf", os.O_RDWR, 0)
	if err != nil {
		log.Printf("garp: open bpf: %v", err)
		return
	}
	var ifr [32]byte
	copy(ifr[:], dev)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), biocsetif, uintptr(unsafe.Pointer(&ifr[0]))); e != 0 {
		log.Printf("garp: BIOCSETIF %s: %v", dev, e)
		f.Close()
		return
	}
	for _, fr := range frames {
		f.Write(fr)
	}
	// the first announcement goes out at once; the repeats run in the background so promote does not wait for them
	go func() {
		defer f.Close()
		for i := 0; i < 2; i++ {
			time.Sleep(100 * time.Millisecond)
			for _, fr := range frames {
				f.Write(fr)
			}
		}
	}()
}

func garpFrame(mac net.HardwareAddr, ip net.IP) []byte {
	b := make([]byte, 42)
	copy(b[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(b[6:12], mac)
	binary.BigEndian.PutUint16(b[12:], 0x0806)
	binary.BigEndian.PutUint16(b[14:], 1)      // ethernet
	binary.BigEndian.PutUint16(b[16:], 0x0800) // ipv4
	b[18], b[19] = 6, 4
	binary.BigEndian.PutUint16(b[20:], 1) // request (announcement: spa == tpa)
	copy(b[22:28], mac)
	copy(b[28:32], ip)
	copy(b[38:42], ip) // target hw stays zero
	return b
}
