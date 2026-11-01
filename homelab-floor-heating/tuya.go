package main

import (
	"bytes"
	"crypto/aes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"strconv"
	"time"
)

// Tuya local protocol 3.3 framing. A frame is:
// prefix, seq, cmd, length (all uint32) | payload | crc32 | suffix,
// where length counts the payload, the CRC and the suffix.
const (
	framePrefix uint32 = 0x000055AA
	frameSuffix uint32 = 0x0000AA55
	cmdDPQuery  uint32 = 0x0A
	devicePort         = "6668"
	maxFrame           = 4096
)

type frame struct {
	seq, cmd uint32
	payload  []byte
}

// query reads a device's data points over its local TCP port. DP_QUERY is the
// only command this app knows, so it has no way to change a thermostat.
func query(ip, id, key string, timeout time.Duration) (map[string]any, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, devicePort), timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	return roundTrip(conn, id, key)
}

func roundTrip(conn net.Conn, id, key string) (map[string]any, error) {
	req, err := encodeQuery(id, key, time.Now())
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(req); err != nil {
		return nil, err
	}
	for {
		f, err := readFrame(conn)
		if errors.Is(err, io.EOF) {
			return nil, errors.New("device closed the connection: wrong local key, or busy with another client")
		}
		if err != nil {
			return nil, err
		}
		if f.cmd != cmdDPQuery {
			continue // unsolicited status push: partial, skip it
		}
		dps, err := decodeReply(f.payload, key)
		if err != nil {
			return nil, err
		}
		if dps != nil {
			return dps, nil
		}
		// Empty acknowledgement: the data follows in the next frame.
	}
}

func encodeQuery(id, key string, now time.Time) ([]byte, error) {
	body, err := json.Marshal(map[string]string{
		"gwId": id, "devId": id, "uid": id, "t": strconv.FormatInt(now.Unix(), 10),
	})
	if err != nil {
		return nil, err
	}
	enc, err := encrypt(body, key)
	if err != nil {
		return nil, err
	}
	return encodeFrame(frame{seq: 1, cmd: cmdDPQuery, payload: enc}), nil
}

func encodeFrame(f frame) []byte {
	var b bytes.Buffer
	for _, v := range []uint32{framePrefix, f.seq, f.cmd, uint32(len(f.payload) + 8)} {
		binary.Write(&b, binary.BigEndian, v)
	}
	b.Write(f.payload)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(b.Bytes()))
	binary.Write(&b, binary.BigEndian, frameSuffix)
	return b.Bytes()
}

func readFrame(r io.Reader) (frame, error) {
	head := make([]byte, 16)
	if _, err := io.ReadFull(r, head); err != nil {
		return frame{}, err
	}
	if binary.BigEndian.Uint32(head) != framePrefix {
		return frame{}, errors.New("bad frame prefix")
	}
	length := binary.BigEndian.Uint32(head[12:])
	if length < 8 || length > maxFrame {
		return frame{}, fmt.Errorf("bad frame length %d", length)
	}
	rest := make([]byte, length)
	if _, err := io.ReadFull(r, rest); err != nil {
		return frame{}, err
	}
	n := len(rest) - 8
	if binary.BigEndian.Uint32(rest[n+4:]) != frameSuffix {
		return frame{}, errors.New("bad frame suffix")
	}
	sum := crc32.ChecksumIEEE(append(head[:16:16], rest[:n]...))
	if binary.BigEndian.Uint32(rest[n:]) != sum {
		return frame{}, errors.New("bad frame CRC")
	}
	return frame{
		seq:     binary.BigEndian.Uint32(head[4:]),
		cmd:     binary.BigEndian.Uint32(head[8:]),
		payload: rest[:n],
	}, nil
}

// decodeReply returns the data points of a reply, or nil for an empty one.
func decodeReply(payload []byte, key string) (map[string]any, error) {
	// A device reply starts with a return code; real payloads never start with three zero bytes.
	if len(payload) >= 4 && binary.BigEndian.Uint32(payload)&0xFFFFFF00 == 0 {
		if code := binary.BigEndian.Uint32(payload); code != 0 {
			return nil, fmt.Errorf("device returned code %d", code)
		}
		payload = payload[4:]
	}
	if bytes.HasPrefix(payload, []byte("3.3")) {
		if len(payload) < 15 {
			return nil, errors.New("truncated version header")
		}
		payload = payload[15:]
	}
	if len(payload) == 0 {
		return nil, nil
	}
	plain, err := decrypt(payload, key)
	if err != nil {
		return nil, err
	}
	var reply struct {
		DPS map[string]any `json:"dps"`
	}
	if err := json.Unmarshal(plain, &reply); err != nil || reply.DPS == nil {
		return nil, fmt.Errorf("unexpected reply: %.80q", plain)
	}
	return reply.DPS, nil
}

// encrypt is AES-128-ECB with PKCS#7 padding, as protocol 3.3 requires.
func encrypt(plain []byte, key string) ([]byte, error) {
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	out := append(bytes.Clone(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	for i := 0; i < len(out); i += aes.BlockSize {
		block.Encrypt(out[i:], out[i:])
	}
	return out, nil
}

func decrypt(enc []byte, key string) ([]byte, error) {
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil, err
	}
	if len(enc)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("unencrypted reply: %.80q", enc)
	}
	out := make([]byte, len(enc))
	for i := 0; i < len(enc); i += aes.BlockSize {
		block.Decrypt(out[i:], enc[i:])
	}
	pad := int(out[len(out)-1])
	if pad == 0 || pad > aes.BlockSize || !bytes.HasSuffix(out, bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("cannot decrypt reply: wrong local key?")
	}
	return out[:len(out)-pad], nil
}
