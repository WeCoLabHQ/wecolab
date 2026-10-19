package main

import (
	"net"
	"testing"
	"time"

	dnswire "github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

func TestDNSPacketAdmissionAndResponseRelease(t *testing.T) {
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	guest, err := net.Dial("udp4", listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	budget := newFlowBudget(1)
	limited := newDNSPacketConn(listener, budget)
	query := new(dnswire.Msg)
	query.SetQuestion("example.test.", dnswire.TypeA)
	encoded, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guest.Write(encoded); err != nil {
		t.Fatal(err)
	}
	listener.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1500)
	n, client, err := limited.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(encoded) || budget.active() != 1 {
		t.Fatal("DNS query not admitted")
	}
	if _, err := guest.Write(encoded); err != nil {
		t.Fatal(err)
	}
	listener.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := limited.ReadFrom(buf); err == nil {
		t.Fatal("second query admitted without capacity")
	}
	response := new(dnswire.Msg)
	response.SetReply(query)
	encodedResponse, err := response.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limited.WriteTo(encodedResponse, client); err != nil {
		t.Fatal(err)
	}
	if budget.active() != 0 {
		t.Fatal("DNS slot not released after real response")
	}
	listener.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := guest.Write(encoded); err != nil {
		t.Fatal(err)
	}
	if _, _, err := limited.ReadFrom(buf); err != nil {
		t.Fatal("capacity not restored: ", err)
	}
}

func TestDNSMalformedOrResponseCannotReserve(t *testing.T) {
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	guest, err := net.Dial("udp4", listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	budget := newFlowBudget(1)
	limited := newDNSPacketConn(listener, budget)
	if _, err := guest.Write([]byte{0, 1, 2}); err != nil {
		t.Fatal(err)
	}
	listener.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := limited.ReadFrom(make([]byte, 1500)); err == nil {
		t.Fatal("malformed query delivered")
	}
	if budget.active() != 0 {
		t.Fatal("malformed query retained admission")
	}
}

func TestDNSConnectionSlotWaitsForEndpointCleanup(t *testing.T) {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	defer s.Destroy()
	var wq waiter.Queue
	ep, terr := s.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if terr != nil {
		t.Fatal(terr)
	}
	budget := newFlowBudget(1)
	release, ok := budget.acquire()
	if !ok {
		t.Fatal("connection admission failed")
	}
	local, remote := net.Pipe()
	defer remote.Close()
	conn := &dnsConn{Conn: local, endpoint: ep, release: release}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if budget.active() != 1 {
		t.Fatal("slot released before gVisor endpoint cleanup")
	}
	ep.Close()
	waitFlows(t, budget, 0)
}
