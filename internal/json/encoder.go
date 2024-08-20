package json

import (
	"bytes"
	"reflect"
	"strconv"
	"sync"
)

type (
	Encoder struct {
		buffer           bytes.Buffer
		referenceTracker map[uintptr]bool
	}
)

var (
	_cache sync.Map
)

func NewEncoder() *Encoder {
	e := new(Encoder)
	e.referenceTracker = make(map[uintptr]bool)
	return e
}

func Marshal(v any) []byte {
	vv := reflect.ValueOf(v)
	e := NewEncoder()
	e.Encode(vv)
	return e.Bytes()
}

func (e *Encoder) Bytes() []byte {
	return e.buffer.Bytes()
}

func (e *Encoder) Encode(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		{
			e.encodePtr(v)
		}
	case reflect.Array, reflect.Slice:
		{
			e.encodeList(v)
		}
	case reflect.Map:
		{
			e.encodeMap(v)
		}
	case reflect.Struct:
		{
			e.encodeStruct(v)
		}
	default:
		{
			e.encodeValue(v)
		}
	}
}

func (e *Encoder) encodePtr(v reflect.Value) {
	if v.IsZero() {
		e.buffer.WriteString("null")
		return
	}
	ptr := v.Pointer()
	if _, ok := e.referenceTracker[ptr]; ok {
		e.buffer.WriteString("null")
		return
	}
	e.referenceTracker[ptr] = true
	e.Encode(v.Elem())
}

func (e *Encoder) encodeList(v reflect.Value) {
	e.buffer.WriteByte('[')
	l := v.Len()
	for i := 0; i < l; i++ {
		if i > 0 {
			e.buffer.WriteByte(',')
		}
		e.Encode(v.Index(i))
	}
	e.buffer.WriteByte(']')
}

func (e *Encoder) encodeMap(v reflect.Value) {
	e.buffer.WriteByte('{')
	l := v.Len()
	ks := v.MapKeys()
	for i := 0; i < l; i++ {
		if i > 0 {
			e.buffer.WriteByte(',')
		}
		key := ks[i]
		e.buffer.WriteByte('"')
		e.encodeValue(key)
		e.buffer.WriteByte('"')
		e.buffer.WriteByte(':')
		e.Encode(v.MapIndex(key))
	}
	e.buffer.WriteByte('}')
}

func (e *Encoder) encodeStruct(v reflect.Value) {
	fields := getType(v)
	e.buffer.WriteByte('{')
	l := v.NumField()
	for i := 0; i < l; i++ {
		n := fields[i]
		if i > 0 {
			e.buffer.WriteByte(',')
		}
		e.buffer.WriteByte('"')
		e.buffer.Write(n)
		e.buffer.WriteByte('"')
		e.buffer.WriteByte(':')
		e.Encode(v.Field(i))
	}
	e.buffer.WriteByte('}')
}

func (e *Encoder) encodeValue(v reflect.Value) {
	b := e.buffer.AvailableBuffer()
	switch v.Kind() {
	case reflect.Bool:
		b = strconv.AppendBool(b, v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b = strconv.AppendInt(b, v.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b = strconv.AppendUint(b, v.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		b = strconv.AppendFloat(b, v.Float(), 'g', -1, 64)
	case reflect.Complex128, reflect.Complex64:
		b = append(b, []byte(strconv.FormatComplex(v.Complex(), 'g', -1, 64))...)
	default:
		{
			b = strconv.AppendQuote(b, v.String())
		}
	}
	e.buffer.Write(b)
}

func getType(v reflect.Value) map[int][]byte {
	vtr := v.Type()
	vt, ok := _cache.Load(vtr)
	if !ok {
		l := vtr.NumField()
		m := make(map[int][]byte)
		for i := 0; i < l; i++ {
			f := vtr.Field(i)
			if f.IsExported() {
				name := f.Tag.Get("json")
				if len(name) == 0 {
					name = f.Name
				}
				m[i] = []byte(name)
			}
		}
		_cache.Store(vtr, m)
		vt = m
	}
	return vt.(map[int][]byte)
}
