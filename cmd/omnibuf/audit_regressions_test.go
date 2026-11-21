package main

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	obwire "github.com/omnibuf/omnibuf-go/wire"
)

// TestAuditGO5a_SchemalessNestedSubMessageArrayFieldCollision isolates GO-5a:
// Schemaless JSON encode + decode of `{"items": [{"sub":{"a":1}}, {"sub":{"b":2}}]}`
// assigns field #1 independently inside each element's `sub` object and overwrites
// `Children[1]` in SchemaMeta, silently renaming element 0's `"a"` to `"b"`.
func TestAuditGO5a_SchemalessNestedSubMessageArrayFieldCollision(t *testing.T) {
	inputJSON := `{"items":[{"sub":{"a":1}},{"sub":{"b":2}}]}`

	var wireOut, stderr bytes.Buffer
	if err := run([]string{"encode"}, strings.NewReader(inputJSON), &wireOut, &stderr); err != nil {
		t.Fatalf("schemaless encode failed: %v (stderr: %s)", err, stderr.String())
	}

	var decOut bytes.Buffer
	if err := run([]string{"decode"}, bytes.NewReader(wireOut.Bytes()), &decOut, &stderr); err != nil {
		t.Fatalf("schemaless decode failed: %v (stderr: %s)", err, stderr.String())
	}

	var got map[string]any
	if err := json.Unmarshal(decOut.Bytes(), &got); err != nil {
		t.Fatalf("json.Unmarshal decoded output failed: %v\noutput: %s", err, decOut.String())
	}

	want := map[string]any{
		"items": []any{
			map[string]any{"sub": map[string]any{"a": float64(1)}},
			map[string]any{"sub": map[string]any{"b": float64(2)}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schemaless round-trip corrupted nested keys:\n got: %s\nwant: %v", strings.TrimSpace(decOut.String()), want)
	}
}

// TestAuditGO5b_DecodeAndReencodePreservesWireBytes isolates GO-5b:
// Decoding a packet containing WireBytes to JSON emits raw base64 without the
// `"base64:"` prefix expected by schemaless encode, so re-encoding that JSON
// via omnibuf-cli silently converts WireBytes into WireString.
func TestAuditGO5b_DecodeAndReencodePreservesWireBytes(t *testing.T) {
	wantPayload := []byte{0xde, 0xad, 0xbe, 0xef}
	inputJSON := `{"payload":"base64:3q2+7w=="}`

	var origWire, stderr bytes.Buffer
	if err := run([]string{"encode"}, strings.NewReader(inputJSON), &origWire, &stderr); err != nil {
		t.Fatalf("initial encode failed: %v (stderr: %s)", err, stderr.String())
	}
	if b, ok := obwire.ReadBytes(origWire.Bytes(), 1); !ok || !bytes.Equal(b, wantPayload) {
		t.Fatalf("precondition failed: initial packet field #1 ReadBytes = (%x, %v), want (%x, true)", b, ok, wantPayload)
	}

	var jsonBuf bytes.Buffer
	if err := run([]string{"decode"}, bytes.NewReader(origWire.Bytes()), &jsonBuf, &stderr); err != nil {
		t.Fatalf("decode failed: %v (stderr: %s)", err, stderr.String())
	}

	var reencWire bytes.Buffer
	if err := run([]string{"encode"}, bytes.NewReader(jsonBuf.Bytes()), &reencWire, &stderr); err != nil {
		t.Fatalf("re-encode failed: %v (stderr: %s)", err, stderr.String())
	}

	gotBytes, ok := obwire.ReadBytes(reencWire.Bytes(), 1)
	if !ok {
		gotStr, strOK := obwire.ReadString(reencWire.Bytes(), 1)
		t.Fatalf("re-encoded field #1 lost WireBytes wire type (became WireString=%v %q); decoded JSON was: %s", strOK, gotStr, strings.TrimSpace(jsonBuf.String()))
	}
	if !bytes.Equal(gotBytes, wantPayload) {
		t.Fatalf("re-encoded WireBytes payload = %x, want %x", gotBytes, wantPayload)
	}
}

// TestAuditGO5c_DecodeNonFiniteFloats isolates GO-5c:
// Blind-decoding a packet containing NaN or +Inf in omnibuf-cli fails inside
// json.MarshalIndent because sanitizeForJSON leaves non-finite floats untouched.
func TestAuditGO5c_DecodeNonFiniteFloats(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  float64
	}{
		{name: "nan", val: math.NaN()},
		{name: "pos_inf", val: math.Inf(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enc := obwire.NewEncoder(32)
			enc.WriteFloat64(1, tc.val)
			pkt := append([]byte(nil), enc.Finish()...)

			var stdout, stderr bytes.Buffer
			if err := run([]string{"decode"}, bytes.NewReader(pkt), &stdout, &stderr); err != nil {
				t.Fatalf("omnibuf decode failed on %s packet: %v (stderr: %s)", tc.name, err, stderr.String())
			}
			if !json.Valid(stdout.Bytes()) {
				t.Fatalf("omnibuf decode emitted invalid JSON for %s: %s", tc.name, stdout.String())
			}
		})
	}
}
