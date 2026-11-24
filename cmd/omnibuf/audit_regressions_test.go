package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
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

func TestJQFilterPreserves64BitIntegers(t *testing.T) {
	enc := obwire.NewEncoder(64)
	enc.WriteUint(1, 9007199254740993)     // 2^53 + 1
	enc.WriteUint(2, 18446744073709551615) // math.MaxUint64
	enc.WriteInt(3, -9223372036854775808)  // math.MinInt64
	enc.WriteFloat64(4, 3.14159)
	pkt := append([]byte(nil), enc.Finish()...)

	for _, tc := range []struct {
		name   string
		args   []string
		expect string
	}{
		{name: "uint_above_2_53", args: []string{"decode", "-q", ".field_1"}, expect: "9007199254740993"},
		{name: "max_uint64", args: []string{"decode", "-q", ".field_2"}, expect: "18446744073709551615"},
		{name: "min_int64", args: []string{"decode", "-q", ".field_3"}, expect: "-9223372036854775808"},
		{name: "raw_tostring_max_uint64", args: []string{"decode", "-r", "-q", ".field_2 | tostring"}, expect: "18446744073709551615"},
		{name: "float_preserved", args: []string{"decode", "-q", ".field_4"}, expect: "3.14159"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(tc.args, bytes.NewReader(pkt), &stdout, &stderr); err != nil {
				t.Fatalf("run(%v) failed: %v (stderr: %s)", tc.args, err, stderr.String())
			}
			got := strings.TrimSpace(stdout.String())
			if got != tc.expect {
				t.Fatalf("run(%v) = %q, want %q", tc.args, got, tc.expect)
			}
		})
	}
}

func TestAutoModeDecodeEmptyPacket(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"-d"}, bytes.NewReader(nil), &stdout, &stderr); err != nil {
		t.Fatalf("omnibuf -d on 0-byte packet failed: %v (stderr: %s)", err, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "{}" {
		t.Fatalf("omnibuf -d on 0-byte packet = %q, want \"{}\"", strings.TrimSpace(stdout.String()))
	}

	stdout.Reset()
	stderr.Reset()
	if err := run(nil, bytes.NewReader(nil), &stdout, &stderr); err == nil {
		t.Fatal("omnibuf auto mode without -d on 0-byte input unexpectedly succeeded")
	}
}

func TestMutuallyExclusiveFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{name: "auto_hex_base64", args: []string{"--hex", "--base64"}, wantSub: "--hex"},
		{name: "auto_short_hex_base64", args: []string{"-x", "-b"}, wantSub: "--hex"},
		{name: "auto_field0_nofield0", args: []string{"--field0", "--no-field0"}, wantSub: "--field0"},
		{name: "decode_hex_base64", args: []string{"decode", "--hex", "--base64"}, wantSub: "--hex"},
		{name: "encode_hex_base64", args: []string{"encode", "--hex", "--base64"}, wantSub: "--hex"},
		{name: "encode_field0_nofield0", args: []string{"encode", "--field0", "--no-field0"}, wantSub: "--field0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tc.args, strings.NewReader("{}"), &stdout, &stderr)
			if err == nil {
				t.Fatalf("run(%v) succeeded, want mutually exclusive flag error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("run(%v) error = %q, want substring %q", tc.args, err.Error(), tc.wantSub)
			}
		})
	}
}

func TestSchemalessNestedAndEmptyArrays(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  map[string]any
	}{
		{
			name:  "nested_uint_arrays",
			input: `{"matrix":[[1,2],[3]]}`,
			want: map[string]any{
				"matrix": []any{
					[]any{float64(1), float64(2)},
					[]any{float64(3)},
				},
			},
		},
		{
			name:  "nested_mixed_sign_arrays",
			input: `{"matrix":[[1,2],[-3]]}`,
			want: map[string]any{
				"matrix": []any{
					[]any{float64(1), float64(2)},
					[]any{float64(-3)},
				},
			},
		},
		{
			name:  "nested_with_empty_inner",
			input: `{"matrix":[[1,2],[]]}`,
			want: map[string]any{
				"matrix": []any{
					[]any{float64(1), float64(2)},
					[]any{},
				},
			},
		},
		{
			name:  "empty_array",
			input: `{"empty":[]}`,
			want: map[string]any{
				"empty": []any{},
			},
		},
		{
			name:  "nested_object_arrays",
			input: `{"groups":[[{"a":1}],[{"b":2}]]}`,
			want: map[string]any{
				"groups": []any{
					[]any{map[string]any{"a": float64(1)}},
					[]any{map[string]any{"b": float64(2)}},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var wireOut, decOut, stderr bytes.Buffer
			if err := run([]string{"encode"}, strings.NewReader(tc.input), &wireOut, &stderr); err != nil {
				t.Fatalf("encode failed: %v (stderr: %s)", err, stderr.String())
			}
			if err := run([]string{"decode"}, bytes.NewReader(wireOut.Bytes()), &decOut, &stderr); err != nil {
				t.Fatalf("decode failed: %v (stderr: %s)", err, stderr.String())
			}
			var got map[string]any
			if err := json.Unmarshal(decOut.Bytes(), &got); err != nil {
				t.Fatalf("json.Unmarshal failed: %v\noutput: %s", err, decOut.String())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}

func TestWriteOutputPermissions(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "out.obuf")
	if err := writeOutput(outPath, nil, []byte{0x01}); err != nil {
		t.Fatalf("writeOutput: %v", err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("os.Stat: %v", err)
	}
	// Group/other read bits should be requested (0o644 minus process umask; never 0o600 under standard 0o022 umask).
	if info.Mode().Perm()&0o600 != 0o600 {
		t.Fatalf("unexpected permissions %o", info.Mode().Perm())
	}
	if info.Mode().Perm()&0o044 == 0 {
		t.Fatalf("expected readable permissions from 0o644, got %o", info.Mode().Perm())
	}
}
