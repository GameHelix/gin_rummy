package hyperliquid

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
)

// An action is hashed as MessagePack with its keys in a fixed order, so
// actions are built from omap, not map, and marshal to JSON in that same
// order.
type kv struct {
	k string
	v any
}

type omap []kv

func (m omap) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range m {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(e.k)
		v, err := json.Marshal(e.v)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// pack encodes v as the reference MessagePack packer does: the smallest
// integer form, strings as str, bools, nil, arrays and maps.
func pack(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := packTo(&b, v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func packTo(b *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteByte(0xc0)
	case bool:
		if x {
			b.WriteByte(0xc3)
		} else {
			b.WriteByte(0xc2)
		}
	case int:
		packInt(b, int64(x))
	case int64:
		packInt(b, x)
	case uint64:
		packUint(b, x)
	case string:
		packHead(b, len(x), 0xa0, 31, 0xd9, 0xda, 0xdb)
		b.WriteString(x)
	case omap:
		packHead(b, len(x), 0x80, 15, 0, 0xde, 0xdf)
		for _, e := range x {
			if err := packTo(b, e.k); err != nil {
				return err
			}
			if err := packTo(b, e.v); err != nil {
				return err
			}
		}
	case []any:
		packHead(b, len(x), 0x90, 15, 0, 0xdc, 0xdd)
		for _, e := range x {
			if err := packTo(b, e); err != nil {
				return err
			}
		}
	case []omap:
		packHead(b, len(x), 0x90, 15, 0, 0xdc, 0xdd)
		for _, e := range x {
			if err := packTo(b, e); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("msgpack: unsupported %T", v)
	}
	return nil
}

// packHead writes the length prefix: the fixed form up to fixMax, then the
// 8-bit form (when it exists), 16-bit and 32-bit forms.
func packHead(b *bytes.Buffer, n int, fix byte, fixMax int, c8, c16, c32 byte) {
	switch {
	case n <= fixMax:
		b.WriteByte(fix | byte(n))
	case c8 != 0 && n <= math.MaxUint8:
		b.WriteByte(c8)
		b.WriteByte(byte(n))
	case n <= math.MaxUint16:
		b.WriteByte(c16)
		binary.Write(b, binary.BigEndian, uint16(n))
	default:
		b.WriteByte(c32)
		binary.Write(b, binary.BigEndian, uint32(n))
	}
}

func packInt(b *bytes.Buffer, x int64) {
	if x >= 0 {
		packUint(b, uint64(x))
		return
	}
	switch {
	case x >= -32:
		b.WriteByte(byte(x))
	case x >= math.MinInt8:
		b.WriteByte(0xd0)
		b.WriteByte(byte(int8(x)))
	case x >= math.MinInt16:
		b.WriteByte(0xd1)
		binary.Write(b, binary.BigEndian, int16(x))
	case x >= math.MinInt32:
		b.WriteByte(0xd2)
		binary.Write(b, binary.BigEndian, int32(x))
	default:
		b.WriteByte(0xd3)
		binary.Write(b, binary.BigEndian, x)
	}
}

func packUint(b *bytes.Buffer, x uint64) {
	switch {
	case x < 0x80:
		b.WriteByte(byte(x))
	case x <= math.MaxUint8:
		b.WriteByte(0xcc)
		b.WriteByte(byte(x))
	case x <= math.MaxUint16:
		b.WriteByte(0xcd)
		binary.Write(b, binary.BigEndian, uint16(x))
	case x <= math.MaxUint32:
		b.WriteByte(0xce)
		binary.Write(b, binary.BigEndian, uint32(x))
	default:
		b.WriteByte(0xcf)
		binary.Write(b, binary.BigEndian, x)
	}
}
