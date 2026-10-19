package main

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestGvisorARPAndFilteredPeer(t *testing.T) {
	sockets, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	guestFile := os.NewFile(uintptr(sockets[0]), "guest")
	backendFile := os.NewFile(uintptr(sockets[1]), "backend")
	defer guestFile.Close()
	defer backendFile.Close()
	guestRaw, err := net.FileConn(guestFile)
	if err != nil {
		t.Fatal(err)
	}
	defer guestRaw.Close()
	backendRaw, err := net.FileConn(backendFile)
	if err != nil {
		t.Fatal(err)
	}
	defer backendRaw.Close()
	guest := guestRaw.(*net.UnixConn)
	backend := backendRaw.(*net.UnixConn)
	network, err := newBackend(networkConfig(net.HardwareAddr(guestMAC[:])))
	if err != nil {
		t.Fatal(err)
	}
	defer network.Close()
	hosts := new(hostAddresses)
	if err := hosts.refresh(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- network.AcceptVfkit(ctx, &filteredConn{UnixConn: backend, mac: guestMAC, access: isolated, hosts: hosts})
	}()
	defer func() { cancel(); backend.Close(); <-finished }()
	arp := make([]byte, 42)
	for i := range 6 {
		arp[i] = 0xff
	}
	copy(arp[6:12], guestMAC[:])
	binary.BigEndian.PutUint16(arp[12:14], 0x0806)
	binary.BigEndian.PutUint16(arp[14:16], 1)
	binary.BigEndian.PutUint16(arp[16:18], 0x0800)
	arp[18] = 6
	arp[19] = 4
	binary.BigEndian.PutUint16(arp[20:22], 1)
	copy(arp[22:28], guestMAC[:])
	copy(arp[28:32], []byte{192, 168, 127, 2})
	copy(arp[38:42], []byte{192, 168, 127, 3})
	guest.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := guest.Write(arp); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 1514)
	if _, err := guest.Read(response); err == nil {
		t.Fatal("peer ARP crossed filter")
	}
	arp[41] = 1 // the gateway, the only authorized ARP target
	guest.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := guest.Write(arp); err != nil {
		t.Fatal(err)
	}
	n, err := guest.Read(response)
	if err != nil {
		t.Fatal("gateway ARP was not answered by gvisor: ", err)
	}
	if n < 42 || binary.BigEndian.Uint16(response[20:22]) != 2 || response[28] != 192 || response[31] != 1 {
		t.Fatalf("unexpected ARP answer: %x", response[:n])
	}
}
