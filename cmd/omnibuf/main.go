package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/omnibuf/omnibuf-go"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

var version = "v0.1.0"

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "omnibuf: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "decode", "dec", "d":
			return runDecode(args[1:], stdin, stdout, stderr)
		case "encode", "enc", "e":
			return runEncode(args[1:], stdin, stdout, stderr)
		case "version", "-v", "--version":
			_, err := fmt.Fprintf(stdout, "omnibuf %s\n", version)
			return err
		case "-h", "--help", "help":
			printUsage(stdout)
			return nil
		}
	}
	return runAuto(args, stdin, stdout, stderr)
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  omnibuf [flags] [jq-filter] [file]
  omnibuf <command> [flags] [args...]

jq-Style Quick Usage (Auto-Detects OmniBuf vs JSON):
  # Decode any OmniBuf packet to pretty JSON (no schema required)
  omnibuf packet.obuf
  omnibuf < packet.obuf

  # Query an OmniBuf packet with a jq filter expression
  omnibuf '.location.city' packet.obuf
  omnibuf -r '.router_id' < packet.obuf
  omnibuf -c '.neighbors[] | select(.established) | .router' packet.obuf

  # Encode JSON into a self-describing OmniBuf packet (with Field #0 names)
  omnibuf input.json > packet.obuf
  echo '{"asn": 64512, "router_id": "fra-01", "active": true}' | omnibuf > packet.obuf

