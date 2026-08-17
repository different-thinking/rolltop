// File overview: Resolution of the configured soft heap ceiling.

package memlimit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseRequestAcceptsSizesSharesAndOff(t *testing.T) {
	cases := []struct {
		raw  string
		want Request
	}{
		{"", Request{Percent: DefaultPercent}},
		{"  ", Request{Percent: DefaultPercent}},
		{"auto", Request{Percent: DefaultPercent}},
		{"off", Request{Disabled: true}},
		{"OFF", Request{Disabled: true}},
		{"0", Request{Disabled: true}},
		{"unlimited", Request{Disabled: true}},
		{"60%", Request{Percent: 60}},
		{" 100 %", Request{Percent: 100}},
		{"768MiB", Request{Bytes: 768 << 20}},
		{"768mb", Request{Bytes: 768 << 20}},
		{"2G", Request{Bytes: 2 << 30}},
		{"1073741824", Request{Bytes: 1 << 30}},
	}
	for _, tc := range cases {
		got, err := ParseRequest(tc.raw)
		if err != nil {
			t.Fatalf("ParseRequest(%q) error = %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("ParseRequest(%q) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}
}

func TestParseRequestRejectsUnusableValues(t *testing.T) {
	for _, raw := range []string{"5%", "150%", "many%", "16MiB", "12PB", "MiB", "-1GB", "1.5GB"} {
		if got, err := ParseRequest(raw); err == nil {
			t.Fatalf("ParseRequest(%q) = %+v, want an error", raw, got)
		}
	}
}

func TestResolvePrefersOperatorSettingsOverDetection(t *testing.T) {
	const detected = 4 << 30

	applied := resolve(DefaultRequest(), "", 0, detected, SourceCgroup)
	if applied.Source != SourceCgroup || applied.Bytes != detected*DefaultPercent/100 || applied.Percent != DefaultPercent {
		t.Fatalf("default share = %+v", applied)
	}

	applied = resolve(Request{Percent: 50}, "", 0, detected, SourceMemTotal)
	if applied.Bytes != detected/2 || applied.Source != SourceMemTotal {
		t.Fatalf("configured share = %+v", applied)
	}

	applied = resolve(Request{Bytes: 512 << 20}, "", 0, detected, SourceCgroup)
	if applied.Bytes != 512<<20 || applied.Source != SourceConfig {
		t.Fatalf("absolute limit = %+v", applied)
	}

	applied = resolve(Request{Disabled: true}, "", 0, detected, SourceCgroup)
	if applied.Bytes != 0 || applied.Source != SourceOff {
		t.Fatalf("disabled limit = %+v", applied)
	}

	// An operator who set GOMEMLIMIT already told the runtime what to do, and
	// the runtime applied it before this process reached configuration.
	applied = resolve(Request{Bytes: 512 << 20}, "900MiB", 900<<20, detected, SourceCgroup)
	if applied.Source != SourceEnvGoMem || applied.Bytes != 900<<20 {
		t.Fatalf("GOMEMLIMIT limit = %+v", applied)
	}
}

func TestResolveLeavesRuntimeUnboundedWithoutAUsableCeiling(t *testing.T) {
	applied := resolve(DefaultRequest(), "", 0, 0, sourceUndetected)
	if applied.Bytes != 0 || applied.Source != SourceOff {
		t.Fatalf("undetected limit = %+v", applied)
	}
	// A ceiling this small would keep the collector busy without ever freeing
	// enough to finish a sync turn, which is worse than no ceiling.
	applied = resolve(DefaultRequest(), "", 0, 32<<20, SourceCgroup)
	if applied.Bytes != 0 || applied.Source != SourceOff {
		t.Fatalf("tiny container limit = %+v", applied)
	}
}

func TestDetectLimitReadsCgroupValuesAndSkipsUnlimitedOnes(t *testing.T) {
	dir := t.TempDir()
	limited := filepath.Join(dir, "memory.max")
	unlimited := filepath.Join(dir, "memory.unlimited")
	v1Unlimited := filepath.Join(dir, "memory.limit_in_bytes")
	if err := os.WriteFile(limited, []byte("2147483648\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unlimited, []byte("max\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(v1Unlimited, []byte("9223372036854771712\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := readCgroupLimitFile(limited); !ok || got != 2<<30 {
		t.Fatalf("cgroup v2 limit = %d, %t", got, ok)
	}
	if got, ok := readCgroupLimitFile(unlimited); ok {
		t.Fatalf("cgroup \"max\" reported a limit of %d", got)
	}
	if got, ok := readCgroupLimitFile(v1Unlimited); ok {
		t.Fatalf("cgroup v1 unlimited reported a limit of %d", got)
	}
	if got, ok := readCgroupLimitFile(filepath.Join(dir, "missing")); ok {
		t.Fatalf("missing cgroup file reported a limit of %d", got)
	}

	// The container limit wins over the machine's memory, because the container
	// limit is what stops the process.
	previousCgroup, previousMemInfo := cgroupLimitPaths, memInfoPath
	t.Cleanup(func() { cgroupLimitPaths, memInfoPath = previousCgroup, previousMemInfo })
	memInfo := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(memInfo, []byte("MemTotal:       16384000 kB\nMemFree: 1 kB\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	memInfoPath = memInfo
	cgroupLimitPaths = []string{unlimited, limited}
	if got, source := DetectLimit(); got != 2<<30 || source != SourceCgroup {
		t.Fatalf("DetectLimit() = %d, %q, want the cgroup limit", got, source)
	}
	cgroupLimitPaths = []string{unlimited}
	if got, source := DetectLimit(); got != 16384000*1024 || source != SourceMemTotal {
		t.Fatalf("DetectLimit() = %d, %q, want MemTotal", got, source)
	}
	memInfoPath = filepath.Join(dir, "missing")
	if got, source := DetectLimit(); got != 0 || source != sourceUndetected {
		t.Fatalf("DetectLimit() = %d, %q, want no detected limit", got, source)
	}
}

func TestAppliedDescriptionNamesItsSource(t *testing.T) {
	cases := map[string]Applied{
		"memory limit off, the Go heap may grow until the operating system intervenes": {Source: SourceOff},
		"memory limit 900.0MiB from GOMEMLIMIT":                                        {Bytes: 900 << 20, Source: SourceEnvGoMem},
		"memory limit 512.0MiB from ROLLTOP_MEMORY_LIMIT":                              {Bytes: 512 << 20, Source: SourceConfig},
		"memory limit 1.6GiB (80% of the 2.0GiB cgroup limit)": {
			Bytes: 2 << 30 * 80 / 100, Source: SourceCgroup, Detected: 2 << 30, Percent: 80,
		},
	}
	for want, applied := range cases {
		if got := applied.Description(); got != want {
			t.Fatalf("Description() = %q, want %q", got, want)
		}
	}
}
