package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/itchyny/gojq"
	"github.com/omnibuf/omnibuf-go"
	"github.com/omnibuf/omnibuf-go/wire"
)

type orderedField struct {
	Key   string
	Value any
}

type orderedObject struct {
	Fields []orderedField
}

// parseOrderedJSON parses JSON bytes into *orderedObject / []any / json.Number / string / bool / nil,
// preserving object key order and distinguishing integer vs floating-point literals via UseNumber().
func parseOrderedJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	val, err := readJSONValue(dec)
	if err != nil {
		return nil, err
	}
	if tok, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("unexpected trailing JSON token %v", tok)
	}
	return val, nil
}

func readJSONValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return readJSONToken(dec, tok)
}

func readJSONToken(dec *json.Decoder, tok json.Token) (any, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := &orderedObject{}
			indexByKey := make(map[string]int)
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("expected string object key, got %v", kt)
				}
				val, err := readJSONValue(dec)
				if err != nil {
					return nil, err
				}
				if idx, exists := indexByKey[key]; exists {
					obj.Fields[idx].Value = val
				} else {
					indexByKey[key] = len(obj.Fields)
					obj.Fields = append(obj.Fields, orderedField{Key: key, Value: val})
				}
			}
			if _, err := dec.Token(); err != nil { // consume '}'
				return nil, err
			}
			return obj, nil
		case '[':
			var arr []any
			for dec.More() {
				elem, err := readJSONValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, elem)
			}
			if _, err := dec.Token(); err != nil { // consume ']'
				return nil, err
			}
			return arr, nil
		default:
			return nil, fmt.Errorf("unexpected JSON delimiter %q", t)
		}
	default:
		return t, nil
	}
}

func parseExplicitFieldNumber(key string) (uint32, bool) {
	if !strings.HasPrefix(key, "field_") {
		return 0, false
	}
	numStr := key[len("field_"):]
	if numStr == "" || numStr[0] == '0' {
		return 0, false
	}
	u, err := strconv.ParseUint(numStr, 10, 32)
	if err != nil || u == 0 || u > 536870911 || (u >= 19000 && u <= 19999) {
		return 0, false
	}
	return uint32(u), true
}

func nextAvailableFieldNum(used map[uint32]bool, start uint32) uint32 {
	n := start
	for {
		if n == 0 {
			n = 1
		}
		if n >= 19000 && n <= 19999 {
			n = 20000
		}
		if !used[n] {
			used[n] = true
			return n
		}
		n++
	}
}

// encodeSchemalessJSON encodes a JSON object into OmniBuf wire bytes without requiring a .proto schema.
// Named fields are assigned sequential field numbers (or explicit numbers for "field_<N>" keys),
// and when includeField0 is true, Field #0 (SchemaMeta) is prepended so DecodeBlind recovers all names.
func encodeSchemalessJSON(jsonBytes []byte, includeField0 bool, deterministic bool) ([]byte, error) {
	rootVal, err := parseOrderedJSON(jsonBytes)
	if err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	rootObj, ok := rootVal.(*orderedObject)
	if !ok {
		return nil, errors.New("top-level JSON value must be a JSON object")
	}

	body, meta, err := encodeOrderedObject(rootObj, deterministic, nil)
	if err != nil {
		return nil, err
	}

	if includeField0 && meta != nil && (len(meta.Fields) > 0 || len(meta.Children) > 0) {
		var out []byte
		out = meta.AppendField0(out, true)
		out = append(out, body...)
		return out, nil
	}
	return body, nil
}

type fieldAssignment struct {
	keyToNum map[string]uint32
	meta     *omnibuf.SchemaMeta
}

