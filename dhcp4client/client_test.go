// Copyright 2018 the u-root Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dhcp4client

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/mergetb/dhcp4"
)

type timeoutErr struct{}

func (timeoutErr) Error() string {
	return "i/o timeout"
}

func (timeoutErr) Timeout() bool {
	return true
}

type udpPacket struct {
	source  *net.UDPAddr
	dest    *net.UDPAddr
	payload []byte
}

// mockUDPConn implements net.PacketConn.
type mockUDPConn struct {
	// This'll just be nil for all the methods we don't implement.

	// in is the queue of packets ReadFromUDP reads from.
	//
	// ReadFromUDP returns io.EOF when in is closed.
	in chan udpPacket

	inTimer *time.Timer

	// out is the queue of packets WriteTo writes to.
	out chan<- udpPacket

	closed bool
}

func newMockUDPConn(in chan udpPacket, out chan<- udpPacket) *mockUDPConn {
	return &mockUDPConn{
		in:  in,
		out: out,
	}
}

// SetReadDeadline implements PacketConn.SetReadDeadline.
func (m *mockUDPConn) SetReadDeadline(t time.Time) error {
	duration := t.Sub(time.Now())
	if duration < 0 {
		return fmt.Errorf("deadline must be in the future")
	}
	m.inTimer = time.NewTimer(duration)
	return nil
}

func (m *mockUDPConn) LocalAddr() net.Addr {
	panic("unused")
}

func (m *mockUDPConn) SetWriteDeadline(t time.Time) error {
	panic("unused")
}

func (m *mockUDPConn) SetDeadline(t time.Time) error {
	panic("unused")
}

// Close implements PacketConn.Close.
func (m *mockUDPConn) Close() error {
	m.closed = true
	close(m.out)
	return nil
}

// ReadFrom is a mock for PacketConn.ReadFromUDP.
func (m *mockUDPConn) ReadFrom(b []byte) (int, net.Addr, error) {
	// Make sure we don't have data waiting.
	select {
	case p, ok := <-m.in:
		if !ok {
			// Connection was closed.
			return 0, nil, nil
		}
		return copy(b, p.payload), p.source, nil
	default:
	}

	select {
	case p, ok := <-m.in:
		if !ok {
			return 0, nil, nil
		}
		return copy(b, p.payload), p.source, nil
	case <-m.inTimer.C:
		// This net.OpError will return true for Timeout().
		return 0, nil, &net.OpError{Err: timeoutErr{}}
	}
}

// WriteTo is a mock for PacketConn.WriteTo.
func (m *mockUDPConn) WriteTo(b []byte, dest net.Addr) (int, error) {
	if m.closed {
		return 0, syscall.EBADF
	}

	m.out <- udpPacket{
		dest:    dest.(*net.UDPAddr),
		payload: b,
	}
	return len(b), nil
}

type server struct {
	in  chan udpPacket
	out chan udpPacket

	received []*dhcp4.Packet

	// Each received packet can have more than one response (in theory,
	// from different servers sending different Advertise, for example).
	responses [][]*dhcp4.Packet
}

func (s *server) serve(ctx context.Context) {
	go func() {
		for len(s.responses) > 0 {
			select {
			case udpPkt, ok := <-s.in:
				if !ok {
					break
				}

				// What did we get?
				var pkt dhcp4.Packet
				if err := (&pkt).UnmarshalBinary(udpPkt.payload); err != nil {
					panic(fmt.Sprintf("invalid dhcp6 packet %q: %v", udpPkt.payload, err))
				}
				s.received = append(s.received, &pkt)

				if len(s.responses) > 0 {
					resps := s.responses[0]
					// What should we send in response?
					for _, resp := range resps {
						bin, err := resp.MarshalBinary()
						if err != nil {
							panic(fmt.Sprintf("failed to serialize dhcp6 packet %v: %v", resp, err))
						}
						s.out <- udpPacket{
							source:  udpPkt.dest,
							payload: bin,
						}
					}
					s.responses = s.responses[1:]
				}

			case <-ctx.Done():
				break
			}
		}

		// We're done sending stuff.
		close(s.out)
	}()

}

func ComparePacket(got *dhcp4.Packet, want *dhcp4.Packet) error {
	aa, err := got.MarshalBinary()
	if err != nil {
		panic(err)
	}
	bb, err := want.MarshalBinary()
	if err != nil {
		panic(err)
	}
	if bytes.Compare(aa, bb) != 0 {
		return fmt.Errorf("packet got %v, want %v", got, want)
	}
	return nil
}

