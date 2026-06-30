package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

const pingTimeout = 4 * time.Second

// pingMinecraft performs a Minecraft Server List Ping (the same handshake the
// multiplayer menu uses) against addr and returns the ping/pong round-trip in
// milliseconds. addr is our local cloudflared listener, so the request travels
// the full tunnel to the real server - i.e. the true latency the player feels.
func pingMinecraft(addr string) (int, error) {
	conn, err := net.DialTimeout("tcp", addr, pingTimeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(pingTimeout))

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	var port uint16 = 25565
	fmt.Sscanf(portStr, "%d", &port)

	r := bufio.NewReader(conn)

	// Handshake: protocol version (any works for a status ping), address, port,
	// next state = 1 (status).
	hs := newPacket(0x00)
	hs.varInt(47)
	hs.str(host)
	hs.uShort(port)
	hs.varInt(1)
	if err := hs.send(conn); err != nil {
		return 0, err
	}

	// Status request, then read and discard the status JSON response.
	if err := newPacket(0x00).send(conn); err != nil {
		return 0, err
	}
	if err := discardPacket(r); err != nil {
		return 0, err
	}

	// Ping with a payload; the server echoes it back as Pong. Time that.
	start := time.Now()
	pp := newPacket(0x01)
	pp.long(start.UnixNano())
	if err := pp.send(conn); err != nil {
		return 0, err
	}
	if err := discardPacket(r); err != nil {
		return 0, err
	}
	return int(time.Since(start).Milliseconds()), nil
}

// packet builds a Minecraft protocol packet body (packet id + fields); send()
// prefixes it with its VarInt length.
type packet struct{ buf bytes.Buffer }

func newPacket(id int32) *packet {
	p := &packet{}
	writeVarInt(&p.buf, id)
	return p
}
func (p *packet) varInt(v int32)  { writeVarInt(&p.buf, v) }
func (p *packet) uShort(v uint16) { _ = binary.Write(&p.buf, binary.BigEndian, v) }
func (p *packet) long(v int64)    { _ = binary.Write(&p.buf, binary.BigEndian, v) }
func (p *packet) str(s string) {
	writeVarInt(&p.buf, int32(len(s)))
	p.buf.WriteString(s)
}
func (p *packet) send(conn net.Conn) error {
	var out bytes.Buffer
	writeVarInt(&out, int32(p.buf.Len()))
	out.Write(p.buf.Bytes())
	_, err := conn.Write(out.Bytes())
	return err
}

func writeVarInt(b *bytes.Buffer, v int32) {
	uv := uint32(v)
	for {
		c := byte(uv & 0x7F)
		uv >>= 7
		if uv != 0 {
			c |= 0x80
		}
		b.WriteByte(c)
		if uv == 0 {
			return
		}
	}
}

func readVarInt(r *bufio.Reader) (int32, error) {
	var result int32
	var shift uint
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		result |= int32(b&0x7F) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
		if shift >= 35 {
			return 0, fmt.Errorf("varint too long")
		}
	}
	return result, nil
}

// discardPacket reads one length-prefixed packet and throws away its body.
func discardPacket(r *bufio.Reader) error {
	length, err := readVarInt(r)
	if err != nil {
		return err
	}
	if length < 0 || length > 1<<20 {
		return fmt.Errorf("bad packet length %d", length)
	}
	_, err = io.CopyN(io.Discard, r, int64(length))
	return err
}
