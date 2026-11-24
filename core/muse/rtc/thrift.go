// The small subset of the Thrift Compact protocol that the JOIN needs. This is a modified port of
// the _Writer and _Reader classes in linux/src/musegadget/rtc/join.py from Meta's Muse Gadget
// SDK, Copyright (c) Meta Platforms, Inc. and affiliates, licensed under the Apache License,
// Version 2.0 (see ../LICENSE).

package rtc

import (
	"errors"
	"fmt"
)

// Thrift Compact field type codes.
const (
	tBoolTrue  = 1
	tBoolFalse = 2
	tByte      = 3
	tI16       = 4
	tI32       = 5
	tI64       = 6
	tDouble    = 7
	tBinary    = 8
	tList      = 9
	tSet       = 10
	tMap       = 11
	tStruct    = 12
)

// thriftWriter appends Compact-encoded fields to out. It tracks the last field id of each open
// struct, because field headers carry the delta from the previous id.
type thriftWriter struct {
	out  []byte
	last []int
}

func newThriftWriter() *thriftWriter { return &thriftWriter{last: []int{0}} }

func (w *thriftWriter) varint(n uint64) {
	for n >= 0x80 {
		w.out = append(w.out, byte(n)|0x80)
		n >>= 7
	}
	w.out = append(w.out, byte(n))
}

func (w *thriftWriter) field(id, kind int) {
	top := len(w.last) - 1
	delta := id - w.last[top]
	if delta > 0 && delta <= 15 {
		w.out = append(w.out, byte(delta<<4|kind))
	} else {
		w.out = append(w.out, byte(kind))
		w.varint(uint64(int64(id)<<1 ^ int64(id)>>63))
	}
	w.last[top] = id
}

func (w *thriftWriter) i32(id int, v int32) {
	w.field(id, tI32)
	w.varint(uint64(uint32(v<<1 ^ v>>31)))
}

func (w *thriftWriter) boolean(id int, v bool) {
	if v {
		w.field(id, tBoolTrue)
	} else {
		w.field(id, tBoolFalse)
	}
}

func (w *thriftWriter) binary(id int, v []byte) {
	w.field(id, tBinary)
	w.varint(uint64(len(v)))
	w.out = append(w.out, v...)
}

func (w *thriftWriter) str(id int, v string) { w.binary(id, []byte(v)) }

// begin opens a nested struct as field id; end closes it.
func (w *thriftWriter) begin(id int) {
	w.field(id, tStruct)
	w.last = append(w.last, 0)
}

func (w *thriftWriter) end() {
	w.out = append(w.out, 0)
	w.last = w.last[:len(w.last)-1]
}

// stop ends a top-level struct and starts the next one from field id 0.
func (w *thriftWriter) stop() {
	w.out = append(w.out, 0)
	w.last = []int{0}
}

// Limits for decoding an untrusted JOIN response.
const (
	maxResponseBytes = 16 * 1024
	maxThriftDepth   = 8
)

var errTruncated = errors.New("rtc: truncated thrift message")

// thriftStruct is a decoded struct: field id to value. Values are bool, int64, []byte, nil (for
// a double, which the JOIN never uses), []any (list or set), []thriftPair (map) or thriftStruct.
type thriftStruct map[int]any

// thriftPair is one map entry. Maps decode to a slice because []byte keys are not comparable.
type thriftPair struct{ Key, Value any }

// thriftReader decodes one small Compact message; malformed input is an error, never a panic.
type thriftReader struct {
	data  []byte
	pos   int
	depth int
}

func (r *thriftReader) byte() (byte, error) {
	if r.pos >= len(r.data) {
		return 0, errTruncated
	}
	r.pos++
	return r.data[r.pos-1], nil
}

func (r *thriftReader) take(n uint64) ([]byte, error) {
	if n > uint64(len(r.data)-r.pos) {
		return nil, errTruncated
	}
	start := r.pos
	r.pos += int(n)
	return r.data[start:r.pos], nil
}

func (r *thriftReader) varint() (uint64, error) {
	var result uint64
	for shift := 0; shift < 64; shift += 7 {
		b, err := r.byte()
		if err != nil {
			return 0, err
		}
		result |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			return result, nil
		}
	}
	return 0, errors.New("rtc: thrift varint too long")
}

func (r *thriftReader) zigzag() (int64, error) {
	raw, err := r.varint()
	return int64(raw>>1) ^ -int64(raw&1), err
}

// count bounds a container's element count: every element takes at least one byte.
func (r *thriftReader) count(n uint64) (int, error) {
	if n > uint64(len(r.data)-r.pos) {
		return 0, errors.New("rtc: thrift container larger than its message")
	}
	return int(n), nil
}

func (r *thriftReader) enter() error {
	r.depth++
	if r.depth > maxThriftDepth {
		return errors.New("rtc: thrift message nested too deeply")
	}
	return nil
}

func (r *thriftReader) readStruct() (thriftStruct, error) {
	if err := r.enter(); err != nil {
		return nil, err
	}
	fields := thriftStruct{}
	last := 0
	for {
		header, err := r.byte()
		if err != nil {
			return nil, err
		}
		if header == 0 {
			r.depth--
			return fields, nil
		}
		delta, kind := int(header>>4), int(header&0x0F)
		id := last + delta
		if delta == 0 {
			raw, err := r.zigzag()
			if err != nil {
				return nil, err
			}
			id = int(raw)
		}
		last = id
		if fields[id], err = r.value(kind, false); err != nil {
			return nil, err
		}
	}
}

func (r *thriftReader) value(kind int, inContainer bool) (any, error) {
	switch kind {
	case tBoolTrue, tBoolFalse:
		// A struct field's bool lives in its type; a container element's is a byte.
		if !inContainer {
			return kind == tBoolTrue, nil
		}
		b, err := r.byte()
		return b == 1, err
	case tByte:
		b, err := r.byte()
		return int64(b), err
	case tI16, tI32, tI64:
		return r.zigzag()
	case tDouble:
		_, err := r.take(8)
		return nil, err
	case tBinary:
		n, err := r.varint()
		if err != nil {
			return nil, err
		}
		return r.take(n)
	case tList, tSet:
		return r.list()
	case tMap:
		return r.mapValue()
	case tStruct:
		return r.readStruct()
	}
	return nil, fmt.Errorf("rtc: unsupported thrift compact type %d", kind)
}

func (r *thriftReader) list() ([]any, error) {
	header, err := r.byte()
	if err != nil {
		return nil, err
	}
	n, elem := uint64(header>>4), int(header&0x0F)
	if n == 15 {
		if n, err = r.varint(); err != nil {
			return nil, err
		}
	}
	if err := r.enter(); err != nil {
		return nil, err
	}
	count, err := r.count(n)
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, count)
	for range count {
		v, err := r.value(elem, true)
		if err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	r.depth--
	return items, nil
}

func (r *thriftReader) mapValue() ([]thriftPair, error) {
	n, err := r.varint()
	if err != nil {
		return nil, err
	}
	count, err := r.count(n)
	if err != nil || count == 0 {
		return nil, err
	}
	kinds, err := r.byte()
	if err != nil {
		return nil, err
	}
	if err := r.enter(); err != nil {
		return nil, err
	}
	items := make([]thriftPair, 0, count)
	for range count {
		k, err := r.value(int(kinds>>4), true)
		if err != nil {
			return nil, err
		}
		v, err := r.value(int(kinds&0x0F), true)
		if err != nil {
			return nil, err
		}
		items = append(items, thriftPair{k, v})
	}
	r.depth--
	return items, nil
}