func buildFieldAssignment(objs []*orderedObject, deterministic bool) *fieldAssignment {
	var keys []string
	seen := make(map[string]bool)
	for _, obj := range objs {
		if obj == nil {
			continue
		}
		for _, f := range obj.Fields {
			if !seen[f.Key] {
				seen[f.Key] = true
				keys = append(keys, f.Key)
			}
		}
	}
	if deterministic {
		sort.Strings(keys)
	}

	used := make(map[uint32]bool, len(keys))
	keyToNum := make(map[string]uint32, len(keys))
	meta := &omnibuf.SchemaMeta{
		Fields: make(map[uint32]string),
	}

	// First pass: reserve any explicit "field_<N>" numbers.
	for _, k := range keys {
		if fnum, ok := parseExplicitFieldNumber(k); ok && !used[fnum] {
			used[fnum] = true
			keyToNum[k] = fnum
		}
	}

	// Second pass: assign sequential field numbers to named keys.
	nextNum := uint32(1)
	for _, k := range keys {
		if _, ok := keyToNum[k]; ok {
			continue
		}
		fnum := nextAvailableFieldNum(used, nextNum)
		nextNum = fnum + 1
		keyToNum[k] = fnum
		meta.Fields[fnum] = k
	}
	if len(meta.Fields) == 0 {
		meta.Fields = nil
	}
	return &fieldAssignment{
		keyToNum: keyToNum,
		meta:     meta,
	}
}

func encodeOrderedObject(obj *orderedObject, deterministic bool, assign *fieldAssignment) ([]byte, *omnibuf.SchemaMeta, error) {
	if obj == nil {
		return nil, nil, nil
	}
	if assign == nil {
		assign = buildFieldAssignment([]*orderedObject{obj}, deterministic)
	}

	fields := obj.Fields
	if deterministic {
		fields = make([]orderedField, len(obj.Fields))
		copy(fields, obj.Fields)
		sort.Slice(fields, func(i, j int) bool {
			return assign.keyToNum[fields[i].Key] < assign.keyToNum[fields[j].Key]
		})
	}

	var dst []byte
	for _, f := range fields {
		fnum := assign.keyToNum[f.Key]
		var err error
		var childMeta *omnibuf.SchemaMeta
		dst, childMeta, err = appendSchemalessField(dst, fnum, f.Value, deterministic)
		if err != nil {
			return nil, nil, fmt.Errorf("field %q: %w", f.Key, err)
		}
		if childMeta != nil && (len(childMeta.Fields) > 0 || len(childMeta.Children) > 0) {
			if assign.meta.Children == nil {
				assign.meta.Children = make(map[uint32]*omnibuf.SchemaMeta)
			}
			assign.meta.Children[fnum] = childMeta
		}
	}
	return dst, assign.meta, nil
}

func appendTag(dst []byte, fnum uint32, wt uint8) []byte {
	return wire.AppendVarint(dst, (uint64(fnum)<<4)|uint64(wt))
}

func decodeBase64Literal(s string) ([]byte, bool) {
	if !strings.HasPrefix(s, "base64:") {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(s[len("base64:"):])
	if err != nil {
		return nil, false
	}
	return b, true
}

func classifyJSONNumber(n json.Number) (u64 uint64, i64 int64, f64 float64, kind uint8, err error) {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if u, errU := strconv.ParseUint(s, 10, 64); errU == nil {
			return u, 0, 0, wire.WireUint, nil
		}
		if i, errI := strconv.ParseInt(s, 10, 64); errI == nil {
			return 0, i, 0, wire.WireSint, nil
		}
	}
	f, errF := strconv.ParseFloat(s, 64)
	if errF != nil {
		return 0, 0, 0, 0, errF
	}
	return 0, 0, f, wire.WireFloat64, nil
}