func pktsExpected(got []*dhcp4.Packet, want []*dhcp4.Packet) error {
	if len(got) != len(want) {
		return fmt.Errorf("got %d packets, want %d packets", len(got), len(want))
	}

	for i := range got {
		if err := ComparePacket(got[i], want[i]); err != nil {
			return err
		}
	}
	return nil
}

func serveAndClient(ctx context.Context, responses [][]*dhcp4.Packet) (*Client, *mockUDPConn) {
	// These are the client's channels.
	in := make(chan udpPacket, 100)
	out := make(chan udpPacket, 100)

	mockConn := &mockUDPConn{
		in:  in,
		out: out,
	}

	mc, err := New(nil, WithConn(mockConn), WithRetry(1), WithTimeout(time.Second))
	if err != nil {
		panic(err)
	}

	// Of course, for the server they are reversed.
	s := &server{
		in:        out,
		out:       in,
		responses: responses,
	}
	go s.serve(ctx)

	return mc, mockConn
}

func newPacket(op dhcp4.OpCode, xid [4]byte) *dhcp4.Packet {
	p := dhcp4.NewPacket(op)
	p.TransactionID = xid
	return p
}

func newPacketHType(op dhcp4.OpCode, xid [4]byte, htype uint8) *dhcp4.Packet {
	p := dhcp4.NewPacket(op)
	p.TransactionID = xid
	p.HType = htype
	return p
}

func TestSimpleSendAndRead(t *testing.T) {
	for _, tt := range []struct {
		desc   string
		send   *dhcp4.Packet
		server []*dhcp4.Packet

		// If want is nil, we assume server contains what is wanted.
		want    []*dhcp4.Packet
		wantErr error
	}{
		{
			desc: "two response packets",
			send: newPacket(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33}),
			server: []*dhcp4.Packet{
				newPacket(dhcp4.BootReply, [4]byte{0x33, 0x33, 0x33, 0x33}),
				newPacket(dhcp4.BootReply, [4]byte{0x33, 0x33, 0x33, 0x33}),
			},
		},
		{
			desc: "one response packet",
			send: newPacket(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33}),
			server: []*dhcp4.Packet{
				newPacket(dhcp4.BootReply, [4]byte{0x33, 0x33, 0x33, 0x33}),
			},
		},
		{
			desc: "one response packet, one invalid XID",
			send: newPacket(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33}),
			server: []*dhcp4.Packet{
				newPacket(dhcp4.BootReply, [4]byte{0x33, 0x33, 0x33, 0x33}),
				newPacket(dhcp4.BootReply, [4]byte{0x77, 0x33, 0x33, 0x33}),
			},
			want: []*dhcp4.Packet{
				newPacket(dhcp4.BootReply, [4]byte{0x33, 0x33, 0x33, 0x33}),
			},
		},
		{
			desc: "discard wrong XID",
			send: newPacket(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33}),
			server: []*dhcp4.Packet{
				newPacket(dhcp4.BootReply, [4]byte{0, 0, 0, 0}),
			},
			want:    []*dhcp4.Packet{}, // Explicitly empty.
			wantErr: context.DeadlineExceeded,
		},
		{
			desc:    "no response, timeout",
			send:    newPacket(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33}),
			wantErr: context.DeadlineExceeded,
		},
	} {
		// Both server and client only get 2 seconds.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		mc, _ := serveAndClient(ctx, [][]*dhcp4.Packet{tt.server})
		defer mc.conn.Close()

		wg, out, errCh := mc.SimpleSendAndRead(ctx, DefaultServers, tt.send)

		var rcvd []*dhcp4.Packet
		for packet := range out {
			rcvd = append(rcvd, packet.Packet)
		}

		if err, ok := <-errCh; ok && err.Err != tt.wantErr {
			t.Errorf("SimpleSendAndRead(%v): got %v, want %v", tt.send, err.Err, tt.wantErr)
		} else if !ok && tt.wantErr != nil {
			t.Errorf("didn't get error, want %v", tt.wantErr)
		}

		wg.Wait()
		want := tt.want
		if want == nil {
			want = tt.server
		}
		if err := pktsExpected(rcvd, want); err != nil {
			t.Errorf("got unexpected packets: %v", err)
		}
	}
}