Commands:
  decode        Decode OmniBuf wire bytes to JSON (blind or schema-aware, with optional jq filter)
  encode        Encode JSON to OmniBuf wire bytes (schema-less with Field #0, or with FileDescriptorSet)
  version       Print the omnibuf CLI version

Flags:
  -s, --schema <path>       Optional FileDescriptorSet (.json, .pb, .pb.gz)
  -t, --type <name>         Message type name when --schema is used
  -q, --query <jq-filter>   jq filter expression to apply (e.g. '.location.city')
  -c, --compact             Emit compact single-line JSON
  -r, --raw-output          Emit raw string values without JSON quotes when filtering
  -e, --encode              Force encode mode (JSON -> OmniBuf wire bytes)
  -d, --decode              Force decode mode (OmniBuf wire bytes -> JSON)
  -x, --hex                 Hex-encoded wire bytes (input when decoding, output when encoding)
  -b, --base64              Base64-encoded wire bytes (input when decoding, output when encoding)
      --field0              Include Field #0 inline field names when encoding (default in schema-less mode)
      --no-field0           Omit Field #0 inline field names in schema-less encode mode
      --deterministic       Sort map keys and schema fields deterministically
  -i, --input <path>        Input file path (defaults to positional file or stdin)
  -o, --output <path>       Output file path (defaults to stdout)
`)
}

// rearrangeInterspersedFlags moves flags before positional arguments so flag.FlagSet
// accepts flags placed after positional arguments (e.g., `omnibuf '.city' pkt.obuf -r`).
func rearrangeInterspersedFlags(args []string, valueFlags map[string]bool) []string {
	var flags []string
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && valueFlags[a] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

var commonValueFlags = map[string]bool{
	"-s": true, "--schema": true,
	"-t": true, "--type": true,
	"-i": true, "--input": true,
	"-o": true, "--output": true,
	"-q": true, "--query": true,
	"-f": true, "--filter": true,
	"--max-schema-bytes": true,
}

func resolvePositionalFilterAndInput(posArgs []string, explicitQuery, explicitInput string) (query, inputPath string, err error) {
	query = explicitQuery
	inputPath = explicitInput

	switch len(posArgs) {
	case 0:
		return query, inputPath, nil
	case 1:
		arg := posArgs[0]
		if inputPath == "" && (arg == "-" || isLikelyFilePath(arg)) {
			return query, arg, nil
		}
		if query == "" {
			return arg, inputPath, nil
		}
		if inputPath == "" {
			return query, arg, nil
		}
		return "", "", fmt.Errorf("unexpected extra argument %q", arg)
	case 2:
		if query != "" || inputPath != "" {
			return "", "", fmt.Errorf("unexpected extra arguments: %v", posArgs)
		}
		return posArgs[0], posArgs[1], nil
	default:
		return "", "", fmt.Errorf("unexpected extra arguments: %v", posArgs[2:])
	}
}

func isLikelyFilePath(arg string) bool {
	if _, err := os.Stat(arg); err == nil {
		return true
	}
	lower := strings.ToLower(arg)
	for _, ext := range []string{".obuf", ".bin", ".json", ".hex", ".pb", ".b64", ".txt"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func looksLikeJSONInput(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || !utf8.Valid(trimmed) {
		return false
	}
	if trimmed[0] != '{' {
		return false
	}
	var js map[string]any
	return json.Unmarshal(trimmed, &js) == nil
}

func runAuto(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("omnibuf", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		schemaPath    string
		msgType       string
		inputPath     string
		outputPath    string
		queryExpr     string
		forceEncode   bool
		forceDecode   bool
		field0        bool
		noField0      bool
		deterministic bool
		isHex         bool
		isBase64      bool
		compact       bool
		rawOutput     bool
	)
	fs.StringVar(&schemaPath, "schema", "", "Optional FileDescriptorSet path (.json, .pb, .pb.gz)")
	fs.StringVar(&schemaPath, "s", "", "Short alias for --schema")
	maxSchemaBytes := fs.Int64("max-schema-bytes", 64<<20, "Maximum schema file and decompressed size in bytes")
	fs.StringVar(&msgType, "type", "", "Message type name when --schema is used")
	fs.StringVar(&msgType, "t", "", "Short alias for --type")
	fs.StringVar(&inputPath, "input", "", "Input file path (defaults to positional arg or stdin)")
	fs.StringVar(&inputPath, "i", "", "Short alias for --input")
	fs.StringVar(&outputPath, "output", "", "Output file path (defaults to stdout)")
	fs.StringVar(&outputPath, "o", "", "Short alias for --output")
	fs.StringVar(&queryExpr, "query", "", "jq filter expression")
	fs.StringVar(&queryExpr, "q", "", "Short alias for --query")
	fs.StringVar(&queryExpr, "filter", "", "Alias for --query")
	fs.StringVar(&queryExpr, "f", "", "Short alias for --filter")
	fs.BoolVar(&forceEncode, "encode", false, "Force encode mode (JSON -> OmniBuf)")
	fs.BoolVar(&forceEncode, "e", false, "Short alias for --encode")
	fs.BoolVar(&forceDecode, "decode", false, "Force decode mode (OmniBuf -> JSON)")
	fs.BoolVar(&forceDecode, "d", false, "Short alias for --decode")
	fs.BoolVar(&field0, "field0", false, "Include Field #0 inline field names when encoding")
	fs.BoolVar(&field0, "self-describing", false, "Alias for --field0")
	fs.BoolVar(&noField0, "no-field0", false, "Omit Field #0 inline field names in schema-less encode mode")
	fs.BoolVar(&deterministic, "deterministic", false, "Sort map keys deterministically")
	fs.BoolVar(&isHex, "hex", false, "Hex-encoded wire bytes")
	fs.BoolVar(&isHex, "x", false, "Short alias for --hex")
	fs.BoolVar(&isBase64, "base64", false, "Base64-encoded wire bytes")
	fs.BoolVar(&isBase64, "b", false, "Short alias for --base64")
	fs.BoolVar(&compact, "compact", false, "Emit compact single-line JSON")
	fs.BoolVar(&compact, "c", false, "Short alias for --compact")
	fs.BoolVar(&rawOutput, "raw-output", false, "Emit raw string values without JSON quotes")
	fs.BoolVar(&rawOutput, "r", false, "Short alias for --raw-output")

	normArgs := rearrangeInterspersedFlags(args, commonValueFlags)
	if err := fs.Parse(normArgs); err != nil {
		return err
	}
	if forceEncode && forceDecode {
		return errors.New("cannot specify both --encode (-e) and --decode (-d)")
	}

	var err error
	queryExpr, inputPath, err = resolvePositionalFilterAndInput(fs.Args(), queryExpr, inputPath)
	if err != nil {
		return err
	}

	rawInput, err := readInput(inputPath, stdin)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(rawInput)) == 0 && len(rawInput) == 0 {
		return errors.New("empty input (pass a file path, pipe bytes to stdin, or run 'omnibuf --help')")
	}

	isJSON := looksLikeJSONInput(rawInput)
	if forceEncode && !isJSON {
		// Decode OmniBuf input first, apply optional jq filter, then re-encode to OmniBuf!
		decodedJSON, err := decodeWireToJSON(rawInput, schemaPath, msgType, *maxSchemaBytes, isHex, isBase64, true, false, queryExpr)
		if err != nil {
			return err
		}
		return encodeJSONToOutput(bytes.TrimSpace(decodedJSON), schemaPath, msgType, *maxSchemaBytes, outputPath, stdout, field0, noField0, deterministic, isHex, isBase64)
	}

	if forceEncode || (!forceDecode && isJSON && queryExpr == "") {
		return encodeJSONToOutput(rawInput, schemaPath, msgType, *maxSchemaBytes, outputPath, stdout, field0, noField0, deterministic, isHex, isBase64)
	}

	// If input is JSON and a jq filter was supplied without -e, run the jq filter on the JSON directly.
	if !forceDecode && isJSON && queryExpr != "" {
		filtered, err := applyJQFilter(rawInput, queryExpr, compact, rawOutput)
		if err != nil {
			return err
		}
		return writeOutput(outputPath, stdout, filtered)
	}

	jsonOut, err := decodeWireToJSON(rawInput, schemaPath, msgType, *maxSchemaBytes, isHex, isBase64, compact, rawOutput, queryExpr)
	if err != nil {
		return err
	}
	return writeOutput(outputPath, stdout, jsonOut)
}

func runDecode(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("decode", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		schemaPath string
		msgType    string
		inputPath  string
		outputPath string
		queryExpr  string
		isHex      bool
		isBase64   bool
		compact    bool
		rawOutput  bool
	)
	fs.StringVar(&schemaPath, "schema", "", "Optional FileDescriptorSet path (.json, .pb, .pb.gz); omit for blind decode")
	fs.StringVar(&schemaPath, "s", "", "Short alias for --schema")
	maxSchemaBytes := fs.Int64("max-schema-bytes", 64<<20, "Maximum schema file and decompressed size in bytes")
	fs.StringVar(&msgType, "type", "", "Message type name when --schema is used (auto-selected if schema has 1 message)")
	fs.StringVar(&msgType, "t", "", "Short alias for --type")
	fs.StringVar(&inputPath, "input", "", "Input file path (defaults to positional arg or stdin)")
	fs.StringVar(&inputPath, "i", "", "Short alias for --input")
	fs.StringVar(&outputPath, "output", "", "Output file path (defaults to stdout)")
	fs.StringVar(&outputPath, "o", "", "Short alias for --output")
	fs.StringVar(&queryExpr, "query", "", "Optional jq filter expression")
	fs.StringVar(&queryExpr, "q", "", "Short alias for --query")
	fs.StringVar(&queryExpr, "filter", "", "Alias for --query")
	fs.StringVar(&queryExpr, "f", "", "Short alias for --filter")
	fs.BoolVar(&isHex, "hex", false, "Read hex-encoded OmniBuf wire bytes")
	fs.BoolVar(&isHex, "x", false, "Short alias for --hex")
	fs.BoolVar(&isBase64, "base64", false, "Read base64-encoded OmniBuf wire bytes")
	fs.BoolVar(&isBase64, "b", false, "Short alias for --base64")
	fs.BoolVar(&compact, "compact", false, "Emit compact single-line JSON")
	fs.BoolVar(&compact, "c", false, "Short alias for --compact")
	fs.BoolVar(&rawOutput, "raw-output", false, "Emit raw string values without JSON quotes")
	fs.BoolVar(&rawOutput, "r", false, "Short alias for --raw-output")

	normArgs := rearrangeInterspersedFlags(args, commonValueFlags)
	if err := fs.Parse(normArgs); err != nil {
		return err
	}

	var err error
	queryExpr, inputPath, err = resolvePositionalFilterAndInput(fs.Args(), queryExpr, inputPath)
	if err != nil {
		return err
	}

	rawInput, err := readInput(inputPath, stdin)
	if err != nil {
		return err
	}

	jsonOut, err := decodeWireToJSON(rawInput, schemaPath, msgType, *maxSchemaBytes, isHex, isBase64, compact, rawOutput, queryExpr)
	if err != nil {
		return err
	}
	return writeOutput(outputPath, stdout, jsonOut)
}

func decodeWireToJSON(rawInput []byte, schemaPath, msgType string, maxSchemaBytes int64, isHex, isBase64, compact, rawOutput bool, queryExpr string) ([]byte, error) {
	wireBytes := rawInput
	var err error
	if isHex {
		wireBytes, err = decodeHexBytes(rawInput)
		if err != nil {
			return nil, fmt.Errorf("invalid hex input: %w", err)
		}
	} else if isBase64 {
		wireBytes, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(rawInput)))
		if err != nil {
			return nil, fmt.Errorf("invalid base64 input: %w", err)
		}
	}

	var jsonOut []byte
	if schemaPath == "" {
		if msgType != "" {
			return nil, errors.New("--type requires --schema (-s)")
		}
		blindMap, err := omnibuf.DecodeBlind(wireBytes)
		if err != nil {
			return nil, err
		}
		sanitized := sanitizeForJSON(blindMap)
		if compact && queryExpr == "" && !rawOutput {
			jsonOut, err = json.Marshal(sanitized)
		} else {
			jsonOut, err = json.MarshalIndent(sanitized, "", "  ")
		}
		if err != nil {
			return nil, fmt.Errorf("failed to marshal blind-decoded JSON: %w", err)
		}
	} else {
		md, err := loadMessageDescriptor(schemaPath, msgType, maxSchemaBytes)
		if err != nil {
			return nil, err
		}
		msg := dynamicpb.NewMessage(md)
		if err := omnibuf.Unmarshal(wireBytes, msg); err != nil {
			return nil, err
		}
		mOpts := protojson.MarshalOptions{
			UseProtoNames: true,
		}
		rawJSON, err := mOpts.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal protobuf JSON: %w", err)
		}
		var normBuf bytes.Buffer
		if compact && queryExpr == "" && !rawOutput {
			if err := json.Compact(&normBuf, rawJSON); err != nil {
				return nil, fmt.Errorf("failed to compact JSON: %w", err)
			}
		} else {
			if err := json.Indent(&normBuf, rawJSON, "", "  "); err != nil {
				return nil, fmt.Errorf("failed to indent JSON: %w", err)
			}
		}
		jsonOut = normBuf.Bytes()
	}

	if queryExpr != "" || rawOutput {
		return applyJQFilter(jsonOut, queryExpr, compact, rawOutput)
	}
	jsonOut = append(jsonOut, '\n')
	return jsonOut, nil
}

func runEncode(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("encode", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		schemaPath    string
		msgType       string
		inputPath     string
		outputPath    string
		field0        bool
		noField0      bool
		deterministic bool
		isHex         bool
		isBase64      bool
	)
	fs.StringVar(&schemaPath, "schema", "", "Optional FileDescriptorSet path (.json, .pb, .pb.gz); omit for schema-less Field #0 encode")
	fs.StringVar(&schemaPath, "s", "", "Short alias for --schema")
	maxSchemaBytes := fs.Int64("max-schema-bytes", 64<<20, "Maximum schema file and decompressed size in bytes")
	fs.StringVar(&msgType, "type", "", "Message type name (auto-selected if schema has 1 message)")
	fs.StringVar(&msgType, "t", "", "Short alias for --type")
	fs.StringVar(&inputPath, "input", "", "Input JSON file path (defaults to positional arg or stdin)")
	fs.StringVar(&inputPath, "i", "", "Short alias for --input")
	fs.StringVar(&outputPath, "output", "", "Output file path (defaults to stdout)")
	fs.StringVar(&outputPath, "o", "", "Short alias for --output")
	fs.BoolVar(&field0, "field0", false, "Include Field #0 inline field names for self-describing mode")
	fs.BoolVar(&field0, "self-describing", false, "Alias for --field0")
	fs.BoolVar(&noField0, "no-field0", false, "Omit Field #0 inline field names in schema-less encode mode")
	fs.BoolVar(&deterministic, "deterministic", false, "Sort WireMap keys deterministically for reproducible wire output")
	fs.BoolVar(&isHex, "hex", false, "Write hex-encoded OmniBuf wire bytes")
	fs.BoolVar(&isHex, "x", false, "Short alias for --hex")
	fs.BoolVar(&isBase64, "base64", false, "Write base64-encoded OmniBuf wire bytes")
	fs.BoolVar(&isBase64, "b", false, "Short alias for --base64")

	normArgs := rearrangeInterspersedFlags(args, commonValueFlags)
	if err := fs.Parse(normArgs); err != nil {
		return err
	}
	if inputPath == "" && fs.NArg() > 0 {
		inputPath = fs.Arg(0)
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("unexpected extra arguments: %v", fs.Args()[1:])
	}

	jsonBytes, err := readInput(inputPath, stdin)
	if err != nil {
		return err
	}

	return encodeJSONToOutput(jsonBytes, schemaPath, msgType, *maxSchemaBytes, outputPath, stdout, field0, noField0, deterministic, isHex, isBase64)
}

func encodeJSONToOutput(jsonBytes []byte, schemaPath, msgType string, maxSchemaBytes int64, outputPath string, stdout io.Writer, field0, noField0, deterministic, isHex, isBase64 bool) error {
	var wireBytes []byte
	var err error

	if schemaPath == "" {
		if msgType != "" {
			return errors.New("--type requires --schema (-s)")
		}
		includeField0 := !noField0
		wireBytes, err = encodeSchemalessJSON(jsonBytes, includeField0, deterministic)
		if err != nil {
			return err
		}
	} else {
		md, err := loadMessageDescriptor(schemaPath, msgType, maxSchemaBytes)
		if err != nil {
			return err
		}
		msg := dynamicpb.NewMessage(md)
		uOpts := protojson.UnmarshalOptions{
			DiscardUnknown: false,
		}
		if err := uOpts.Unmarshal(jsonBytes, msg); err != nil {
			return fmt.Errorf("failed to parse input JSON for %s: %w", md.FullName(), err)
		}
		wireBytes, err = omnibuf.MarshalOptions{
			Deterministic:  deterministic,
			SelfDescribing: field0,
		}.Marshal(msg)
		if err != nil {
			return err
		}
	}

	if isHex {
		hexOut := []byte(hex.EncodeToString(wireBytes) + "\n")
		return writeOutput(outputPath, stdout, hexOut)
	}
	if isBase64 {
		b64Out := []byte(base64.StdEncoding.EncodeToString(wireBytes) + "\n")
		return writeOutput(outputPath, stdout, b64Out)
	}
	return writeOutput(outputPath, stdout, wireBytes)
}

// readSchemaBytes bounds both compressed input and the sum of all gzip members.
func readSchemaBytes(r io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("schema exceeds limit of %d bytes (--max-schema-bytes)", maxBytes)
	}
	return data, nil
}

// loadFileDescriptorSet loads a *descriptorpb.FileDescriptorSet from a .json, .pb, or .pb.gz file.
func loadFileDescriptorSet(path string, maxBytes int64) (*descriptorpb.FileDescriptorSet, error) {
	if maxBytes <= 0 || maxBytes == 1<<63-1 {
		return nil, errors.New("max-schema-bytes must be positive and less than MaxInt64")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open schema %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	raw, err := readSchemaBytes(f, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("read schema %q: %w", path, err)
	}

	lower := strings.ToLower(path)
	isGzip := strings.HasSuffix(lower, ".gz") || (len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b)
	if isGzip {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("open gzip schema %q: %w", path, err)
		}
		decompressed, err := readSchemaBytes(zr, maxBytes)
		_ = zr.Close()
		if err != nil {
			return nil, fmt.Errorf("decompress gzip schema %q: %w", path, err)
		}
		raw = decompressed
		lower = strings.TrimSuffix(lower, ".gz")
	}

	trimmed := bytes.TrimSpace(raw)
	isJSON := strings.HasSuffix(lower, ".json") || (len(trimmed) > 0 && trimmed[0] == '{')

	fds := &descriptorpb.FileDescriptorSet{}
	if isJSON {
		if err := protojson.Unmarshal(trimmed, fds); err != nil {
			fdp := &descriptorpb.FileDescriptorProto{}
			if errSingle := protojson.Unmarshal(trimmed, fdp); errSingle == nil && fdp.GetName() != "" {
				fds.File = []*descriptorpb.FileDescriptorProto{fdp}
				return fds, nil
			}
			return nil, fmt.Errorf("unmarshal FileDescriptorSet JSON from %q: %w", path, err)
		}
		if len(fds.File) == 0 {
			return nil, fmt.Errorf("FileDescriptorSet JSON %q contains 0 files", path)
		}
		return fds, nil
	}

	if err := proto.Unmarshal(raw, fds); err != nil {
		return nil, fmt.Errorf("unmarshal FileDescriptorSet binary from %q: %w", path, err)
	}
	if len(fds.File) == 0 {
		return nil, fmt.Errorf("FileDescriptorSet %q contains 0 files", path)
	}
	return fds, nil
}

func loadMessageDescriptor(schemaPath, msgType string, maxBytes int64) (protoreflect.MessageDescriptor, error) {
	fds, err := loadFileDescriptorSet(schemaPath, maxBytes)
	if err != nil {
		return nil, err
	}

	files, err := protodesc.NewFiles(fds)
	if err != nil {
		return nil, fmt.Errorf("build descriptor registry from %q: %w", schemaPath, err)
	}

	cleanType := strings.TrimPrefix(strings.TrimSpace(msgType), ".")
	if cleanType != "" {
		if desc, err := files.FindDescriptorByName(protoreflect.FullName(cleanType)); err == nil {
			if md, ok := desc.(protoreflect.MessageDescriptor); ok {
				return md, nil
			}
			return nil, fmt.Errorf("%q is not a message descriptor", cleanType)
		}
	}

	var allMessages []protoreflect.MessageDescriptor
	var topLevelMessages []protoreflect.MessageDescriptor
	var collectNested func(mds protoreflect.MessageDescriptors, top bool)
	collectNested = func(mds protoreflect.MessageDescriptors, top bool) {
		for i := 0; i < mds.Len(); i++ {
			md := mds.Get(i)
			if md.IsMapEntry() {
				continue
			}
			allMessages = append(allMessages, md)
			if top {
				topLevelMessages = append(topLevelMessages, md)
			}
			collectNested(md.Messages(), false)
		}
	}
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		collectNested(fd.Messages(), true)
		return true
	})

	if cleanType == "" {
		if len(topLevelMessages) == 1 {
			return topLevelMessages[0], nil
		}
		if len(allMessages) == 1 {
			return allMessages[0], nil
		}
		names := make([]string, 0, len(allMessages))
		for _, md := range allMessages {
			names = append(names, string(md.FullName()))
		}
		return nil, fmt.Errorf("please specify --type (-t); available messages in %q: %s", schemaPath, strings.Join(names, ", "))
	}

	var matches []protoreflect.MessageDescriptor
	suffix := "." + cleanType
	for _, md := range allMessages {
		full := string(md.FullName())
		if string(md.Name()) == cleanType || strings.HasSuffix(full, suffix) {
			matches = append(matches, md)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, md := range matches {
			names = append(names, string(md.FullName()))
		}
		return nil, fmt.Errorf("ambiguous message type %q matches multiple messages: %s", msgType, strings.Join(names, ", "))
	}

	names := make([]string, 0, len(allMessages))
	for _, md := range allMessages {
		names = append(names, string(md.FullName()))
	}
	return nil, fmt.Errorf("message type %q not found in %q (available: %s)", msgType, schemaPath, strings.Join(names, ", "))
}

func readInput(path string, stdin io.Reader) ([]byte, error) {
	if path == "" || path == "-" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	return data, nil
}

func writeOutput(path string, stdout io.Writer, data []byte) error {
	if path == "" || path == "-" {
		_, err := stdout.Write(data)
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}

func decodeHexBytes(raw []byte) ([]byte, error) {
	var clean []byte
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			clean = append(clean, b)
		}
	}
	if len(clean) >= 2 && clean[0] == '0' && (clean[1] == 'x' || clean[1] == 'X') {
		clean = clean[2:]
	}
	out := make([]byte, hex.DecodedLen(len(clean)))
	n, err := hex.Decode(out, clean)
	if err != nil {
		return nil, err
	}
	return out[:n], nil
}

func sanitizeForJSON(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, elem := range val {
			out[k] = sanitizeForJSON(elem)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(val))
		for k, elem := range val {
			out[fmt.Sprint(k)] = sanitizeForJSON(elem)
		}
		return out
	case []map[string]any:
		out := make([]any, len(val))
		for i, elem := range val {
			out[i] = sanitizeForJSON(elem)
		}
		return out
	case []string:
		out := make([]string, len(val))
		for i, s := range val {
			if utf8.ValidString(s) {
				out[i] = s
			} else {
				out[i] = "base64:" + base64.StdEncoding.EncodeToString([]byte(s))
			}
		}
		return out
	case string:
		if utf8.ValidString(val) {
			return val
		}
		return "base64:" + base64.StdEncoding.EncodeToString([]byte(val))
	default:
		return v
	}
}