func appendSchemalessField(dst []byte, fnum uint32, val any, deterministic bool) ([]byte, *omnibuf.SchemaMeta, error) {
	if val == nil {
		return dst, nil, nil
	}
	switch v := val.(type) {
	case bool:
		if v {
			dst = appendTag(dst, fnum, wire.WireBoolTrue)
		} else {
			dst = appendTag(dst, fnum, wire.WireBoolFalse)
		}
		return dst, nil, nil

	case json.Number:
		u64, i64, f64, kind, err := classifyJSONNumber(v)
		if err != nil {
			return nil, nil, err
		}
		switch kind {
		case wire.WireUint:
			dst = appendTag(dst, fnum, wire.WireUint)
			dst = wire.AppendVarint(dst, u64)
		case wire.WireSint:
			dst = appendTag(dst, fnum, wire.WireSint)
			dst = wire.AppendVarint(dst, wire.EncodeZigZag(i64))
		default:
			dst = appendTag(dst, fnum, wire.WireFloat64)
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], math.Float64bits(f64))
			dst = append(dst, b[:]...)
		}
		return dst, nil, nil

	case string:
		if rawBytes, ok := decodeBase64Literal(v); ok {
			dst = appendTag(dst, fnum, wire.WireBytes)
			dst = wire.AppendVarint(dst, uint64(len(rawBytes)))
			dst = append(dst, rawBytes...)
			return dst, nil, nil
		}
		dst = appendTag(dst, fnum, wire.WireString)
		dst = wire.AppendVarint(dst, uint64(len(v)))
		dst = append(dst, v...)
		return dst, nil, nil

	case *orderedObject:
		subBody, childMeta, err := encodeOrderedObject(v, deterministic, nil)
		if err != nil {
			return nil, nil, err
		}
		dst = appendTag(dst, fnum, wire.WireSubMsg)
		dst = wire.AppendVarint(dst, uint64(len(subBody)))
		dst = append(dst, subBody...)
		return dst, childMeta, nil

	case []any:
		return appendSchemalessArray(dst, fnum, v, deterministic)

	default:
		return nil, nil, fmt.Errorf("unsupported JSON value %T", val)
	}
}

