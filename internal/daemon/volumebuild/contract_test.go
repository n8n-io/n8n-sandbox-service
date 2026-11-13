package volumebuild

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func envLookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

// kernelEnv is the environment init gets from a kernel command line holding
// args: the kernel splits it at spaces and passes on each NAME=value pair.
func kernelEnv(args string) map[string]string {
	env := map[string]string{"HOME": "/", "TERM": "linux"}
	for arg := range strings.FieldsSeq(args) {
		if name, value, ok := strings.Cut(arg, "="); ok {
			env[name] = value
		}
	}
	return env
}

func TestParamsSurviveTheKernelCommandLine(t *testing.T) {
	want := Params{TarBytes: 10240, Entries: 7, ContentBytes: 0, DecompressedBytes: 20480}
	args := want.KernelArgs()

	// A name with a dot would be kept by the kernel as a module parameter.
	if strings.ContainsAny(args, `."'\`) {
		t.Errorf("kernel args %q hold a character the kernel treats specially", args)
	}
	got, requested, err := ParamsFromEnv(envLookup(kernelEnv("console=ttyS0 reboot=k init=/sandbox-daemon " + args)))
	if err != nil || !requested || got != want {
		t.Fatalf("ParamsFromEnv() = %+v, %v, %v; want %+v", got, requested, err, want)
	}
}

func TestParamsFromEnvLeavesNormalStartsAlone(t *testing.T) {
	_, requested, err := ParamsFromEnv(envLookup(kernelEnv("console=ttyS0 init=/sandbox-daemon ip=172.16.0.10::172.16.0.1:255.255.255.0::eth0:off")))
	if requested || err != nil {
		t.Fatalf("ParamsFromEnv() = requested %v, %v; want a normal start", requested, err)
	}
}

func TestParamsFromEnvRefusesIncompleteOrInvalidParameters(t *testing.T) {
	valid := map[string]string{envTarBytes: "1024", envEntries: "3", envContentBytes: "12", envDecompressedBytes: "2048"}
	with := func(name, value string) map[string]string {
		env := map[string]string{}
		for key, val := range valid {
			env[key] = val
		}
		env[name] = value
		return env
	}
	without := func(name string) map[string]string {
		env := with(name, "")
		delete(env, name)
		return env
	}
	for name, env := range map[string]map[string]string{
		"tar bytes missing":              without(envTarBytes),
		"entries missing":                without(envEntries),
		"content bytes missing":          without(envContentBytes),
		"decompressed bytes missing":     without(envDecompressedBytes),
		"empty":                          with(envEntries, ""),
		"negative":                       with(envContentBytes, "-1"),
		"signed":                         with(envEntries, "+3"),
		"hex":                            with(envEntries, "0x3"),
		"spaces":                         with(envEntries, " 3"),
		"fraction":                       with(envTarBytes, "1024.5"),
		"text":                           with(envTarBytes, "many"),
		"over int64":                     with(envTarBytes, "9223372036854775808"),
		"zero tar bytes":                 with(envTarBytes, "0"),
		"zero entries":                   with(envEntries, "0"),
		"zero decompressed bytes":        with(envDecompressedBytes, "0"),
		"too many tar bytes":             with(envTarBytes, "1099511627777"),
		"too many entries":               with(envEntries, "268435457"),
		"too many content":               with(envContentBytes, "1099511627777"),
		"too many decompressed":          with(envDecompressedBytes, "4398046511105"),
		"more content than decompressed": with(envContentBytes, "2049"),
	} {
		params, requested, err := ParamsFromEnv(envLookup(env))
		if !requested || err == nil || params != (Params{}) {
			t.Errorf("%s: ParamsFromEnv() = %+v, requested %v, %v; want build mode requested and refused", name, params, requested, err)
		}
	}

	if params, _, err := ParamsFromEnv(envLookup(with(envContentBytes, "0"))); err != nil || params.ContentBytes != 0 {
		t.Errorf("a tar without content: %+v, %v", params, err)
	}
}

func TestDriveBytesPadsToASector(t *testing.T) {
	for tarBytes, want := range map[int64]int64{1: 512, 511: 512, 512: 512, 513: 1024, 10240: 10240} {
		if got := DriveBytes(tarBytes); got != want {
			t.Errorf("DriveBytes(%d) = %d, want %d", tarBytes, got, want)
		}
	}
}

func TestDevicePathsFollowTheAttachOrder(t *testing.T) {
	for drive, want := range map[int]string{DriveRootfs: "/dev/vda", DriveTar: "/dev/vdb", DriveOutput: "/dev/vdc", DriveStatus: "/dev/vdd"} {
		if got := devicePath(drive); got != want {
			t.Errorf("drive %d is %s, want %s", drive, got, want)
		}
	}
}

func TestStatusRoundTrip(t *testing.T) {
	for _, want := range []Status{
		{Version: statusVersion, Result: ResultOK, Files: 3, Bytes: 1 << 40, UsedBytes: 1 << 41, FormatMs: 12, ExtractMs: 3456, SyncMs: 7, FsckMs: 89},
		{Version: statusVersion, Result: ResultFailed, Reason: ReasonNoSpace, Files: 1, ExtractMs: 5},
	} {
		record, err := want.encode()
		if err != nil {
			t.Fatalf("encode(%+v) failed: %v", want, err)
		}
		if len(record) != StatusBytes {
			t.Fatalf("record has %d bytes, want %d", len(record), StatusBytes)
		}
		got, err := ParseStatus(record)
		if err != nil || got != want {
			t.Errorf("ParseStatus() = %+v, %v; want %+v", got, err, want)
		}
	}
}

func TestParseStatusAcceptsEveryReason(t *testing.T) {
	for _, reason := range []string{ReasonTar, ReasonTooLarge, ReasonNoSpace, ReasonFormat, ReasonMount, ReasonFsck, ReasonInternal} {
		record, err := Status{Version: statusVersion, Result: ResultFailed, Reason: reason}.encode()
		if err != nil {
			t.Fatal(err)
		}
		if status, err := ParseStatus(record); err != nil || status.Reason != reason {
			t.Errorf("reason %q: %+v, %v", reason, status, err)
		}
	}
}

func TestParseStatusRefusesHostileRecords(t *testing.T) {
	padded := func(text string) []byte {
		record := make([]byte, StatusBytes)
		copy(record, text)
		return record
	}
	const ok = `{"version":1,"result":"ok","files":1,"bytes":2,"used_bytes":3,"format_ms":4,"extract_ms":5,"sync_ms":6,"fsck_ms":7}`
	if _, err := ParseStatus(padded(ok)); err != nil {
		t.Fatalf("the reference record is refused: %v", err)
	}

	hostile := map[string][]byte{
		"never written":            make([]byte, StatusBytes),
		"too short":                []byte(ok),
		"too long":                 append(padded(ok), 0),
		"no padding, no end":       bytes.Repeat([]byte("{"), StatusBytes),
		"not JSON":                 padded("ok"),
		"an array":                 padded(`[1]`),
		"null":                     padded(`null`),
		"a string":                 padded(`"ok"`),
		"data after the record":    append(padded(ok)[:StatusBytes-1], 'x'),
		"a second record":          padded(ok + ok),
		"a second value":           padded(ok + ` "x"`),
		"a record after the zeros": padded(ok + "\x00" + ok),
		"an unknown field":         padded(`{"version":1,"result":"ok","message":"hello"}`),
		"a newer version":          padded(`{"version":2,"result":"ok"}`),
		"no version":               padded(`{"result":"ok"}`),
		"no result":                padded(`{"version":1}`),
		"an unknown result":        padded(`{"version":1,"result":"success"}`),
		"ok with a reason":         padded(`{"version":1,"result":"ok","reason":"tar"}`),
		"failed without a reason":  padded(`{"version":1,"result":"failed"}`),
		"an unknown reason":        padded(`{"version":1,"result":"failed","reason":"timeout"}`),
		"a reason with text":       padded(`{"version":1,"result":"failed","reason":"tar\nlevel=error msg=forged"}`),
		"a reason in other case":   padded(`{"version":1,"result":"failed","reason":"TAR"}`),
		"a number as a string":     padded(`{"version":1,"result":"ok","files":"1"}`),
		"a fraction":               padded(`{"version":1,"result":"ok","files":1.5}`),
		"a negative count":         padded(`{"version":1,"result":"ok","bytes":-1}`),
		"a negative duration":      padded(`{"version":1,"result":"ok","fsck_ms":-1}`),
		"a number over int64":      padded(`{"version":1,"result":"ok","bytes":9223372036854775808}`),
		"a nested object":          padded(`{"version":1,"result":"ok","files":{"a":1}}`),
		"deep nesting":             padded(strings.Repeat("[", 2000)),
		// The same values as the reference record, spelled another way.
		"counts left out":     padded(`{"version":1,"result":"ok"}`),
		"keys in other case":  padded(strings.Replace(ok, `"result"`, `"RESULT"`, 1)),
		"a key twice":         padded(strings.Replace(ok, `"result":"ok"`, `"result":"failed","result":"ok"`, 1)),
		"null for the reason": padded(strings.Replace(ok, `"files":1`, `"reason":null,"files":1`, 1)),
		"spaces":              padded(strings.ReplaceAll(ok, `,`, `, `)),
		"another field order": padded(strings.Replace(ok, `"version":1,"result":"ok"`, `"result":"ok","version":1`, 1)),
		"an exponent":         padded(strings.Replace(ok, `"files":1`, `"files":1e0`, 1)),
	}
	for name, record := range hostile {
		status, err := ParseStatus(record)
		if err == nil {
			t.Errorf("%s: accepted as %+v", name, status)
			continue
		}
		if status != (Status{}) {
			t.Errorf("%s: refused, but returned %+v", name, status)
		}
		// Errors end up in runner logs: fixed text and numbers only, nothing
		// quoted from the record.
		if !parseStatusError.MatchString(err.Error()) {
			t.Errorf("%s: the error is not one ParseStatus writes: %v", name, err)
		}
	}
}

var parseStatusError = regexp.MustCompile(`^status record (has \d+ bytes, want \d+|is empty|has data after its end|` +
	`is not the expected JSON object|holds more than one JSON value|has version -?\d+, want \d+|` +
	`has an unknown result or reason|has a negative count|is not written as the builder writes it)$`)

func TestInodesCoverEachEntryTwice(t *testing.T) {
	for _, entries := range []int64{1, 2, 1000, maxEntries} {
		if got, need := Inodes(entries), 2*entries+reservedInodes; got < need {
			t.Errorf("Inodes(%d) = %d, fewer than %d", entries, got, need)
		}
	}
	if Inodes(maxEntries) >= 1<<32 {
		t.Errorf("Inodes(%d) = %d does not fit ext4's 32-bit inode numbers", int64(maxEntries), Inodes(maxEntries))
	}
}

func TestImageBytes(t *testing.T) {
	const mib = 1 << 20
	tests := []struct {
		name     string
		params   Params
		min, max int64
	}{
		// 269 inodes and 256 spare blocks dominate.
		{"one empty file", Params{TarBytes: 1024, Entries: 1}, 1 * mib, 2 * mib},
		// Two blocks, two inodes and an eighth of a block each: about 9 KiB.
		{"many tiny files", Params{TarBytes: 1, Entries: 20000, ContentBytes: 20000}, 170 * mib, 200 * mib},
		{"one large file", Params{TarBytes: 1, Entries: 1, ContentBytes: 1500 * mib}, 1500 * mib, 1550 * mib},
		{"a deep tree", Params{TarBytes: 1, Entries: 1000}, 8 * mib, 12 * mib},
		{"the largest request", Params{TarBytes: maxBytes, Entries: maxEntries, ContentBytes: maxBytes}, maxBytes, 4 * maxBytes},
	}
	for _, tt := range tests {
		got := ImageBytes(tt.params)
		if got < tt.min || got > tt.max {
			t.Errorf("%s: %d bytes (%d MiB), want %d to %d MiB", tt.name, got, got/mib, tt.min/mib, tt.max/mib)
		}
		if got%blockBytes != 0 {
			t.Errorf("%s: %d bytes is not whole blocks", tt.name, got)
		}
		if payload := tt.params.ContentBytes + 2*blockBytes*tt.params.Entries; got < payload {
			t.Errorf("%s: %d bytes cannot hold the content and two blocks per entry (%d)", tt.name, got, payload)
		}
	}
}

func TestImageBytesNeverShrinksWithMoreToHold(t *testing.T) {
	// Steps that cross block, inode table block and block group boundaries.
	previous := int64(0)
	for content := int64(0); content < 600<<20; content += 7<<20 + 123 {
		got := ImageBytes(Params{TarBytes: 1, Entries: 10, ContentBytes: content})
		if got < previous {
			t.Fatalf("%d content bytes take %d, fewer than the step before (%d)", content, got, previous)
		}
		previous = got
	}
	previous = 0
	for entries := int64(1); entries < 200000; entries += 997 {
		got := ImageBytes(Params{TarBytes: 1, Entries: entries, ContentBytes: 1 << 20})
		if got < previous {
			t.Fatalf("%d entries take %d, fewer than the step before (%d)", entries, got, previous)
		}
		previous = got
	}
}
