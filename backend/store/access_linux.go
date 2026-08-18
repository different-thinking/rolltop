//go:build linux

// File overview: Filesystem identification for the SQLite access mode. Only the
// superblock type is read; nothing here touches the databases themselves.

package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
)

// Filesystems whose shared memory mappings and byte-range locks SQLite can
// rely on. These are local block or memory filesystems: a `-shm` mapping is
// backed by the same page cache every connection sees.
var sharedMemorySafeFilesystems = map[int64]string{
	0xef53:     "ext2/3/4",
	0x58465342: "xfs",
	0x9123683e: "btrfs",
	0x01021994: "tmpfs",
	0x2fc12fc1: "zfs",
	0x794c7630: "overlayfs",
	0xf2f52010: "f2fs",
	0x3153464a: "jfs",
	0x52654973: "reiserfs",
	0xca451a4e: "bcachefs",
	0x0000482b: "hfsplus",
}

// Filesystems that carry a database over a network or through FUSE. SQLite
// documents WAL as unsupported on these: the wal-index lives in a file that
// every connection maps with MAP_SHARED, and neither the coherence of that
// mapping nor the byte-range locks are guaranteed once a file server sits in
// the path. virtiofs reports the FUSE superblock, which is how a CephFS volume
// handed to a guest shows up.
var sharedMemoryUnsafeFilesystems = map[int64]string{
	0x65735546: "fuse (virtiofs or another FUSE mount)",
	0x00c36400: "cephfs",
	0x6969:     "nfs",
	0xff534d42: "cifs",
	0xfe534d42: "smb2",
	0x517b:     "smb",
	0x01021997: "9p",
	0x5346414f: "afs",
	0x6b414653: "afs",
	0x0bd00bd0: "lustre",
	0x7461636f: "ocfs2",
	0x01161970: "gfs2",
}

// DetectFilesystem identifies the storage under a path. An unrecognized
// filesystem is reported by its superblock number and treated as safe, so a
// local filesystem this list has not heard of keeps the behavior it had.
//
// A data directory that does not exist yet is the normal case on a first start,
// and the mount it will be created in is what matters, so the walk continues up
// to the nearest existing ancestor rather than giving up on the first ENOENT.
func DetectFilesystem(path string) FilesystemReport {
	magic, ok := filesystemMagic(path)
	if !ok {
		return FilesystemReport{Name: "unknown", SharedMemorySafe: true}
	}
	if name, ok := sharedMemoryUnsafeFilesystems[magic]; ok {
		return FilesystemReport{Name: name, SharedMemorySafe: false}
	}
	if name, ok := sharedMemorySafeFilesystems[magic]; ok {
		return FilesystemReport{Name: name, SharedMemorySafe: true}
	}
	return FilesystemReport{Name: fmt.Sprintf("unknown (0x%x)", magic), SharedMemorySafe: true}
}

func filesystemMagic(path string) (int64, bool) {
	for path != "" {
		var stat syscall.Statfs_t
		if err := syscall.Statfs(path, &stat); err == nil {
			return int64(stat.Type), true
		} else if !errors.Is(err, syscall.ENOENT) {
			return 0, false
		}
		parent := filepath.Dir(path)
		if parent == path {
			return 0, false
		}
		path = parent
	}
	return 0, false
}
