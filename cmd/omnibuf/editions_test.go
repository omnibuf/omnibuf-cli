package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	obwire "github.com/omnibuf/omnibuf-go/wire"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

type compiledSchemaSet struct {
	pbPath   string
	pbGzPath string
	jsonPath string
	fds      *descriptorpb.FileDescriptorSet
}

func findProtocForTest(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("PROTOC"); p != "" {
		return p
	}
	if p, err := exec.LookPath("protoc"); err == nil {
		return p
	}
	if _, err := os.Stat("/opt/homebrew/bin/protoc"); err == nil {
		return "/opt/homebrew/bin/protoc"
	}
	t.Skip("protoc not found in PATH")
	return ""
}

func compileProtoSchemaSet(t *testing.T, protoSources map[string]string, targets []string, includeImports bool) compiledSchemaSet {
	t.Helper()
	protocPath := findProtocForTest(t)
	dir := t.TempDir()

	for name, src := range protoSources {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	pbPath := filepath.Join(dir, "schema.pb")
	args := []string{"-I" + dir, "--descriptor_set_out=" + pbPath}
	if includeImports {
		args = append(args, "--include_imports")
	}
	for _, tgt := range targets {
		args = append(args, filepath.Join(dir, tgt))
	}

	if out, err := exec.Command(protocPath, args...).CombinedOutput(); err != nil {
		t.Fatalf("protoc failed: %v\n%s", err, out)
	}

	pbBytes, err := os.ReadFile(pbPath)
	if err != nil {
		t.Fatalf("read %s: %v", pbPath, err)
	}

	fds := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(pbBytes, fds); err != nil {
		t.Fatalf("proto.Unmarshal FileDescriptorSet: %v", err)
	}

	var gzBuf bytes.Buffer
	zw := gzip.NewWriter(&gzBuf)
	if _, err := zw.Write(pbBytes); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	pbGzPath := filepath.Join(dir, "schema.pb.gz")
	if err := os.WriteFile(pbGzPath, gzBuf.Bytes(), 0o600); err != nil {
		t.Fatalf("write %s: %v", pbGzPath, err)
	}

	jsonBytes, err := protojson.MarshalOptions{Indent: "  "}.Marshal(fds)
	if err != nil {
		t.Fatalf("protojson.Marshal(fds): %v", err)
	}
	jsonPath := filepath.Join(dir, "schema.json")
	if err := os.WriteFile(jsonPath, jsonBytes, 0o600); err != nil {
		t.Fatalf("write %s: %v", jsonPath, err)
	}

	return compiledSchemaSet{
		pbPath:   pbPath,
		pbGzPath: pbGzPath,
		jsonPath: jsonPath,
		fds:      fds,
	}
}

func TestCLIEditions_2023_2024_2026_AllSchemaFormats(t *testing.T) {
	editions := []struct {
		year        string
		extraHeader string
		visPrefix   string
	}{
		{
			year:        "2023",
			extraHeader: "option features.json_format = LEGACY_BEST_EFFORT;",
		},
		{
			year:        "2024",
			extraHeader: "option features.enforce_naming_style = STYLE2024;\noption features.default_symbol_visibility = EXPORT_TOP_LEVEL;",
		},
		{
			year:        "2026",
			extraHeader: "option features.enforce_naming_style = STYLE2026;\noption features.default_symbol_visibility = STRICT;",
			visPrefix:   "export ",
		},
	}

	for _, ed := range editions {
		t.Run("edition_"+ed.year, func(t *testing.T) {
			src := `edition = "` + ed.year + `";
package cli.ed` + ed.year + `;

import "google/protobuf/descriptor.proto";
` + ed.extraHeader + `

extend google.protobuf.FieldOptions {
  string cli_note = 50011;
}

` + ed.visPrefix + `enum OpenState {
  OPEN_STATE_UNSPECIFIED = 0;
  OPEN_STATE_READY = 1;
}

` + ed.visPrefix + `message Child {
  int32 id = 1 [default = 0];
  string note = 2 [default = "", (cli_note) = "ok"];
}

` + ed.visPrefix + `message Record {
  int32 explicit_int = 1 [default = 0];
  bool explicit_bool = 2 [default = false];
  string explicit_string = 3 [default = ""];
  bytes explicit_bytes = 4 [default = ""];
  double explicit_double = 5 [default = 0.0];
  OpenState status = 6 [default = OPEN_STATE_UNSPECIFIED];

  int32 implicit_count = 7 [features.field_presence = IMPLICIT];
  bool implicit_flag = 8 [features.field_presence = IMPLICIT];

  Child child_lp = 9;
  Child child_delim = 10 [features.message_encoding = DELIMITED];

  repeated int32 packed_vals = 11 [features.repeated_field_encoding = PACKED];
  repeated int32 expanded_vals = 12 [features.repeated_field_encoding = EXPANDED];
  repeated Child delim_items = 13 [features.message_encoding = DELIMITED];

  map<string, int32> scores = 14;

  oneof choice {
    int32 choice_int = 15;
    bool choice_flag = 16;
    string choice_text = 17;
  }
}
`
			schemas := compileProtoSchemaSet(t, map[string]string{"record.proto": src}, []string{"record.proto"}, true)
			files, err := protodesc.NewFiles(schemas.fds)
			if err != nil {
				t.Fatalf("protodesc.NewFiles: %v", err)
			}
			fullType := "cli.ed" + ed.year + ".Record"
			desc, err := files.FindDescriptorByName(protoreflect.FullName(fullType))
			if err != nil {
				t.Fatalf("FindDescriptorByName(%s): %v", fullType, err)
			}
			recMD := desc.(protoreflect.MessageDescriptor)

			formats := []struct {
				name string
				path string
			}{
				{name: "pb", path: schemas.pbPath},
				{name: "pb.gz", path: schemas.pbGzPath},
				{name: "json", path: schemas.jsonPath},
			}

			// 1. Explicit presence zeros vs absent fields
			explicitZerosJSON := `{
  "explicit_int": 0,
  "explicit_bool": false,
  "explicit_string": "",
  "explicit_bytes": "",
  "explicit_double": 0,
  "status": "OPEN_STATE_UNSPECIFIED",
  "choice_flag": false
}`
			for _, fmtCase := range formats {
				t.Run(fmtCase.name+"_explicit_zeros", func(t *testing.T) {
					var wireOut, stderr bytes.Buffer
					if err := run([]string{"encode", "--deterministic", "-s", fmtCase.path, "-t", "Record"}, strings.NewReader(explicitZerosJSON), &wireOut, &stderr); err != nil {
						t.Fatalf("encode explicit zeros failed: %v (stderr: %s)", err, stderr.String())
					}
					if b, ok := obwire.ReadBool(wireOut.Bytes(), 2); !ok || b {
						t.Fatalf("explicit_bool must emit WireBoolFalse, got (%v, %v)", b, ok)
					}
					if b, ok := obwire.ReadBool(wireOut.Bytes(), 16); !ok || b {
						t.Fatalf("oneof choice_flag=false must emit WireBoolFalse, got (%v, %v)", b, ok)
					}
					if _, _, ok := obwire.LocateField(wireOut.Bytes(), 1); !ok {
						t.Fatalf("explicit_int=0 must be emitted on wire")
					}
					if _, _, ok := obwire.LocateField(wireOut.Bytes(), 3); !ok {
						t.Fatalf("explicit_string=\"\" must be emitted on wire")
					}

					var decOut bytes.Buffer
					if err := run([]string{"decode", "-s", fmtCase.path, "-t", fullType}, bytes.NewReader(wireOut.Bytes()), &decOut, &stderr); err != nil {
						t.Fatalf("decode explicit zeros failed: %v (stderr: %s)", err, stderr.String())
					}
					var decoded map[string]any
					if err := json.Unmarshal(decOut.Bytes(), &decoded); err != nil {
						t.Fatalf("unmarshal decoded explicit zeros JSON: %v\n%s", err, decOut.String())
					}
					if decoded["explicit_int"] != float64(0) {
						t.Fatalf("explicit_int = %#v, want 0", decoded["explicit_int"])
					}
					if decoded["explicit_bool"] != false {
						t.Fatalf("explicit_bool = %#v, want false", decoded["explicit_bool"])
					}
					if decoded["explicit_string"] != "" {
						t.Fatalf("explicit_string = %#v, want empty string", decoded["explicit_string"])
					}
					if decoded["explicit_bytes"] != "" {
						t.Fatalf("explicit_bytes = %#v, want empty bytes base64", decoded["explicit_bytes"])
					}
					if decoded["status"] != "OPEN_STATE_UNSPECIFIED" {
						t.Fatalf("status = %#v, want OPEN_STATE_UNSPECIFIED", decoded["status"])
					}
					if decoded["choice_flag"] != false {
						t.Fatalf("choice_flag = %#v, want false", decoded["choice_flag"])
					}
					if _, present := decoded["implicit_count"]; present {
						t.Fatalf("unset implicit_count should not be emitted in protojson output: %v", decoded)
					}

					// Empty message must omit absent explicit fields.
					var emptyWire, emptyDec bytes.Buffer
					if err := run([]string{"encode", "-s", fmtCase.path, "-t", "Record"}, strings.NewReader("{}"), &emptyWire, &stderr); err != nil {
						t.Fatalf("encode empty failed: %v (stderr: %s)", err, stderr.String())
					}
					if _, _, ok := obwire.LocateField(emptyWire.Bytes(), 2); ok {
						t.Fatalf("absent explicit_bool must not be emitted on wire")
					}
					if err := run([]string{"decode", "-s", fmtCase.path, "-t", "Record"}, bytes.NewReader(emptyWire.Bytes()), &emptyDec, &stderr); err != nil {
						t.Fatalf("decode empty failed: %v (stderr: %s)", err, stderr.String())
					}
					if strings.TrimSpace(emptyDec.String()) != "{}" {
						t.Fatalf("empty message decoded to %q, want \"{}\"", strings.TrimSpace(emptyDec.String()))
					}
				})
			}

			// 2. Full message with DELIMITED submessages, unknown negative open enum, packed/expanded, maps, oneof, --field0, jq
			fullJSON := `{
  "explicit_int": -42,
  "explicit_bool": false,
  "explicit_string": "ed-` + ed.year + `",
  "explicit_bytes": "qrs=",
  "explicit_double": 2.5,
  "status": -7,
  "implicit_count": 19,
  "implicit_flag": true,
  "child_lp": {"id": 1, "note": "lp"},
  "child_delim": {"id": 2, "note": "delim-` + ed.year + `"},
  "packed_vals": [-3, 0, 9],
  "expanded_vals": [-3, 0, 9],
  "delim_items": [
    {"id": 10, "note": "first"},
    {"id": 11, "note": "second"}
  ],
  "scores": {"alpha": 1, "beta": -2},
  "choice_int": 0
}`
			wantMsg := dynamicpb.NewMessage(recMD)
			if err := protojson.Unmarshal([]byte(fullJSON), wantMsg); err != nil {
				t.Fatalf("protojson.Unmarshal wantMsg: %v", err)
			}

			for _, fmtCase := range formats {
				for _, field0 := range []bool{false, true} {
					mode := "shared"
					if field0 {
						mode = "field0"
					}
					t.Run(fmtCase.name+"_"+mode, func(t *testing.T) {
						dir := t.TempDir()
						pktFile := filepath.Join(dir, "record.obuf")
						encArgs := []string{"encode", "--deterministic", "-s", fmtCase.path, "-t", fullType, "-o", pktFile}
						if field0 {
							encArgs = append(encArgs, "--field0")
						}
						var stdout, stderr bytes.Buffer
						if err := run(encArgs, strings.NewReader(fullJSON), &stdout, &stderr); err != nil {
							t.Fatalf("encode failed: %v (stderr: %s)", err, stderr.String())
						}

						// Schema-aware decode
						stdout.Reset()
						if err := run([]string{"decode", "-s", fmtCase.path, "-t", "Record", pktFile}, strings.NewReader(""), &stdout, &stderr); err != nil {
							t.Fatalf("decode failed: %v (stderr: %s)", err, stderr.String())
						}
						gotMsg := dynamicpb.NewMessage(recMD)
						if err := protojson.Unmarshal(stdout.Bytes(), gotMsg); err != nil {
							t.Fatalf("protojson.Unmarshal decoded output: %v\n%s", err, stdout.String())
						}
						if !proto.Equal(wantMsg, gotMsg) {
							t.Fatalf("round-trip mismatch:\ngot:  %v\nwant: %v", gotMsg, wantMsg)
						}

						// Blind decode check
						stdout.Reset()
						if err := run([]string{"decode", pktFile}, strings.NewReader(""), &stdout, &stderr); err != nil {
							t.Fatalf("blind decode failed: %v (stderr: %s)", err, stderr.String())
						}
						var blind map[string]any
						if err := json.Unmarshal(stdout.Bytes(), &blind); err != nil {
							t.Fatalf("unmarshal blind JSON: %v\n%s", err, stdout.String())
						}
						if field0 {
							cd, ok := blind["child_delim"].(map[string]any)
							if !ok || cd["note"] != "delim-"+ed.year {
								t.Fatalf("blind child_delim = %#v", blind["child_delim"])
							}
							if blind["explicit_bool"] != false || blind["status"] != float64(-7) || blind["choice_int"] != float64(0) {
								t.Fatalf("unexpected blind fields: %v", blind)
							}
						} else {
							cd, ok := blind["field_10"].(map[string]any)
							if !ok || cd["field_2"] != "delim-"+ed.year {
								t.Fatalf("blind field_10 = %#v", blind["field_10"])
							}
						}

						// jq filter query on schema-aware decode
						stdout.Reset()
						if err := run([]string{"-s", fmtCase.path, "-t", "Record", "-r", ".child_delim.note", pktFile}, strings.NewReader(""), &stdout, &stderr); err != nil {
							t.Fatalf("jq query failed: %v (stderr: %s)", err, stderr.String())
						}
						if got := strings.TrimSpace(stdout.String()); got != "delim-"+ed.year {
							t.Fatalf("jq .child_delim.note = %q, want %q", got, "delim-"+ed.year)
						}
					})
				}
			}
		})
	}
}

func TestCLIEditions_IncludeImportsAndOptionDependencyAutoSelect(t *testing.T) {
	t.Run("descriptor_import_location_name_collision", func(t *testing.T) {
		src := `edition = "2026";
package demo.geo;

import "google/protobuf/descriptor.proto";

extend google.protobuf.FieldOptions {
  string geo_hint = 50021;
}

export message Location {
  string city = 1 [(geo_hint) = "iata"];
  int32 rack = 2;
}
`
		schemas := compileProtoSchemaSet(t, map[string]string{"geo.proto": src}, []string{"geo.proto"}, true)

		// Omitting --type must auto-select demo.geo.Location despite google/protobuf/descriptor.proto in FDS.
		var wireOut, decOut, stderr bytes.Buffer
		input := `{"city": "Zurich", "rack": 0}`
		if err := run([]string{"encode", "-s", schemas.pbPath}, strings.NewReader(input), &wireOut, &stderr); err != nil {
			t.Fatalf("auto-select encode failed: %v (stderr: %s)", err, stderr.String())
		}
		if err := run([]string{"decode", "-s", schemas.pbPath}, bytes.NewReader(wireOut.Bytes()), &decOut, &stderr); err != nil {
			t.Fatalf("auto-select decode failed: %v (stderr: %s)", err, stderr.String())
		}
		var decoded map[string]any
		if err := json.Unmarshal(decOut.Bytes(), &decoded); err != nil {
			t.Fatalf("json.Unmarshal: %v\n%s", err, decOut.String())
		}
		if decoded["city"] != "Zurich" || decoded["rack"] != float64(0) {
			t.Fatalf("unexpected decoded Location: %v", decoded)
		}

		// Short name -t Location must resolve demo.geo.Location and not collide with google.protobuf.SourceCodeInfo.Location.
		decOut.Reset()
		if err := run([]string{"decode", "-s", schemas.pbPath, "-t", "Location"}, bytes.NewReader(wireOut.Bytes()), &decOut, &stderr); err != nil {
			t.Fatalf("-t Location decode failed: %v (stderr: %s)", err, stderr.String())
		}
		if !strings.Contains(decOut.String(), `"city": "Zurich"`) {
			t.Fatalf("unexpected -t Location output: %s", decOut.String())
		}
	})

	t.Run("edition_2024_import_option_with_and_without_imports", func(t *testing.T) {
		sources := map[string]string{
			"opts.proto": `edition = "2024";
package demo.opts;
import "google/protobuf/descriptor.proto";
message TagInfo {
  string owner = 1;
}
extend google.protobuf.FieldOptions {
  TagInfo tag_info = 50022;
}
`,
			"item.proto": `edition = "2024";
package demo.ed24;
import option "opts.proto";
message Item {
  int32 id = 1 [(demo.opts.tag_info) = { owner: "core" }];
  bool active = 2;
}
`,
		}

		for _, includeImports := range []bool{false, true} {
			label := "no_imports"
			if includeImports {
				label = "with_imports"
			}
			t.Run(label, func(t *testing.T) {
				schemas := compileProtoSchemaSet(t, sources, []string{"item.proto"}, includeImports)
				for _, scPath := range []string{schemas.pbPath, schemas.pbGzPath, schemas.jsonPath} {
					var wireOut, decOut, stderr bytes.Buffer
					if err := run([]string{"encode", "-s", scPath}, strings.NewReader(`{"id": 0, "active": false}`), &wireOut, &stderr); err != nil {
						t.Fatalf("encode (%s) failed: %v (stderr: %s)", filepath.Base(scPath), err, stderr.String())
					}
					if err := run([]string{"decode", "-s", scPath}, bytes.NewReader(wireOut.Bytes()), &decOut, &stderr); err != nil {
						t.Fatalf("decode (%s) failed: %v (stderr: %s)", filepath.Base(scPath), err, stderr.String())
					}
					var decoded map[string]any
					if err := json.Unmarshal(decOut.Bytes(), &decoded); err != nil {
						t.Fatalf("json.Unmarshal: %v", err)
					}
					if decoded["id"] != float64(0) || decoded["active"] != false {
						t.Fatalf("unexpected decoded Item: %v", decoded)
					}
				}
			})
		}
	})

	t.Run("edition_2026_import_option_feature_extension_json", func(t *testing.T) {
		src := `edition = "2026";
package demo.ed26;

import option "google/protobuf/cpp_features.proto";
option features.(pb.cpp).string_type = VIEW;

export message Item {
  string name = 1;
}
`
		schemas := compileProtoSchemaSet(t, map[string]string{"item26.proto": src}, []string{"item26.proto"}, true)

		// Inject a buf-style custom language feature extension key into the JSON FDS
		// to verify loadFileDescriptorSet discards unknown extension keys without failing.
		rawJSON, err := os.ReadFile(schemas.jsonPath)
		if err != nil {
			t.Fatalf("read schema.json: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(rawJSON, &doc); err != nil {
			t.Fatalf("unmarshal schema.json: %v", err)
		}
		fileList := doc["file"].([]any)
		lastFile := fileList[len(fileList)-1].(map[string]any)
		opts, _ := lastFile["options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
			lastFile["options"] = opts
		}
		opts["features"] = map[string]any{
			"[pb.cpp]": map[string]any{"stringType": "VIEW"},
		}
		extJSON, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatalf("marshal extended schema.json: %v", err)
		}
		extJSONPath := filepath.Join(t.TempDir(), "schema_ext.json")
		if err := os.WriteFile(extJSONPath, extJSON, 0o600); err != nil {
			t.Fatalf("write schema_ext.json: %v", err)
		}

		for _, scPath := range []string{schemas.pbPath, extJSONPath} {
			var wireOut, decOut, stderr bytes.Buffer
			if err := run([]string{"encode", "-s", scPath}, strings.NewReader(`{"name": ""}`), &wireOut, &stderr); err != nil {
				t.Fatalf("encode (%s) failed: %v (stderr: %s)", filepath.Base(scPath), err, stderr.String())
			}
			if err := run([]string{"decode", "-s", scPath}, bytes.NewReader(wireOut.Bytes()), &decOut, &stderr); err != nil {
				t.Fatalf("decode (%s) failed: %v (stderr: %s)", filepath.Base(scPath), err, stderr.String())
			}
			var decoded map[string]any
			if err := json.Unmarshal(decOut.Bytes(), &decoded); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			if decoded["name"] != "" {
				t.Fatalf("decoded name = %#v, want empty string", decoded["name"])
			}
		}
	})
}

func TestCLIEditions_UnsupportedFeatureErrors(t *testing.T) {
	cases := []struct {
		name        string
		protoSrc    string
		msgType     string
		inputJSON   string
		wantContain string
	}{
		{
			name: "closed_enum",
			protoSrc: `edition = "2023";
package rej.cli.closed;
enum ClosedE {
  option features.enum_type = CLOSED;
  CLOSED_E_UNSPECIFIED = 0;
}
message M {
  ClosedE e = 1;
}
`,
			msgType:     "rej.cli.closed.M",
			inputJSON:   `{"e": "CLOSED_E_UNSPECIFIED"}`,
			wantContain: "CLOSED enums",
		},
		{
			name: "legacy_required",
			protoSrc: `edition = "2024";
package rej.cli.req;
message M {
  int32 x = 1 [features.field_presence = LEGACY_REQUIRED];
}
`,
			msgType:     "rej.cli.req.M",
			inputJSON:   `{"x": 1}`,
			wantContain: "LEGACY_REQUIRED",
		},
		{
			name: "utf8_validation_none",
			protoSrc: `edition = "2026";
package rej.cli.utf8;
export message M {
  string s = 1 [features.utf8_validation = NONE];
}
`,
			msgType:     "rej.cli.utf8.M",
			inputJSON:   `{"s": "ok"}`,
			wantContain: "utf8_validation = NONE",
		},
		{
			name: "nonzero_default",
			protoSrc: `edition = "2026";
package rej.cli.def;
export message M {
  int32 x = 1 [default = 7];
}
`,
			msgType:     "rej.cli.def.M",
			inputJSON:   `{"x": 7}`,
			wantContain: "non-zero default",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schemas := compileProtoSchemaSet(t, map[string]string{"bad.proto": tc.protoSrc}, []string{"bad.proto"}, false)
			for _, scPath := range []string{schemas.pbPath, schemas.jsonPath} {
				var stdout, stderr bytes.Buffer
				err := run([]string{"encode", "-s", scPath, "-t", tc.msgType}, strings.NewReader(tc.inputJSON), &stdout, &stderr)
				if err == nil || !strings.Contains(err.Error(), tc.wantContain) {
					t.Fatalf("encode (%s) err = %v, want substring %q", filepath.Base(scPath), err, tc.wantContain)
				}

				stdout.Reset()
				stderr.Reset()
				err = run([]string{"decode", "-s", scPath, "-t", tc.msgType}, bytes.NewReader(nil), &stdout, &stderr)
				if err == nil || !strings.Contains(err.Error(), tc.wantContain) {
					t.Fatalf("decode (%s) err = %v, want substring %q", filepath.Base(scPath), err, tc.wantContain)
				}
			}
		})
	}
}