func appendSchemalessArray(dst []byte, fnum uint32, rawElems []any, deterministic bool) ([]byte, *omnibuf.SchemaMeta, error) {
	var elems []any
	for _, e := range rawElems {
		if e != nil {
			elems = append(elems, e)
		}
	}
	if len(elems) == 0 {
		return dst, nil, nil
	}

	switch elems[0].(type) {
	case bool:
		var body []byte
		body = append(body, wire.WireBoolFalse)
		body = wire.AppendVarint(body, uint64(len(elems)))
		for _, e := range elems {
			b, ok := e.(bool)
			if !ok {
				return nil, nil, errors.New("mixed types in boolean array")
			}
			if b {
				body = append(body, 1)
			} else {
				body = append(body, 0)
			}
		}
		dst = appendTag(dst, fnum, wire.WireArray)
		dst = wire.AppendVarint(dst, uint64(len(body)))
		dst = append(dst, body...)
		return dst, nil, nil

	case string:
		strs := make([]string, len(elems))
		allBase64 := true
		decodedBytes := make([][]byte, len(elems))
		for i, e := range elems {
			s, ok := e.(string)
			if !ok {
				return nil, nil, errors.New("mixed types in string array")
			}
			strs[i] = s
			if allBase64 {
				if b, isB64 := decodeBase64Literal(s); isB64 {
					decodedBytes[i] = b
				} else {
					allBase64 = false
				}
			}
		}
		var body []byte
		if allBase64 {
			body = append(body, wire.WireBytes)
			body = wire.AppendVarint(body, uint64(len(decodedBytes)))
			for _, b := range decodedBytes {
				body = wire.AppendVarint(body, uint64(len(b)))
				body = append(body, b...)
			}
		} else {
			body = append(body, wire.WireString)
			body = wire.AppendVarint(body, uint64(len(strs)))
			for _, s := range strs {
				body = wire.AppendVarint(body, uint64(len(s)))
				body = append(body, s...)
			}
		}
		dst = appendTag(dst, fnum, wire.WireArray)
		dst = wire.AppendVarint(dst, uint64(len(body)))
		dst = append(dst, body...)
		return dst, nil, nil

	case json.Number:
		allUint := true
		allInt := true
		uVals := make([]uint64, len(elems))
		iVals := make([]int64, len(elems))
		fVals := make([]float64, len(elems))
		for i, e := range elems {
			num, ok := e.(json.Number)
			if !ok {
				return nil, nil, errors.New("mixed types in numeric array")
			}
			u64, i64, f64, kind, err := classifyJSONNumber(num)
			if err != nil {
				return nil, nil, err
			}
			switch kind {
			case wire.WireUint:
				uVals[i] = u64
				if u64 <= math.MaxInt64 {
					iVals[i] = int64(u64)
				} else {
					allInt = false
				}
				fVals[i] = float64(u64)
			case wire.WireSint:
				allUint = false
				iVals[i] = i64
				fVals[i] = float64(i64)
			default:
				allUint = false
				allInt = false
				fVals[i] = f64
			}
		}
		var body []byte
		switch {
		case allUint:
			body = append(body, wire.WireUint)
			body = wire.AppendVarint(body, uint64(len(uVals)))
			for _, u := range uVals {
				body = wire.AppendVarint(body, u)
			}
		case allInt:
			body = append(body, wire.WireSint)
			body = wire.AppendVarint(body, uint64(len(iVals)))
			for _, iv := range iVals {
				body = wire.AppendVarint(body, wire.EncodeZigZag(iv))
			}
		default:
			body = append(body, wire.WireFloat64)
			body = wire.AppendVarint(body, uint64(len(fVals)))
			for _, fv := range fVals {
				var b [8]byte
				binary.LittleEndian.PutUint64(b[:], math.Float64bits(fv))
				body = append(body, b[:]...)
			}
		}
		dst = appendTag(dst, fnum, wire.WireArray)
		dst = wire.AppendVarint(dst, uint64(len(body)))
		dst = append(dst, body...)
		return dst, nil, nil

	case *orderedObject:
		objs := make([]*orderedObject, len(elems))
		for i, e := range elems {
			o, ok := e.(*orderedObject)
			if !ok {
				return nil, nil, errors.New("mixed types in object array")
			}
			objs[i] = o
		}
		sharedAssign := buildFieldAssignment(objs, deterministic)
		var body []byte
		body = append(body, wire.WireSubMsg)
		body = wire.AppendVarint(body, uint64(len(objs)))
		for _, o := range objs {
			elemBytes, _, err := encodeOrderedObject(o, deterministic, sharedAssign)
			if err != nil {
				return nil, nil, err
			}
			body = wire.AppendVarint(body, uint64(len(elemBytes)))
			body = append(body, elemBytes...)
		}
		dst = appendTag(dst, fnum, wire.WireArray)
		dst = wire.AppendVarint(dst, uint64(len(body)))
		dst = append(dst, body...)
		return dst, sharedAssign.meta, nil

	default:
		return nil, nil, fmt.Errorf("unsupported array element type %T", elems[0])
	}
}

// applyJQFilter runs a jq filter expression against a JSON document and returns formatted output bytes.
func applyJQFilter(jsonBytes []byte, filterExpr string, compact, rawOutput bool) ([]byte, error) {
	if filterExpr == "" {
		filterExpr = "."
	}
	query, err := gojq.Parse(filterExpr)
	if err != nil {
		return nil, fmt.Errorf("invalid jq filter %q: %w", filterExpr, err)
	}
	var input any
	if err := json.Unmarshal(jsonBytes, &input); err != nil {
		return nil, fmt.Errorf("parse JSON for jq filter: %w", err)
	}

	var out bytes.Buffer
	iter := query.Run(input)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, isErr := v.(error); isErr {
			var haltErr *gojq.HaltError
			if errors.As(err, &haltErr) && haltErr.Value() == nil {
				break
			}
			return nil, fmt.Errorf("jq filter error: %w", err)
		}
		if rawOutput {
			if s, isStr := v.(string); isStr {
				out.WriteString(s)
				out.WriteByte('\n')
				continue
			}
		}
		var b []byte
		if compact {
			b, err = json.Marshal(v)
		} else {
			b, err = json.MarshalIndent(v, "", "  ")
		}
		if err != nil {
			return nil, fmt.Errorf("marshal jq result: %w", err)
		}
		out.Write(b)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}