func TestSimpleSendAndReadHandleCancel(t *testing.T) {
	pkt := newPacket(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33})

	responses := []*dhcp4.Packet{
		newPacketHType(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33}, 1),
		newPacketHType(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33}, 2),
		newPacketHType(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33}, 3),
		newPacketHType(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33}, 4),
	}

	// Both the server and client only get 2 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mc, udpConn := serveAndClient(ctx, [][]*dhcp4.Packet{responses})
	defer mc.conn.Close()

	wg, out, errCh := mc.SimpleSendAndRead(ctx, DefaultServers, pkt)

	var counter int
	for range out {
		counter++
		if counter == 2 {
			cancel()
		}
	}

	wg.Wait()
	if err, ok := <-errCh; ok {
		t.Errorf("got %v, want nil error", err)
	}

	// Make sure that two packets are still in the queue to be read.
	for packet := range udpConn.in {
		bin, err := responses[counter].MarshalBinary()
		if err != nil {
			panic(err)
		}
		if bytes.Compare(packet.payload, bin) != 0 {
			t.Errorf("SimpleSendAndRead read more packets than expected!")
		}
		counter++
	}
}

func TestSimpleSendAndReadDiscardGarbage(t *testing.T) {
	pkt := newPacket(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33})

	responses := []*dhcp4.Packet{
		newPacket(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33}),
	}

	// Both the server and client only get 2 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mc, udpConn := serveAndClient(ctx, [][]*dhcp4.Packet{responses})
	defer mc.conn.Close()

	udpConn.in <- udpPacket{
		payload: []byte{0x01}, // Too short for valid DHCPv4 packet.
	}

	wg, out, errCh := mc.SimpleSendAndRead(ctx, DefaultServers, pkt)

	var i int
	for recvd := range out {
		if err := ComparePacket(recvd.Packet, responses[i]); err != nil {
			t.Error(err)
		}
		i++
	}

	wg.Wait()
	if err, ok := <-errCh; ok {
		t.Errorf("SimpleSendAndRead(%v): got %v %v, want %v", pkt, ok, err, nil)
	}
	if i != len(responses) {
		t.Errorf("should have received %d valid packet, counter is %d", len(responses), i)
	}
}

func TestSimpleSendAndReadDiscardGarbageTimeout(t *testing.T) {
	pkt := newPacket(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33})

	// Both the server and client only get 2 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	mc, udpConn := serveAndClient(ctx, nil)
	defer mc.conn.Close()

	udpConn.in <- udpPacket{
		payload: []byte{0x01}, // Too short for valid DHCPv6 packet.
	}

	wg, out, errCh := mc.SimpleSendAndRead(ctx, DefaultServers, pkt)

	var counter int
	for range out {
		counter++
	}

	wg.Wait()
	if err, ok := <-errCh; !ok || err == nil || err.Err != context.DeadlineExceeded {
		t.Errorf("SimpleSendAndRead(%v): got %v %v, want %v", pkt, ok, err, context.DeadlineExceeded)
	}
	if counter != 0 {
		t.Errorf("should not have received a valid packet, counter is %d", counter)
	}
}

func TestMultipleSendAndReadOne(t *testing.T) {
	for _, tt := range []struct {
		desc    string
		send    []*dhcp4.Packet
		server  [][]*dhcp4.Packet
		wantErr []error
	}{
		{
			desc: "two requests, two responses",
			send: []*dhcp4.Packet{
				newPacket(dhcp4.BootRequest, [4]byte{0x33, 0x33, 0x33, 0x33}),
				newPacket(dhcp4.BootRequest, [4]byte{0x44, 0x44, 0x44, 0x44}),
			},
			server: [][]*dhcp4.Packet{
				[]*dhcp4.Packet{ // Response for first packet.
					newPacket(dhcp4.BootReply, [4]byte{0x33, 0x33, 0x33, 0x33}),
				},
				[]*dhcp4.Packet{ // Response for second packet.
					newPacket(dhcp4.BootReply, [4]byte{0x44, 0x44, 0x44, 0x44}),
				},
			},
			wantErr: []error{
				nil,
				nil,
			},
		},
	} {
		// Both server and client only get 2 seconds.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		mc, _ := serveAndClient(ctx, tt.server)
		defer mc.conn.Close()

		for i, send := range tt.send {
			rcvd, err := mc.SendAndReadOne(send)

			if wantErr := tt.wantErr[i]; err != wantErr {
				t.Errorf("SendAndReadOne(%v): got %v, want %v", send, err, wantErr)
			}
			if err := pktsExpected([]*dhcp4.Packet{rcvd}, tt.server[i]); err != nil {
				t.Errorf("got unexpected packets: %v", err)
			}
		}
	}
}
