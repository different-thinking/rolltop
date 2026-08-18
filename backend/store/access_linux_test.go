//go:build linux

// File overview: Classification of the filesystems under a data directory.

package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// The volume that made this necessary was CephFS handed to a guest through
// virtiofs, which reports the FUSE superblock.
func TestFilesystemClassificationCoversNetworkAndFUSEVolumes(t *testing.T) {
	for magic, name := range map[int64]string{
		0x65735546: "fuse",
		0x00c36400: "cephfs",
		0x6969:     "nfs",
		0xff534d42: "cifs",
	} {
		got, ok := sharedMemoryUnsafeFilesystems[magic]
		if !ok || !strings.Contains(got, name) {
			t.Fatalf("filesystem 0x%x = %q, %t, want %q to be listed as unsafe", magic, got, ok, name)
		}
		if _, both := sharedMemorySafeFilesystems[magic]; both {
			t.Fatalf("filesystem 0x%x is listed as both safe and unsafe", magic)
		}
	}
	for _, magic := range []int64{0xef53, 0x58465342, 0x01021994} {
		if _, ok := sharedMemorySafeFilesystems[magic]; !ok {
			t.Fatalf("local filesystem 0x%x is not listed as safe", magic)
		}
	}
}

// A filesystem this list has not heard of keeps the behavior it had, and says
// which one it was so it can be classified later.
func TestDetectFilesystemNamesWhatItFound(t *testing.T) {
	report := DetectFilesystem(t.TempDir())
	if report.Name == "" {
		t.Fatal("DetectFilesystem returned no name")
	}
	if strings.HasPrefix(report.Name, "unknown (") && !report.SharedMemorySafe {
		t.Fatalf("unknown filesystem %q was treated as unsafe", report.Name)
	}
	// A path with no inspectable ancestor at all is the only case left that
	// cannot be classified.
	if unstattable := DetectFilesystem(""); unstattable.Name != "unknown" || !unstattable.SharedMemorySafe {
		t.Fatalf("unstattable path = %+v", unstattable)
	}
}

// A first start creates the data directory, so detection has to describe the
// mount it will be created in rather than giving up on the missing path. Getting
// this wrong picks shared mode on exactly the volumes that cannot host it.
func TestDetectFilesystemWalksUpToAnExistingAncestor(t *testing.T) {
	existing := t.TempDir()
	want := DetectFilesystem(existing)
	missing := filepath.Join(existing, "users", "1", "not-created-yet")
	if got := DetectFilesystem(missing); got != want {
		t.Fatalf("DetectFilesystem(%q) = %+v, want the ancestor's %+v", missing, got, want)
	}
	if magic, ok := filesystemMagic(missing); !ok || magic == 0 {
		t.Fatalf("filesystemMagic(%q) = 0x%x, %t", missing, magic, ok)
	}
}
